// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's DiffFormatter, src/borg/helpers/parseformat.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/item"
)

// borg has three format key sets, not two: the archive one that repo-list, prune, info and
// check share, the item one that list and find share, and this - the changes between two
// versions of a path. It is the third that borge did not have, which is why "diff
// --format" could not simply reuse something.
//
// The keys are borg's DiffFormatter.KEY_DESCRIPTIONS, and so are the renderings. Those
// matter more than they look: {content} is a padded field so that a column of them lines
// up, and {link} and its relatives are padded to 27 characters for the same reason - borg
// comments that 27 "is the length of the content change". Getting the width wrong makes
// every line of a long diff ragged.

// diffKeyOrder is the order {change} concatenates the individual keys in. borg builds it
// by iterating its call_keys dict, which in Python preserves insertion order, so this is
// that insertion order and not an alphabetical one.
var diffKeyOrder = []string{
	"content", "mode", "type", "owner", "group", "user",
	"link", "directory", "blkdev", "chrdev", "fifo", "mtime", "ctime",
}

// diffValues renders every key for one changed path.
func diffValues(d archive.Diff, units string) map[string]any {
	byKind := map[archive.ChangeKind]archive.Change{}
	for _, c := range d.Changes {
		byKind[c.Kind] = c
	}

	values := map[string]any{"path": d.Path}
	for _, k := range diffKeyOrder {
		values[k] = ""
	}
	values["isomtime"], values["isoctime"] = "", ""

	// A presence change is filed under the *kind* of thing that appeared or disappeared,
	// except for a regular file, which borg reports under content with its size.
	for _, kind := range []archive.ChangeKind{archive.ChangeAdded, archive.ChangeRemoved} {
		c, ok := byKind[kind]
		if !ok {
			continue
		}
		if c.ItemKind != "" {
			// "added directory", padded to the width of a content change.
			values[c.ItemKind] = pad27(string(kind) + " " + c.ItemKind)
			continue
		}
		if kind == archive.ChangeAdded {
			values["content"] = fmt.Sprintf("added: %20s", formatBytesIn(c.Added, units))
		} else {
			values["content"] = fmt.Sprintf("removed: %18s", formatBytesIn(c.Removed, units))
		}
	}
	if c, ok := byKind[archive.ChangeModified]; ok {
		if c.Added == 0 && c.Removed == 0 {
			// borg says so rather than printing two zeroes, because "no change" and "we
			// could not compare the chunk ids" are different answers.
			values["content"] = "modified:  (can't get size)"
		} else {
			values["content"] = fmt.Sprintf("modified: %8s %8s",
				formatBytesPrec(c.Added, units, 1, true), formatBytesPrec(-c.Removed, units, 1, true))
		}
	}
	if c, ok := byKind[archive.ChangeLink]; ok {
		// borg records only that the link changed, not what it changed to.
		_ = c
		values["link"] = pad27("changed link")
	}
	if c, ok := byKind[archive.ChangeMode]; ok {
		values["mode"] = fmt.Sprintf("[%s -> %s]", c.From, c.To)
	}
	if c, ok := byKind[archive.ChangeType]; ok {
		values["type"] = fmt.Sprintf("[%s -> %s]", c.From, c.To)
	}
	if c, ok := byKind[archive.ChangeOwner]; ok {
		values["owner"] = fmt.Sprintf("[%s -> %s]", c.From, c.To)
		// borg emits user and group as well, but only for the half that actually differs:
		// a change of group alone leaves {user} empty.
		fromUser, fromGroup := splitOwner(c.From)
		toUser, toGroup := splitOwner(c.To)
		if fromUser != toUser {
			values["user"] = fmt.Sprintf("[%s -> %s]", fromUser, toUser)
		}
		if fromGroup != toGroup {
			values["group"] = fmt.Sprintf("[%s -> %s]", fromGroup, toGroup)
		}
	}
	for _, tc := range []struct {
		key  string
		kind archive.ChangeKind
	}{{"mtime", archive.ChangeMTime}, {"ctime", archive.ChangeCTime}} {
		c, ok := byKind[tc.kind]
		if !ok {
			continue
		}
		values[tc.key] = fmt.Sprintf("[%s: %s -> %s]", tc.key, c.From, c.To)
		iso := func(v any, fallback string) string {
			if s, ok := v.(string); ok {
				return s
			}
			return fallback
		}
		values["iso"+tc.key] = fmt.Sprintf("[%s: %s -> %s]", tc.key,
			iso(c.FromJSON, c.From), iso(c.ToJSON, c.To))
	}

	// {change} is every key above, in borg's order, with the empty ones dropped. The iso
	// forms are deliberately not in it: borg skips them so that a default listing does not
	// print each timestamp twice.
	var parts []string
	for _, k := range diffKeyOrder {
		if s, _ := values[k].(string); s != "" {
			parts = append(parts, s)
		}
	}
	values["change"] = strings.Join(parts, " ")
	return values
}

// pad27 is borg's ljust(27), the width of a content change, so that a column of mixed
// change kinds lines up.
func pad27(s string) string {
	if len(s) >= 27 {
		return s
	}
	return s + strings.Repeat(" ", 27-len(s))
}

// splitOwner takes "user:group" apart. borge's comparison renders an owner change as one
// string; borg reports the two halves separately as well.
func splitOwner(s string) (string, string) {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// diffSortKeys is borg's DIFF_SORT_KEYS: the fields "diff --sort-by" accepts.
//
// A third key set again - these are neither the format keys above nor list's item keys.
// "size_added" can be sorted by and not printed; "{content}" can be printed and not sorted
// by. borg keeps them apart and so does this.
var diffSortKeys = []string{
	"path", "size_added", "size_removed", "size_diff", "size",
	"user", "group", "uid", "gid", "ctime", "mtime", "ctime_diff", "mtime_diff",
}

// diffSortKey computes one changed path's key for one field.
//
// # Where the values come from, which is not where it looks
//
// borg builds an ItemDiff from two items and fills the missing side with
// Item.create_deleted(path) - an item carrying its path and nothing else. Its key function
// then reads the plain attributes from item2 (`d._item2 or d._item1`, and _item2 is never
// None), so for a *removed* path the owner is "", the uid is -1 and the timestamps are 0:
// the sort sees the placeholder, not the version that was there before.
//
// That reads like a bug and is not worth diverging over - "sort by user" is asking about
// the state in the second archive, which is precisely what a removed path has none of. It
// is reproduced here by reading Item2 and treating nil as that placeholder.
func diffSortKey(field string, d archive.Diff) sortKey {
	switch field {
	case "path":
		// borg sorts by remove_surrogates(path): a byte that is not valid UTF-8 compares
		// as "?", so two paths differing only in undecodable bytes sort as equal and keep
		// their stream order. Sorting by the raw bytes would be defensible and would not
		// be borg's answer.
		return textSortKey(approximateText(d.Path))
	case "size_added":
		added, _ := diffContentSizes(d)
		return numSortKey(added)
	case "size_removed":
		_, removed := diffContentSizes(d)
		return numSortKey(removed)
	case "size_diff":
		added, removed := diffContentSizes(d)
		return numSortKey(added - removed)
	case "size":
		// The size in the second archive, and zero where there is no second version.
		if d.Item2 == nil {
			return numSortKey(0)
		}
		return numSortKey(itemSize(d.Item2))
	case "ctime_diff", "mtime_diff":
		return numSortKey(diffTimestamp(field[:5], d.Item2) - diffTimestamp(field[:5], d.Item1))
	case "user":
		if d.Item2 == nil {
			return textSortKey("")
		}
		return textSortKey(stringOrEmpty(d.Item2.User))
	case "group":
		if d.Item2 == nil {
			return textSortKey("")
		}
		return textSortKey(stringOrEmpty(d.Item2.Group))
	case "uid":
		if d.Item2 == nil {
			return numSortKey(-1)
		}
		return numSortKey(idOrMissing(d.Item2.UID))
	case "gid":
		if d.Item2 == nil {
			return numSortKey(-1)
		}
		return numSortKey(idOrMissing(d.Item2.GID))
	case "ctime", "mtime":
		return numSortKey(diffTimestamp(field, d.Item2))
	}
	// Unreachable: the spec is validated against diffSortKeys before either archive is read.
	return textSortKey("")
}

// diffContentSizes returns the bytes added and removed by the *content* change, and zero
// for a path whose change is anything else.
//
// A directory or a symlink that came or went is filed under its own key rather than under
// content (see diffValues above), and borg's key function looks up "content" alone - so
// sorting by size_added puts a new directory with the zeroes, not with the new files.
func diffContentSizes(d archive.Diff) (added, removed int64) {
	for _, c := range d.Changes {
		switch c.Kind {
		case archive.ChangeModified:
			return c.Added, c.Removed
		case archive.ChangeAdded:
			if c.ItemKind == "" {
				return c.Added, 0
			}
		case archive.ChangeRemoved:
			if c.ItemKind == "" {
				return 0, c.Removed
			}
		}
	}
	return 0, 0
}

// diffTimestamp reads a raw nanosecond timestamp, defaulting to zero.
//
// No fallback to mtime here, unlike the item sort keys: borg's diff key function reads
// item.get(ts, 0) directly, and a "sort by ctime" that quietly became a sort by mtime
// would make ctime_diff report differences that are not there.
func diffTimestamp(field string, it *item.Item) int64 {
	if it == nil {
		return 0
	}
	var ts *int64
	switch field {
	case "mtime":
		ts = it.MTime
	case "ctime":
		ts = it.CTime
	}
	if ts == nil {
		return 0
	}
	return *ts
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
