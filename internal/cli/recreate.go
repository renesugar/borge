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
	sel.register(fs, selectorExtras{})
	pf.register(fs)
	target := fs.String("target", "", "write to this archive name instead of replacing the original")
	comment := fs.String("comment", "", "replace the archive's comment")
	compression := fs.String("C", "", "recompress with this spec, e.g. zstd,3")
	fs.StringVar(compression, "compression", "", "recompress with this spec")
	chunkerParams := fs.String("chunker-params", "", "re-chunk with these parameters")
	dryRun := fs.Bool("dry-run", false, "say what would happen, change nothing")
	fs.BoolVar(dryRun, "n", false, "say what would happen, change nothing")
	excludeCaches := fs.Bool("exclude-caches", false,
		"drop directories holding a CACHEDIR.TAG with the standard signature")
	var excludeIfPresent multiFlag
	fs.Var(&excludeIfPresent, "exclude-if-present",
		"drop directories holding a file with this name (repeatable)")
	keepExcludeTags := fs.Bool("keep-exclude-tags", false,
		"keep the tag files themselves when their directories are dropped")
	list := fs.Bool("list", false, "print each item as it is processed")
	statusFilter := fs.String("filter", "", "only list items whose status is one of these characters")
	stats := fs.Bool("stats", false, "print statistics when finished")
	fs.BoolVar(stats, "s", false, "print statistics when finished")
	var timestamp timestampFlag
	timestamp.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	// borg validates both of these at parse time, so a bad --target or --comment is
	// refused before the archive is read.
	targetGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "target" {
			targetGiven = true
		}
	})
	if targetGiven {
		// An explicit empty --target is a bad name, not an absent one.
		if err := validateArchiveName(*target); err != nil {
			return e.fail(err)
		}
	}
	if err := validateComment(*comment); err != nil {
		return e.fail(err)
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

	e.setStatusFilter(*statusFilter)

	// The positional arguments are PATHS, not an archive name.
	//
	// borg's recreate takes "[PATH ...]" and nothing else: archives are selected with -a,
	// and every positional is a path to keep. borge read the first positional as an
	// archive name instead, so the same command line meant two different things - and the
	// dangerous direction is not the one you would guess. "borge recreate ARCHIVE" keeps
	// the whole archive; run under borg, that same line recreates EVERY archive in the
	// repository keeping only paths matching "ARCHIVE", which empties all of them.
	//
	// The option gate could not see this: it compares options, and this is a positional.
	// Found on 2026-08-20 by passing borg an archive name where it wanted a path and
	// watching the archive come back empty. See DIVERGENCES.md #54.
	matcher, err := pf.matcher(fs.Args())
	if err != nil {
		return e.fail(err)
	}

	selector, err := sel.options(e)
	if err != nil {
		return e.fail(err)
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

	tagged := *excludeCaches || len(excludeIfPresent) > 0
	// Whether there is work to do is decided before the loop, so it has to be asked of the
	// base matcher: the per-archive tag patterns are added inside it. An empty matcher
	// includes everything, so only one holding patterns counts as a filter.
	hasPatterns := !matcher.Empty()
	opts := archive.RecreateOptions{
		Timestamp:      timestamp.value(),
		Target:         *target,
		Comment:        commentPtr,
		ChunkerParams:  params,
		ChunkSeed:      chunkSeed,
		Compressor:     compressor,
		DeleteOriginal: *target == "",
		DryRun:         *dryRun,
		// Tag exclusion counts as work even with no patterns: it is the option that adds
		// them, one archive at a time.
		ExcludesByTag: tagged || hasPatterns,
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
		fmt.Fprintln(e.Stderr, "nothing to do: pass --chunker-params, --compression, "+
			"a pattern, or --comment")
		return ExitOK
	}
	if *list {
		// On stderr, where borg puts it: "borg recreate --list" writes its listing to
		// stderr and leaves stdout for the command's data. Under --log-json it becomes a
		// file_status object, which borg emits for create and recreate and no other
		// command.
		opts.OnItem = func(st byte, p string) { e.logFileStatus(st, p) }
	}

	for _, info := range infos {
		// The tag scan is per archive, as borg's is - the tagged directories of one
		// archive are not the tagged directories of another. borg shares one matcher
		// across every archive in the run, so its patterns accumulate; borge gives each
		// archive its own copy of the pattern set, which is the same answer for a single
		// archive and a defensible one for several. See DIVERGENCES.md #54.
		archiveMatcher := matcher
		if tagged {
			a, err := archive.Open(m, info.ID)
			if err != nil {
				return e.fail(fmt.Errorf("%s: %w", info.Name, err))
			}
			archiveMatcher = matcher.Clone()
			if err := archive.AddTaggedDirs(a, archiveMatcher, *excludeCaches,
				excludeIfPresent, *keepExcludeTags); err != nil {
				return e.fail(fmt.Errorf("%s: %w", info.Name, err))
			}
		}
		// An empty matcher includes everything, so passing it as a filter would make every
		// recreate look like it has work to do. Only a matcher with patterns counts.
		opts.Filter = nil
		if !archiveMatcher.Empty() {
			opts.Filter = func(it *item.Item) bool { return archiveMatcher.Match(it.Path) }
		}

		result, newID, err := archive.Recreate(m, info.ID, opts)
		if err != nil {
			return e.fail(fmt.Errorf("%s: %w", info.Name, err))
		}
		if *dryRun {
			fmt.Fprintf(e.Stderr, "%s: would keep %d item(s), exclude %d\n",
				info.Name, result.ItemsKept, result.ItemsExcluded)
			continue
		}
		if *stats || common.verbose {
			fmt.Fprintf(e.Stderr,
				"%s -> %s: kept %d item(s), excluded %d, rewrote %d file(s), read %d chunk(s) (%d bytes)\n",
				info.Name, hex.EncodeToString(newID)[:8], result.ItemsKept, result.ItemsExcluded,
				result.FilesRewrit, result.ChunksRead, result.BytesRead)
		}
	}

	if !*dryRun {
		if err := m.Write(); err != nil {
			return e.fail(err)
		}
		fmt.Fprintln(e.Stderr, "the original archives are soft-deleted; "+
			"run 'borge compact' to reclaim the space")
	}
	return ExitOK
}
