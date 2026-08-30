// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the path helpers in borg's src/borg/helpers/fs.py
// (make_path_safe, slashify, map_chars, assert_sanitized_path, to_sanitized_path).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package item

import (
	"fmt"
	"strings"
)

// Path sanitisation. This is a security boundary, not a tidiness measure: an archive is
// attacker-controlled input as far as extraction is concerned, and a stored path
// containing ".." or a leading "/" would let an archive write outside the extraction
// directory. borg checks on both the write and the read side, and so does borge.
//
// Paths are stored relative, normalised, and with forward slashes.

// ErrUnsafePath means a path contained a ".." element or was otherwise not safe to
// store or extract.
type ErrUnsafePath struct {
	Path   string
	Reason string
}

func (e *ErrUnsafePath) Error() string {
	return fmt.Sprintf("item: unsafe path %q: %s", e.Path, e.Reason)
}

// MakePathSafe makes a path relative and normalised, rejecting any ".." element.
//
// It removes a leading "/", collapses repeated slashes, drops "." elements and any
// trailing slash. An empty result becomes ".", matching posixpath.normpath.
//
// The Windows-specific steps in borg's version (backslash conversion, drive-letter
// folding, and mapping reserved characters into the Unicode private use area) are
// no-ops on POSIX and are omitted here; see the note on Windows below.
func MakePathSafe(path string) (string, error) {
	// borg checks the backslash forms first, before any conversion, so a path is
	// rejected on a POSIX system too even though backslash is a legal filename
	// character there. Reproduced deliberately: a path that borg would refuse to store
	// must not become storable just because borge wrote it.
	if strings.Contains(path, `\..`) || strings.Contains(path, `..\`) {
		return "", &ErrUnsafePath{Path: path, Reason: `contains a '..' element (backslash form)`}
	}

	p := strings.TrimLeft(path, "/")

	if p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return "", &ErrUnsafePath{Path: path, Reason: "contains a '..' element"}
	}

	return normPath(p), nil
}

// normPath is posixpath.normpath for a path that is already relative and known to
// contain no ".." elements: collapse slashes, drop "." components, drop a trailing
// slash. An empty path normalises to ".".
//
// The general posixpath.normpath resolves ".." against earlier components and
// preserves exactly two leading slashes. Neither case can arise here - MakePathSafe
// has already rejected ".." and stripped every leading slash - so this covers the
// reachable behaviour rather than reimplementing the whole function.
func normPath(p string) string {
	if p == "" {
		return "."
	}
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return "."
	}
	return strings.Join(out, "/")
}

// AssertSanitizedPath is borg's encode-side check: a path being stored must already be
// sanitised. borg checks again here as a precaution because pattern matching relies on
// it, and so does borge.
func AssertSanitizedPath(path string) (string, error) {
	safe, err := MakePathSafe(path)
	if err != nil {
		return "", err
	}
	if safe != path {
		return "", &ErrUnsafePath{Path: path, Reason: fmt.Sprintf("is not sanitised (would become %q)", safe)}
	}
	return path, nil
}

// ToSanitizedPath is borg's decode-side conversion: legacy borg versions allowed
// unsanitised paths to be stored, so paths are sanitised again when read.
//
// borg 2 makes paths safe before storing them, so for a borg 2 archive this is a no-op
// - but it is what stops a hostile or corrupt archive from directing an extraction
// outside its target directory, so it stays on the read path regardless.
func ToSanitizedPath(path string) (string, error) {
	return MakePathSafe(path)
}

// Slashify converts backslashes to forward slashes on Windows. borge stores forward
// slashes always; on POSIX this is the identity, because a backslash is an ordinary
// character in a filename and converting it would corrupt the name.
func Slashify(path string) string {
	// POSIX only for now: borge does not support Windows (plans/PORTING_PLAN.md §15).
	// When it does, this becomes strings.ReplaceAll(path, `\`, "/") under a build tag,
	// together with MapChars and the drive-letter handling in MakePathSafe.
	return path
}

// MapChars maps characters that are reserved on Windows into the Unicode private use
// area, the way cifs mapchars does, so an archived POSIX path stays usable there.
//
// The mapping is bijective: '<' '>' ':' '"' '\' '|' '?' '*' become U+F03C U+F03E
// U+F03A U+F022 U+F05C U+F07C U+F03F U+F02A. On POSIX it is the identity.
func MapChars(path string) string {
	return path // POSIX only; see Slashify
}
