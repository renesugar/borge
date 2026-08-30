// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause AND PSF-2.0
//
// This file is a Go port of borg's src/borg/helpers/shellpattern.py, which is itself
// derived from Python's fnmatch module.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Portions derived from the Python standard library, Copyright (C) 2001-2016 Python
// Software Foundation, licensed under the PSF License Version 2.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package patterns implements borg's pattern matching: the shell-style patterns used for
// include and exclude rules, and the pattern styles that select archives by name.
//
// # Why this is worth getting exactly right
//
// A pattern that is subtly wrong does not fail - it silently backs up or restores the
// wrong set of files, and nobody finds out until a restore is needed. So the translation
// below follows borg's character by character, and the tests compare *match results*
// against borg's own matcher over a corpus rather than comparing the regexes, which
// cannot be identical: Go's RE2 and Python's re spell some things differently.
package patterns

import (
	"fmt"
	"regexp"
	"strings"
)

// PathSeparator is the separator the patterns are written against.
//
// borg uses os.path.sep, so a pattern written on Windows would translate differently
// there. borge targets POSIX for 1.0 (plans/PORTING_PLAN.md §0.6) and hardcodes "/", which
// also means a repository's patterns mean the same thing wherever they are read.
const PathSeparator = "/"

// Translate converts a shell-style pattern into a Go regular expression.
//
//   - `**/` matches zero or more whole directory levels.
//   - `*` matches anything except a path separator.
//   - `?` matches one character other than a path separator.
//   - `[abc]`, `[!abc]` are character classes; `[?]` matches a literal `?`.
//   - `{a,b}` matches either alternative.
//
// matchEnd is appended to the result; pass `\z` to anchor at the end of the string, or
// "" to leave the pattern open-ended.
//
// The result is anchored only at the end, if at all - callers use it with a
// leading-anchored match, which is what borg's re.match does.
func Translate(pat, matchEnd string) string {
	pat = translateAlternatives(pat)

	sep := PathSeparator
	var res strings.Builder
	runes := []rune(pat)
	n := len(runes)

	for i := 0; i < n; {
		c := runes[i]
		i++

		switch {
		case c == '*':
			if i+1 < n && runes[i] == '*' && string(runes[i+1]) == sep {
				// "**/" - zero or more complete directory names, each with its slash. The
				// trailing separator is part of the group, so "**/x" matches a bare "x".
				res.WriteString("(?:[^" + sep + "]*" + sep + ")*")
				i += 2
			} else {
				res.WriteString("[^" + sep + "]*")
			}
		case c == '?':
			res.WriteString("[^" + sep + "]")
		case c == '[':
			j := i
			if j < n && runes[j] == '!' {
				j++
			}
			if j < n && runes[j] == ']' {
				// A "]" immediately after "[" or "[!" is a literal, not the end of the class.
				j++
			}
			for j < n && runes[j] != ']' {
				j++
			}
			if j >= n {
				// Unterminated: a literal "[", which is what a shell does with it too.
				res.WriteString(`\[`)
			} else {
				stuff := strings.ReplaceAll(string(runes[i:j]), `\`, `\\`)
				i = j + 1
				switch {
				case strings.HasPrefix(stuff, "!"):
					stuff = "^" + stuff[1:]
				case strings.HasPrefix(stuff, "^"):
					stuff = `\` + stuff
				}
				res.WriteString("[" + stuff + "]")
			}
		case c == '\\' && i < n && (runes[i] == '(' || runes[i] == '|' || runes[i] == ')'):
			// An escaped group character is a literal. **This is where borge stops
			// reproducing borg**, deliberately; see DIVERGENCES #63.
			//
			// borg guards the passthrough below with `pat[i - 1] != "\\"`, but by then i
			// has already moved past the character, so pat[i-1] *is* the character itself
			// and the guard is always true. What that costs is worse than a wrong match:
			// "a\(b" translates to a literal backslash followed by an unterminated group,
			// which does not compile at all, in either tool.
			//
			// Restoring borg's *stated* intent would not fix it either. The guard has no
			// else branch, so an escaped "(" contributes nothing and "a\(b" would match
			// "a\b" - still not a way to write a literal parenthesis. So this is a
			// decision rather than a repair, and it is the narrowest one that answers the
			// complaint: a backslash escapes only the three characters borg treats as
			// regex syntax. Everything else keeps its current meaning, which matters
			// because a backslash is a legal filename character on Linux - "a\b" still
			// matches a file called "a\b", and "\\(" still matches one called "\(".
			res.WriteString(regexp.QuoteMeta(string(runes[i])))
			i++
		case c == '(' || c == '|' || c == ')':
			// Passed through as regex syntax, so the alternatives translateAlternatives
			// produced survive. Unescaped, as borg does.
			res.WriteRune(c)
		default:
			res.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	return "(?ms)" + res.String() + matchEnd
}

// parseBraces returns the index pairs of matched, unescaped braces.
//
// A left-to-right scan with a stack, rather than a regular expression: a regex cannot
// distinguish "{a,b}{c,d}" (two groups) from one group containing braces, and getting
// that wrong turns a pattern into something that matches almost nothing.
func parseBraces(runes []rune) [][2]int {
	var stack []int
	pairs := map[int]int{}
	order := []int{}

	for idx, c := range runes {
		switch c {
		case '{':
			if idx == 0 || runes[idx-1] != '\\' {
				pairs[idx] = -1
				order = append(order, idx)
				stack = append(stack, idx)
			}
		case '}':
			if len(stack) > 0 && idx > 0 && runes[idx-1] != '\\' {
				open := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				pairs[open] = idx
			}
		}
	}

	var out [][2]int
	for _, open := range order {
		if close := pairs[open]; close > 0 {
			out = append(out, [2]int{open, close})
		}
	}
	return out
}

// translateAlternatives rewrites `{a,b}` as `(a|b)`.
//
// Braces without a comma are left alone: `{a}` is a literal in a shell, and turning it
// into a group would silently change what an existing pattern matches.
func translateAlternatives(pat string) string {
	runes := []rune(pat)
	pairs := parseBraces(runes)

	for _, pair := range pairs {
		open, close := pair[0], pair[1]
		commas := 0
		for i := open + 1; i < close; i++ {
			switch runes[i] {
			case ',':
				if i == open || runes[i-1] != '\\' {
					runes[i] = '|'
					commas++
				}
			case '|':
				// A nested group's commas were converted while walking its parent, so a
				// pipe here may be a comma from the original pattern.
				if i == open || runes[i-1] != '\\' {
					commas++
				}
			}
		}
		if commas > 0 {
			runes[open] = '('
			runes[close] = ')'
		}
	}
	return string(runes)
}

// Compile translates a shell pattern and compiles it, anchored at both ends.
func Compile(pat string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(`\A` + Translate(pat, `\z`))
	if err != nil {
		return nil, fmt.Errorf("patterns: %q is not a usable pattern: %w", pat, err)
	}
	return re, nil
}
