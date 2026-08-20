// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of interval() and int_or_interval() in borg's
// src/borg/helpers/parseformat.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/manifest"
)

// "number or time interval", which is what every one of borg 2's --keep-* options takes.
//
// borg 1 had --keep-within for the interval case and a plain count everywhere else. borg 2
// merged them: "--keep-daily 7" keeps one archive a day for seven days' worth of daily
// groups, "--keep-daily 7d" keeps one a day for archives made in the last seven days, and
// "--keep 7d" - every archive being its own group - keeps everything from the last seven
// days, which is exactly what --keep-within used to mean.
//
// The units are borg's and two of them are approximations it chose deliberately: a month is
// 31 days and a year is 365, so an interval never expires an archive earlier than the
// calendar would.

// intervalUnits is borg's multiplier table. The letters are case-sensitive: "m" is minutes
// nowhere here - it is months - and "M" is minutes, which is the opposite of what most
// duration syntaxes do and is why this is a table rather than a call to time.ParseDuration.
var intervalUnits = map[byte]time.Duration{
	'y': 365 * 24 * time.Hour,
	'm': 31 * 24 * time.Hour,
	'w': 7 * 24 * time.Hour,
	'd': 24 * time.Hour,
	'H': time.Hour,
	'M': time.Minute,
	'S': time.Second,
}

// intervalUnitOrder is only for the error message, so that it names the units in borg's
// order rather than in a Go map's.
const intervalUnitOrder = "y, m, w, d, H, M, S"

// parseInterval reads "7d", "2w", "30M" as a duration.
func parseInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("Unexpected time unit \"\": choose from %s", intervalUnitOrder)
	}
	unit := s[len(s)-1]
	mult, ok := intervalUnits[unit]
	if !ok {
		return 0, fmt.Errorf("Unexpected time unit %q: choose from %s", string(unit), intervalUnitOrder)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0, fmt.Errorf("Unexpected interval number %q: expected an integer", s[:len(s)-1])
	}
	if n < 0 {
		return 0, fmt.Errorf("Unexpected interval number %q: expected a positive integer", s[:len(s)-1])
	}
	return time.Duration(n) * mult, nil
}

// parseIntOrInterval reads a --keep-* value: a count, "all", or an interval.
//
// "all" is borg's spelling of the -1 sentinel, and -1 itself is accepted for the same
// reason: both mean "no limit on how many this rule keeps".
func parseIntOrInterval(s string) (manifest.KeepValue, error) {
	if s == "all" {
		return manifest.KeepCount(-1), nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return manifest.KeepCount(n), nil
	}
	d, err := parseInterval(s)
	if err != nil {
		return manifest.KeepValue{}, fmt.Errorf("Value is neither an integer nor an interval: %v", err)
	}
	return manifest.KeepInterval(d), nil
}

// keepFlag is one --keep-* option: parsed when it is set, and distinguishable from absent.
//
// The distinction is load-bearing here in a way it usually is not. borg's rules are "not
// given" (the rule does not run at all) and "given 0" (the rule runs and keeps nothing,
// which is an error if every rule is 0) - and a Go int holding 0 cannot tell them apart.
// That is also what decides which rule is the *last active* one, and therefore which rule
// keeps the oldest archive.
type keepFlag struct {
	value manifest.KeepValue
	set   bool
}

func (k *keepFlag) String() string {
	if k == nil || !k.set {
		return ""
	}
	return k.value.String()
}

func (k *keepFlag) Set(v string) error {
	parsed, err := parseIntOrInterval(v)
	if err != nil {
		return err
	}
	k.value, k.set = parsed, true
	return nil
}

// keepSpelling is one rule's option name, its short form, and the word borg's help uses.
type keepSpelling struct {
	rule  manifest.RuleKind
	long  string
	short string
	// noun is how borg's help describes what is kept: "daily archives", "quarterly
	// archives (13 week strategy)".
	noun string
}

// keepSpellings is every --keep-* option, in borg's order.
//
// The option name and the rule name are not always the same word: "--keep-13weekly" sets
// the rule borg calls "quarterly_13weekly", and that name is what appears in the listing
// and in the JSON. Keeping the two apart here is what stops one being printed for the other.
var keepSpellings = []keepSpelling{
	{manifest.RuleKeep, "keep", "", "archives"},
	{manifest.RuleSecondly, "keep-secondly", "", "secondly archives"},
	{manifest.RuleMinutely, "keep-minutely", "", "minutely archives"},
	{manifest.RuleHourly, "keep-hourly", "H", "hourly archives"},
	{manifest.RuleDaily, "keep-daily", "d", "daily archives"},
	{manifest.RuleWeekly, "keep-weekly", "w", "weekly archives"},
	{manifest.RuleMonthly, "keep-monthly", "m", "monthly archives"},
	{manifest.RuleQuarterly13Weekly, "keep-13weekly", "", "quarterly archives (13 week strategy)"},
	{manifest.RuleQuarterly3Monthly, "keep-3monthly", "", "quarterly archives (3 month strategy)"},
	{manifest.RuleYearly, "keep-yearly", "y", "yearly archives"},
}

// keepOptionNames lists the options in borg's error messages, quoted as borg quotes them.
func keepOptionNames() string {
	var parts []string
	for _, s := range keepSpellings {
		parts = append(parts, `"`+s.long+`"`)
	}
	last := len(parts) - 1
	return strings.Join(parts[:last], ", ") + ", or " + parts[last]
}
