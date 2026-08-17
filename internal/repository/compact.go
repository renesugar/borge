// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the pack-rewriting half of ArchiveGarbageCollector in borg's
// src/borg/archiver/compact_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package repository

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/store"
)

// Compaction: reclaiming the space of chunks no archive references any more.
//
// # Why it is a separate operation
//
// Deleting an archive removes a pointer, not data. Working out whether a chunk is still
// needed means reading *every* archive, because any of them might reference it - so a
// delete that reclaimed space would cost as much as a full scan, every time. Separating
// them lets a hundred deletes share one scan.
//
// # Why packs make it awkward
//
// The store can only delete whole objects, and a pack is one object holding many chunks.
// So a pack with one dead chunk among many live ones cannot be shrunk; it has to be
// rewritten without the dead one, which costs a read and a write of everything still
// alive in it. That is only worth doing when enough of the pack is dead, which is what
// the threshold below is for.

// DefaultCompactThreshold is the percentage of a pack that must be reclaimable before the
// pack is rewritten. borg's default is 10.
const DefaultCompactThreshold = 10

// CompactOptions configure a compaction.
type CompactOptions struct {
	// Threshold is the percentage of a pack that must be dead before it is rewritten.
	// Zero uses DefaultCompactThreshold.
	Threshold int
	// DryRun computes everything and changes nothing.
	DryRun bool
	// OnProgress, if set, is called with a human-readable line per decision.
	OnProgress func(string)
}

// CompactStats is what a compaction did.
type CompactStats struct {
	PacksBefore  int
	PacksDropped int
	PacksRewrit  int
	PacksKept    int
	BytesBefore  int64
	BytesAfter   int64
	ChunksBefore int
	ChunksAlive  int
	ChunksFreed  int
	// Missing counts chunks an archive references that the repository does not have. A
	// non-zero value means the repository is damaged.
	Missing int
}

// Compact removes chunks no longer marked used from the repository.
//
// used must contain every chunk id that is still referenced. It is the caller's job to
// build it by walking the archives, because that walk needs the archive layer, which sits
// above this package.
//
// # The one rule that matters
//
// A chunk that is *not* in `used` but *is* still referenced would be deleted and the
// referencing archive silently broken. So a caller that could not read some archive must
// not call this at all - and Compact refuses if `used` names chunks the repository does
// not have, because that is the signature of an incomplete scan.
func (r *Repository) Compact(used map[string]bool, opts CompactOptions) (*CompactStats, error) {
	if !r.opened {
		return nil, fmt.Errorf("repository: not open")
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = DefaultCompactThreshold
	}
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	chunks, err := r.Chunks()
	if err != nil {
		return nil, err
	}
	stats := &CompactStats{ChunksBefore: chunks.Len()}

	// A chunk an archive references but the index does not have means the scan saw a
	// broken archive, or the index is wrong. Either way, deciding what is dead from an
	// incomplete picture is how a compaction destroys data.
	for id := range used {
		raw, err := hex.DecodeString(id)
		if err != nil {
			return nil, fmt.Errorf("repository: %q is not a chunk id", id)
		}
		if _, ok := chunks.Get(raw); !ok {
			stats.Missing++
		}
	}
	if stats.Missing > 0 {
		return stats, fmt.Errorf("repository: %d referenced chunk(s) are missing from this repository; "+
			"refusing to compact, because deciding what is unused from an incomplete picture "+
			"is how a compaction destroys data (run borge check first)", stats.Missing)
	}

	// Pass 1: what the store actually holds, and what the index says about each pack.
	packSize := map[string]int64{}
	names, err := r.store.ListNames("packs", false)
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	for _, name := range names {
		info, err := r.store.Info(PackName(mustHex(name)), false)
		if err != nil {
			continue
		}
		packSize[name] = info.Size
		stats.BytesBefore += info.Size
	}
	stats.PacksBefore = len(packSize)

	type packInfo struct {
		indexed int64 // bytes of every index entry in this pack, alive or not
		alive   int64 // bytes of the entries still referenced
		liveIDs [][]byte
		deadIDs [][]byte
	}
	packs := map[string]*packInfo{}
	var stale int

	chunks.Iterate(func(id []byte, e hashindex.Entry) bool {
		pid := hex.EncodeToString(e.PackID[:])
		if _, ok := packSize[pid]; !ok {
			// An index entry naming a pack the store does not have. borg keeps these
			// rather than repairing the index (borg #8572), and so does borge: the entry
			// may be an archive's only pointer to a chunk, and dropping it here would turn
			// a recoverable problem into a lost one.
			stale++
			return true
		}
		p := packs[pid]
		if p == nil {
			p = &packInfo{}
			packs[pid] = p
		}
		size := int64(e.ObjSize)
		p.indexed += size
		idCopy := append([]byte(nil), id...)
		if used[hex.EncodeToString(id)] {
			p.alive += size
			p.liveIDs = append(p.liveIDs, idCopy)
			stats.ChunksAlive++
		} else {
			p.deadIDs = append(p.deadIDs, idCopy)
		}
		return true
	})
	if stale > 0 {
		progress(fmt.Sprintf("%d index entries name a pack this repository does not have; leaving them alone", stale))
	}

	// Pass 2: decide each pack's fate, in a stable order so two runs agree.
	var pids []string
	for pid := range packs {
		pids = append(pids, pid)
	}
	sort.Strings(pids)

	var toDrop, toRewrite []string
	for _, pid := range pids {
		p := packs[pid]
		total := packSize[pid]
		reclaimable := p.indexed - p.alive

		switch {
		case reclaimable == 0:
			stats.PacksKept++
		case p.alive == 0 && p.indexed == total:
			// Every byte of the file is indexed and dead: the whole pack can go.
			toDrop = append(toDrop, pid)
		case total > 0 && 100*reclaimable/total >= int64(threshold):
			toRewrite = append(toRewrite, pid)
		default:
			// Below the threshold. Rewriting a large pack to reclaim a little costs more
			// than it saves, and every rewrite invalidates every client's cached index.
			stats.PacksKept++
		}
	}

	progress(fmt.Sprintf("%d pack(s): %d to drop, %d to rewrite, %d unchanged",
		stats.PacksBefore, len(toDrop), len(toRewrite), stats.PacksKept))

	if opts.DryRun {
		stats.PacksDropped = len(toDrop)
		stats.PacksRewrit = len(toRewrite)
		stats.BytesAfter = stats.BytesBefore
		return stats, nil
	}

	// Drop first: a dropped pack needs no reading, and doing the cheap work before the
	// expensive work means an interruption has already achieved something.
	for _, pid := range toDrop {
		for _, id := range packs[pid].deadIDs {
			chunks.Delete(id)
			stats.ChunksFreed++
		}
		if err := r.store.Delete(PackName(mustHex(pid)), false); err != nil && !isNotFound(err) {
			return stats, err
		}
		stats.PacksDropped++
	}

	for _, pid := range toRewrite {
		freed, err := r.rewritePack(pid, packs[pid].liveIDs, chunks)
		if err != nil {
			return stats, err
		}
		for _, id := range packs[pid].deadIDs {
			chunks.Delete(id)
			stats.ChunksFreed++
		}
		stats.PacksRewrit++
		_ = freed
	}

	// The index has changed in ways an incremental write cannot express - entries were
	// removed - so it is rewritten whole and the older fragments dropped.
	if stats.PacksDropped > 0 || stats.PacksRewrit > 0 {
		if err := r.WriteFullChunkIndex(); err != nil {
			return stats, err
		}
	}

	// Recount what is on disk now.
	stats.BytesAfter = 0
	names, err = r.store.ListNames("packs", false)
	if err != nil && !isNotFound(err) {
		return stats, err
	}
	for _, name := range names {
		if info, err := r.store.Info(PackName(mustHex(name)), false); err == nil {
			stats.BytesAfter += info.Size
		}
	}
	return stats, nil
}

// rewritePack copies a pack's live objects into new packs and removes the old one.
//
// The old pack is deleted **after** the new ones are written, so an interruption leaves
// the data twice rather than not at all. The duplicate copies are what the next
// compaction reclaims.
func (r *Repository) rewritePack(packHex string, liveIDs [][]byte, chunks *hashindex.ChunkIndex) (int64, error) {
	packID := mustHex(packHex)
	reader := NewPackReader(r.store, packID)

	// Read every live object, then write them through the ordinary pack writer, which
	// gives them fresh locations and updates the index as it goes.
	type object struct {
		id   []byte
		data []byte
	}
	var objects []object
	var moved int64
	for _, id := range liveIDs {
		entry, ok := chunks.Get(id)
		if !ok {
			continue
		}
		data, err := reader.Read(int(entry.ObjOffset), int(entry.ObjSize))
		if err != nil {
			return 0, fmt.Errorf("repository: reading %s from pack %s: %w",
				hex.EncodeToString(id), packHex, err)
		}
		objects = append(objects, object{id: append([]byte(nil), id...), data: append([]byte(nil), data...)})
		moved += int64(len(data))
	}

	for _, o := range objects {
		if _, err := r.packWriter.Add(o.id, o.data); err != nil {
			return 0, err
		}
	}
	// Flush before deleting the source: the new copies must be durable first.
	if err := r.Flush(); err != nil {
		return 0, err
	}
	if err := r.store.Delete(PackName(packID), false); err != nil && !isNotFound(err) {
		return 0, err
	}
	return moved, nil
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func isNotFound(err error) bool {
	return err != nil && (errors.Is(err, store.ErrObjectNotFound) || errors.Is(err, ErrObjectNotFound))
}

// RebuildChunkIndex discards the stored index and rebuilds it by reading every pack.
//
// This is what makes a repair trustworthy. The index is a cache, and a cache that
// disagrees with the packs is worse than no cache: a repair working from it would decide
// what to fix against a picture of the repository that is not true. Reading every pack is
// slow, which is exactly why the ordinary read path does not do it.
func (r *Repository) RebuildChunkIndex() error {
	if err := r.Flush(); err != nil {
		return err
	}
	chunks, err := BuildChunkIndex(r.store, true)
	if err != nil {
		return err
	}
	r.chunks = chunks
	if r.packWriter != nil {
		r.packWriter.index = chunks
	}
	return r.WriteFullChunkIndex()
}
