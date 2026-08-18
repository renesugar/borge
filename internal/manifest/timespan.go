// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's relative_time_marker_validator and
// calculate_relative_offset / offset_n_months (src/borg/helpers/parseformat.py and
// src/borg/helpers/time.py).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package manifest

import (
	"fmt"
	"strconv"
	"time"
)

// A Timespan is borg's "relative time marker": a count and a unit, as in "7d" or "12m".
//
// # Two kinds of arithmetic, deliberately
//
// borg does not treat all units the same way, and reproducing that matters more than
// making it tidy. Years and months are **calendar** arithmetic: "1m" from 31 January is
// 28 February, not 2 or 3 March, because the day is clamped to the target month's last.
// Weeks, days, hours, minutes and seconds are **exact durations** - borg adds a
// timedelta, so "1d" is 24 hours and not "the same wall-clock time tomorrow".
//
// In practice archive timestamps and "now" are both UTC, where the two readings of a day
// coincide; the distinction is kept because the port's rule is to match borg's arithmetic
// rather than to match it where it currently shows.
//
// Note this is *not* borg's interval() - the one behind "prune --keep-within" - which
// approximates a month as 31 days and a year as 365. Same-looking strings, different
// meanings, and conflating them would quietly shift retention boundaries.
type Timespan struct {
	n    int
	unit byte
}

// ParseTimespan reads "7d", "12m" and the like.
//
// borg's validator is anchored - "^\d+[ymwdHMS]$" - so no sign, no space and no
// multi-unit "1d12H". The units are case-sensitive and the case is load-bearing: "m" is
// months and "M" is minutes.
func ParseTimespan(s string) (Timespan, error) {
	if len(s) < 2 {
		return Timespan{}, fmt.Errorf("manifest: %q is not a time span; write a count and a unit, e.g. 7d", s)
	}
	unit := s[len(s)-1]
	switch unit {
	case 'y', 'm', 'w', 'd', 'H', 'M', 'S':
	default:
		return Timespan{}, fmt.Errorf("manifest: %q has no known unit; choose from "+
			"y (years), m (months), w (weeks), d (days), H (hours), M (minutes), S (seconds)", s)
	}
	digits := s[:len(s)-1]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return Timespan{}, fmt.Errorf("manifest: %q is not a time span; the count must be "+
				"digits only, e.g. 7d", s)
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return Timespan{}, fmt.Errorf("manifest: %q is not a time span: %w", s, err)
	}
	return Timespan{n: n, unit: unit}, nil
}

func (t Timespan) String() string { return strconv.Itoa(t.n) + string(t.unit) }

// IsZero reports whether this is the unset Timespan rather than a parsed one.
func (t Timespan) IsZero() bool { return t.unit == 0 }

// Offset moves from by this span, backwards when earlier is true.
func (t Timespan) Offset(from time.Time, earlier bool) time.Time {
	n := t.n
	if earlier {
		n = -n
	}
	switch t.unit {
	case 'y':
		return offsetMonths(from, n*12)
	case 'm':
		return offsetMonths(from, n)
	case 'w':
		return from.Add(time.Duration(n) * 7 * 24 * time.Hour)
	case 'd':
		return from.Add(time.Duration(n) * 24 * time.Hour)
	case 'H':
		return from.Add(time.Duration(n) * time.Hour)
	case 'M':
		return from.Add(time.Duration(n) * time.Minute)
	case 'S':
		return from.Add(time.Duration(n) * time.Second)
	}
	return from
}

// offsetMonths is borg's offset_n_months: calendar months, with the day clamped to the
// target month's last rather than spilling into the next one.
//
// Go's time.Date normalises an out-of-range day instead - time.Date(2026, 2, 31, ...) is
// 3 March - so the month and the day are computed separately here. Using AddDate would
// have been shorter and would have put "31 January plus one month" three days past where
// borg puts it.
func offsetMonths(from time.Time, months int) time.Time {
	total := from.Year()*12 + int(from.Month()) + months - 1
	targetMonth := time.Month(floorMod(total, 12) + 1)
	targetYear := floorDiv(total, 12)

	// The last day of the target month is the day before the first of the following one.
	// time.Date normalises month 13 to January of the next year, which is what is wanted.
	firstOfFollowing := time.Date(targetYear, targetMonth+1, 1, 0, 0, 0, 0, from.Location())
	maxDay := firstOfFollowing.AddDate(0, 0, -1).Day()

	day := from.Day()
	if day > maxDay {
		day = maxDay
	}
	return time.Date(targetYear, targetMonth, day,
		from.Hour(), from.Minute(), from.Second(), from.Nanosecond(), from.Location())
}

// floorDiv and floorMod round towards negative infinity, as Python's // and % do. Go's /
// truncates towards zero, which would put a date before year 0 in the wrong month - not a
// case anybody will hit, but the arithmetic is borg's and this is what borg's means.
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorMod(a, b int) int { return a - floorDiv(a, b)*b }
