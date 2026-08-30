// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the item stream reading in borg's src/borg/archive.py:
// archive_get_items, DownloadPipeline.unpack_many and DownloadPipeline.fetch_many.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package archive reads an archive: its metadata, the stream of items it contains, and
// the file contents those items point at.
//
// # The three levels of indirection
//
// An archive's metadata object does not hold its items, and does not even hold the list
// of chunks that do. It holds `item_ptrs`:
//
//	archive metadata   ->  item_ptrs:  [chunk id, ...]        type "A"
//	  each of those    ->  a block of: [chunk id, ...]        type "C"
//	    each of those  ->  a piece of the item stream         type "S"
//	      the stream   ->  msgpack items, back to back
//
// One level would be enough for a small archive. The extra one exists because an archive
// of a few million files has a chunk id list of tens of megabytes, and borg needs the
// *archive object itself* to stay small - it is read by every listing, and rewritten
// whenever the archive is touched.
//
// The item stream is a plain concatenation of msgpack maps with no framing, so an item
// may straddle a chunk boundary. That is why reading is a streaming unpack rather than
// "decode each chunk".
package archive

import (
	"errors"
	"fmt"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/msgpackx"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
)

// IDsPerChunk is how many chunk ids borg puts in one "C" block: MAX_DATA_SIZE // 40,
// 40 being a generous estimate of the msgpack size of one 32-byte id.
const IDsPerChunk = repoobj.MaxDataSize / 40

// Errors.
var (
	// ErrNoItems means the archive metadata has neither item_ptrs nor items.
	ErrNoItems = errors.New("archive: archive metadata has no item list")
	// ErrArchiveNotFound means no archive of that name or id is in the repository.
	ErrArchiveNotFound = errors.New("archive: archive not found")
)

// Archive is an opened archive, ready to be read.
type Archive struct {
	manifest *manifest.Manifest
	repo     *repository.Repository
	ro       *repoobj.RepoObj

	// ID is the chunk id of the archive's metadata object.
	ID []byte
	// Meta is the archive metadata as stored.
	Meta *item.ArchiveItem
	// Info is the summary a listing shows.
	Info manifest.Info

	// itemIDs is the resolved list of item stream chunk ids, built on first use: for a
	// large archive it is megabytes, and a caller that only wants the summary should not
	// pay for it.
	itemIDs [][]byte
}

// Open reads the archive with the given metadata chunk id.
func Open(m *manifest.Manifest, id []byte) (*Archive, error) {
	info, err := m.Archives.Get(id)
	if err != nil {
		return nil, err
	}
	if !info.Exists {
		return nil, fmt.Errorf("%w: %s", ErrArchiveNotFound, info.Problem)
	}
	meta, err := m.Archives.Item(id)
	if err != nil {
		return nil, err
	}
	return &Archive{
		manifest: m,
		repo:     m.Repository(),
		ro:       m.RepoObj(),
		ID:       append([]byte(nil), id...),
		Meta:     meta,
		Info:     *info,
	}, nil
}

// OpenByName reads the archive with the given name.
//
// Names are not unique; the most recent match wins, which is what a user naming an
// archive without an id means. Pass an id to be unambiguous.
func OpenByName(m *manifest.Manifest, name string) (*Archive, error) {
	info, err := m.Archives.ByName(name)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("%w: no archive named %q", ErrArchiveNotFound, name)
	}
	return Open(m, info.ID)
}

// ItemStreamIDs returns the chunk ids of the item metadata stream, in order.
//
// Resolving them means reading every "C" block, which is why the result is cached: a
// caller that iterates items twice should not fetch the index twice.
func (a *Archive) ItemStreamIDs() ([][]byte, error) {
	if a.itemIDs != nil {
		return a.itemIDs, nil
	}

	switch {
	case a.Meta.ItemPtrsSet:
		if a.Meta.ItemsSet {
			// borg asserts the two are mutually exclusive. An archive with both is not a
			// shape either tool produces, and guessing which to believe could restore the
			// wrong contents.
			return nil, fmt.Errorf("archive: metadata has both item_ptrs and items")
		}
		ids := make([][]byte, 0, len(a.Meta.ItemPtrs)*64)
		for _, ptr := range a.Meta.ItemPtrs {
			block, err := a.repo.Get(ptr)
			if err != nil {
				return nil, fmt.Errorf("archive: cannot read item pointer block: %w", err)
			}
			_, data, err := a.ro.Parse(ptr, block, repoobj.TypeArchiveChunkIDs, repoobj.ParseOptions{})
			if err != nil {
				return nil, err
			}
			decoded, err := msgpackx.Unmarshal(data)
			if err != nil {
				return nil, fmt.Errorf("archive: item pointer block is not msgpack: %w", err)
			}
			list, ok := decoded.([]any)
			if !ok {
				return nil, fmt.Errorf("archive: item pointer block is a %T, want a list", decoded)
			}
			for _, e := range list {
				id, ok := e.([]byte)
				if !ok {
					return nil, fmt.Errorf("archive: item pointer block holds a %T, want bytes", e)
				}
				ids = append(ids, id)
			}
		}
		a.itemIDs = ids

	case a.Meta.ItemsSet:
		// A borg 1.x archive keeps the stream chunk ids in the metadata directly. borge
		// reads them so a v1 archive can be listed and transferred; it never writes them.
		a.itemIDs = a.Meta.Items

	default:
		return nil, ErrNoItems
	}
	return a.itemIDs, nil
}

// Items calls fn for each item in the archive, in stream order.
//
// Returning ErrStopIteration from fn stops the iteration; that is not an error.
func (a *Archive) Items(fn func(*item.Item) error) error {
	return a.RawItems(func(v any) error {
		m, isMap := v.(*msgpackx.Map)
		if !isMap {
			return fmt.Errorf("archive: item stream holds a %T, want a map", v)
		}
		it, err := item.DecodeItem(m)
		if err != nil {
			return err
		}
		return fn(it)
	})
}

// RawItems calls fn for each item as the decoded msgpack value, before it becomes an Item.
//
// **Corrected 2026-08-30 by R0 T8.** This used to say that "a key borge does not know about
// is dropped", and that was the stated reason for the command. It is not true and has not
// been: Decode keeps unrecognised keys in Item.Unknown and writes them back out, and
// item.TestUnknownKeysArePreserved requires the re-encoding to be byte-identical. The claim
// was false while the code around it was correct, which is the failure mode §2.1 exists to
// catch; it also seeded a roadmap item proposing a format change to fix a problem that was
// not there.
//
// What is genuinely lost on the way to an Item is narrower: a borg 1.x chunk-list entry's
// third element, the compressed size, which borg 2 does not read either
// (TestChunkListDropsLegacyCompressedSize). borge cannot open a borg 1.x repository at all
// (§0.6), so that path is unreachable in practice.
//
// The command is still right to read the stream through here, for a reason that survives
// the correction: an Item renders the keys it models as named fields, so a key carried in
// Unknown would not appear under its own name in the JSON. `debug dump-archive` exists to
// show what is *actually* in the repository - byte order, exact encodings and all - which
// is what somebody reaching for it is asking about.
//
// Returning ErrStopIteration from fn stops the iteration; that is not an error.
func (a *Archive) RawItems(fn func(any) error) error {
	ids, err := a.ItemStreamIDs()
	if err != nil {
		return err
	}

	var u streamUnpacker
	for _, id := range ids {
		obj, err := a.repo.Get(id)
		if err != nil {
			return fmt.Errorf("archive: cannot read item stream chunk: %w", err)
		}
		_, data, err := a.ro.Parse(id, obj, repoobj.TypeArchiveStream, repoobj.ParseOptions{})
		if err != nil {
			return err
		}
		u.feed(data)

		for {
			v, ok, err := u.next()
			if err != nil {
				return err
			}
			if !ok {
				break // the rest of this item is in the next chunk
			}
			if err := fn(v); err != nil {
				if errors.Is(err, ErrStopIteration) {
					return nil
				}
				return err
			}
		}
	}

	// Anything left over is a truncated item: the stream ended mid-value. Say so rather
	// than returning a short list, which would look like a successful listing of an
	// archive that is missing files.
	if u.pending() > 0 {
		return fmt.Errorf("archive: the item stream ends with %d unusable byte(s); "+
			"the archive is truncated or damaged", u.pending())
	}
	return nil
}

// ErrStopIteration ends an Items walk early without reporting an error.
var ErrStopIteration = errors.New("archive: stop iteration")

// streamUnpacker decodes a sequence of msgpack values arriving in arbitrary pieces.
//
// The item stream has no framing, so a value may be split across chunks. Decoding stops
// cleanly when the buffer holds only part of a value and resumes once more arrives; any
// other decoding failure is a real error and is reported.
type streamUnpacker struct {
	buf []byte
	// off is how much of buf has been consumed. The consumed prefix is dropped only
	// occasionally, so a stream of small items does not copy on every value.
	off int
}

// compactThreshold is when the consumed prefix is dropped. Sixty-four kilobytes bounds
// the wasted memory while keeping the copying rare.
const compactThreshold = 64 * 1024

func (u *streamUnpacker) feed(data []byte) {
	u.compact()
	u.buf = append(u.buf, data...)
}

func (u *streamUnpacker) compact() {
	if u.off == 0 {
		return
	}
	if u.off >= len(u.buf) {
		u.buf = u.buf[:0]
		u.off = 0
		return
	}
	if u.off >= compactThreshold {
		n := copy(u.buf, u.buf[u.off:])
		u.buf = u.buf[:n]
		u.off = 0
	}
}

// pending is how many bytes are buffered but not yet decoded.
func (u *streamUnpacker) pending() int { return len(u.buf) - u.off }

// next decodes the next value. ok is false when the buffer holds only part of one.
func (u *streamUnpacker) next() (any, bool, error) {
	if u.pending() == 0 {
		return nil, false, nil
	}
	d := msgpackx.NewDecoder(u.buf[u.off:])
	v, err := d.Decode()
	if err != nil {
		if errors.Is(err, msgpackx.ErrShortBuffer) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("archive: item stream is corrupt: %w", err)
	}
	u.off += d.Pos()
	return v, true, nil
}
