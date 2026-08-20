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
