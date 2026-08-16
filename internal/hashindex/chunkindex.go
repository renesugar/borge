// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the ChunkIndex class in borg's src/borg/hashindex.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package hashindex

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Sizes fixed by the format.
const (
	// ChunkIDSize is the length of a chunk id: a 256-bit keyed hash.
	ChunkIDSize = 32
	// EntrySize is the packed size of a ChunkIndexEntry: the Python struct format
	// "<II32sII" - flags, size, pack_id, obj_offset, obj_size, no padding.
	EntrySize = 4 + 4 + 32 + 4 + 4
)

// Entry flags (src/borg/hashindex.pyx).
//
// The split between user and system flags is part of the API, not an implementation
// detail: system flags are masked out of every value handed to a caller, so borge's
// bookkeeping cannot be observed or corrupted from outside this package.
const (
	FNone = 0

	// User flags.
	FUsed     = 1 << 0 // the chunk is referenced
	FCompress = 1 << 1 // the chunk should be (re-)compressed
	FPending  = 1 << 2 // the pack location is not resolved yet

	// System flag: the chunk is not present in the repository's index/ yet.
	FNew = 1 << 24

	MaskUser   = 0x00ffffff
	MaskSystem = 0xff000000
)

// Placeholders for a pack location that is not yet known (UNKNOWN_INT32 /
// UNKNOWN_BYTES32 in src/borg/constants.py).
const UnknownInt32 uint32 = 0xFFFFFFFF

// UnknownBytes32 is the placeholder pack id.
var UnknownBytes32 = make([]byte, 32)

// Entry is what the index records about one chunk.
type Entry struct {
	// Flags is a combination of the F* constants. System flags are never visible here.
	Flags uint32
	// Size is the plaintext size of the chunk. It is filled in by the cache layer;
	// PackWriter records 0 when it first adds a chunk.
	Size uint32
	// PackID, ObjOffset and ObjSize locate the chunk inside a pack file. They are
	// meaningless while FPending is set.
	PackID    [32]byte
	ObjOffset uint32
	ObjSize   uint32
}

// encode packs an entry into its 48-byte little-endian form.
func (e Entry) encode(buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:], e.Flags)
	binary.LittleEndian.PutUint32(buf[4:], e.Size)
	copy(buf[8:40], e.PackID[:])
	binary.LittleEndian.PutUint32(buf[40:], e.ObjOffset)
	binary.LittleEndian.PutUint32(buf[44:], e.ObjSize)
}

func decodeEntry(buf []byte) Entry {
	var e Entry
	e.Flags = binary.LittleEndian.Uint32(buf[0:])
	e.Size = binary.LittleEndian.Uint32(buf[4:])
	copy(e.PackID[:], buf[8:40])
	e.ObjOffset = binary.LittleEndian.Uint32(buf[40:])
	e.ObjSize = binary.LittleEndian.Uint32(buf[44:])
	return e
}

// ChunkIndex maps a chunk id to what borg knows about that chunk.
//
// It is not safe for concurrent use; see the note on Table.
type ChunkIndex struct {
	ht *Table
	// newCount tracks entries carrying FNew. It is -1 when unknown, which is the state
	// a table loaded from disk starts in - borg computes it lazily there because the
	// flag has to be read from every entry.
	newCount int
	scratch  [EntrySize]byte
}

// NewChunkIndex returns an empty index. usable is a hint for how many chunks it will
// hold; the table is sized at twice that, for a load factor of 0.5.
func NewChunkIndex(usable int) (*ChunkIndex, error) {
	capacity := minCapacity
	if usable > 0 && usable*2 > capacity {
		capacity = usable * 2
	}
	ht, err := NewTable(ChunkIDSize, EntrySize, capacity)
	if err != nil {
		return nil, err
	}
	return &ChunkIndex{ht: ht, newCount: 0}, nil
}

// Len reports how many chunks the index holds.
func (c *ChunkIndex) Len() int { return c.ht.Len() }

// Table exposes the underlying table, for serialisation.
func (c *ChunkIndex) Table() *Table { return c.ht }

// hideSystemFlags masks off the flags callers must not see.
func hideSystemFlags(e Entry) Entry {
	e.Flags &= MaskUser
	return e
}

// Get returns the entry for a chunk id, with system flags masked off.
func (c *ChunkIndex) Get(id []byte) (Entry, bool) {
	raw, ok := c.ht.Get(id)
	if !ok {
		return Entry{}, false
	}
	return hideSystemFlags(decodeEntry(raw)), true
}

// Contains reports whether the index knows this chunk.
func (c *ChunkIndex) Contains(id []byte) bool { return c.ht.Contains(id) }

// Set stores an entry.
//
// Two pieces of bookkeeping happen here and nowhere else, both ported from borg's
// __setitem__:
//
//   - System flags are preserved from the existing entry; a caller's flags are masked
//     to the user bits. Callers cannot set or clear FNew.
//   - FNew is sticky. An entry that was new stays new when overwritten, because the
//     question it answers is "does the repository's index/ already have this?", and
//     overwriting the in-memory entry does not change that.
func (c *ChunkIndex) Set(id []byte, e Entry) error {
	var systemFlags uint32
	inserting := true
	isNew := true

	if raw, ok := c.ht.Get(id); ok {
		prev := decodeEntry(raw)
		inserting = false
		isNew = prev.Flags&FNew != 0
		systemFlags = prev.Flags & MaskSystem
	}
	if isNew {
		systemFlags |= FNew
	} else {
		systemFlags &^= FNew
	}

	stored := e
	stored.Flags = systemFlags | (e.Flags & MaskUser)
	stored.encode(c.scratch[:])
	if err := c.ht.Set(id, c.scratch[:]); err != nil {
		return err
	}
	if inserting && c.newCount >= 0 {
		c.newCount++
	}
	return nil
}

// Add records a chunk that is being written, matching borg's ChunkIndex.add.
//
// It marks the chunk used and pending, and resets the pack location: re-adding a chunk
// invalidates whatever location it had until the next flush resolves it. Getting that
// wrong would leave the index pointing at a pack that no longer holds the chunk.
func (c *ChunkIndex) Add(id []byte, size uint32) error {
	flags := uint32(FUsed)
	if existing, ok := c.Get(id); ok {
		if existing.Size != 0 && existing.Size != size {
			return fmt.Errorf("hashindex: chunk %x already recorded with size %d, now given %d",
				id, existing.Size, size)
		}
		flags = existing.Flags | FUsed
	}
	e := Entry{
		Flags:     flags | FPending,
		Size:      size,
		ObjOffset: UnknownInt32,
		ObjSize:   UnknownInt32,
	}
	copy(e.PackID[:], UnknownBytes32)
	return c.Set(id, e)
}

// Delete removes a chunk, reporting whether it was present.
func (c *ChunkIndex) Delete(id []byte) bool {
	raw, ok := c.ht.Get(id)
	if !ok {
		return false
	}
	wasNew := decodeEntry(raw).Flags&FNew != 0
	if !c.ht.Delete(id) {
		return false
	}
	if c.newCount >= 0 && wasNew {
		c.newCount--
	}
	return true
}

// Clear empties the index.
func (c *ChunkIndex) Clear() {
	c.ht.Clear()
	c.newCount = 0
}

// IsPending reports whether a chunk's pack location is still unresolved.
func (c *ChunkIndex) IsPending(id []byte) bool {
	e, ok := c.Get(id)
	return ok && e.Flags&FPending != 0
}

// PackResult is one chunk's resolved location, as PackWriter reports it.
type PackResult struct {
	ChunkID   []byte
	PackID    [32]byte
	ObjOffset uint32
	ObjSize   uint32
}

// UpdatePackInfo records resolved pack locations and clears FPending on each chunk.
func (c *ChunkIndex) UpdatePackInfo(results []PackResult) error {
	for _, r := range results {
		e, ok := c.Get(r.ChunkID)
		if !ok {
			return fmt.Errorf("hashindex: pack result for unknown chunk %x", r.ChunkID)
		}
		e.Flags &^= FPending
		e.PackID = r.PackID
		e.ObjOffset = r.ObjOffset
		e.ObjSize = r.ObjSize
		if err := c.Set(r.ChunkID, e); err != nil {
			return err
		}
	}
	return nil
}

// NewCount reports how many entries carry the FNew system flag.
//
// For an index loaded from disk this is computed on first use and maintained
// incrementally afterwards, as borg does - reading the flag from every entry is not
// something to repeat on every call at 1.6 million chunks.
func (c *ChunkIndex) NewCount() int {
	if c.newCount < 0 {
		count := 0
		c.ht.Iterate(func(_, value []byte) bool {
			if decodeEntry(value).Flags&FNew != 0 {
				count++
			}
			return true
		})
		c.newCount = count
	}
	return c.newCount
}

// ClearNew clears the FNew flag on every entry.
func (c *ChunkIndex) ClearNew() {
	type update struct {
		key   [ChunkIDSize]byte
		entry Entry
	}
	var pending []update
	c.ht.Iterate(func(key, value []byte) bool {
		e := decodeEntry(value)
		if e.Flags&FNew != 0 {
			var u update
			copy(u.key[:], key)
			e.Flags &^= FNew
			u.entry = e
			pending = append(pending, u)
		}
		return true
	})
	// Collected first, then applied: writing through Iterate would mutate the table
	// while walking it.
	for _, u := range pending {
		u.entry.encode(c.scratch[:])
		_ = c.ht.Set(u.key[:], c.scratch[:])
	}
	c.newCount = 0
}

// Iterate calls fn for each chunk, with system flags masked off. fn returning false
// stops the iteration. The key slice is only valid for the call.
func (c *ChunkIndex) Iterate(fn func(id []byte, e Entry) bool) {
	c.ht.Iterate(func(key, value []byte) bool {
		return fn(key, hideSystemFlags(decodeEntry(value)))
	})
}

// Write serialises the index.
func (c *ChunkIndex) Write(w io.Writer) error {
	return WriteTable(w, c.ht, ChunkIndexLayout)
}

// WriteFile serialises the index to a path, writing to a temporary file and renaming,
// so an interrupted write cannot leave a half-written index in place of a good one.
func (c *ChunkIndex) WriteFile(path string) error {
	tmp, err := os.CreateTemp(fileDir(path), ".borge-index-*")
	if err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	if err := c.Write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}
	return nil
}

// ReadChunkIndex parses a serialised chunk index.
func ReadChunkIndex(r io.Reader) (*ChunkIndex, error) {
	ht, layout, err := ReadTable(r)
	if err != nil {
		return nil, err
	}
	if ht.KeySize() != ChunkIDSize || ht.ValueSize() != EntrySize {
		return nil, fmt.Errorf("hashindex: not a chunk index (key %d, value %d; want %d, %d)",
			ht.KeySize(), ht.ValueSize(), ChunkIDSize, EntrySize)
	}
	if layout.ValueTypeName != ChunkIndexLayout.ValueTypeName {
		return nil, fmt.Errorf("hashindex: value type is %q, want %q",
			layout.ValueTypeName, ChunkIndexLayout.ValueTypeName)
	}
	// newCount is unknown until something asks: computing it here would walk every
	// entry on load, for a number most commands never look at.
	return &ChunkIndex{ht: ht, newCount: -1}, nil
}

// ReadChunkIndexFile parses a serialised chunk index from a path.
func ReadChunkIndexFile(path string) (*ChunkIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("hashindex: %w", err)
	}
	defer f.Close()
	return ReadChunkIndex(f)
}

func fileDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
