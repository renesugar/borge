// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/recreate_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// cmdRecreate rewrites archives with different chunking, compression or contents.
//
// It is the only way to change what is already stored. Excluding a path from future
// backups does nothing about the copies already in the repository; recreate is what
// removes them.
func cmdRecreate(e *Env, args []string) int {
	fs := newFlagSet(e, "recreate")
	var common commonFlags
	var sel listSelectors
	var pf patternFlags
	common.register(fs)
	sel.register(fs)
	pf.register(fs)
	target := fs.String("target", "", "write to this archive name instead of replacing the original")
	comment := fs.String("comment", "", "replace the archive's comment")
	compression := fs.String("C", "", "recompress with this spec, e.g. zstd,3")
	fs.StringVar(compression, "compression", "", "recompress with this spec")
	chunkerParams := fs.String("chunker-params", "", "re-chunk with these parameters")
	dryRun := fs.Bool("dry-run", false, "say what would happen, change nothing")
	list := fs.Bool("list", false, "print each item as it is processed")
	stats := fs.Bool("stats", false, "print statistics when finished")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	var params *chunker.Params
	if *chunkerParams != "" {
		p, err := chunker.ParseParams(*chunkerParams)
		if err != nil {
			return e.fail(err)
		}
		params = &p
	}
	var compressor compress.Compressor
	if *compression != "" {
		c, err := compress.FromSpec(*compression)
		if err != nil {
			return e.fail(err)
		}
		compressor = c
	}

	// A comment is only replaced when the flag was actually given, so an empty --comment
	// clears it and no --comment leaves it alone.
	var commentPtr *string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "comment" {
			commentPtr = comment
		}
	})

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, err := repository.Open(path, repository.Options{Exclusive: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	k, unlocked, err := repo.Unlock(e.passphrase())
	if err != nil {
		return e.fail(err)
	}
	m, err := manifest.Load(repo, k, manifest.OpWrite)
	if err != nil {
		return e.fail(err)
	}
	var chunkSeed uint32
	if unlocked != nil {
		chunkSeed = uint32(key.ChunkSeed(unlocked.Material))
	}

	matcher, err := pf.matcher(nil)
	if err != nil {
		return e.fail(err)
	}
	// An empty matcher includes everything, so passing it as a filter would make every
	// recreate look like it has work to do. Only a matcher with patterns counts.
	var filter func(*item.Item) bool
	if !matcher.Empty() {
		filter = func(it *item.Item) bool { return matcher.Match(it.Path) }
	}

	selector := sel.options()
	if selector.Match == nil && fs.NArg() > 0 {
		selector.Match = []string{fs.Arg(0)}
	}
	infos, err := m.Archives.List(selector)
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		e.errorf("no archive matches")
		return ExitError
	}
	if *target != "" && len(infos) > 1 {
		e.errorf("--target names one archive, but %d were selected", len(infos))
		return ExitError
	}

	opts := archive.RecreateOptions{
		Target:         *target,
		Comment:        commentPtr,
		ChunkerParams:  params,
		ChunkSeed:      chunkSeed,
		Compressor:     compressor,
		Filter:         filter,
		DeleteOriginal: *target == "",
		DryRun:         *dryRun,
	}
	// --compression on its own would appear to work and do nothing. A chunk's id is the
	// hash of its plaintext, so a recompressed chunk has the same id, and every path that
	// writes chunks deduplicates - the repository keeps the bytes it already had. borg has
	// the same behaviour and does not say so; borge points at the command that does work.
	if compressor != nil && params == nil {
		e.errorf("--compression on its own recompresses nothing: a recompressed chunk has the " +
			"same id, so it deduplicates against what is already stored. Use " +
			"'borge repo-compress -C ...' to recompress an existing repository, or pass " +
			"--chunker-params as well to re-chunk and recompress together.")
		return ExitError
	}
	if !opts.NeedsWork() {
		fmt.Fprintln(e.Stdout, "nothing to do: pass --chunker-params, --compression, "+
			"a pattern, or --comment")
		return ExitOK
	}
	if *list {
		opts.OnItem = func(st byte, p string) { fmt.Fprintf(e.Stdout, "%c %s\n", st, p) }
	}

	for _, info := range infos {
		result, newID, err := archive.Recreate(m, info.ID, opts)
		if err != nil {
			return e.fail(fmt.Errorf("%s: %w", info.Name, err))
		}
		if *dryRun {
			fmt.Fprintf(e.Stdout, "%s: would keep %d item(s), exclude %d\n",
				info.Name, result.ItemsKept, result.ItemsExcluded)
			continue
		}
		if *stats || common.verbose {
			fmt.Fprintf(e.Stdout,
				"%s -> %s: kept %d item(s), excluded %d, rewrote %d file(s), read %d chunk(s) (%d bytes)\n",
				info.Name, hex.EncodeToString(newID)[:8], result.ItemsKept, result.ItemsExcluded,
				result.FilesRewrit, result.ChunksRead, result.BytesRead)
		}
	}

	if !*dryRun {
		if err := m.Write(); err != nil {
			return e.fail(err)
		}
		fmt.Fprintln(e.Stdout, "the original archives are soft-deleted; "+
			"run 'borge compact' to reclaim the space")
	}
	return ExitOK
}
