// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/helpers/sorting.py, and of the sort keys in
// src/borg/archiver/list_cmd.py and src/borg/archiver/diff_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"sort"
	"strings"
)

// --sort-by, the multi-field sort that "list" and "diff" share.
//
// # Not the same --sort-by as repo-list's
//
// borg has two, and its own source says so at the top of sorting.py: repo-list sorts
// *archives* by a simpler spec with no direction prefixes, while list and diff sort the
// *contents* of an archive and accept "<" and ">" per field. borge already had the first
// one; this is the second, and they stay apart for the same reason borg keeps them apart -
// the key sets have nothing in common.
//
// # Why last field first
//
// A multi-field sort is done as one stable pass per field, applied from the last field to
// the first, so the first field is the primary criterion and every later one breaks the
// ties above it. Sorting by all fields at once with a compound comparison would give the
// same answer here, but this is what borg does, and doing it the same way means a field
// borge computes slightly differently cannot reorder anything the fields above it settled.

const (
	sortAscending  = '<'
	sortDescending = '>'
)

// sortField is one field of a spec, with the direction it was given with.
type sortField struct {
	name       string
	descending bool
}

// parseSortSpec splits a spec into its fields. An empty or blank spec gives none, which is
// borg's signal to leave the sequence alone rather than to sort by nothing.
func parseSortSpec(spec string) []sortField {
	var out []sortField
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		switch part[0] {
		case sortDescending:
			out = append(out, sortField{name: part[1:], descending: true})
		case sortAscending:
			out = append(out, sortField{name: part[1:]})
		default:
			out = append(out, sortField{name: part})
		}
	}
	return out
}

// validateSortSpec checks every field against the allowed set and returns the canonical
// spelling of the spec: descending fields keep their ">", ascending ones lose their "<".
//
// borg validates in argparse, before the repository is opened, and so does every caller
// here: a spec is checked as soon as the flags are parsed. A bad field found after an
// archive has been read has already cost the read, and - for list - already printed the
// first ten thousand lines.
func validateSortSpec(spec string, allowed []string) (string, error) {
	fields := parseSortSpec(spec)
	if len(fields) == 0 {
		return "", fmt.Errorf("unsupported sort field: empty spec")
	}
	var canonical []string
	for _, f := range fields {
		ok := false
		for _, a := range allowed {
			if f.name == a {
				ok = true
				break
			}
		}
		if !ok {
			return "", fmt.Errorf("unsupported sort field: %s, supported: %s",
				f.name, strings.Join(allowed, ", "))
		}
		if f.descending {
			canonical = append(canonical, string(sortDescending)+f.name)
		} else {
			canonical = append(canonical, f.name)
		}
	}
	return strings.Join(canonical, ","), nil
}

// sortKey is one element's value for one field.
//
// A field always yields the same type - "path" is text, "size" is a number - so a key is
// one or the other and never compared across types. Python has the same property by
// accident of the same code; here it is a struct so that the compiler keeps it.
type sortKey struct {
	text   string
	num    int64
	isText bool
}

func textSortKey(s string) sortKey { return sortKey{text: s, isText: true} }
func numSortKey(n int64) sortKey   { return sortKey{num: n} }

func (k sortKey) less(other sortKey) bool {
	if k.isText != other.isText {
		// A field that returned text for one element and a number for another is a bug in
		// the key function, not something to order. Keep it deterministic and let the
		// tests, which compare against borg, show it.
		return k.isText
	}
	if k.isText {
		return k.text < other.text
	}
	return k.num < other.num
}

// sortBySpec sorts items in place, one stable pass per field from last to first.
//
// Descending is a reversed comparison rather than a reversed slice, which is what keeps
// ties in their original order - Python's sort(reverse=True) does not reverse ties either,
// and reversing the slice afterwards would.
func sortBySpec[T any](items []T, spec string, keyFor func(field string, item T) sortKey) {
	fields := parseSortSpec(spec)
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		sort.SliceStable(items, func(x, y int) bool {
			kx, ky := keyFor(f.name, items[x]), keyFor(f.name, items[y])
			if f.descending {
				return ky.less(kx)
			}
			return kx.less(ky)
		})
	}
}
