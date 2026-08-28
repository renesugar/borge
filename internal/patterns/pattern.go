// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the pattern classes and PatternMatcher in borg's
// src/borg/patterns.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package patterns

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// The five file-pattern styles, named by their two-letter prefix.
//
// The prefix is what a user writes: "sh:home/*/.cache". Without one, the caller's default
// applies - fnmatch for --exclude, shell for --pattern - which is a wart borg carries and
// borge reproduces, because changing it would silently change what an existing exclude
// file matches.
const (
	// StyleFnmatch is "fm:", Python's fnmatch. Its "*" crosses path separators.
	StyleFnmatch = "fm"
	// StyleShellPath is "sh:", the shell dialect where "*" stops at a separator and
	// "**/" spans directory levels.
	StyleShellPath = "sh"
	// StyleRegexPath is "re:", an unanchored regular expression searched anywhere in the
	// path.
	StyleRegexPath = "re"
	// StylePathPrefix is "pp:", a path prefix: the path itself and everything below it.
	StylePathPrefix = "pp"
	// StylePathFull is "pf:", one exact path and nothing else.
	StylePathFull = "pf"
)

// Pattern matches archived paths.
type Pattern interface {
	// Match reports whether the path is matched. Paths are as stored in an archive:
	// relative, "/"-separated, no leading slash.
	Match(path string) bool
	// RecurseDir reports whether a directory this pattern matched should still be
	// descended into.
	RecurseDir() bool
	// String is the pattern as the user wrote it.
	String() string
	// MatchCount is how many paths this pattern has matched, used to report an include
	// pattern that never matched anything.
	MatchCount() int
}

// basePattern carries what every style shares.
type basePattern struct {
	orig       string
	recurseDir bool
	count      int
	match      func(string) bool
}

func (p *basePattern) Match(path string) bool {
	if p.match(path) {
		p.count++
		return true
	}
	return false
}

func (p *basePattern) RecurseDir() bool { return p.recurseDir }
func (p *basePattern) String() string   { return p.orig }
func (p *basePattern) MatchCount() int  { return p.count }

// normalizePattern is posixpath.normpath followed by stripping the leading slash.
//
// Archived paths have no leading slash, so a pattern written as "/home/user" has to lose
// its slash or it would match nothing at all - which is the mistake this quietly fixes for
// the user.
func normalizePattern(p string) string {
	return strings.TrimPrefix(path.Clean(p), "/")
}

// NewPattern builds a pattern of the given style.
//
// recurseDir says whether a matched directory is still descended into. It is true for
// include patterns and for "-" excludes (so an include *inside* an excluded directory can
// still be found) and false for "!" excludes, which is the whole difference between the
// two exclude forms.
func NewPattern(style, pattern string, recurseDir bool) (Pattern, error) {
	p := &basePattern{orig: pattern, recurseDir: recurseDir}

	switch style {
	case StylePathFull:
		want := normalizePattern(pattern)
		fp := &fullPathPattern{basePattern: *p, want: want}
		fp.match = func(path string) bool { return path == want }
		return fp, nil

	case StylePathPrefix:
		// A trailing separator is added to both sides, so "home/user" matches
		// "home/user/x" but not "home/username".
		want := strings.TrimPrefix(strings.TrimSuffix(path.Clean(pattern), "/")+"/", "/")
		p.match = func(path string) bool { return strings.HasPrefix(path+"/", want) }

	case StyleFnmatch:
		expr := preparePathPattern(pattern, "/*")
		re, err := regexp.Compile(FnmatchTranslate(expr))
		if err != nil {
			return nil, fmt.Errorf("patterns: %q is not a usable fnmatch pattern: %w", pattern, err)
		}
		p.match = func(path string) bool { return re.MatchString(path + "/") }

	case StyleShellPath:
		expr := preparePathPattern(pattern, "/**/*")
		re, err := regexp.Compile(`\A` + Translate(expr, ""))
		if err != nil {
			return nil, fmt.Errorf("patterns: %q is not a usable shell pattern: %w", pattern, err)
		}
		p.match = func(path string) bool { return re.MatchString(path + "/") }

	case StyleRegexPath:
		// Not normalised and not anchored: a regular expression is searched anywhere in
		// the path, and the leading slash is *not* stripped, because the user wrote a
		// regex and borge must not edit it.
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("patterns: %q is not a usable regular expression: %w", pattern, err)
		}
		p.match = func(path string) bool { return re.FindStringIndex(path) != nil }

	default:
		return nil, fmt.Errorf("patterns: unknown pattern style %q", style)
	}
	return p, nil
}

// preparePathPattern applies the trailing-slash rule shared by the fnmatch and shell
// styles.
//
// A pattern ending in "/" excludes a directory's *contents* but not the directory itself,
// which is what lets an empty directory be kept while everything in it is dropped. The
// suffix differs per style because their wildcards mean different things.
func preparePathPattern(pattern, suffix string) string {
	if strings.HasSuffix(pattern, "/") {
		return strings.TrimPrefix(strings.TrimSuffix(path.Clean(pattern), "/")+suffix+"/", "/")
	}
	return strings.TrimPrefix(path.Clean(pattern)+suffix, "/")
}

// ParsePattern reads a pattern with an optional "xx:" style prefix.
//
// fallback is the style to use when there is no prefix.
func ParsePattern(pattern, fallback string, recurseDir bool) (Pattern, error) {
	style := fallback
	if len(pattern) > 2 && pattern[2] == ':' && isAlnum(pattern[:2]) {
		style, pattern = pattern[:2], pattern[3:]
	}
	return NewPattern(style, pattern, recurseDir)
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return len(s) > 0
}

// ---------------------------------------------------------------- commands

// Command is what a pattern does to the paths it matches.
type Command int

const (
	// CmdInclude keeps a matching path.
	CmdInclude Command = iota
	// CmdExclude drops it, but still descends into a matching directory so an include
	// inside it can be found.
	CmdExclude
	// CmdExcludeNoRecurse drops it and does not descend, which is faster and is what
	// --exclude does.
	CmdExcludeNoRecurse
	// CmdRootPath is a root path from a pattern file, not a pattern.
	CmdRootPath
	// CmdPatternStyle changes the default style for the rest of a pattern file.
	CmdPatternStyle
)

// IsInclude reports whether the command keeps matching paths.
func (c Command) IsInclude() bool { return c == CmdInclude }

// RecursesDir reports whether a directory matched by this command is descended into.
func (c Command) RecursesDir() bool { return c != CmdExcludeNoRecurse }

// ---------------------------------------------------------------- matcher

// entry is one pattern and what to do with it.
type entry struct {
	pattern Pattern
	cmd     Command
}

// Matcher decides whether a path is included.
//
// # How the decision is made
//
// Patterns are tried in the order they were given and the **first** match wins, so a
// later pattern cannot undo an earlier one. That is what makes an include-then-exclude
// sequence mean what it reads like.
//
// When nothing matches, Fallback decides. It is false when include paths were given
// (nothing outside them is wanted) and true otherwise (everything is wanted except what
// was excluded).
type Matcher struct {
	entries []entry
	// fullPaths is an exact-path fast path, so a long list of "pf:" patterns does not
	// turn every decision into a linear scan.
	fullPaths map[string]Command

	// Fallback is returned when no pattern matches.
	Fallback bool
	// RecurseDirDefault is whether an unmatched directory is descended into. It must be
	// true, or an include inside an excluded directory could never be reached.
	RecurseDirDefault bool

	// recurseDir records what the last Match call decided about descending.
	recurseDir bool

	includePatterns []Pattern
}

// NewMatcher returns an empty matcher with the given fallback.
func NewMatcher(fallback bool) *Matcher {
	return &Matcher{
		fullPaths:         map[string]Command{},
		Fallback:          fallback,
		RecurseDirDefault: true,
		recurseDir:        true,
	}
}

// Empty reports whether the matcher holds no patterns.
func (m *Matcher) Empty() bool { return len(m.entries) == 0 && len(m.fullPaths) == 0 }

// Clone returns a copy that can be added to without affecting the original.
//
// recreate needs one: the tagged directories it finds belong to the archive it found them
// in, and adding them to a matcher shared across several archives would carry one archive's
// caches into the next. borg shares the matcher and accumulates; see DIVERGENCES.md #54.
func (m *Matcher) Clone() *Matcher {
	out := &Matcher{
		entries:           append([]entry(nil), m.entries...),
		fullPaths:         make(map[string]Command, len(m.fullPaths)),
		Fallback:          m.Fallback,
		RecurseDirDefault: m.RecurseDirDefault,
		recurseDir:        m.recurseDir,
		includePatterns:   append([]Pattern(nil), m.includePatterns...),
	}
	for k, v := range m.fullPaths {
		out.fullPaths[k] = v
	}
	return out
}

// Add appends a pattern.
func (m *Matcher) Add(p Pattern, cmd Command) {
	// A full-path pattern goes into the map instead of the list. borg does the same, and
	// it turns the cost of a long exclude list from a linear scan per path into a lookup.
	if fp, ok := p.(*fullPathPattern); ok {
		m.fullPaths[fp.want] = cmd
		return
	}
	m.entries = append(m.entries, entry{pattern: p, cmd: cmd})
}

// fullPathPattern is the "pf:" style, kept as its own type so the matcher can recognise
// it and use the map.
type fullPathPattern struct {
	basePattern
	want string
}

// AddIncludePaths adds the paths a command line named, as path-prefix includes.
//
// Giving any include path flips the fallback to false: naming what you want means not
// wanting anything else.
func (m *Matcher) AddIncludePaths(paths []string) error {
	for _, p := range paths {
		// StylePathPrefix is the *fallback*, not the style. borg parses a leading "xx:"
		// off a positional path just as it does off a --pattern, so "borge list ARCHIVE
		// sh:**/*.jpg" is a shell pattern and only a bare path is a prefix.
		//
		// Forcing the prefix style here made every styled positional path match nothing at
		// all: not an error, just an empty result, on list, extract, diff and export-tar
		// alike. Found while writing the tests for `find`, which is the command whose
		// documentation makes the most of the styles.
		pattern, err := ParsePattern(p, StylePathPrefix, true)
		if err != nil {
			return err
		}
		m.Add(pattern, CmdInclude)
		m.includePatterns = append(m.includePatterns, pattern)
	}
	m.Fallback = len(m.includePatterns) == 0
	return nil
}

// UnmatchedIncludePatterns lists include paths that never matched anything, which is worth
// warning about: it usually means a typo, and the user would otherwise get a successful
// run that restored nothing.
func (m *Matcher) UnmatchedIncludePatterns() []Pattern {
	var out []Pattern
	for _, p := range m.includePatterns {
		if p.MatchCount() == 0 {
			out = append(out, p)
		}
	}
	return out
}

// Match reports whether a path is included.
func (m *Matcher) Match(p string) bool {
	p = strings.TrimPrefix(p, "/")

	if cmd, ok := m.fullPaths[p]; ok {
		m.recurseDir = cmd.RecursesDir()
		return cmd.IsInclude()
	}
	for _, e := range m.entries {
		if e.pattern.Match(p) {
			m.recurseDir = e.pattern.RecurseDir()
			return e.cmd.IsInclude()
		}
	}
	m.recurseDir = m.RecurseDirDefault
	return m.Fallback
}

// RecurseDir reports what the last Match decided about descending into a directory.
//
// It is state left behind by Match rather than a return value, which is borg's shape.
// It matters for a directory that was excluded: whether to walk into it anyway depends on
// which exclude form matched.
func (m *Matcher) RecurseDir() bool { return m.recurseDir }

// ---------------------------------------------------------------- pattern files

// FileEntry is one parsed line of a pattern file.
type FileEntry struct {
	Cmd Command
	// Pattern is set for include and exclude commands.
	Pattern Pattern
	// Value is set for a root path or a pattern style.
	Value string
}

// ParseInclExclCommand reads one line of a --patterns-from file.
//
// The leading character is the command: "+" include, "-" exclude, "!" exclude without
// recursing, "R"/"r" a root path, "P"/"p" a default style for what follows.
func ParseInclExclCommand(line, fallback string) (FileEntry, error) {
	if line == "" {
		return FileEntry{}, fmt.Errorf("patterns: a pattern or command must not be empty")
	}
	var cmd Command
	switch line[0] {
	case '-':
		cmd = CmdExclude
	case '!':
		cmd = CmdExcludeNoRecurse
	case '+':
		cmd = CmdInclude
	case 'R', 'r':
		cmd = CmdRootPath
	case 'P', 'p':
		cmd = CmdPatternStyle
	default:
		return FileEntry{}, fmt.Errorf("patterns: %q must start with one of -, !, +, R, r, P or p", line)
	}

	rest := strings.TrimLeft(line[1:], " \t")
	if rest == "" {
		return FileEntry{}, fmt.Errorf("patterns: %q has a command but no value", line)
	}

	switch cmd {
	case CmdRootPath:
		return FileEntry{Cmd: cmd, Value: rest}, nil
	case CmdPatternStyle:
		switch rest {
		case StyleFnmatch, StyleShellPath, StyleRegexPath, StylePathPrefix, StylePathFull:
			return FileEntry{Cmd: cmd, Value: rest}, nil
		default:
			return FileEntry{}, fmt.Errorf("patterns: %q is not a pattern style", rest)
		}
	default:
		p, err := ParsePattern(rest, fallback, cmd.RecursesDir())
		if err != nil {
			return FileEntry{}, err
		}
		return FileEntry{Cmd: cmd, Pattern: p}, nil
	}
}

// LoadPatternFile reads a --patterns-from file.
//
// Blank lines and lines whose first non-blank character is "#" are ignored, so a pattern
// file can be commented.
func LoadPatternFile(r io.Reader, fallback string) (entries []FileEntry, roots []string, err error) {
	if fallback == "" {
		fallback = StyleShellPath
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e, err := ParseInclExclCommand(line, fallback)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		switch e.Cmd {
		case CmdRootPath:
			roots = append(roots, e.Value)
		case CmdPatternStyle:
			fallback = e.Value
		default:
			entries = append(entries, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return entries, roots, nil
}

// LoadExcludeFile reads an --exclude-from file: one pattern per line, no command
// characters, fnmatch by default, none of them recursing.
func LoadExcludeFile(r io.Reader) ([]Pattern, error) {
	var out []Pattern
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := ParsePattern(line, StyleFnmatch, false)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Style describes one file-pattern style for the documentation.
type Style struct {
	// Prefix is the two letters a user writes, without the colon.
	Prefix string
	// Description is the user-facing explanation, as "borge help patterns" prints it.
	Description string
}

// Styles lists the file-pattern styles, in the order the documentation presents them:
// the two defaults first, then the rest.
//
// This is the source. The help topic renders this list rather than restating it, so a
// style added here appears in the documentation and a style removed here disappears from
// it - neither can be forgotten, because there is nowhere to forget it.
func Styles() []Style {
	return []Style{
		{StyleFnmatch, "fnmatch. * matches anything including /, ? matches one character, " +
			"[abc] matches a character class. This is the default for --exclude."},
		{StyleShellPath, "shell style. * stops at a directory separator, ** crosses them, " +
			"? matches one character, and {a,b} alternates."},
		{StyleRegexPath, "a regular expression, matched against the whole path (Go's regexp " +
			"syntax, which is RE2 - no backreferences and no lookaround)."},
		{StylePathPrefix, "path prefix. Matches PATH and everything under it. This is the " +
			"default for a positional PATH argument."},
		{StylePathFull, "path full. Matches that one path exactly and nothing under it."},
	}
}
