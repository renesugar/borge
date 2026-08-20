// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of PackRecompressor in borg's
// src/borg/archiver/repo_compress_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
)

// cmdRepoCompress recompresses everything already stored.
//
// # Why this is not "recreate --compression"
//
// A chunk's id is the hash of its *plaintext*, so compression lives below the id: a
// recompressed chunk has the same id as before. Every path that writes chunks
// deduplicates, so writing one again is a no-op - the repository already has that id and
// keeps the bytes it already had.
//
// Recompressing therefore cannot go through the chunk-writing path at all. It has to
// rewrite the stored *objects* in place, pack by pack, which is what this does. borg has
// the same split for the same reason.
func cmdRepoCompress(e *Env, args []string) int {
	fs := newFlagSet(e, "repo-compress")
	var common commonFlags
	common.register(fs)
	compression := fs.String("C", "lz4", "recompress everything to this spec, e.g. zstd,3")
	fs.StringVar(compression, "compression", "lz4", "recompress everything to this spec")
	dryRun := fs.Bool("dry-run", false, "report what would be recompressed, change nothing (borge only on this command)")
	stats := fs.Bool("stats", false, "print statistics when finished")
	fs.BoolVar(stats, "s", false, "print statistics when finished")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	compressor, err := compress.FromSpec(*compression)
	if err != nil {
		return e.fail(err)
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, true, manifest.OpCheck)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	ro := o.manifest.RepoObj()
	ro.SetCompressor(compressor)

	result, err := recompressRepository(o.repo, ro, *dryRun, func(line string) {
		if common.verbose || *dryRun {
			fmt.Fprintln(e.Stderr, line)
		}
	})
	if err != nil {
		return e.fail(err)
	}

	if *stats || common.verbose || *dryRun {
		fmt.Fprintf(e.Stderr, "Objects: %d total, %d recompressed, %d already as wanted, %d kept (no gain)\n",
			result.Total, result.Recompressed, result.AlreadyWanted, result.Kept)
		fmt.Fprintf(e.Stderr, "Repository: %d bytes before, %d after\n", result.BytesBefore, result.BytesAfter)
	}
	return ExitOK
}

// recompressResult counts what a recompression did. The three object counters are
// disjoint and sum to Total.
type recompressResult struct {
	Total         int
	Recompressed  int
	AlreadyWanted int
	Kept          int
	BytesBefore   int64
	BytesAfter    int64
}

// recompressRepository rewrites every pack whose objects are not already stored with the
// wanted compression.
//
// Pack at a time, not object at a time. Replacing objects individually would leave the old
// copies as bytes no index entry covers, and compaction deliberately preserves those - so
// the repository would grow rather than shrink. borg's transform_pack exists for the same
// reason.
func recompressRepository(repo *repository.Repository, ro *repoobj.RepoObj, dryRun bool,
	report func(string),
) (*recompressResult, error) {
	result := &recompressResult{}

	chunks, err := repo.Chunks()
	if err != nil {
		return nil, err
	}
	result.BytesBefore = packBytes(repo)

	// The packs to consider, in a stable order so two runs agree.
	packSet := map[string]bool{}
	chunks.Iterate(func(_ []byte, entry hashindex.Entry) bool {
		packSet[hex.EncodeToString(entry.PackID[:])] = true
		return true
	})
	var packs []string
	for pid := range packSet {
		packs = append(packs, pid)
	}
	sort.Strings(packs)

	wantType, wantLevel := ro.Compressor().ID(), uint8(ro.Compressor().Level())

	for _, pid := range packs {
		packID, err := hex.DecodeString(pid)
		if err != nil {
			continue
		}
		transform := func(id, obj []byte) ([]byte, error) {
			result.Total++
			meta, err := ro.ParseMeta(id, obj, repoobj.TypeDontCare)
			if err != nil {
				// Unreadable metadata is check's problem. Carrying the object forward
				// unchanged is the only safe thing a re-encoding can do with it.
				report(fmt.Sprintf("carrying %s forward unchanged: %v", hex.EncodeToString(id), err))
				return obj, nil
			}
			if meta.CType == wantType && meta.CLevel == wantLevel {
				result.AlreadyWanted++
				return obj, nil
			}
			fullMeta, data, err := ro.Parse(id, obj, repoobj.TypeDontCare, repoobj.ParseOptions{})
			if err != nil {
				report(fmt.Sprintf("carrying %s forward unchanged: %v", hex.EncodeToString(id), err))
				return obj, nil
			}
			newObj, err := ro.Format(id, &repoobj.Meta{Type: fullMeta.Type}, data)
			if err != nil {
				return nil, err
			}
			// The compressor may have decided not to compress after all: a deciding
			// compressor stores incompressible data uncompressed whatever it was told.
			// Rewriting such an object on every run for no gain is the loop borg guards
			// against, so it is carried forward as it is.
			newMeta, err := ro.ParseMeta(id, newObj, repoobj.TypeDontCare)
			if err != nil {
				return nil, err
			}
			if newMeta.CType == meta.CType && newMeta.CLevel == meta.CLevel {
				result.Kept++
				return obj, nil
			}
			result.Recompressed++
			return newObj, nil
		}

		if dryRun {
			// Run the transform for its accounting and throw the result away.
			reader := repository.NewPackReader(repo.Store(), packID)
			if err := reader.IterHeaders(func(e repository.PackEntry) bool {
				data, err := reader.Read(e.Offset, e.Size)
				if err != nil {
					return true
				}
				_, _ = transform(e.ChunkID, data)
				return true
			}); err != nil {
				report(fmt.Sprintf("pack %s: %v", pid[:8], err))
			}
			continue
		}

		changed, err := repo.TransformPack(packID, transform)
		if err != nil {
			return result, err
		}
		if changed {
			report(fmt.Sprintf("rewrote pack %s", pid[:8]))
		}
	}

	if !dryRun && result.Recompressed > 0 {
		if err := repo.Flush(); err != nil {
			return result, err
		}
		if err := repo.WriteFullChunkIndex(); err != nil {
			return result, err
		}
	}
	result.BytesAfter = packBytes(repo)
	return result, nil
}

// packBytes is the on-disk size of a repository's packs.
func packBytes(repo *repository.Repository) int64 {
	names, err := repo.Store().ListNames("packs", false)
	if err != nil {
		return 0
	}
	var total int64
	for _, name := range names {
		id, err := hex.DecodeString(name)
		if err != nil {
			continue
		}
		if info, err := repo.Store().Info(repository.PackName(id), false); err == nil {
			total += info.Size
		}
	}
	return total
}
