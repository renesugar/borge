// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the chunk index persistence functions in borg's
// src/borg/cache.py (write_chunkindex_to_repo, read_chunkindex_from_repo,
// build_chunkindex_from_repo and their helpers).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/store"
)

// Index persistence constants (src/borg/constants.py).
const (
	// ChunkIndexFragmentEntriesMax bounds one fragment, so no single index object gets
	// too large - about 32 MB at this count.
	ChunkIndexFragmentEntriesMax = 400000
	// ChunkIndexInvalidSentinel marks the index as mid-deletion. Leftover fragments
	// after an interrupted delete would otherwise look like a complete index.
	ChunkIndexInvalidSentinel = "chunkindex-invalid"
	// chunkIndexMergeAttempts bounds the retry when a fragment vanishes under us.
	chunkIndexMergeAttempts = 3
)

func indexName(hash string) string { return "index/" + hash }
func invalidMarkerName() string    { return "index/" + ChunkIndexInvalidSentinel }

// listIndexHashes returns the content hashes of the stored index fragments, excluding
// the invalid marker.
func listIndexHashes(s *store.Store) ([]string, error) {
	names, err := s.ListNames("index", false)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == ChunkIndexInvalidSentinel {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// indexIsInvalid reports whether a previous deletion was interrupted.
func indexIsInvalid(s *store.Store) (bool, error) {
	info, err := s.Info(invalidMarkerName(), false)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return false, nil
		}
		return false, err
	}
	return info.Exists, nil
}

func writeInvalidMarker(s *store.Store) error {
	return s.Store(invalidMarkerName(), []byte{})
}

func deleteInvalidMarker(s *store.Store) error {
	err := s.Delete(invalidMarkerName(), false)
	if err != nil && errors.Is(err, store.ErrObjectNotFound) {
		return nil
	}
	return err
}

// storeIndexFragment serialises one batch and stores it under index/<sha256 of content>.
//
// Flags and sizes are deliberately not serialised - callers zero them - so the same set
// of entries always produces the same bytes and therefore the same name. A fragment
// whose content is already in the repository is not written again.
func storeIndexFragment(s *store.Store, batch *hashindex.ChunkIndex, stored map[string]bool, forceWrite bool) (string, bool, error) {
	var buf bytes.Buffer
	if err := batch.Write(&buf); err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(buf.Bytes())
	hash := hex.EncodeToString(sum[:])

	if !forceWrite && stored[hash] {
		return hash, false, nil
	}
	if err := s.Store(indexName(hash), buf.Bytes()); err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// WriteIndexOptions tunes WriteChunkIndex.
type WriteIndexOptions struct {
	// Incremental writes only the entries flagged new, which is what a backup run
	// produces. Otherwise the whole index is written.
	Incremental bool
	// ForceWrite stores a fragment even when one with the same content hash exists, and
	// writes an empty fragment when there is nothing to write. Repository creation uses
	// it so the first operation does not have to rebuild the index by listing packs.
	ForceWrite bool
	// DeleteOther removes every fragment that is not part of what was just written.
	DeleteOther bool
}

// WriteChunkIndex persists a chunk index as one or more index/ fragments.
//
// # Why fragments, and why sorted
//
// The output is split into bounded, immutable fragments rather than one large index.
// Each fragment is named by its content hash, so writing is idempotent: a fragment that
// already exists is not stored again, and two clients that select the same entries
// converge on the same object.
//
// That only works if the bytes are deterministic, which is why the keys are sorted
// before a fragment is built. Without the sort, the same set of entries would serialise
// differently depending on the order they happened to be inserted into the hash table,
// and every run would upload a fresh set of near-duplicate fragments.
func WriteChunkIndex(s *store.Store, chunks *hashindex.ChunkIndex, opts WriteIndexOptions) ([]string, error) {
	storedHashes, err := listIndexHashes(s)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]bool, len(storedHashes))
	for _, h := range storedHashes {
		stored[h] = true
	}

	// Collect the keys to write. borg partitions by key prefix to keep peak memory down
	// at millions of chunks; borge collects and sorts, which is simpler and adequate
	// until stage 9 says otherwise - the keys are 32 bytes each, so 1.6 million of them
	// is about 52 MB, against the 143 MB the index itself already occupies.
	var keys [][]byte
	var collectErr error
	chunks.Iterate(func(id []byte, e hashindex.Entry) bool {
		if opts.Incremental && !chunks.IsNew(id) {
			return true
		}
		if e.Flags&hashindex.FPending != 0 {
			// A chunk still buffered in the pack writer has no location yet. Serialising
			// it would record a pointer to a pack that may never exist.
			collectErr = fmt.Errorf("repository: chunk %x has no pack location yet; flush before persisting", id)
			return false
		}
		k := make([]byte, len(id))
		copy(k, id)
		keys = append(keys, k)
		return true
	})
	if collectErr != nil {
		return nil, collectErr
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })

	var batches [][][]byte
	switch {
	case len(keys) > 0:
		for start := 0; start < len(keys); start += ChunkIndexFragmentEntriesMax {
			end := start + ChunkIndexFragmentEntriesMax
			if end > len(keys) {
				end = len(keys)
			}
			batches = append(batches, keys[start:end])
		}
	case opts.ForceWrite:
		batches = [][][]byte{{}} // one empty fragment
	default:
		// Do not persist an empty fragment: if it became the only index/ object, a
		// rebuild would return it as-is instead of rebuilding from the packs. With
		// nothing to write, the repository is already correct.
		return nil, nil
	}

	newHashes := make(map[string]bool, len(batches))
	var order []string
	written := 0
	for _, batchKeys := range batches {
		batch, err := hashindex.NewChunkIndex(len(batchKeys))
		if err != nil {
			return nil, err
		}
		for _, k := range batchKeys {
			e, ok := chunks.Get(k)
			if !ok {
				continue
			}
			// Flags and size are not serialised.
			if err := batch.Set(k, hashindex.Entry{
				PackID:    e.PackID,
				ObjOffset: e.ObjOffset,
				ObjSize:   e.ObjSize,
			}); err != nil {
				return nil, err
			}
		}
		hash, didStore, err := storeIndexFragment(s, batch, stored, opts.ForceWrite)
		if err != nil {
			return nil, err
		}
		if !newHashes[hash] {
			newHashes[hash] = true
			order = append(order, hash)
		}
		if didStore {
			written++
		}
	}

	if written > 0 {
		// The repository now holds these entries, so they are no longer new.
		chunks.ClearNew()
	}

	if len(newHashes) > 0 && opts.DeleteOther {
		var toDelete []string
		for _, h := range storedHashes {
			if !newHashes[h] {
				toDelete = append(toDelete, h)
			}
		}
		if len(toDelete) > 0 {
			// A rewrite that drops entries must not leave older fragments to be merged
			// back after a crash, so the deletion is guarded by the invalid marker.
			if err := writeInvalidMarker(s); err != nil {
				return nil, err
			}
			for _, h := range toDelete {
				if err := s.Delete(indexName(h), false); err != nil && !errors.Is(err, store.ErrObjectNotFound) {
					return nil, err
				}
			}
			if err := deleteInvalidMarker(s); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}

// ReadChunkIndexFragment loads one fragment. A vanished fragment is reported as
// (nil, nil): another client may have merged and removed it, which is normal.
func ReadChunkIndexFragment(s *store.Store, hash string) (*hashindex.ChunkIndex, error) {
	data, err := s.Load(indexName(hash), 0, -1, false)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil, nil
		}
		return nil, err
	}
	idx, err := hashindex.ReadChunkIndex(bytes.NewReader(data))
	if err != nil {
		// The name is the content hash, so the bytes are what was written; failing to
		// deserialise them means the fragment itself is bad, not that it changed.
		return nil, fmt.Errorf("repository: chunk index fragment %s is corrupt: %w", hash, err)
	}
	return idx, nil
}

// DeleteChunkIndex removes every stored fragment.
//
// The invalid marker is written *before* the first deletion and cleared after the last,
// so an interrupted deletion is detectable: leftover fragments would otherwise look
// like a complete index and produce spurious missing-chunk errors.
func DeleteChunkIndex(s *store.Store) error {
	hashes, err := listIndexHashes(s)
	if err != nil {
		return err
	}
	invalid, err := indexIsInvalid(s)
	if err != nil {
		return err
	}
	if len(hashes) > 0 {
		if err := writeInvalidMarker(s); err != nil {
			return err
		}
	}
	for _, h := range hashes {
		if err := s.Delete(indexName(h), false); err != nil && !errors.Is(err, store.ErrObjectNotFound) {
			return err
		}
	}
	if len(hashes) > 0 || invalid {
		if err := deleteInvalidMarker(s); err != nil {
			return err
		}
	}
	return nil
}

// BuildChunkIndex reconstructs the chunk index.
//
// It first tries to merge the stored index/ fragments, which is fast. If they cannot be
// read as a complete set - a fragment vanished under a concurrent repack, or one is
// corrupt, or a previous deletion was interrupted - it falls back to walking every pack.
//
// The index must be built from *all* fragments or none: a partially merged index would
// be missing chunks that exist in the repository, which shows up as spurious
// missing-object errors and as lost deduplication. That is why a failed load re-lists
// and retries rather than using what it got.
func BuildChunkIndex(s *store.Store, slowRebuild bool) (*hashindex.ChunkIndex, error) {
	if !slowRebuild {
		idx, err := buildFromFragments(s)
		if err != nil {
			return nil, err
		}
		if idx != nil {
			return idx, nil
		}
	}
	return buildFromPacks(s)
}

// buildFromFragments merges the stored fragments, or returns nil if it cannot get a
// complete set.
func buildFromFragments(s *store.Store) (*hashindex.ChunkIndex, error) {
	for attempt := 0; attempt < chunkIndexMergeAttempts; attempt++ {
		invalid, err := indexIsInvalid(s)
		if err != nil {
			return nil, err
		}
		if invalid {
			// Leftovers from an interrupted deletion may be incomplete or stale. Finish
			// the deletion (best effort - a read-only client cannot) and rebuild.
			_ = DeleteChunkIndex(s)
			return nil, nil
		}

		hashes, err := listIndexHashes(s)
		if err != nil {
			return nil, err
		}
		if len(hashes) == 0 {
			return nil, nil
		}

		chunks, err := hashindex.NewChunkIndex(0)
		if err != nil {
			return nil, err
		}
		complete := true
		for _, h := range hashes {
			fragment, err := ReadChunkIndexFragment(s, h)
			if err != nil {
				// Retrying would re-read the same corrupt fragment, so rebuild from the
				// packs instead.
				return nil, nil
			}
			if fragment == nil {
				// It vanished mid-merge: another client replaced it. Re-list and retry;
				// the fresh listing has the replacement.
				complete = false
				break
			}
			var mergeErr error
			fragment.Iterate(func(id []byte, e hashindex.Entry) bool {
				if err := chunks.Set(id, e); err != nil {
					mergeErr = err
					return false
				}
				return true
			})
			if mergeErr != nil {
				return nil, mergeErr
			}
		}
		if complete {
			// Merging marked every entry new; the repository already holds them.
			chunks.ClearNew()
			return chunks, nil
		}
	}
	return nil, nil
}

// buildFromPacks walks every pack and rebuilds the index from the object headers.
//
// This is the slow path: it lists the packs/ namespace and reads each pack's headers.
// It is what makes a repository self-describing - the index is a cache, and losing it
// costs time but not data.
func buildFromPacks(s *store.Store) (*hashindex.ChunkIndex, error) {
	chunks, err := hashindex.NewChunkIndex(0)
	if err != nil {
		return nil, err
	}

	names, err := s.ListNames("packs", false)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		packID, err := hex.DecodeString(name)
		if err != nil {
			// Not one of ours; a listing may hold anything.
			continue
		}
		data, err := s.Load("packs/"+name, 0, -1, false)
		if err != nil {
			if errors.Is(err, store.ErrObjectNotFound) {
				continue // deleted under us by a concurrent compaction
			}
			return nil, err
		}
		reader := NewPackReaderFromBytes(packID, data)

		var packErr error
		err = reader.IterHeaders(func(e PackEntry) bool {
			var pid [32]byte
			copy(pid[:], packID)
			if err := chunks.Set(e.ChunkID, hashindex.Entry{
				Flags:     hashindex.FUsed,
				PackID:    pid,
				ObjOffset: uint32(e.Offset),
				ObjSize:   uint32(e.Size),
			}); err != nil {
				packErr = err
				return false
			}
			return true
		})
		if err != nil {
			return nil, err
		}
		if packErr != nil {
			return nil, packErr
		}
	}

	// Everything here came from the repository, so nothing is new.
	chunks.ClearNew()
	return chunks, nil
}
