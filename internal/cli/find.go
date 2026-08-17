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
	"time"

	"github.com/renesugar/borge/internal/archive"
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
	sel.register(fs)
	jsonLines := fs.Bool("json-lines", false, "print one JSON object per match")
	short := fs.Bool("short", false, "print only archive id and path")
	if err := fs.Parse(args); err != nil {
		return ExitError
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

	enc := json.NewEncoder(e.Stdout)
	matches := 0
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
		err = a.Items(func(it *item.Item) error {
			if !matcher.Match(it.Path) {
				return nil
			}
			matches++
			switch {
			case *jsonLines:
				return enc.Encode(foundJSON{
					ArchiveID:   id,
					ArchiveName: info.Name,
					Time:        formatTime(info.Time.Local()),
					Item:        toItemJSON(it),
				})
			case *short:
				_, err := fmt.Fprintf(e.Stdout, "%s %s\n", id[:8], it.Path)
				return err
			default:
				_, err := fmt.Fprintf(e.Stdout, "%s %s %s\n", id[:8], info.Name, findItemLine(it))
				return err
			}
		})
		if err != nil {
			e.warnf("archive %s (%s): %v", id[:8], info.Name, err)
			status = ExitWarning
		}
	}

	if common.verbose {
		fmt.Fprintf(e.Stdout, "%d match(es) in %d archive(s)\n", matches, len(infos))
	}
	return status
}

// foundJSON is one match. The archive fields are what distinguish it from list's output:
// the archives of a series share a name, so only the id says which one this is.
type foundJSON struct {
	ArchiveID   string   `json:"archive_id"`
	ArchiveName string   `json:"archive_name"`
	Time        string   `json:"archive_time"`
	Item        itemJSON `json:"item"`
}

// findItemLine renders one item the way list's default format does.
func findItemLine(it *item.Item) string {
	mode := it.ModeOr(0)
	owner, group := "", ""
	if it.User != nil {
		owner = *it.User
	}
	if it.Group != nil {
		group = *it.Group
	}
	mtime := ""
	if it.MTime != nil {
		mtime = formatTime(time.Unix(0, *it.MTime))
	}
	line := fmt.Sprintf("%s %-6s %-6s %8d %s %s",
		item.FormatMode(mode), owner, group, itemSize(it), mtime, it.Path)
	if it.IsSymlink() && it.Target != nil {
		line += " -> " + *it.Target
	}
	return line
}
