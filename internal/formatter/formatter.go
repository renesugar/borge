// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the format-string handling in borg's
// src/borg/helpers/parseformat.py (BaseFormatter and the Python format spec it relies on).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package formatter expands borg's "--format" strings.
//
// borg builds these on Python's str.format, so "{archive:<36} {time} [{id}]{NL}" is a
// template with three fields, one of them padded to 36 columns. This package implements
// the part of Python's format-spec mini-language that borg's own defaults and documented
// examples use - fill, alignment and width - and refuses the rest rather than guessing.
//
// # Why refuse rather than ignore
//
// An unknown key or an unsupported spec is an error. Python would raise too, but the
// reason here is narrower: a format string that quietly drops a field produces output that
// looks like a listing and is missing a column, and the reader has no way to tell. The
// same argument as everywhere else in this port - a silent no-op looks like success.
package formatter

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Static are the formatting aids borg defines on every formatter: a newline that survives
// being written through a shell, and its friends.
var Static = map[string]string{
	"LF":      "\n",
	"SPACE":   " ",
	"TAB":     "\t",
	"CR":      "\r",
	"NUL":     "\x00",
	"NEWLINE": "\n",
	"NL":      "\n",
}

// Keys returns the field names a template uses, in order and without duplicates.
//
// It exists so a caller can refuse an unknown key before doing any work, and so that a
// command can tell whether an expensive value is wanted at all.
func Keys(template string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	err := walk(template, func(literal string) error { return nil }, func(name, spec string) error {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Format expands a template against a set of values.
//
// A value may be a string or an integer; the distinction matters because Python pads them
// differently by default - a string left, a number right - and borg's own default format
// for "list" relies on it, with "{size:8}" right-aligned against "{user:6}" left.
func Format(template string, values map[string]any) (string, error) {
	var b strings.Builder
	err := walk(template,
		func(literal string) error {
			b.WriteString(literal)
			return nil
		},
		func(name, spec string) error {
			v, ok := values[name]
			if !ok {
				if s, isStatic := Static[name]; isStatic {
					v = s
				} else {
					return fmt.Errorf("formatter: {%s} is not a key here; available: %s",
						name, strings.Join(available(values), ", "))
				}
			}
			text, err := apply(v, spec)
			if err != nil {
				return fmt.Errorf("formatter: {%s}: %w", name, err)
			}
			b.WriteString(text)
			return nil
		})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

func available(values map[string]any) []string {
	out := make([]string, 0, len(values)+len(Static))
	for k := range values {
		out = append(out, k)
	}
	for k := range Static {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// walk splits a template into literals and fields, handling doubled braces.
func walk(template string, literal func(string) error, field func(name, spec string) error) error {
	for i := 0; i < len(template); {
		c := template[i]
		if c != '{' && c != '}' {
			j := i
			for j < len(template) && template[j] != '{' && template[j] != '}' {
				j++
			}
			if err := literal(template[i:j]); err != nil {
				return err
			}
			i = j
			continue
		}
		// Doubled braces are literals, as in Python.
		if i+1 < len(template) && template[i+1] == c {
			if err := literal(string(c)); err != nil {
				return err
			}
			i += 2
			continue
		}
		if c == '}' {
			return fmt.Errorf("formatter: unmatched } at offset %d in %q "+
				"(write }} for a literal brace)", i, template)
		}
		end := strings.IndexByte(template[i:], '}')
		if end < 0 {
			return fmt.Errorf("formatter: unmatched { at offset %d in %q "+
				"(write {{ for a literal brace)", i, template)
		}
		body := template[i+1 : i+end]
		i += end + 1

		if strings.ContainsRune(body, '!') {
			return fmt.Errorf("formatter: {%s} uses a conversion, which borge does not "+
				"support (it would be a Python repr)", body)
		}
		name, spec, _ := strings.Cut(body, ":")
		if name == "" {
			return fmt.Errorf("formatter: {} is not a field; write {{}} for literal braces")
		}
		if err := field(name, spec); err != nil {
			return err
		}
	}
	return nil
}

// apply renders one value under a Python format spec.
//
// Supported: [[fill]align][width][.precision], where align is one of "<", ">" or "^" and
// precision truncates. That is what borg's own defaults use - "{id:.8}" is eight hex
// characters, "{archive:<15}" is a padded column. Anything else - type codes, sign,
// grouping, zero padding - is refused, because implementing a subset of Python's numeric
// formatting and getting it subtly wrong would be worse than saying so.
func apply(v any, spec string) (string, error) {
	text, numeric := render(v)
	if spec == "" {
		return text, nil
	}

	rest := spec
	fill := ' '
	align := byte(0)

	// The fill character is whatever precedes an alignment character, so the alignment is
	// looked for at the second position first.
	runes := []rune(rest)
	switch {
	case len(runes) >= 2 && isAlign(byte(runes[1])):
		fill, align = runes[0], byte(runes[1])
		rest = string(runes[2:])
	case len(runes) >= 1 && isAlign(byte(runes[0])):
		align = byte(runes[0])
		rest = string(runes[1:])
	}

	// "{x:08}" is zero padding in Python, which is a numeric thing; borg's formats do not
	// use it, and treating the 0 as a width would silently produce different columns.
	if strings.HasPrefix(rest, "0") && len(rest) > 1 {
		return "", fmt.Errorf("zero padding (%q) is not supported; write a fill and an "+
			"alignment instead, e.g. 0>%s", spec, strings.TrimPrefix(rest, "0"))
	}
	// Precision truncates, which for a listing is how a comment column stays a column.
	widthPart, precisionPart, hasPrecision := strings.Cut(rest, ".")
	if hasPrecision {
		precision, err := strconv.Atoi(precisionPart)
		if err != nil || precision < 0 {
			return "", fmt.Errorf("%q is not a precision", spec)
		}
		if numeric {
			// Python would read it as a float precision and reject it for an int. borge
			// has no float keys, so this can only be a mistake.
			return "", fmt.Errorf("%q gives a precision for a number, which truncates "+
				"nothing; drop the .%d", spec, precision)
		}
		if r := []rune(text); len(r) > precision {
			text = string(r[:precision])
		}
	}
	rest = widthPart

	if rest == "" {
		return text, nil
	}
	width, err := strconv.Atoi(rest)
	if err != nil || width < 0 {
		return "", fmt.Errorf("%q is not a width; borge supports "+
			"[[fill]align][width][.precision], where align is <, > or ^", spec)
	}

	if align == 0 {
		// Python's default: numbers right, everything else left.
		if numeric {
			align = '>'
		} else {
			align = '<'
		}
	}
	return pad(text, fill, align, width), nil
}

func isAlign(c byte) bool { return c == '<' || c == '>' || c == '^' }

func render(v any) (text string, numeric bool) {
	switch x := v.(type) {
	case string:
		return x, false
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	default:
		return fmt.Sprint(v), false
	}
}

// pad widens text to width columns. Width is counted in runes, as Python counts characters:
// padding a listing by bytes would misalign every non-ASCII name.
func pad(text string, fill rune, align byte, width int) string {
	n := utf8.RuneCountInString(text)
	if n >= width {
		return text
	}
	missing := width - n
	switch align {
	case '>':
		return strings.Repeat(string(fill), missing) + text
	case '^':
		left := missing / 2
		return strings.Repeat(string(fill), left) + text + strings.Repeat(string(fill), missing-left)
	default:
		return text + strings.Repeat(string(fill), missing)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
