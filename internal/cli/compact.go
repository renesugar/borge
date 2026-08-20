// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ArchiveGarbageCollector's archive analysis in borg's
// src/borg/archiver/compact_cmd.py. The pack rewriting is in internal/repository.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"fmt"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// cmdCompact reclaims the space of chunks no archive references any more.
//
// # The order of operations, and why it is that order
//
//  1. Walk every archive and collect the chunk ids it references.
//  2. If anything is missing, stop - and *keep* the soft-deleted archives, because a
//     damaged repository is one where `borge undelete` may still be the way out.
//  3. Otherwise remove the soft-deleted archives' directory entries for good, which is
//     what makes their chunks unreferenced.
//  4. Sweep.
//
// Step 2 is the one that matters. A garbage collector that proceeds on a partial view of
// what is referenced deletes live data, and it does so silently: the archive that needed
// the chunk is not read again until a restore, which is exactly the moment the user
// cannot afford to find out.
func cmdCompact(e *Env, args []string) int {
	fs := newFlagSet(e, "compact")
	var common commonFlags
	common.register(fs)
	threshold := fs.Int("threshold", repository.DefaultCompactThreshold,
		"rewrite a pack when this percentage of it is reclaimable")
	dryRun := fs.Bool("dry-run", false, "say what would be reclaimed, change nothing")
	stats := fs.Bool("stats", false, "print statistics when finished")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, true, manifest.OpDelete)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	// Step 1: what is still referenced.
	used, live, err := collectReferences(e, o, common.verbose)
	if err != nil {
		return e.fail(err)
	}

	// Step 2: a soft-deleted archive's chunks count as referenced only while the
	// repository is intact. Collecting them here would be the difference between "undelete
	// still works" and "the data is gone".
	deletedIDs, err := o.manifest.Archives.IDs(true)
	if err != nil {
		return e.fail(err)
	}

	// Step 3: remove the soft-deleted directory entries, which is what releases their
	// chunks. Only once the live scan came back complete.
	if !*dryRun {
		for _, id := range deletedIDs {
			if err := o.manifest.Archives.Nuke(id); err != nil {
				return e.fail(err)
			}
			if common.verbose {
				fmt.Fprintf(e.Stderr, "removed soft-deleted archive %s\n", hex.EncodeToString(id)[:8])
			}
		}
	} else if len(deletedIDs) > 0 {
		fmt.Fprintf(e.Stderr, "would remove %d soft-deleted archive(s)\n", len(deletedIDs))
	}

	// Step 4: sweep.
	result, err := o.repo.Compact(used, repository.CompactOptions{
		Threshold: *threshold,
		DryRun:    *dryRun,
		OnProgress: func(line string) {
			if common.verbose || *dryRun {
				fmt.Fprintln(e.Stderr, line)
			}
		},
	})
	if err != nil {
		return e.fail(err)
	}

	if !*dryRun {
		if err := o.manifest.Write(); err != nil {
			return e.fail(err)
		}
	}

	if *stats || common.verbose || *dryRun {
		fmt.Fprintf(e.Stderr, "Archives: %d\n", live)
		fmt.Fprintf(e.Stderr, "Chunks: %d, of which %d still referenced\n",
			result.ChunksBefore, result.ChunksAlive)
		fmt.Fprintf(e.Stderr, "Packs: %d before, %d dropped, %d rewritten, %d unchanged\n",
			result.PacksBefore, result.PacksDropped, result.PacksRewrit, result.PacksKept)
		fmt.Fprintf(e.Stderr, "Repository size: %d bytes before, %d after (%d reclaimed)\n",
			result.BytesBefore, result.BytesAfter, result.BytesBefore-result.BytesAfter)
	}
	return ExitOK
}

// collectReferences walks every archive and returns the set of chunk ids it references,
// keyed by hex id, along with the number of archives seen.
//
// Both live and soft-deleted archives are read *for their own metadata objects*, but only
// live ones contribute references - a soft-deleted archive is about to stop existing. An
// archive that cannot be read at all is an error, not a warning: proceeding would treat
// its chunks as unreferenced.
func collectReferences(e *Env, o *opened, verbose bool) (map[string]bool, int, error) {
	infos, err := o.manifest.Archives.List(manifest.ListOptions{})
	if err != nil {
		return nil, 0, err
	}

	used := map[string]bool{}
	// The archive object and its item pointer blocks are referenced too, not just the
	// file content: they are ordinary chunks in ordinary packs, and forgetting them would
	// delete every archive's own metadata.
	for _, info := range infos {
		if !info.Exists {
			return nil, 0, fmt.Errorf("archive %s is unreadable (%s); refusing to compact, "+
				"because its chunks would be treated as unreferenced",
				hex.EncodeToString(info.ID)[:8], info.Problem)
		}
		a, err := archive.Open(o.manifest, info.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("archive %s: %w", info.Name, err)
		}
		used[hex.EncodeToString(info.ID)] = true
		for _, ptr := range a.Meta.ItemPtrs {
			used[hex.EncodeToString(ptr)] = true
		}
		streamIDs, err := a.ItemStreamIDs()
		if err != nil {
			return nil, 0, fmt.Errorf("archive %s: %w", info.Name, err)
		}
		for _, id := range streamIDs {
			used[hex.EncodeToString(id)] = true
		}
		if err := a.Items(func(it *item.Item) error {
			for _, c := range it.Chunks {
				used[hex.EncodeToString(c.ID)] = true
			}
			return nil
		}); err != nil {
			return nil, 0, fmt.Errorf("archive %s: %w", info.Name, err)
		}
		if verbose {
			fmt.Fprintf(e.Stderr, "analysed %s (%s)\n", info.Name, hex.EncodeToString(info.ID)[:8])
		}
	}
	return used, len(infos), nil
}
