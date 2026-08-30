// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ChunkerBase.process/fill in borg's
// src/borg/chunkers/base.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package chunker

import (
	"errors"
	"fmt"
	"io"
)

// readerBlockSize is borg's reader_block_size: how much it asks the file reader for at
// a time. It does not affect where chunks are cut, only how often the reader is called.
const readerBlockSize = 1024 * 1024

// driver implements the buffering and cut loop shared by the window-less chunkers
// (fastcdc, and upstream's AES chunkers). The windowed ones (buzhash, buzhash64) need a
// different loop and have their own; see windowDriver.
//
// borg keeps one buffer of exactly max_size bytes and moves two indices through it,
// compacting lazily. borge keeps a slice that starts at the current chunk's first byte,
// which is the same thing with the bookkeeping made explicit: buf[:pos] is the chunk so
// far, buf[pos:] is buffered but unscanned, and the buffer is never allowed past
// maxSize - which is what enforces the maximum chunk size.
type driver struct {
	cfg *config
	r   io.Reader

	// backing is exactly maxSize bytes, which is what bounds the chunk size: when it
	// is full and no cut has been found, the caller's loop cuts there.
	backing []byte
	n       int // valid bytes in backing, starting at the current chunk's first byte
	pos     int // scan position within backing
	pending int // bytes of the previously emitted chunk, not yet compacted away

	eof  bool
	done bool

	bytesRead    int64
	bytesYielded int64
}

func (d *driver) init(cfg *config, r io.Reader) {
	d.cfg = cfg
	d.backing = make([]byte, cfg.maxSize)
	d.reset(r)
}

// reset points the driver at a new stream, keeping the buffer it already has.
//
// Everything below the buffer is per-stream state and must go, or the next file would
// inherit the previous one's and be chunked differently - a correctness failure, not a
// performance one. TestResetChunksIdentically holds it, and was checked by omitting each
// assignment in turn: n, pending, eof, done and bytesRead each make it fail.
//
// pos is the exception and is reset defensively rather than necessarily. Both scan loops
// assign it (d.pos = cfg.minSize) before reading it, so a stale value is unobservable
// today and omitting the line here does not fail the test. It is reset anyway, because
// "unobservable today" is a property of the two call sites rather than of the field, and
// the cost is one store per file.
func (d *driver) reset(r io.Reader) {
	d.r = r
	d.n = 0
	d.pos = 0
	d.pending = 0
	d.eof = false
	d.done = false
	d.bytesRead = 0
	d.bytesYielded = 0
}

// buf is the live window: the current chunk so far plus everything buffered after it.
func (d *driver) buf() []byte { return d.backing[:d.n] }

// compact drops the previously emitted chunk from the front of the buffer.
//
// It is deferred to the start of the next call rather than done at emit time so the
// slice handed to the caller stays valid until then - that is the contract Chunker
// documents, and it saves a copy of every chunk.
func (d *driver) compact() {
	if d.pending == 0 {
		return
	}
	copy(d.backing, d.backing[d.pending:d.n])
	d.n -= d.pending
	d.pending = 0
}

// emit hands back the first pos bytes as a chunk and schedules their removal.
func (d *driver) emit(pos int) []byte {
	d.pending = pos
	d.bytesYielded += int64(pos)
	return d.backing[:pos]
}

// fill reads more data, never letting the buffer exceed maxSize.
//
// The cap is what enforces the maximum chunk size: when the buffer is full and no cut
// has been found, the caller's loop sees that fill added nothing and cuts there.
func (d *driver) fill() error {
	if d.eof {
		return nil
	}
	space := len(d.backing) - d.n
	if space <= 0 {
		return nil
	}
	want := readerBlockSize
	if want > space {
		want = space
	}

	n, err := io.ReadFull(d.r, d.backing[d.n:d.n+want])
	d.n += n
	d.bytesRead += int64(n)

	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("chunker: read: %w", err)
	}

	// EOF is flagged only when a read returns *zero* bytes, never on a short read -
	// exactly what borg's fill() does ("if n > 0: remaining += n; else: eof = 1").
	//
	// This is not a detail. A short read followed by a later zero-length read gives the
	// windowed chunkers one more round of scanning before eof is set, and they emit
	// everything buffered the moment it is. Treating a short read as EOF made borge
	// merge the last few chunks of every file into one - identical boundaries
	// everywhere else, then a silent divergence in the tail.
	if n == 0 {
		d.eof = true
	}
	return nil
}

// scanFunc looks for a cut point in p, updating the rolling hash. It returns the index
// of the matching byte, or -1.
type scanFunc func(p []byte, digest *uint64, mask uint64) int

// restartFunc gives the hash state at the first scan position of a new chunk. Window-less
// hashes restart from zero.
type restartFunc func() uint64

// next produces the next chunk, or io.EOF.
func (d *driver) next(scan scanFunc, restart restartFunc) ([]byte, error) {
	cfg := d.cfg
	d.compact()

	if d.done {
		if d.bytesRead != d.bytesYielded {
			return nil, fmt.Errorf("chunker: byte count mismatch: read %d, yielded %d",
				d.bytesRead, d.bytesYielded)
		}
		return nil, io.EOF
	}

	// Ensure at least minSize+1 bytes are buffered, or that the stream is exhausted.
	for d.n < cfg.minSize+1 && !d.eof {
		if err := d.fill(); err != nil {
			return nil, err
		}
	}

	// At EOF with less than minSize+1 bytes left: the remainder is the final chunk. It
	// is emitted without a content-defined cut, because there is nowhere to cut.
	if d.eof && d.n < cfg.minSize+1 {
		d.done = true
		if d.n == 0 {
			if d.bytesRead != d.bytesYielded {
				return nil, fmt.Errorf("chunker: byte count mismatch: read %d, yielded %d",
					d.bytesRead, d.bytesYielded)
			}
			return nil, io.EOF
		}
		return d.emit(d.n), nil
	}

	// No cut is allowed below minSize, so skip that region without hashing it. This is
	// FastCDC's sub-minimum cut-point skipping, and it is why the hash restarts at
	// minSize into the chunk rather than at its first byte.
	d.pos = cfg.minSize
	digest := restart()

	for {
		mask := cfg.maskFor(d.pos)

		if d.pos >= d.n {
			if d.eof {
				break // cut at end of data
			}
			before := d.n
			if err := d.fill(); err != nil {
				return nil, err
			}
			if d.n == before {
				break // buffer full: the chunk reached maxSize, force a cut
			}
			continue
		}

		stop := d.n
		if cfg.ncLevel != 0 && d.pos < cfg.normalSize {
			// Do not scan past the strict-to-loose transition; the mask has to be
			// re-evaluated exactly there, or chunks near the boundary get cut with the
			// wrong probability and every subsequent boundary shifts.
			if cfg.normalSize < stop {
				stop = cfg.normalSize
			}
		}

		if r := scan(d.backing[d.pos:stop], &digest, mask); r >= 0 {
			d.pos += r + 1 // cut immediately after the matching byte
			break
		}
		d.pos = stop
	}

	return d.emit(d.pos), nil
}

// windowDriver implements the loop the windowed chunkers need (buzhash, buzhash64).
//
// It differs from driver in two ways that both affect where chunks are cut:
//
//   - The hash is computed over a window of windowSize bytes starting at the scan
//     position, so scanning must stop windowSize bytes short of the buffered data - a
//     cut decision needs the whole window to be present.
//   - Reaching EOF ends the chunk immediately, emitting everything buffered as the
//     final chunk, rather than continuing to look for a cut in what is left.
type windowDriver struct {
	driver
	windowSize int
}

func (d *windowDriver) initWindow(cfg *config, windowSize int, r io.Reader) error {
	if windowSize+cfg.minSize+1 > cfg.maxSize {
		return fmt.Errorf("chunker: max chunk size %d is too small for window %d and min %d",
			cfg.maxSize, windowSize, cfg.minSize)
	}
	d.windowSize = windowSize
	d.driver.init(cfg, r)
	return nil
}

// windowScanFunc advances one byte at a time over a window. It returns how many bytes
// were consumed before a cut was found (or the whole span if none was).
type windowScanFunc func(p []byte, windowSize int, sum *uint64, mask uint64) int

// windowHashFunc computes the hash of a full window from scratch.
type windowHashFunc func(window []byte) uint64

func (d *windowDriver) next(hash windowHashFunc, scan windowScanFunc) ([]byte, error) {
	cfg := d.cfg
	d.compact()

	if d.done {
		if d.bytesRead != d.bytesYielded {
			return nil, fmt.Errorf("chunker: byte count mismatch: read %d, yielded %d",
				d.bytesRead, d.bytesYielded)
		}
		return nil, io.EOF
	}

	for d.n < cfg.minSize+d.windowSize+1 && !d.eof {
		if err := d.fill(); err != nil {
			return nil, err
		}
	}

	// Note this is *not* the same condition as the window-less driver's. borg checks
	// eof alone here, so once the stream is exhausted the whole remaining buffer is
	// emitted as one chunk without looking for a cut in it. That is a real behavioural
	// difference between the two families and it is reproduced deliberately.
	if d.eof {
		d.done = true
		if d.n == 0 {
			if d.bytesRead != d.bytesYielded {
				return nil, fmt.Errorf("chunker: byte count mismatch: read %d, yielded %d",
					d.bytesRead, d.bytesYielded)
			}
			return nil, io.EOF
		}
		return d.emit(d.n), nil
	}

	d.pos = cfg.minSize
	sum := hash(d.backing[d.pos : d.pos+d.windowSize])

	for {
		mask := cfg.maskL
		normalPos := 0
		if cfg.ncLevel != 0 {
			normalPos = cfg.normalSize
			if d.pos < normalPos {
				mask = cfg.maskS
			}
		}

		remaining := d.n - d.pos
		if !(remaining > d.windowSize && sum&mask != 0 && !(d.eof && remaining <= d.windowSize)) {
			break
		}

		stop := d.n - d.windowSize
		if cfg.ncLevel != 0 && d.pos < normalPos && normalPos < stop {
			stop = normalPos
		}

		did := scan(d.backing[d.pos:d.n], stop-d.pos, &sum, mask)
		d.pos += did

		if d.n-d.pos <= d.windowSize {
			if err := d.fill(); err != nil {
				return nil, err
			}
		}
	}

	if d.n-d.pos <= d.windowSize {
		d.pos = d.n
	}

	return d.emit(d.pos), nil
}
