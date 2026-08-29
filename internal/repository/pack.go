// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of PackWriter, PackReader and check_pack_objects in borg's
// src/borg/repository.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/store"
)

// Pack sizing (src/borg/constants.py).
const (
	// DefaultPackMaxSize is the byte cap on one pack. Note it is decimal, not binary.
	DefaultPackMaxSize = 50 * 1000 * 1000
	// MinPackSize is the floor borg uses when merging packs.
	MinPackSize = DefaultPackMaxSize / 50
)

// PackResult is one chunk's resolved location, reported when a pack is written.
type PackResult = hashindex.PackResult

// PackWriter buffers chunks into pack files and writes them to the store.
//
// # The concurrency invariant
//
// At most one pack store is in flight at a time, handed to a background goroutine so
// the caller can assemble the next pack while the previous one is hashed and stored.
// The rule that makes this safe, stated in borg's own docstring and restated here
// because it is the thing to preserve:
//
//	the ChunkIndex is only ever touched by the calling goroutine; the background
//	goroutine touches only the store.
//
// Three consequences follow, and all three are observable:
//
//   - Add returns the *previous* pack's results while the current store is in flight.
//   - A store error surfaces one pack later, from whichever Add or Flush joins.
//   - On failure the failed pack's chunk ids are removed from the index, and the
//     still-buffered pieces are dropped, so no FPending leftovers survive into the
//     index persisted at close.
//
// Getting this wrong gives a data race that corrupts repositories under load and
// reproduces rarely, which is why the plan calls it out and why `make race` exists.
type PackWriter struct {
	store *store.Store
	index *hashindex.ChunkIndex

	maxCount int // 0 means no count limit
	maxSize  int // 0 means no size limit

	pieces []piece
	size   int

	// inflight is the single background store, or nil.
	inflight *inflightPack
	// async can be turned off for debugging, matching BORG_PACK_ASYNC=no.
	async bool
}

type piece struct {
	chunkID [repoobj.ChunkIDSize]byte
	data    []byte
}

// inflightPack is one background store and its outcome.
type inflightPack struct {
	done chan struct{}
	// pendingIDs are the chunk ids to drop from the index if the store fails.
	pendingIDs [][repoobj.ChunkIDSize]byte
	results    []PackResult
	err        error
}

// NewPackWriter returns a pack writer over a store and a chunk index.
//
// maxCount bounds how many chunks a pack holds and maxSize its byte size; zero
// disables a limit, but at least one must be set or the buffer would be unbounded.
func NewPackWriter(s *store.Store, index *hashindex.ChunkIndex, maxCount, maxSize int, async bool) (*PackWriter, error) {
	if s == nil || index == nil {
		return nil, fmt.Errorf("repository: pack writer needs a store and a chunk index")
	}
	if maxCount <= 0 && maxSize <= 0 {
		return nil, fmt.Errorf("repository: pack writer needs a count or size limit, or the buffer is unbounded")
	}
	return &PackWriter{store: s, index: index, maxCount: maxCount, maxSize: maxSize, async: async}, nil
}

// MaxSize reports the configured byte cap, or the default when only a count limit is set.
func (w *PackWriter) MaxSize() int {
	if w.maxSize > 0 {
		return w.maxSize
	}
	return DefaultPackMaxSize
}

// Add buffers a chunk.
//
// When the chunk fills the pack, the pack is written and the results of the
// *previously* written pack (asynchronously) or of this pack (synchronously) are
// returned. It returns nil when there is nothing to report.
func (w *PackWriter) Add(chunkID []byte, data []byte) ([]PackResult, error) {
	if len(chunkID) != repoobj.ChunkIDSize {
		return nil, fmt.Errorf("repository: chunk id must be %d bytes, got %d",
			repoobj.ChunkIDSize, len(chunkID))
	}

	// The size recorded here is 0; the cache layer fills in the plaintext size later.
	if err := w.index.Add(chunkID, 0); err != nil {
		return nil, err
	}

	var p piece
	copy(p.chunkID[:], chunkID)
	p.data = data
	w.pieces = append(w.pieces, p)
	w.size += len(data)

	full := (w.maxCount > 0 && len(w.pieces) >= w.maxCount) ||
		(w.maxSize > 0 && w.size >= w.maxSize)
	if !full {
		return nil, nil
	}

	if !w.async {
		return w.Flush()
	}
	// Apply the previous pack's store (or raise its error) before handing off this one,
	// so at most one is ever in flight.
	results, err := w.JoinInflight()
	if err != nil {
		return nil, err
	}
	w.handoff()
	return results, nil
}

// takePieces takes the buffered pieces, leaving an empty buffer.
func (w *PackWriter) takePieces() []piece {
	pieces := w.pieces
	w.pieces, w.size = nil, 0
	return pieces
}

// handoff starts a background store of the buffered pieces.
func (w *PackWriter) handoff() {
	pieces := w.takePieces()
	if len(pieces) == 0 {
		return
	}
	in := &inflightPack{done: make(chan struct{})}
	in.pendingIDs = make([][repoobj.ChunkIDSize]byte, len(pieces))
	for i, p := range pieces {
		in.pendingIDs[i] = p.chunkID
	}
	w.inflight = in

	go func() {
		defer close(in.done)
		in.results, in.err = storePack(w.store, pieces)
	}()
}

// storePack builds, hashes and stores one pack.
//
// It runs on the background goroutine and must touch only the store - never the
// ChunkIndex. See the type comment.
func storePack(s *store.Store, pieces []piece) ([]PackResult, error) {
	total := 0
	for _, p := range pieces {
		total += len(p.data)
	}
	// Build the pack bytes once. borg joins rather than concatenating incrementally to
	// avoid quadratic copying; the same reasoning applies to a Go append loop without a
	// pre-sized buffer.
	//
	// Each chunk's buffer is released as soon as it has been copied, and the sizes are
	// kept because that is all the results loop below needs from it. Without that, the
	// pieces and the assembled pack are both live across the Store call - two copies of
	// up to BORGE_PACK_MAX_SIZE, held across the slowest part of the write. Peak RSS on
	// the corpus of §12.1b measured 383 MB at the default 50 MB pack and 244 MB at 2 MB,
	// a ratio of about 2.9 copies per pack; this is one of them.
	//
	// takePieces has already detached the slice from the writer, so nothing else can read
	// these buffers.
	packData := make([]byte, 0, total)
	sizes := make([]int, len(pieces))
	for i := range pieces {
		sizes[i] = len(pieces[i].data)
		packData = append(packData, pieces[i].data...)
		pieces[i].data = nil
	}

	// The pack is named by the SHA-256 of its bytes, so the name commits to the content
	// and the store can verify and cache it.
	packID := sha256.Sum256(packData)

	results := make([]PackResult, 0, len(pieces))
	offset := 0
	for i, p := range pieces {
		id := make([]byte, repoobj.ChunkIDSize)
		copy(id, p.chunkID[:])
		results = append(results, PackResult{
			ChunkID:   id,
			PackID:    packID,
			ObjOffset: uint32(offset),
			ObjSize:   uint32(sizes[i]),
		})
		offset += sizes[i]
	}

	if err := s.Store(PackName(packID[:]), packData); err != nil {
		return nil, err
	}
	return results, nil
}

// PackName is the store name of a pack.
func PackName(packID []byte) string { return "packs/" + hex.EncodeToString(packID) }

// JoinInflight waits for a background store and applies it to the index.
//
// It returns that pack's results, or nil when nothing was in flight. If the store
// failed, the writer is emptied and the error is returned.
func (w *PackWriter) JoinInflight() ([]PackResult, error) {
	if w.inflight == nil {
		return nil, nil
	}
	in := w.inflight
	<-in.done
	w.inflight = nil

	results, err := w.applyOutcome(in)
	if err != nil {
		w.dropBuffered()
		return nil, err
	}
	return results, nil
}

// applyOutcome records a finished store in the index. Calling goroutine only.
func (w *PackWriter) applyOutcome(in *inflightPack) ([]PackResult, error) {
	if in.err != nil {
		// The pack was not stored: drop its chunks from the index, or they would point
		// at a pack that does not exist.
		for _, id := range in.pendingIDs {
			w.index.Delete(id[:]) // a chunk id may appear more than once in one pack
		}
		return nil, fmt.Errorf("repository: storing a pack failed: %w", in.err)
	}
	if err := w.index.UpdatePackInfo(in.results); err != nil {
		return nil, err
	}
	return in.results, nil
}

// dropBuffered discards the buffered pieces and their still-pending index entries.
//
// Called when a store failed and the caller is aborting: chunks not yet handed to the
// store die with it, and dropping their entries keeps FPending leftovers out of the
// index that gets persisted at close.
func (w *PackWriter) dropBuffered() {
	for _, p := range w.takePieces() {
		w.index.Delete(p.chunkID[:])
	}
}

// Flush writes the current pack to the store.
//
// It is a barrier: an in-flight store is joined first and the current buffer is written
// synchronously, so afterwards nothing is buffered, nothing is in flight, and no chunk
// written through this writer is still pending.
func (w *PackWriter) Flush() ([]PackResult, error) {
	results, err := w.JoinInflight()
	if err != nil {
		return nil, err
	}
	if len(w.pieces) == 0 {
		return results, nil
	}

	pieces := w.takePieces()
	in := &inflightPack{}
	in.pendingIDs = make([][repoobj.ChunkIDSize]byte, len(pieces))
	for i, p := range pieces {
		in.pendingIDs[i] = p.chunkID
	}
	in.results, in.err = storePack(w.store, pieces)

	more, err := w.applyOutcome(in)
	if err != nil {
		w.dropBuffered()
		return nil, err
	}
	return append(results, more...), nil
}

// Buffered reports how many chunks are waiting to be written. It is used by Close to
// assert that a flush happened.
func (w *PackWriter) Buffered() int { return len(w.pieces) }

// ---------------------------------------------------------------------- PackReader

// PackReader reads objects out of a pack.
//
// It works either against the store, reading ranges, or over a pack already held in
// memory. The range path is what makes a header walk cheap: locating every object costs
// one short read apiece rather than a whole-pack download - though on a high-latency
// backend the store's pack cache collapses those into a single fetch anyway
// (internal/store, TestPackCacheCollapsesHeaderReads).
type PackReader struct {
	store    *store.Store
	packID   []byte
	name     string
	contents []byte
}

// NewPackReader reads a pack from the store.
func NewPackReader(s *store.Store, packID []byte) *PackReader {
	return &PackReader{store: s, packID: packID, name: PackName(packID)}
}

// NewPackReaderFromBytes reads a pack already held in memory.
func NewPackReaderFromBytes(packID, contents []byte) *PackReader {
	return &PackReader{packID: packID, contents: contents}
}

// Read returns size bytes of the pack from offset. A short result at the end of the
// pack is not an error.
func (r *PackReader) Read(offset, size int) ([]byte, error) {
	if r.contents != nil {
		if offset >= len(r.contents) {
			return nil, nil
		}
		end := offset + size
		if end > len(r.contents) {
			end = len(r.contents)
		}
		return r.contents[offset:end], nil
	}
	return r.store.Load(r.name, int64(offset), int64(size), false)
}

// Size reports the pack's length. For a store-backed pack this is one metadata lookup.
func (r *PackReader) Size() (int, error) {
	if r.contents != nil {
		return len(r.contents), nil
	}
	info, err := r.store.Info(r.name, false)
	if err != nil {
		return 0, err
	}
	if !info.Exists {
		return 0, &store.ObjectNotFoundError{Name: r.name}
	}
	return int(info.Size), nil
}

// PackEntry is one object located inside a pack.
type PackEntry struct {
	ChunkID []byte
	Offset  int
	Size    int
}

// IterHeaders walks the pack's fixed object headers, yielding every object's id,
// offset and size without reading any payload.
//
// The error rules are load-bearing:
//
//   - A trailing *partial* header is the clean end of the pack, not corruption.
//   - A bad magic, or an object claiming to extend past the end of the pack, is
//     corruption and stops the walk with an error.
//
// The second must not silently end the walk instead. borg spells out why: a chunk index
// rebuilt from a truncated walk would quietly be missing the rest of the pack, and
// `borg check --repair` would then "fix" the archives by dropping those chunks. A wrong
// repair is worse than a loud failure.
func (r *PackReader) IterHeaders(fn func(PackEntry) bool) error {
	packSize, err := r.Size()
	if err != nil {
		return err
	}
	packHex := "<no id>"
	if r.packID != nil {
		packHex = hex.EncodeToString(r.packID)
	}

	offset := 0
	for {
		hdr, err := r.Read(offset, repoobj.HeaderSize)
		if err != nil {
			return err
		}
		if len(hdr) < repoobj.HeaderSize {
			return nil // clean EOF, or trailing partial bytes
		}

		chunkID, objSize, err := repoobj.ParseHeader(hdr)
		if err != nil {
			return fmt.Errorf("pack %s: no object header at offset %d (pack corruption), run a check: %w",
				packHex, offset, err)
		}
		if offset+objSize > packSize {
			return fmt.Errorf("pack %s: object at offset %d extends past the end of the file "+
				"(pack corruption), run a check", packHex, offset)
		}

		id := make([]byte, len(chunkID))
		copy(id, chunkID)
		if !fn(PackEntry{ChunkID: id, Offset: offset, Size: objSize}) {
			return nil
		}
		offset += objSize
	}
}

// CheckPackObjects validates a pack's indexed object ranges against its size.
//
// ranges must be ordered by offset. An overlap between two objects, or an object ending
// past the pack, means the *index* is corrupt rather than the pack.
func CheckPackObjects(packHex string, ranges []PackEntry, packSize int) error {
	covered := 0
	for _, e := range ranges {
		if e.Offset < covered {
			return fmt.Errorf("pack %s: overlapping objects at offset %d (index corruption), run a check",
				packHex, e.Offset)
		}
		covered = e.Offset + e.Size
	}
	if covered > packSize {
		return fmt.Errorf("pack %s: indexed objects end at %d, past the %d byte pack (index corruption)",
			packHex, covered, packSize)
	}
	return nil
}

// packCache is a small LRU of whole packs, so a run of reads from the same pack does not
// re-fetch it. borg keeps three.
type packCache struct {
	mu       sync.Mutex
	capacity int
	order    []string
	entries  map[string]*PackReader
}

func newPackCache(capacity int) *packCache {
	return &packCache{capacity: capacity, entries: map[string]*PackReader{}}
}

func (c *packCache) get(packID []byte) *PackReader {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[string(packID)]
}

func (c *packCache) put(packID []byte, r *PackReader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := string(packID)
	if _, ok := c.entries[k]; !ok {
		c.order = append(c.order, k)
		for len(c.order) > c.capacity {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.entries[k] = r
}

func (c *packCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]*PackReader{}
	c.order = nil
}
