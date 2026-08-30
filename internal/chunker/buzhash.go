// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/chunkers/buzhash64.pyx and
// src/borg/chunkers/buzhash.pyx, and the sequential kernels of their _impl.c files.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package chunker

import (
	"fmt"
	"io"
	"math/bits"
)

// The buzhash family: a cyclic polynomial rolling hash over a sliding window.
//
// A known property, documented upstream: the hash is designed for inputs up to 64
// bytes but the chunker runs it over a 4095-byte window, so bytes repeating at a
// distance of 64 within the window can cancel each other out. That is a real weakness
// of the construction, and it is part of why fastcdc is borg 2's default. It is
// reproduced faithfully here regardless - the point is compatibility, not improvement.

// ---------------------------------------------------------------------- buzhash64

// Buzhash64 is the 64-bit keyed buzhash chunker.
//
// Its table is *balanced*: for every one of the 64 bit positions, exactly 128 of the
// 256 entries have that bit set. borg builds it by shuffling the indices 0..255 once
// per bit position with the CSPRNG and setting the bit on the first 128. Both the
// shuffle and the order it runs in are format-visible.
type Buzhash64 struct {
	cfg      config
	table    [256]uint64
	tableRot [256]uint64 // table[b] rotated left by windowSize%64, saving a rotate per byte
	window   int
	d        windowDriver
}

func newBuzhash64(p Params, key []byte, r io.Reader) (*Buzhash64, error) {
	cfg, err := newConfig(AlgoBuzhash64, p, false) // uniform output: low-bit masks
	if err != nil {
		return nil, err
	}
	if p.WindowSize <= 0 {
		return nil, fmt.Errorf("chunker: buzhash64 needs a positive window size")
	}
	table, err := buzhash64Table(key)
	if err != nil {
		return nil, err
	}

	c := &Buzhash64{cfg: cfg, table: table, window: p.WindowSize}
	lenmod := uint(p.WindowSize) & 0x3f
	for i := range table {
		c.tableRot[i] = bits.RotateLeft64(table[i], int(lenmod))
	}
	if err := c.d.initWindow(&c.cfg, p.WindowSize, r); err != nil {
		return nil, err
	}
	return c, nil
}

// buzhash64Table derives the balanced keyed table (buzhash64_init_table).
func buzhash64Table(key []byte) ([256]uint64, error) {
	var table [256]uint64
	rng, err := NewCSPRNG(key)
	if err != nil {
		return table, fmt.Errorf("chunker: buzhash64 table: %w", err)
	}
	for bitPos := 0; bitPos < 64; bitPos++ {
		indices := make([]int, 256)
		for i := range indices {
			indices[i] = i
		}
		if err := rng.Shuffle(indices); err != nil {
			return table, fmt.Errorf("chunker: buzhash64 table: %w", err)
		}
		for i := 0; i < 128; i++ {
			table[indices[i]] |= uint64(1) << uint(bitPos)
		}
	}
	return table, nil
}

func (c *Buzhash64) Algorithm() string { return AlgoBuzhash64 }

// Reset points the chunker at a new stream, keeping the keyed table and its rotated
// twin - the two things that cost 4.35 ms to build.
func (c *Buzhash64) Reset(r io.Reader) { c.d.reset(r) }

func (c *Buzhash64) Next() (Chunk, error) {
	data, err := c.d.next(c.hashWindow, c.scan)
	if err != nil {
		return Chunk{}, err
	}
	return classify(data), nil
}

// hashWindow computes the hash of a full window from scratch (_buzhash64).
//
// The rotation amount decreases with distance from the end of the window, so the most
// recent byte contributes unrotated. Note the modulo 64: with a 4095-byte window the
// rotations wrap many times over, which is the cancellation property noted above.
func (c *Buzhash64) hashWindow(window []byte) uint64 {
	if len(window) == 0 {
		return 0
	}
	var sum uint64
	for i := len(window) - 1; i > 0; i-- {
		imod := uint(uint64(i) & 0x3f)
		sum ^= bits.RotateLeft64(c.table[window[len(window)-1-i]], int(imod))
	}
	return sum ^ c.table[window[len(window)-1]]
}

// scan rolls the window forward, stopping as soon as the hash matches the mask or n
// positions have been consumed (bz64_scan_seq).
//
// p starts at the current scan position: p[j] is the byte leaving the window and
// p[j+windowSize] the byte entering it.
func (c *Buzhash64) scan(p []byte, n int, sum *uint64, mask uint64) int {
	s := *sum
	j := 0
	for j < n && s&mask != 0 {
		s = bits.RotateLeft64(s, 1) ^ c.tableRot[p[j]] ^ c.table[p[j+c.window]]
		j++
	}
	*sum = s
	return j
}

// Table exposes the derived table for tests and the differential harness.
func (c *Buzhash64) Table() [256]uint64 { return c.table }

// ------------------------------------------------------------------------ buzhash

// Buzhash is the 32-bit buzhash chunker borg 1.x used.
//
// It is kept bit-compatible with borg 1.x, which is why it has no nc_level parameter
// and why its table is a fixed constant XORed with an integer seed rather than being
// derived through the CSPRNG. Both are format constraints, not oversights.
type Buzhash struct {
	cfg    config
	table  [256]uint32
	window int
	d      windowDriver
}

func newBuzhash(p Params, seed uint32, r io.Reader) (*Buzhash, error) {
	if p.NCLevel != 0 {
		return nil, fmt.Errorf("chunker: buzhash has no nc_level parameter; it must stay " +
			"bit-compatible with borg 1.x")
	}
	cfg, err := newConfig(AlgoBuzhash, p, false)
	if err != nil {
		return nil, err
	}
	if p.WindowSize <= 0 {
		return nil, fmt.Errorf("chunker: buzhash needs a positive window size")
	}

	c := &Buzhash{cfg: cfg, window: p.WindowSize}
	for i := range c.table {
		c.table[i] = buzhashTableBase[i] ^ seed
	}
	if err := c.d.initWindow(&c.cfg, p.WindowSize, r); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Buzhash) Algorithm() string { return AlgoBuzhash }

// Reset points the chunker at a new stream, keeping the seeded table.
func (c *Buzhash) Reset(r io.Reader) { c.d.reset(r) }

func (c *Buzhash) Next() (Chunk, error) {
	data, err := c.d.next(c.hashWindow, c.scan)
	if err != nil {
		return Chunk{}, err
	}
	return classify(data), nil
}

func (c *Buzhash) hashWindow(window []byte) uint64 {
	if len(window) == 0 {
		return 0
	}
	var sum uint32
	for i := len(window) - 1; i > 0; i-- {
		imod := uint(uint32(i) & 0x1f) // 32-bit rotation, so modulo 32
		sum ^= bits.RotateLeft32(c.table[window[len(window)-1-i]], int(imod))
	}
	return uint64(sum ^ c.table[window[len(window)-1]])
}

func (c *Buzhash) scan(p []byte, n int, sum *uint64, mask uint64) int {
	s := uint32(*sum)
	m := uint32(mask)
	lenmod := int(uint32(c.window) & 0x1f)
	j := 0
	for j < n && s&m != 0 {
		s = bits.RotateLeft32(s, 1) ^ bits.RotateLeft32(c.table[p[j]], lenmod) ^ c.table[p[j+c.window]]
		j++
	}
	*sum = uint64(s)
	return j
}

// Table exposes the seeded table for tests.
func (c *Buzhash) Table() [256]uint32 { return c.table }

// -------------------------------------------------------------------------- fixed

// Fixed cuts at fixed offsets, with an optional differently-sized header block.
//
// It exists for data that stays at stable offsets - raw disk images, block devices,
// database files with a header plus fixed-size records - where content-defined
// chunking buys nothing and costs a rolling hash over every byte.
type Fixed struct {
	blockSize  int
	headerSize int
	r          io.Reader
	buf        []byte
	headerDone bool
	done       bool
}

func newFixed(p Params, r io.Reader) (*Fixed, error) {
	if p.BlockSize <= 0 {
		return nil, fmt.Errorf("chunker: fixed chunker needs a positive block size")
	}
	if p.HeaderSize < 0 {
		return nil, fmt.Errorf("chunker: negative header size %d", p.HeaderSize)
	}
	size := p.BlockSize
	if p.HeaderSize > size {
		size = p.HeaderSize
	}
	return &Fixed{blockSize: p.BlockSize, headerSize: p.HeaderSize, r: r, buf: make([]byte, size)}, nil
}

func (c *Fixed) Algorithm() string { return AlgoFixed }

// Reset points the chunker at a new stream. Fixed has no table, but it does have a buffer
// worth keeping, and the header state has to go or the next stream would be read as a
// continuation of the previous one.
func (c *Fixed) Reset(r io.Reader) {
	c.r = r
	c.headerDone = false
	c.done = false
}

func (c *Fixed) Next() (Chunk, error) {
	if c.done {
		return Chunk{}, io.EOF
	}
	want := c.blockSize
	if !c.headerDone && c.headerSize > 0 {
		want = c.headerSize
	}
	c.headerDone = true

	n, err := io.ReadFull(c.r, c.buf[:want])
	switch {
	case err == nil:
	case err == io.EOF:
		c.done = true
		return Chunk{}, io.EOF
	case err == io.ErrUnexpectedEOF:
		// A short final block is expected and not an error: the last block of a data or
		// hole range may be smaller than the block size.
		c.done = true
	default:
		return Chunk{}, fmt.Errorf("chunker: read: %w", err)
	}
	if n == 0 {
		c.done = true
		return Chunk{}, io.EOF
	}
	return classify(c.buf[:n]), nil
}
