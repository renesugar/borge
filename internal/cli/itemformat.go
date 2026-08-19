// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's ItemFormatter (src/borg/helpers/parseformat.py).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"strconv"
	"time"

	"github.com/renesugar/borge/internal/formatter"
	"github.com/renesugar/borge/internal/item"
)

// itemValues are the keys borg's ItemFormatter offers for one archived path.
//
// # What is not here, and why
//
// borg also offers "fingerprint" and the content hashes (md5, sha256, blake3 and the
// rest). Those read every chunk of every file, which turns a listing into a full restore's
// worth of I/O; borg guards them with format_needs_cache and a warning. They are a
// separate piece of work rather than a line in this map, and asking for one is an error
// naming them rather than an empty column - see PORTING_PLAN.md §11.2.
//
// # uid and user
//
// borg falls back to the numeric id when the name was not stored: "user" is
// item.get("user", str(uid)). borge stored an empty string there, which would print a
// blank column where borg prints "1000". Matching borg matters more than the tidier
// answer, because a listing is something people diff.
func itemValues(it *item.Item, archiveName, archiveID string) map[string]any {
	mode := item.FormatMode(it.ModeOr(0))

	target := ""
	if it.Target != nil {
		target = *it.Target
	}
	extra := ""
	if target != "" {
		// borg's own KEY_DESCRIPTIONS says this also prepends " link to " for hard links,
		// but get_item_data only ever writes the arrow. The code is what borge matches.
		extra = " -> " + target
	}

	values := map[string]any{
		"archivename": archiveName,
		"archiveid":   archiveID,
		"path":        it.Path,
		"target":      target,
		"extra":       extra,
		"type":        mode[:1],
		"mode":        mode,
		"size":        itemSize(it),
		"num_chunks":  int64(len(it.Chunks)),
		"hlid":        hex.EncodeToString(it.HLID),
	}

	values["uid"] = intOrNone(it.UID)
	values["gid"] = intOrNone(it.GID)
	values["user"] = nameOrID(it.User, it.UID)
	values["group"] = nameOrID(it.Group, it.GID)
	values["flags"] = intOrNone(it.BSDFlags)
	if it.Inode != nil {
		values["inode"] = int64(*it.Inode)
	} else {
		values["inode"] = "None"
	}

	// borg's format_time is OutputTimestamp(item.get(key) or item.mtime): every one of
	// the three falls back to the modification time. That matters because borg does not
	// store atime unless asked, so "{atime}" is empty here and a date in borg - which is
	// how this was found, by diffing the two. Python's "or" also treats a zero timestamp
	// as absent, and so does this.
	for key, ts := range map[string]*int64{"mtime": it.MTime, "ctime": it.CTime, "atime": it.ATime} {
		if ts == nil || *ts == 0 {
			ts = it.MTime
		}
		values[key] = timeOrEmpty(ts, formatTime)
		values["iso"+key] = timeOrEmpty(ts, func(t time.Time) string {
			return t.Local().Format("2006-01-02T15:04:05.000000-07:00")
		})
	}
	return values
}

// intOrNone is borg's rendering of an absent number in a format string.
//
// borg's item_data holds Python's None for a key the item does not carry, and formatting
// it produces the four letters "None". That is a Python artifact showing through, and it
// looks like a bug in Go source - but it is what borg prints, and a script that greps a
// listing for it would break if borge printed an empty column instead. Reproduced
// deliberately; see docs/DIVERGENCES.md #33 for the whole list of keys it reaches.
func intOrNone(v *int64) any {
	if v == nil {
		return "None"
	}
	return *v
}

// nameOrID is borg's item.get("user", str(uid)): the stored name, or the numeric id when
// there is none. With neither - a streamed item given no --stdin-user - borg formats
// str(None), so this is "None" too. See intOrNone.
func nameOrID(name *string, id *int64) string {
	if name != nil && *name != "" {
		return *name
	}
	if id != nil {
		return strconv.FormatInt(*id, 10)
	}
	return "None"
}

func timeOrEmpty(ns *int64, format func(time.Time) string) string {
	if ns == nil {
		return ""
	}
	return format(time.Unix(0, *ns))
}

// itemFormatKeys is every key itemValues can produce, for validating a template before any
// archive is read. borg validates up front for the same reason: a format error found on
// the ten-thousandth item has already printed nine thousand lines.
func itemFormatKeys() map[string]any {
	var empty item.Item
	return itemValues(&empty, "", "")
}

// itemFormat is the template an item-listing command renders with.
//
// borg's precedence: an explicit --format wins, then --short, then the command's
// environment variable, then its built-in default.
//
// The environment value is passed in rather than looked up here, and that is not a style
// choice. TestHelpEnvironmentTopicListsEveryVariable finds the variables borge reads by
// scanning the source for a lookup with a *literal* name; a helper that took the name as a
// parameter made BORGE_LIST_FORMAT and BORGE_FIND_FORMAT invisible to it, so the check that
// keeps the environment topic honest went blind exactly when two new variables arrived.
// Each caller does its own literal lookup, and the guard keeps working.
//
// The scan reads comments too, so writing the call shape out here would invent a variable
// that does not exist - which is what happened, and is why this paragraph describes it
// instead of quoting it.
func itemFormat(given string, short bool, fromEnv, def string) string {
	switch {
	case given != "":
		return given
	case short:
		return "{path}{NL}"
	case fromEnv != "":
		return fromEnv
	}
	return def
}

// checkItemFormat rejects a template naming a key no item has.
func checkItemFormat(template string) error {
	_, err := formatter.Format(template, itemFormatKeys())
	return err
}
