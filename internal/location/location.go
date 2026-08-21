// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the Location class and its helpers in borg's
// src/borg/helpers/parseformat.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package location parses the thing a user writes after -r: a repository location.
//
// It is one small type doing one thing, and it is worth its own package because
// everything above it - the CLI, the repository, the store - has until now assumed a
// repository is a directory. It is not, in borg 2: a location names either a local path
// or one of several remote backends, and which of those it is decides how the bytes are
// reached.
//
// # What borg 2 parses, and what it deliberately does not
//
// borg 2 has no repository protocol of its own. Only three forms carry fields borg needs
// for itself:
//
//   - a local path, absolute or relative, made absolute here;
//   - "file://" plus an absolute path, the same thing said explicitly;
//   - "rest://[user@host[:port]]/path", whose parts build the command that serves it.
//
// A fourth, "ssh://", is borg 1.x only and is parsed so that it can be *refused* by name.
//
// Everything else - sftp, s3, b2, rclone, http, https - borg only detects the scheme of
// and hands to borgstore whole (BORGSTORE_SCHEMES in parseformat.py). borge does the same,
// because the alternative is two parsers for one URL that must agree forever.
//
// # Why a scheme borge has never heard of is a local path
//
// borg's rule, reproduced here: a location that is not one of the forms above and does not
// begin with "//" is a *path*, whatever it looks like. So "ftp://host/x" names a directory
// called "ftp:" with subdirectories - it is not an error, and it must not be, because a
// path is allowed to contain a colon. Only the schemes borg knows are treated as schemes.
package location

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Schemes that borg detects and hands to borgstore without parsing. borg's
// BORGSTORE_SCHEMES, in its order.
var borgstoreSchemes = []string{"sftp", "http", "https", "s3", "b2", "rclone"}

// Location is a parsed repository location.
type Location struct {
	// Proto is "file", "rest", "ssh", or one of the borgstore schemes.
	Proto string
	// User, Host and Port are filled in for "rest" and "ssh" only; every other proto
	// either has no such parts or keeps them inside the raw URL. Host keeps the square
	// brackets of an IPv6 address, because putting the URL back together needs them;
	// HostName() is the bare form.
	User string
	Host string
	Port int
	// Path is the repository path, for the protos that have one as a separate field:
	// "file" (absolute), "rest" and "ssh" (as written after the separating slash, which
	// is why it usually has no leading one).
	Path string

	// Raw is the location exactly as the user wrote it, placeholders and all.
	// Processed is the same string after placeholder expansion, and it is what a backend
	// is opened from - so it is also the one that may carry a secret.
	Raw       string
	Processed string
}

// Parse parses a location that has already had its placeholders expanded.
func Parse(text string) (*Location, error) { return ParseRaw(text, text) }

// ParseRaw parses processed while remembering raw.
//
// The two are separate because borg keeps both: the raw form is what a user typed and what
// gets re-expanded when a command needs the location as of a particular timestamp, and the
// processed form is what is actually opened.
func ParseRaw(raw, processed string) (*Location, error) {
	loc, ok := parse(processed)
	if !ok {
		// borg's message, which reaches the user through argparse's type validator.
		return nil, fmt.Errorf("Invalid location format: %q", processed)
	}
	loc.Raw = raw
	loc.Processed = processed
	return loc, nil
}

// Local returns a location for a local path, without going through the syntax at all.
//
// This is for callers that already hold a path and never had a URL - tests, and the parts
// of borge that create a repository in a directory they made themselves. A path is made
// absolute, as Parse would.
func Local(path string) (*Location, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &Location{Proto: "file", Path: abs, Raw: path, Processed: "file://" + abs}, nil
}

// MustLocal is Local for a path this process already holds and knows to be usable.
//
// Local can only fail when the working directory cannot be read, which is a broken process
// rather than a bad location, so callers that are wiring up a path they made themselves -
// and tests - should not have to carry an error for it.
func MustLocal(path string) *Location {
	loc, err := Local(path)
	if err != nil {
		panic("location: " + err.Error())
	}
	return loc
}

// schemeRe is borg's scheme detector: a scheme is a letter followed by letters, digits and
// the three characters RFC 3986 allows, then a colon.
var schemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*:`)

func parse(text string) (*Location, bool) {
	// borg tries five regexes in this order and takes the first that matches. The order
	// is not decoration: each form here can *fail to match* and be reconsidered as
	// something else by a later one, which is where the surprises live.
	if loc, ok := parseAuthority("ssh", text, true); ok {
		return loc, true
	}
	if loc, ok := parseAuthority("rest", text, false); ok {
		return loc, true
	}
	if loc, ok := parseFileURL(text); ok {
		return loc, true
	}
	if m := schemeRe.FindString(text); m != "" {
		scheme := strings.TrimSuffix(m, ":")
		for _, known := range borgstoreSchemes {
			if scheme == known {
				// No fields: borgstore parses this one, and borge's backend for it
				// gets the URL exactly as written.
				return &Location{Proto: scheme}, true
			}
		}
		// Not a scheme borg knows. Fall through to the local form, which is what makes
		// "ftp://x" a directory name rather than an error.
	}

	// borg's local_path_re, whose negative lookahead RE2 has no equivalent for: a local
	// path must not begin with "//", with "ssh://" or "file://", or with one of the
	// borgstore schemes.
	//
	// Note which prefix is *not* on that list: "rest://". So a rest URL that failed to
	// parse above - no path, a port that is not a number - is not an error in borg. It is
	// a relative path, and "rest://host" names a directory called "rest:" holding one
	// called "host". Measured, not deduced; see DIVERGENCES.md #56.
	if text == "" || strings.HasPrefix(text, "//") {
		return nil, false
	}
	for _, prefix := range append([]string{"ssh://", "file://"}, colonForms(borgstoreSchemes)...) {
		if strings.HasPrefix(text, prefix) {
			return nil, false
		}
	}
	abs, err := filepath.Abs(text)
	if err != nil {
		return nil, false
	}
	return &Location{Proto: "file", Path: abs}, true
}

// parseFileURL parses borg's file_re: "file://" and an absolute path.
func parseFileURL(text string) (*Location, bool) {
	rest, ok := cut(text, "file://")
	if !ok {
		return nil, false
	}
	// borg's abs_path_re is "/.+": at least one character after the slash.
	if !strings.HasPrefix(rest, "/") || len(rest) < 2 {
		return nil, false
	}
	return &Location{Proto: "file", Path: normpath(rest)}, true
}

// colonForms turns the scheme list into the "scheme:" prefixes borg's lookahead excludes.
//
// They can only be reached when the scheme test above did not return: that happens for
// "rest:" without "//", say, which borg's lookahead does not exclude - so this list is
// exactly borg's, no more.
func colonForms(schemes []string) []string {
	out := make([]string, 0, len(schemes))
	for _, s := range schemes {
		out = append(out, s+":")
	}
	return out
}

// parseAuthority parses "[user@]host[:port]/path", the shape ssh:// and rest:// share.
//
// requireHost is what separates them: "rest:///srv/repo" is a local rest repository served
// over stdio, so its authority may be empty, while ssh:// must name a host.
func parseAuthority(proto, text string, requireHost bool) (*Location, bool) {
	text, ok := cut(text, proto+"://")
	if !ok {
		return nil, false
	}
	// The separator is the first slash, and it is not part of the path: borg's regexes
	// spell that out, and it is why a rest:// path usually has no leading slash.
	slash := strings.Index(text, "/")
	if slash < 0 {
		return nil, false
	}
	authority, path := text[:slash], text[slash+1:]
	if path == "" {
		return nil, false // borg's ".+": a location with no path is not one
	}
	loc := &Location{Proto: proto, Path: normpath(path)}
	if authority == "" {
		if requireHost {
			return nil, false
		}
		return loc, true
	}

	// user must not contain "@", ":" or "/", so the first "@" is the separator - and a
	// host may contain "@" itself, which is why this is not the last one.
	if at := strings.Index(authority, "@"); at >= 0 {
		user := authority[:at]
		if user == "" || strings.ContainsAny(user, ":/") {
			return nil, false
		}
		loc.User = user
		authority = authority[at+1:]
	}

	host := authority
	if strings.HasPrefix(host, "[") {
		// An IPv6 address in brackets, and only the characters an address is made of.
		end := strings.Index(host, "]")
		if end < 0 {
			return nil, false
		}
		inner := host[1:end]
		if inner == "" || strings.Trim(inner, "0123456789abcdefABCDEF:.") != "" {
			return nil, false
		}
		loc.Host = host[:end+1]
		host = host[end+1:]
	} else {
		// A hostname or IPv4 address: no colon, no slash, and it may not end with "]"
		// (borg's lookbehind, which stops half of a bracketed address matching here).
		colon := strings.Index(host, ":")
		if colon >= 0 {
			loc.Host, host = host[:colon], host[colon:]
		} else {
			loc.Host, host = host, ""
		}
		if loc.Host == "" || strings.HasSuffix(loc.Host, "]") {
			return nil, false
		}
	}
	if host != "" {
		// Whatever is left can only be ":port", and a port is digits.
		if !strings.HasPrefix(host, ":") {
			return nil, false
		}
		digits := host[1:]
		if digits == "" || strings.Trim(digits, "0123456789") != "" {
			return nil, false // borg's "\d+", which takes no sign and no spaces
		}
		port, err := strconv.Atoi(digits)
		if err != nil {
			return nil, false
		}
		loc.Port = port
	}
	return loc, true
}

// HostName is Host without the brackets of an IPv6 address, which is what a connection
// wants and what borg's Location.host property returns.
func (l *Location) HostName() string {
	return strings.Trim(l.Host, "[]")
}

// Openable is the location in the form something else can open it with: the absolute path
// for a local repository, and the processed URL for every other kind.
//
// This is what to hand a child process or another tool. It is deliberately not Canonical -
// canonicalisation strips credentials, so a child given the canonical form of an S3
// location could not open it - and deliberately not Processed either, because a local
// repository written as "sub/repo" would then reach the child as a relative path whose
// meaning depends on the child's working directory.
func (l *Location) Openable() string {
	if l.Proto == "file" {
		return l.Path
	}
	return l.Processed
}

// IsLocal reports whether this location is a directory on this machine.
func (l *Location) IsLocal() bool { return l.Proto == "file" }

// Canonical is borg's canonical_path: the location in one agreed spelling, with any
// embedded credentials removed.
//
// It is what borg writes into its security state and what both tools print, so two spellings
// of one repository ("repo", "./repo", "file:///abs/repo") must produce one string.
func (l *Location) Canonical() string {
	switch l.Proto {
	case "file":
		// borg normalises the unicode form here, but only on macOS (issue #2913). On
		// Linux the path is returned unchanged, and borge is Linux.
		return l.Path
	case "rest", "ssh":
		var b strings.Builder
		b.WriteString(l.Proto)
		b.WriteString("://")
		if l.User != "" {
			b.WriteString(l.User)
			b.WriteString("@")
		}
		b.WriteString(l.Host)
		if l.Port != 0 {
			b.WriteString(":")
			b.WriteString(strconv.Itoa(l.Port))
		}
		b.WriteString("/")
		b.WriteString(l.Path)
		return b.String()
	default:
		return Redact(l.Processed)
	}
}

// String is Canonical, so that printing a location cannot print a secret.
//
// Go will call this for any %s or %v of a *Location, which is the point: the field that
// carries credentials is Processed, and reaching it has to be deliberate.
func (l *Location) String() string { return l.Canonical() }

var (
	netlocCredentials = regexp.MustCompile(`(://)[^/@]+@`)
	opaqueCredentials = regexp.MustCompile(`^((?:s3|b2):)[^@/]+@`)
)

// Redact removes embedded credentials from a URL, for display and for identity.
//
// borg's _redact_url_credentials, and the reason it exists is worth keeping in view: an S3
// location carries the access key and the secret in the URL itself, so every place that
// prints a location, writes it to a log, or uses it as a key in a state file is a place
// that would otherwise publish a credential.
func Redact(url string) string {
	url = netlocCredentials.ReplaceAllString(url, "$1")
	return opaqueCredentials.ReplaceAllString(url, "$1")
}

// cut returns the remainder after prefix, and whether it was there.
func cut(s, prefix string) (string, bool) {
	return strings.TrimPrefix(s, prefix), strings.HasPrefix(s, prefix)
}

// normpath is Python's os.path.normpath, which is Go's path.Clean with one difference that
// shows up in a URL: POSIX gives a path beginning with exactly two slashes an
// implementation-defined meaning, and Python preserves it where Go's Clean collapses it.
func normpath(p string) string {
	if p == "" {
		return "."
	}
	leading := ""
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		leading = "/"
	}
	return leading + filepath.Clean(p)
}
