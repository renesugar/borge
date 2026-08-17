// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of get_regex_from_pattern in borg's src/borg/patterns.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package patterns

import (
	"fmt"
	"regexp"
	"strings"
)

// Name-pattern styles. These select archives by name, and are generic string matching
// rather than the path matching the file patterns do.
const (
	// StyleIdentical requires an exact match. It is the default, so a plain archive name
	// means that name and nothing else - a user typing "backup" does not want "backup2".
	StyleIdentical = "id"
	// StyleShell is a shell-style pattern.
	StyleShell = "sh"
	// StyleRegex is a regular expression, used as given.
	StyleRegex = "re"
)

// RegexFromName turns an archive-name pattern into a regular expression string.
//
// The style is taken from a two-letter prefix followed by a colon - "sh:", "re:", "id:" -
// and defaults to identical matching.
func RegexFromName(pattern string) (string, error) {
	style := StyleIdentical
	if len(pattern) > 2 && pattern[2] == ':' {
		switch pattern[:2] {
		case StyleShell, StyleRegex, StyleIdentical:
			style, pattern = pattern[:2], pattern[3:]
		}
	}

	switch style {
	case StyleShell:
		// match_end is empty here: the caller appends its own anchor, because "borge list"
		// anchors differently from "borge delete".
		return strings.TrimPrefix(Translate(pattern, ""), "(?ms)"), nil
	case StyleRegex:
		return pattern, nil
	case StyleIdentical:
		return regexp.QuoteMeta(pattern), nil
	default:
		return "", fmt.Errorf("patterns: unknown pattern style %q", style)
	}
}

// CompileName compiles an archive-name pattern, anchored at the start and end.
//
// Anchoring at the start is what borg's re.match does; anchoring at the end is borg's
// default match_end of `\Z`. Both matter: an unanchored "backup" would match
// "old-backup-2" and delete the wrong archive.
func CompileName(pattern string) (*regexp.Regexp, error) {
	expr, err := RegexFromName(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(`\A(?:` + expr + `)\z`)
	if err != nil {
		return nil, fmt.Errorf("patterns: %q is not a usable pattern: %w", pattern, err)
	}
	return re, nil
}
