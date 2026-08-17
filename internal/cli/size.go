// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of parse_file_size, format_file_size and sizeof_fmt in borg's
// src/borg/helpers/parseformat.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Sizes on the command line, in and out.
//
// # Decimal, not binary, and deliberately so
//
// "1G" means 1,000,000,000 bytes, not 1,073,741,824. That is borg's parsing and it is what
// disk vendors and df -H mean too. Reading it as a power of two would make "borge
// repo-space --reserve 1G" reserve 7% more than "borg repo-space --reserve 1G" did, and
// the two tools have to agree about the size of a reservation they both manage.
//
// Output follows BORG_UNITS, as borg's does: "si" (the default, powers of 1000), "iec"
// (powers of 1024, "MiB"), or "raw" (exact byte counts, for scripts that want to do their
// own arithmetic).

// parseFileSize reads a size like "1234", "55G" or "1.7T".
//
// The suffix is a decimal multiplier. An unrecognised trailing letter is an error rather
// than a silently ignored suffix: "--reserve 1GB" meaning one byte would be a very
// expensive misreading in the one situation this option exists for.
func parseFileSize(s string) (int64, error) {
	orig := strings.TrimSpace(s)
	if orig == "" {
		return 0, fmt.Errorf("an empty string is not a size")
	}
	up := strings.ToUpper(orig)

	factor := int64(1)
	switch up[len(up)-1] {
	case 'K':
		factor = 1e3
	case 'M':
		factor = 1e6
	case 'G':
		factor = 1e9
	case 'T':
		factor = 1e12
	case 'P':
		factor = 1e15
	}
	if factor != 1 {
		up = up[:len(up)-1]
	}

	v, err := strconv.ParseFloat(up, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size: expected a number with an optional "+
			"K, M, G, T or P suffix (decimal, so 1G is 1000000000)", orig)
	}
	if v < 0 {
		return 0, fmt.Errorf("%q is negative; a size cannot be", orig)
	}
	f := v * float64(factor)
	if f > math.MaxInt64 {
		return 0, fmt.Errorf("%q is too large", orig)
	}
	return int64(f), nil
}

// sizeUnits reports which unit family to print in, honouring BORG_UNITS.
func (e *Env) sizeUnits() string {
	v, _ := e.lookupBorg("UNITS")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "iec":
		return "iec"
	case "raw":
		return "raw"
	case "si", "":
		return "si"
	default:
		// borg warns once and carries on with the default rather than failing; a bad
		// environment variable should not stop a backup.
		return "si"
	}
}

// fmtBytes renders a size the way this environment asked for, via BORG_UNITS.
func (e *Env) fmtBytes(n int64) string { return formatBytesIn(n, e.sizeUnits()) }

// formatBytesIn renders a size in the named unit family.
func formatBytesIn(n int64, units string) string {
	if units == "raw" {
		return fmt.Sprintf("%d B", n)
	}
	power := 1000.0
	names := []string{"", "k", "M", "G", "T", "P", "E", "Z", "Y"}
	if units == "iec" {
		power = 1024.0
		names = []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "Zi", "Yi"}
	}

	v := float64(n)
	prec := 0
	unit := names[len(names)-1]
	for i, name := range names[:len(names)-1] {
		// Rounded before the comparison, as borg does: 999.996 bytes must print as "1.00
		// kB" rather than "1000.00 B", which is a unit the loop has already left behind.
		if math.Abs(round(v, 2)) < power {
			unit = name
			if i > 0 {
				prec = 2
			}
			break
		}
		v /= power
	}
	return fmt.Sprintf("%.*f %sB", prec, v, unit)
}

func round(v float64, places int) float64 {
	shift := math.Pow(10, float64(places))
	return math.Round(v*shift) / shift
}
