// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/diff_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"io"
	"reflect"
	"sort"
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

	changed := 0
	err = archive.Compare(a, b, opts, func(d archive.Diff) error {
		changed++
		if *jsonLines {
			return writeDiffJSON(e.Stdout, d)
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

// writeDiffJSON prints one changed path as borg's --json-lines does.
//
// borge printed a document of its own here: "kind" where borg says "type", a
// "description" borg does not send at all, and "from"/"to" where borg says "item1"/
// "item2". The values differed too - timestamps in borg's human layout rather than
// ISO-8601, and an owner change as one "user:group" string rather than a two-element
// array. See docs/DIVERGENCES.md #43.
//
// The names borg uses for "type" are not uniform, so they are spelled out rather than
// derived: a content change is the bare word ("added", "removed", "modified"), a timestamp
// is the bare attribute ("mtime"), and everything else is the phrase ("changed mode").
func writeDiffJSON(w io.Writer, d archive.Diff) error {
	// dumpObject rather than a map, because the encoder below is pydump's: it writes
	// Python's escapes and takes Python's types. Keys go in sorted, as borg's
	// json.dumps(..., sort_keys=True) writes them.
	changes := make([]any, 0, len(d.Changes))
	for _, c := range d.Changes {
		out := map[string]any{}
		switch c.Kind {
		case archive.ChangeAdded, archive.ChangeRemoved:
			// borg sends both counts, including the zero: an "added" change says how much
			// was added and that nothing was removed.
			out["type"] = string(c.Kind)
			out["added"] = c.Added
			out["removed"] = c.Removed
		case archive.ChangeModified:
			out["type"] = string(c.Kind)
			// Only when the chunk ids could be compared. borg sends "modified" alone when
			// they could not, and a zero pair would claim a measurement it did not make.
			if c.Added != 0 || c.Removed != 0 {
				out["added"] = c.Added
				out["removed"] = c.Removed
			}
		case archive.ChangeMTime, archive.ChangeCTime:
			out["type"] = string(c.Kind)
			out["item1"], out["item2"] = c.FromJSON, c.ToJSON
		case archive.ChangeLink:
			// borg records that the link changed and not what it changed to; borge's text
			// form names both, which is more useful and is not this document.
			out["type"] = c.Description
		default:
			out["type"] = c.Description
			out["item1"], out["item2"] = diffValue(c.FromJSON, c.From), diffValue(c.ToJSON, c.To)
		}
		changes = append(changes, sortedDumpObject(out))
	}
	doc := newDumpObject().set("changes", changes).set("path", d.Path)
	// Written through the surrogate-escaping encoder rather than encoding/json, because
	// diff is one of the two places borg leaves a non-unicode path as Python's \udcXX
	// escapes - the other being "debug dump-*". The item and archive objects use the ?
	// plus _b64 form instead. Two representations, both borg's, and this is the one that
	// belongs here.
	if err := writeDumpJSON(w, doc, 0); err != nil {
		return err
	}
	// One object per line: writeDumpJSON is shared with "debug dump-*", which writes a
	// single document and ends it itself.
	_, err := io.WriteString(w, "\n")
	return err
}

// diffValue prefers the JSON-shaped value where the comparison provided one.
func diffValue(jsonForm any, text string) any {
	if jsonForm != nil {
		return jsonForm
	}
	return text
}

// sortedDumpObject turns a change into the encoder's object type, keys in sorted order.
func sortedDumpObject(m map[string]any) *dumpObject {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := newDumpObject()
	for _, k := range keys {
		v := m[k]
		// The encoder speaks Python's types: an int64 rather than Go's int, and []any
		// rather than a typed slice.
		switch t := v.(type) {
		case int:
			v = int64(t)
		case []any:
			// already fine
		}
		o.set(k, v)
	}
	return o
}
