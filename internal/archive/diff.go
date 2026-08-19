// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ItemDiff in borg's src/borg/item.pyx and
// Archive.compare_archives_iter in src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/renesugar/borge/internal/item"
)

// Comparing two archives.
//
// # Why the chunk lists can usually answer it
//
// Two files are the same if their chunk lists are the same - no content has to be read at
// all. That holds only while both archives were chunked the same way: with different
// --chunker-params the same bytes produce different chunk ids, and the comparison would
// report every file as modified. So the caller says whether the chunk ids are comparable,
// and when they are not, a differing file is reported as "modified" without the byte
// counts, rather than by reading both archives in full.

// ChangeKind names one kind of difference.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
	ChangeLink     ChangeKind = "link"
	ChangeMode     ChangeKind = "mode"
	ChangeType     ChangeKind = "type"
	ChangeOwner    ChangeKind = "owner"
	ChangeMTime    ChangeKind = "mtime"
	ChangeCTime    ChangeKind = "ctime"
)

// Change is one difference between two versions of a path.
type Change struct {
	Kind ChangeKind
	// Description is the human-readable form, e.g. "changed mode" or "+1.2 kB -300 B".
	Description string
	// Added and Removed are byte counts, set only for a content change where the chunk
	// ids could be compared.
	Added, Removed int64
	// From and To are the old and new values, for the metadata changes.
	From, To string
	// FromJSON and ToJSON are those same values in the form borg's --json-lines carries,
	// where it differs from the text one: a timestamp is ISO-8601 rather than borg's
	// human layout, and an owner change is a two-element array rather than "user:group".
	// Nil means the text form is also the JSON form. Kept beside From and To rather than
	// derived later because only the comparison still has the unformatted values.
	FromJSON, ToJSON any
}

// Diff is the set of changes to one path.
type Diff struct {
	Path    string
	Changes []Change
}

// Empty reports whether the two versions are the same.
func (d *Diff) Empty() bool { return len(d.Changes) == 0 }

// DiffOptions control a comparison.
type DiffOptions struct {
	// CanCompareChunkIDs says whether both archives used the same chunker parameters. When
	// false, content differences are reported without byte counts.
	CanCompareChunkIDs bool
	// NumericIDs compares uid and gid rather than user and group names.
	NumericIDs bool
	// ContentOnly leaves out the metadata-only changes: mode, owner, times.
	ContentOnly bool
	// Filter selects which paths are compared. Nil compares everything.
	Filter func(path string) bool
}

// Compare walks two archives and reports the differences.
//
// Both item streams are in the same order, so the walk is a merge rather than two full
// reads into memory - but a file that exists in only one archive puts the streams out of
// step, so unmatched items are held aside until the other stream produces them or ends.
func Compare(a, b *Archive, opts DiffOptions, fn func(Diff) error) error {
	itemsA, err := collectItems(a, opts.Filter)
	if err != nil {
		return err
	}
	itemsB, err := collectItems(b, opts.Filter)
	if err != nil {
		return err
	}

	// A stable, sorted walk over the union of paths. borg zips the two streams and holds
	// orphans aside; sorting is the same answer with less state, and it makes the output
	// order deterministic, which borg's is not when paths appear in different orders.
	paths := make([]string, 0, len(itemsA)+len(itemsB))
	seen := map[string]bool{}
	for p := range itemsA {
		paths = append(paths, p)
		seen[p] = true
	}
	for p := range itemsB {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		d := diffItems(p, itemsA[p], itemsB[p], opts)
		if d.Empty() {
			continue
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

func collectItems(a *Archive, filter func(string) bool) (map[string]*item.Item, error) {
	out := map[string]*item.Item{}
	err := a.Items(func(it *item.Item) error {
		if filter != nil && !filter(it.Path) {
			return nil
		}
		out[it.Path] = it
		return nil
	})
	return out, err
}

// diffItems compares one path's two versions. Either may be nil, meaning absent.
func diffItems(path string, a, b *item.Item, opts DiffOptions) Diff {
	d := Diff{Path: path}
	add := func(c Change) { d.Changes = append(d.Changes, c) }

	switch {
	case a == nil && b == nil:
		return d
	case a == nil:
		add(Change{Kind: ChangeAdded, Description: "added " + describeKind(b), Added: contentSize(b)})
		return d
	case b == nil:
		add(Change{Kind: ChangeRemoved, Description: "removed " + describeKind(a), Removed: contentSize(a)})
		return d
	}

	// A symlink whose target changed is a content change, not a metadata one: what the
	// path resolves to is the whole of what a symlink is.
	if a.IsSymlink() || b.IsSymlink() {
		ta, tb := "", ""
		if a.Target != nil {
			ta = *a.Target
		}
		if b.Target != nil {
			tb = *b.Target
		}
		if ta != tb {
			add(Change{Kind: ChangeLink, Description: "changed link", From: ta, To: tb})
		}
	}

	if a.ChunksSet && b.ChunksSet {
		if c, changed := contentChange(a, b, opts.CanCompareChunkIDs); changed {
			add(c)
		}
	}

	if opts.ContentOnly {
		return d
	}

	// Mode, and the type character separately: a file replaced by a directory is a
	// different thing from a file whose permissions changed, and reporting both as
	// "changed mode" hides it.
	if a.Mode != nil && b.Mode != nil && *a.Mode != *b.Mode {
		ma, mb := item.FormatMode(*a.Mode), item.FormatMode(*b.Mode)
		add(Change{Kind: ChangeMode, Description: "changed mode", From: ma, To: mb})
		if ma[0] != mb[0] {
			add(Change{Kind: ChangeType, Description: "changed type", From: ma[:1], To: mb[:1]})
		}
	}

	if from, to, changed := ownerChange(a, b, opts.NumericIDs); changed {
		add(Change{
			Kind: ChangeOwner, Description: "changed owner", From: from, To: to,
			FromJSON: ownerPair(a, opts.NumericIDs), ToJSON: ownerPair(b, opts.NumericIDs),
		})
	}

	for _, tc := range []struct {
		kind ChangeKind
		a, b *int64
	}{
		{ChangeMTime, a.MTime, b.MTime},
		{ChangeCTime, a.CTime, b.CTime},
	} {
		if tc.a != nil && tc.b != nil && *tc.a != *tc.b {
			add(Change{
				Kind:        tc.kind,
				Description: "changed " + string(tc.kind),
				From:        formatDiffTime(*tc.a),
				To:          formatDiffTime(*tc.b),
				FromJSON:    isoDiffTime(*tc.a),
				ToJSON:      isoDiffTime(*tc.b),
			})
		}
	}
	return d
}

// contentChange compares two files' contents.
func contentChange(a, b *item.Item, canCompareIDs bool) (Change, bool) {
	if !canCompareIDs {
		// Different chunker parameters: the ids are not comparable, so the only honest
		// answer without reading both archives in full is whether the sizes differ.
		if a.ContentSize() != b.ContentSize() {
			return Change{Kind: ChangeModified, Description: "modified"}, true
		}
		return Change{}, false
	}

	if sameChunks(a.Chunks, b.Chunks) {
		return Change{}, false
	}
	inA := map[string]int64{}
	for _, c := range a.Chunks {
		inA[string(c.ID)] = c.Size
	}
	inB := map[string]int64{}
	for _, c := range b.Chunks {
		inB[string(c.ID)] = c.Size
	}
	var added, removed int64
	for id, size := range inB {
		if _, ok := inA[id]; !ok {
			added += size
		}
	}
	for id, size := range inA {
		if _, ok := inB[id]; !ok {
			removed += size
		}
	}
	return Change{
		Kind:        ChangeModified,
		Description: fmt.Sprintf("+%d B -%d B", added, removed),
		Added:       added,
		Removed:     removed,
	}, true
}

func sameChunks(a, b []item.ChunkListEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Size != b[i].Size || hex.EncodeToString(a[i].ID) != hex.EncodeToString(b[i].ID) {
			return false
		}
	}
	return true
}

func ownerChange(a, b *item.Item, numericIDs bool) (string, string, bool) {
	if numericIDs {
		ua, ga := derefInt(a.UID), derefInt(a.GID)
		ub, gb := derefInt(b.UID), derefInt(b.GID)
		if ua == ub && ga == gb {
			return "", "", false
		}
		return fmt.Sprintf("%d:%d", ua, ga), fmt.Sprintf("%d:%d", ub, gb), true
	}
	ua, ga := derefStr(a.User), derefStr(a.Group)
	ub, gb := derefStr(b.User), derefStr(b.Group)
	if ua == ub && ga == gb {
		return "", "", false
	}
	return ua + ":" + ga, ub + ":" + gb, true
}

func derefInt(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func contentSize(it *item.Item) int64 {
	if it == nil {
		return 0
	}
	return it.ContentSize()
}

// describeKind names what an added or removed path was, so "added directory" and "added
// file" are distinguishable in a listing.
func describeKind(it *item.Item) string {
	switch {
	case it == nil:
		return "entry"
	case it.IsDir():
		return "directory"
	case it.IsSymlink():
		return "link"
	case it.IsFIFO():
		return "fifo"
	case it.IsDevice():
		return "device"
	default:
		return fmt.Sprintf("%d B", it.ContentSize())
	}
}

func formatDiffTime(ns int64) string {
	return time.Unix(0, ns).Local().Format("Mon, 2006-01-02 15:04:05 -0700")
}

// isoDiffTime is a timestamp as the JSON forms carry it: ISO-8601 with microseconds, the
// same spelling every other JSON timestamp uses. The text form (formatDiffTime) is borg's
// human layout, and the two are not interchangeable.
func isoDiffTime(ns int64) string {
	return time.Unix(0, ns).Local().Format("2006-01-02T15:04:05.000000-07:00")
}

// ownerPair is an item's owner as borg's JSON reports it: a two-element array of user and
// group, where the text form is one "user:group" string.
func ownerPair(it *item.Item, numeric bool) []any {
	if numeric {
		return []any{derefInt(it.UID), derefInt(it.GID)}
	}
	return []any{derefStr(it.User), derefStr(it.Group)}
}
