// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause AND PSF-2.0
//
// This file is a Go port of Python's fnmatch.translate, which borg's
// src/borg/patterns.py uses for its "fm:" pattern style.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Portions derived from the Python standard library, Copyright (C) 2001-2016 Python
// Software Foundation, licensed under the PSF License Version 2.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package patterns

import (
	"regexp"
	"strings"
)

// FnmatchTranslate converts a pattern in Python's fnmatch dialect to a Go regular
// expression, anchored at both ends.
//
// # How this differs from Translate
//
// The two dialects look alike and are not. In fnmatch, `*` matches **anything at all**,
// including path separators, and `**` means nothing special. In the shell dialect
// (Translate, used by "sh:") `*` stops at a separator and `**/` spans directory levels.
// That is the difference between "fm:*.txt" matching "a/b/c.txt" - it does - and
// "sh:*.txt" not matching it.
//
// borg's default pattern style for --exclude is fnmatch, so this is the one most users
// actually get.
//
// # Why the regex is not Python's
//
// Python 3.12 emits atomic groups and a negative lookahead for the empty character class.
// Go's RE2 has neither, and does not need the first: they exist to stop a backtracking
// engine from going exponential, and RE2 does not backtrack. The translation below is
// therefore semantically equivalent rather than textually identical, which is why the
// tests compare match results against borg rather than comparing regexes.
func FnmatchTranslate(pat string) string {
	// star marks a "*" in the parts list, so consecutive stars can be collapsed and the
	// pieces joined with ".*" at the end.
	const star = "\x00STAR\x00"

	var parts []string
	runes := []rune(pat)
	n := len(runes)

	for i := 0; i < n; {
		c := runes[i]
		i++

		switch c {
		case '*':
			if len(parts) == 0 || parts[len(parts)-1] != star {
				parts = append(parts, star)
			}
		case '?':
			parts = append(parts, ".")
		case '[':
			j := i
			if j < n && runes[j] == '!' {
				j++
			}
			if j < n && runes[j] == ']' {
				j++
			}
			for j < n && runes[j] != ']' {
				j++
			}
			if j >= n {
				parts = append(parts, `\[`)
				break
			}
			stuff, next := translateClass(runes, i, j)
			i = next
			parts = append(parts, stuff)
		default:
			parts = append(parts, regexp.QuoteMeta(string(c)))
		}
	}

	var b strings.Builder
	for i, p := range parts {
		if p == star {
			b.WriteString(".*")
			continue
		}
		_ = i
		b.WriteString(p)
	}
	// (?s) so "." matches a newline, which a file name may contain.
	return `(?s)\A` + b.String() + `\z`
}

// translateClass renders a bracket expression, following Python's rules for ranges,
// negation and the set operators it escapes.
func translateClass(runes []rune, i, j int) (string, int) {
	stuff := string(runes[i:j])

	if !strings.Contains(stuff, "-") {
		stuff = strings.ReplaceAll(stuff, `\`, `\\`)
	} else {
		// Split on the hyphens that form ranges, so the ones that do not can be escaped
		// without breaking the ones that do.
		var chunks []string
		start := i
		k := i + 1
		if runes[i] == '!' {
			k = i + 2
		}
		for {
			idx := indexRune(runes, '-', k, j)
			if idx < 0 {
				break
			}
			chunks = append(chunks, string(runes[start:idx]))
			start = idx + 1
			k = idx + 3
		}
		if chunk := string(runes[start:j]); chunk != "" {
			chunks = append(chunks, chunk)
		} else if len(chunks) > 0 {
			chunks[len(chunks)-1] += "-"
		}
		// Drop ranges that run backwards - they are invalid in a regular expression.
		for k := len(chunks) - 1; k > 0; k-- {
			if len(chunks[k-1]) > 0 && len(chunks[k]) > 0 &&
				chunks[k-1][len(chunks[k-1])-1] > chunks[k][0] {
				chunks[k-1] = chunks[k-1][:len(chunks[k-1])-1] + chunks[k][1:]
				chunks = append(chunks[:k], chunks[k+1:]...)
			}
		}
		for idx := range chunks {
			chunks[idx] = strings.ReplaceAll(chunks[idx], `\`, `\\`)
			chunks[idx] = strings.ReplaceAll(chunks[idx], "-", `\-`)
		}
		stuff = strings.Join(chunks, "-")
	}

	// Escape the set operators, so "[a&&b]" is three literals rather than an intersection.
	stuff = setOperatorRe.ReplaceAllString(stuff, `\$1`)

	switch {
	case stuff == "":
		// An empty class matches nothing. Python spells this "(?!)", a negative lookahead
		// RE2 does not have; a character class that can hold no character says the same
		// thing and is a regular expression.
		return `[^\x00-\x{10FFFF}]`, j + 1
	case stuff == "!":
		// A negated empty class matches any character.
		return ".", j + 1
	}
	switch stuff[0] {
	case '!':
		stuff = "^" + stuff[1:]
	case '^', '[':
		stuff = `\` + stuff
	}
	return "[" + stuff + "]", j + 1
}

var setOperatorRe = regexp.MustCompile(`([&~|])`)

func indexRune(runes []rune, want rune, from, to int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i < to && i < len(runes); i++ {
		if runes[i] == want {
			return i
		}
	}
	return -1
}
