// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's timestamp() argument type (src/borg/helpers/time.py).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"os"
	"time"
)

// isoTimestampLayouts are the forms "--timestamp" accepts, tried in order.
//
// borg parses with Python's datetime.fromisoformat, which accepts more shapes than its own
// help text documents ("yyyy-mm-ddThh:mm:ss[(+|-)HH:MM]"). These are the documented form
// plus the ones a person actually types: a date on its own, a space instead of the T, and
// fractional seconds. A shape borg accepts and this list does not is a divergence rather
// than a crash, and the error names the forms rather than leaving the user guessing.
var isoTimestampLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseTimestamp reads a "--timestamp" value: a reference file whose modification time is
// taken, or an ISO 8601 instant.
//
// # The file comes first, and that ordering is borg's
//
// borg stats the argument before trying to parse it, so a string that happens to name an
// existing file wins over an identical-looking timestamp. It is observable - a file called
// "2026-01-01T00:00:00" in the working directory changes what
// "--timestamp 2026-01-01T00:00:00" means - and it is reproduced rather than tidied,
// because a user who passes a reference file is relying on exactly that branch.
//
// # A naive timestamp is local time
//
// With no offset, borg assumes the local timezone. Reading it as UTC instead would silently
// move every archive by the offset, which is the kind of error that only shows up when
// somebody sorts a listing months later.
func parseTimestamp(s string) (time.Time, error) {
	if info, err := os.Stat(s); err == nil {
		return info.ModTime(), nil
	}
	for _, layout := range isoTimestampLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--timestamp: %q is neither a file nor a timestamp; "+
		"write yyyy-mm-ddThh:mm:ss with an optional %s offset, or the path of a file to "+
		"take the time from", s, "(+|-)HH:MM")
}

// timestampFlag is a "--timestamp" option that parses when it is set.
//
// Parsing here rather than later means a typo is refused before the repository is opened,
// which is where borg refuses it, and it distinguishes "given" from "given empty" - an
// empty value is what "--timestamp $WHEN" expands to when WHEN is unset, and reading it as
// absent would silently date the archive now (PORTING_PLAN.md §2.3).
type timestampFlag struct {
	when time.Time
	set  bool
}

func (t *timestampFlag) String() string {
	if !t.set {
		return ""
	}
	return t.when.Format(time.RFC3339)
}

func (t *timestampFlag) Set(v string) error {
	when, err := parseTimestamp(v)
	if err != nil {
		return err
	}
	t.when, t.set = when, true
	return nil
}

// register adds the option under borg's name and help text.
func (t *timestampFlag) register(fs *flagSet) {
	fs.Var(t, "timestamp", "the archive's nominal time: an ISO 8601 instant, or a file to "+
		"take the modification time from")
}

// value is the parsed instant, or the zero time when the option was not given - which is
// what every SaveOptions.Timestamp treats as "use the start time".
func (t *timestampFlag) value() time.Time {
	if !t.set {
		return time.Time{}
	}
	return t.when
}
