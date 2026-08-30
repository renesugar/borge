// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the find command in borg's src/borg/archiver/find_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/cache"
	"github.com/renesugar/borge/internal/formatter"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
)

// cmdFind searches for paths across archives.
//
// # What it costs, and why it is still worth having
//
// There is no path index. Answering "which archives contain this file?" means reading the
// item stream of every selected archive - the same work `list` does, once per archive. On
// a repository with hundreds of archives that is minutes, not milliseconds.
//
// The alternative is running `list` in a shell loop, which costs exactly the same and
// gets the archive ordering, the pattern styles and the soft-delete handling wrong. So the
// command exists to make the expensive thing correct rather than to make it cheap.
//
// # Newest first
//
// Archives are searched newest first, because the question behind "where did that file end
// up" is nearly always "when did it last exist". --reverse turns it around for the other
// question, "when did it first appear".
func cmdFind(e *Env, args []string) int {
	fs := newFlagSet(e, "find")
	var common commonFlags
	var pf patternFlags
	var sel listSelectors
	common.register(fs)
	pf.register(fs)
	sel.register(fs, selectorExtras{deleted: true, reverse: true})
	jsonLines := fs.Bool("json-lines", false, "print one JSON object per match")
	short := fs.Bool("short", false, "print only archive id and path (borge only on this command)")
	format := fs.String("format", "", "output format, e.g. '{archivename} {path}{NL}'")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	// borg's default carries the archive id and name, because a find crosses archives and
	// a bare path would not say which one it came from.
	findFormat, _ := e.lookupBorg("FIND_FORMAT")
	template := itemFormat(*format, false, findFormat,
		"{archiveid:.8} {archivename} {mode} {user:6} {group:6} {size:8} {mtime} {path}{extra}{NL}")
	if err := checkItemFormat(template); err != nil {
		return e.fail(err)
	}
	paths := fs.Args()
	if len(paths) == 0 && !pf.any() {
		e.errorf("find needs a path or a pattern; with neither it would print every item " +
			"of every archive, which is what 'list' is for")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false, manifest.OpRead)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	matcher, err := pf.matcher(paths)
	if err != nil {
		return e.fail(err)
	}

	// Newest first unless asked otherwise: see the note above. listSelectors.reverse is
	// relative to the manifest's own oldest-first order, so it is inverted here.
	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
	}
	opts.Reverse = !opts.Reverse
	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}

	// Can the cache answer outright, or only prove a negative?
	//
	// `find` normally prints whole items, so a path cache cannot serve it. But the two
	// commonest interactive forms - --short, and a --format naming nothing but the path and
	// its archive - need only what the cache already holds, and for those the item stream
	// need not be read at all. Anything else (--json-lines, or a template asking for size,
	// mode, target...) falls back to reading it.
	//
	// This matters for more than speed. Without it the cache makes a search whose pattern
	// matches everywhere *slower*: every path gets matched twice, once from the cache and
	// once from the stream, and matching is a third of the work. Measured at +25% before
	// this existed.
	servedFromPaths := !*jsonLines
	if servedFromPaths && !*short {
		keys, kerr := formatter.Keys(template)
		if kerr != nil {
			servedFromPaths = false
		}
		for _, k := range keys {
			if _, static := formatter.Static[k]; static {
				continue // NL, TAB and friends need no item
			}
			switch k {
			case "path", "archivename", "archiveid":
			default:
				servedFromPaths = false
			}
		}
	}

	enc := json.NewEncoder(e.Stdout)
	matches := 0
	skipped := 0
	served := 0
	// collect points at the slice gathering this archive's paths, when the cache missed and
	// the stream is being read anyway. nil means the cache answered.
	var collect *[]string
	status := ExitOK

	for i, info := range infos {
		if !info.Exists {
			// A directory entry whose object is missing is reported rather than skipped:
			// a search that quietly omits an archive answers "no" when it means "unknown".
			e.warnf("archive %s: %s (not searched)", hex.EncodeToString(info.ID)[:8], info.Problem)
			status = ExitWarning
			continue
		}
		if common.verbose {
			e.warnf("searching %s %s (%d/%d)", info.Name,
				formatTime(info.Time.Local()), i+1, len(infos))
		}

		a, err := archive.Open(o.manifest, info.ID)
		if err != nil {
			e.warnf("archive %s (%s): %v", hex.EncodeToString(info.ID)[:8], info.Name, err)
			status = ExitWarning
			continue
		}

		id := hex.EncodeToString(info.ID)

		// The path cache decides one thing only: that this archive holds nothing matching,
		// so its item stream need not be read. That is safe whatever --format or
		// --json-lines asks for, because an archive with no match prints nothing in any of
		// them. Everything above this point - the warnings, the verbose "searching" line -
		// has already run, so skipping changes no output at all, only how long it took.
		//
		// A cache that is missing, truncated, foreign or corrupt returns ErrNoPaths and the
		// item stream is read as before.
		if cached, cerr := cache.ReadPaths(o.repo.ID(), info.ID); cerr == nil {
			var hits []string
			for _, p := range cached {
				if matcher.Match(p) {
					hits = append(hits, p)
					if !servedFromPaths {
						break // only the existence of a match is needed
					}
				}
			}
			if len(hits) == 0 {
				skipped++
				continue
			}
			if servedFromPaths {
				served++
				emitErr := error(nil)
				for _, p := range hits {
					matches++
					if *short {
						if _, werr := fmt.Fprintf(e.Stdout, "%s %s\n", id[:8], p); werr != nil {
							emitErr = werr
							break
						}
						continue
					}
					line, ferr := formatter.Format(template, map[string]any{
						"path": p, "archivename": info.Name, "archiveid": id,
					})
					if ferr != nil {
						emitErr = ferr
						break
					}
					if _, werr := fmt.Fprint(e.Stdout, line); werr != nil {
						emitErr = werr
						break
					}
				}
				if emitErr != nil {
					e.warnf("archive %s (%s): %v", id[:8], info.Name, emitErr)
					status = ExitWarning
				}
				continue
			}
		} else {
			// Reading the stream anyway, so record the paths on the way past. An archive is
			// immutable, so this entry can never go stale - only be evicted or corrupted.
			var seen []string
			collect = &seen
		}

		err = a.Items(func(it *item.Item) error {
			if collect != nil {
				*collect = append(*collect, it.Path)
			}
			if !matcher.Match(it.Path) {
				return nil
			}
			matches++
			switch {
			case *jsonLines:
				// One flat item object, as borg emits: the archive it came from is named
				// by the archivename and archiveid keys *inside* it, and only when the
				// format asks for them. borge wrapped the item in an envelope of its own
				// ({archive_id, archive_name, archive_time, item}), which is a different
				// document from borg's under the same option. See docs/DIVERGENCES.md #43.
				data, err := itemJSONData(it, template, info.Name, id)
				if err != nil {
					return err
				}
				return enc.Encode(data)
			case *short:
				_, err := fmt.Fprintf(e.Stdout, "%s %s\n", id[:8], it.Path)
				return err
			default:
				line, err := formatter.Format(template, itemValues(it, info.Name, id))
				if err != nil {
					return err
				}
				_, err = fmt.Fprint(e.Stdout, line)
				return err
			}
		})
		if err != nil {
			e.warnf("archive %s (%s): %v", id[:8], info.Name, err)
			status = ExitWarning
		} else if collect != nil {
			// Only after a clean pass: a stream that failed part way through has a partial
			// path list, and a partial list is one that can wrongly prove a negative.
			if werr := cache.WritePaths(o.repo.ID(), info.ID, *collect); werr != nil && common.verbose {
				e.warnf("path cache for %s: %v", id[:8], werr)
			}
		}
		collect = nil
	}

	if common.verbose {
		fmt.Fprintf(e.Stdout, "%d match(es) in %d archive(s), %d skipped and %d served by the path cache\n",
			matches, len(infos), skipped, served)
	}
	return status
}
