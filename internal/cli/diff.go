// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/diff_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/manifest"
)

// cmdDiff reports what changed between two archives.
func cmdDiff(e *Env, args []string) int {
	fs := newFlagSet(e, "diff")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	jsonLines := fs.Bool("json-lines", false, "print one JSON object per changed path")
	contentOnly := fs.Bool("content-only", false, "report only content changes, not metadata")
	numericIDs := fs.Bool("numeric-ids", false, "compare numeric uid/gid rather than names")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 2 {
		e.errorf("diff needs two archives")
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

	a, err := openArchive(o.manifest, fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	b, err := openArchive(o.manifest, fs.Arg(1))
	if err != nil {
		return e.fail(err)
	}

	matcher, err := pf.matcher(fs.Args()[2:])
	if err != nil {
		return e.fail(err)
	}

	// Chunk ids are only comparable when both archives were chunked the same way. If they
	// were not, every file would look modified, so the comparison falls back to sizes and
	// says nothing it cannot support.
	comparable := sameChunkerParams(a, b)
	if !comparable && common.verbose {
		e.warnf("the two archives were chunked differently (%v vs %v), so content is compared "+
			"by size only", a.Meta.ChunkerParams, b.Meta.ChunkerParams)
	}

	opts := archive.DiffOptions{
		CanCompareChunkIDs: comparable,
		NumericIDs:         *numericIDs,
		ContentOnly:        *contentOnly,
		Filter:             func(p string) bool { return matcher.Match(p) },
	}

	enc := json.NewEncoder(e.Stdout)
	changed := 0
	err = archive.Compare(a, b, opts, func(d archive.Diff) error {
		changed++
		if *jsonLines {
			type change struct {
				Kind        string `json:"kind"`
				Description string `json:"description"`
				From        string `json:"from,omitempty"`
				To          string `json:"to,omitempty"`
				Added       int64  `json:"added,omitempty"`
				Removed     int64  `json:"removed,omitempty"`
			}
			out := struct {
				Path    string   `json:"path"`
				Changes []change `json:"changes"`
			}{Path: d.Path}
			for _, c := range d.Changes {
				out.Changes = append(out.Changes, change{
					Kind: string(c.Kind), Description: c.Description,
					From: c.From, To: c.To, Added: c.Added, Removed: c.Removed,
				})
			}
			return enc.Encode(out)
		}
		var parts []string
		for _, c := range d.Changes {
			if c.From != "" || c.To != "" {
				parts = append(parts, fmt.Sprintf("%s: %s -> %s", c.Description, c.From, c.To))
			} else {
				parts = append(parts, c.Description)
			}
		}
		_, err := fmt.Fprintf(e.Stdout, "%s %s\n", strings.Join(parts, " "), d.Path)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	if common.verbose {
		fmt.Fprintf(e.Stdout, "%d path(s) differ\n", changed)
	}
	return ExitOK
}

// sameChunkerParams reports whether two archives were chunked identically.
//
// It compares the recorded parameter list rather than assuming: an archive made with
// different --chunker-params has different chunk ids for the same bytes, and comparing
// them would report every file as modified.
func sameChunkerParams(a, b *archive.Archive) bool {
	if !a.Meta.ChunkerParamsSet || !b.Meta.ChunkerParamsSet {
		// An archive that does not record them could have been made with anything.
		return false
	}
	return reflect.DeepEqual(a.Meta.ChunkerParams, b.Meta.ChunkerParams)
}
