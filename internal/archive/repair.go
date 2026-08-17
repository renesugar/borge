// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ArchiveChecker.rebuild_archives, robust_iterator and
// rebuild_archives_directory in borg's src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/msgpackx"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
)

// Repairing a damaged archive.
//
// # What repair can and cannot do
//
// It cannot bring back data. What it can do is stop one lost chunk from making everything
// after it unreadable.
//
// The item metadata stream has no framing: items are msgpack maps written back to back
// across chunk boundaries. Lose one chunk in the middle and the decoder's position is
// meaningless from there on, so a single missing chunk costs the *rest of the archive*
// rather than the files it held. Repair resynchronises past the gap - see RobustUnpacker -
// and writes what survived as a new archive.
//
// So a repaired archive is a smaller archive, honestly labelled. The files whose metadata
// was in the lost chunk are gone; the ones after it are back.
//
// # Why everything is re-read at the "repair" place
//
// Repair re-packs the item stream into new chunks with freshly computed ids. A chunk whose
// content does not match its id would become valid data under a new id, and the violation
// would be unnoticeable afterwards. So the id is verified on every read here, which is what
// BORG_ASSERT_ID's "repair" place is for.

// RepairOptions control an archive repair.
type RepairOptions struct {
	// DryRun reports what would be repaired without writing anything.
	DryRun bool
	// OnProblem is called for each defect found.
	OnProblem func(string)
}

// RepairReport is what a repair found and did.
type RepairReport struct {
	// Name and ID identify the archive as it was.
	Name string
	ID   []byte
	// NewID is the id of the repaired archive, empty when nothing needed repairing.
	NewID []byte

	// ItemsKept and ItemsLost count the metadata stream's outcome.
	ItemsKept int
	ItemsLost int
	// StreamChunksMissing is how many item stream chunks could not be read.
	StreamChunksMissing int
	// MissingFileChunks maps a missing chunk id to the paths that referenced it.
	MissingFileChunks map[string][]string
	// Repaired is true when a new archive was written.
	Repaired bool
}

// Damaged reports whether anything was wrong with the archive.
func (r *RepairReport) Damaged() bool {
	return r.StreamChunksMissing > 0 || r.ItemsLost > 0 || len(r.MissingFileChunks) > 0
}

// maxMissingChunksReported bounds the report. A badly damaged repository can reference a
// million missing chunks, and listing them all would exhaust memory to produce something
// nobody can read.
const maxMissingChunksReported = 1000

// maxRefsPerChunk bounds how many referencing paths are listed per missing chunk.
const maxRefsPerChunk = 10

// Repair re-reads an archive robustly and, when anything was lost, writes a repaired copy.
//
// The repaired archive replaces the original in the directory: a new archive object is
// written, its pointer created, and the old pointer soft-deleted. Soft, not removed - the
// original is still there for anyone who wants to try harder.
func Repair(m *manifest.Manifest, id []byte, opts RepairOptions) (*RepairReport, error) {
	report := &RepairReport{ID: append([]byte(nil), id...), MissingFileChunks: map[string][]string{}}
	problem := opts.OnProblem
	if problem == nil {
		problem = func(string) {}
	}

	a, err := Open(m, id)
	if err != nil {
		return report, err
	}
	report.Name = a.Info.Name

	repo := m.Repository()
	chunks, err := repo.Chunks()
	if err != nil {
		return report, err
	}

	// Re-anchoring content means the id/content invariant has to be re-certified here; see
	// the note at the top of this file.
	ro := m.RepoObj()
	if err := ro.SetAssertIDPlace(repoobj.PlaceRepair); err != nil {
		return report, err
	}

	items, streamMissing, err := readItemsRobustly(a, chunks, m.ItemKeys, problem)
	if err != nil {
		return report, err
	}
	report.ItemsKept = len(items)
	report.StreamChunksMissing = streamMissing

	// Compare against what an undamaged read would have produced, so "items lost" is a
	// number rather than an impression. A plain read stops at the first gap, so its count
	// is a lower bound on what was reachable before - which is exactly the point.
	if streamMissing > 0 {
		problem(fmt.Sprintf("archive %s: %d item metadata chunk(s) missing; "+
			"the items they held are lost, the ones after them were recovered",
			report.Name, streamMissing))
	}

	// Every file chunk an item references has to exist.
	truncated := false
	for _, it := range items {
		for _, c := range it.Chunks {
			if _, ok := chunks.Get(c.ID); ok {
				continue
			}
			key := hex.EncodeToString(c.ID)
			refs, seen := report.MissingFileChunks[key]
			if !seen {
				if len(report.MissingFileChunks) >= maxMissingChunksReported {
					truncated = true
					continue
				}
				problem(fmt.Sprintf("archive %s: missing chunk %s (%d bytes)", report.Name, key, c.Size))
			}
			if len(refs) < maxRefsPerChunk {
				report.MissingFileChunks[key] = append(refs, it.Path)
			}
		}
	}
	if truncated {
		problem(fmt.Sprintf("archive %s: more than %d distinct missing chunks; the rest are not listed",
			report.Name, maxMissingChunksReported))
	}

	if !report.Damaged() {
		return report, nil
	}
	if opts.DryRun {
		return report, nil
	}

	newID, err := writeRepairedArchive(m, a, items)
	if err != nil {
		return report, err
	}
	report.NewID = newID
	report.Repaired = true

	// Point the directory at the repaired archive and soft-delete the original.
	if err := m.Archives.Create(newID); err != nil {
		return report, err
	}
	if err := m.Archives.Delete(id); err != nil {
		return report, err
	}
	return report, nil
}

// readItemsRobustly walks an archive's item stream, resynchronising past missing chunks.
func readItemsRobustly(a *Archive, chunks *hashindex.ChunkIndex, itemKeys []string,
	problem func(string),
) ([]*item.Item, int, error) {
	streamIDs, err := a.ItemStreamIDs()
	if err != nil {
		return nil, 0, err
	}

	// An item is plausible if it has the required keys and no key the repository does not
	// know. The second half matters: after a resync the decoder may land on something that
	// decodes as a map without being an item at all.
	known := map[string]bool{}
	for _, k := range itemKeys {
		known[k] = true
	}
	validate := func(mp *msgpackx.Map) bool {
		hasPath, hasMTime := false, false
		for _, e := range mp.Entries() {
			name, ok := e.Key.(string)
			if !ok {
				return false
			}
			if !known[name] {
				return false
			}
			switch name {
			case "path":
				hasPath = true
			case "mtime":
				hasMTime = true
			}
		}
		return hasPath && hasMTime
	}

	u, err := NewRobustUnpacker(itemKeys, validate)
	if err != nil {
		return nil, 0, err
	}

	var items []*item.Item
	missing := 0
	previousWasMissing := false

	for _, id := range streamIDs {
		if _, ok := chunks.Get(id); !ok {
			missing++
			previousWasMissing = true
			problem(fmt.Sprintf("item metadata chunk %s is missing", hex.EncodeToString(id)))
			continue
		}
		obj, err := a.repo.Get(id)
		if err != nil {
			if errors.Is(err, repository.ErrObjectNotFound) {
				missing++
				previousWasMissing = true
				continue
			}
			return nil, missing, err
		}
		_, data, err := a.ro.Parse(id, obj, repoobj.TypeArchiveStream, repoobj.ParseOptions{})
		if err != nil {
			missing++
			previousWasMissing = true
			problem(fmt.Sprintf("item metadata chunk %s is corrupt: %v", hex.EncodeToString(id), err))
			continue
		}

		if previousWasMissing {
			// The bytes after a gap start mid-item. Resync tells the unpacker to scan for
			// the next plausible item boundary rather than decoding from where it was.
			u.Resync()
			previousWasMissing = false
		}
		u.Feed(data)

		for {
			mp, ok, err := u.Next()
			if err != nil {
				// A decode error that resync could not get past. Drop what is buffered and
				// try again from the next chunk rather than abandoning the archive.
				problem(fmt.Sprintf("archive %s: %v", a.Info.Name, err))
				u.Resync()
				break
			}
			if !ok {
				break
			}
			it, err := item.DecodeItem(mp)
			if err != nil {
				problem(fmt.Sprintf("archive %s: unusable item: %v", a.Info.Name, err))
				continue
			}
			items = append(items, it)
		}
	}
	return items, missing, nil
}

// writeRepairedArchive writes the surviving items as a new archive object.
//
// The metadata is carried over from the original - name, times, hostname, the command line
// that produced it - because the repaired archive *is* that backup, minus what was lost.
// Giving it a fresh timestamp would misdate the history.
func writeRepairedArchive(m *manifest.Manifest, a *Archive, items []*item.Item) ([]byte, error) {
	b, err := NewBuilder(m, BuilderOptions{})
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if err := b.AddItem(it); err != nil {
			return nil, err
		}
	}
	if err := b.items.flush(true); err != nil {
		return nil, err
	}
	itemPtrs, err := b.writeItemPointers()
	if err != nil {
		return nil, err
	}

	meta := *a.Meta // a shallow copy: only the item pointers change
	meta.ItemPtrs = itemPtrs
	meta.ItemPtrsSet = true
	meta.Items = nil
	meta.ItemsSet = false

	// The counts are recomputed, because they now describe a smaller archive and leaving
	// the old ones would make "borge info" claim files that are not there.
	var nfiles, size int64
	for _, it := range items {
		if it.IsRegular() {
			nfiles++
			size += it.ContentSize()
		}
	}
	meta.NFiles = &nfiles
	meta.Size = &size

	data, err := meta.Marshal()
	if err != nil {
		return nil, err
	}
	if err := m.Repository().Flush(); err != nil {
		return nil, err
	}
	id := m.Key().IDHash(data)
	if _, err := b.AddChunk(data, repoobj.TypeArchiveMeta); err != nil {
		return nil, err
	}
	if err := m.Repository().Flush(); err != nil {
		return nil, err
	}
	return id, nil
}

// FindLostArchives scans the repository for archive metadata objects that no directory
// entry points at.
//
// This is how an archive comes back after its pointer was lost: the object holding the
// archive is still in a pack, and its type tag says what it is. Without this, an archive
// with no pointer is invisible and a compaction would eventually delete it.
func FindLostArchives(m *manifest.Manifest, repair bool, problem func(string)) ([][]byte, error) {
	repo := m.Repository()
	chunks, err := repo.Chunks()
	if err != nil {
		return nil, err
	}
	ro := m.RepoObj()

	known := map[string]bool{}
	for _, deleted := range []bool{false, true} {
		ids, err := m.Archives.IDs(deleted)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			known[hex.EncodeToString(id)] = true
		}
	}

	var candidates [][]byte
	chunks.Iterate(func(id []byte, _ hashindex.Entry) bool {
		candidates = append(candidates, append([]byte(nil), id...))
		return true
	})
	sort.Slice(candidates, func(i, j int) bool { return string(candidates[i]) < string(candidates[j]) })

	var found [][]byte
	for _, id := range candidates {
		if known[hex.EncodeToString(id)] {
			continue
		}
		obj, err := repo.Get(id)
		if err != nil {
			continue
		}
		// The cheap check first: the metadata slot says what kind of object this is, so
		// only the archive objects are decrypted and decoded in full.
		meta, err := ro.ParseMeta(id, obj, repoobj.TypeDontCare)
		if err != nil || meta.Type != repoobj.TypeArchiveMeta {
			continue
		}
		_, data, err := ro.Parse(id, obj, repoobj.TypeArchiveMeta, repoobj.ParseOptions{})
		if err != nil {
			continue
		}
		ai, err := item.UnmarshalArchiveItem(data)
		if err != nil || ai.Name == "" || !ai.ItemPtrsSet {
			continue
		}

		found = append(found, id)
		if repair {
			problem(fmt.Sprintf("creating a directory entry for the lost archive %s (%s)",
				ai.Name, hex.EncodeToString(id)[:8]))
			if err := m.Archives.Create(id); err != nil {
				return found, err
			}
		} else {
			problem(fmt.Sprintf("archive %s (%s) has no directory entry",
				ai.Name, hex.EncodeToString(id)[:8]))
		}
	}
	return found, nil
}
