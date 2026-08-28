// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the placeholder substitution in borg's
// src/borg/helpers/parseformat.py (_replace_placeholders, format_line, DatetimeWrapper).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package placeholders substitutes {now}, {hostname} and friends into archive names.
//
// # Why this is hand-written
//
// borg gets it for free from Python: the placeholders are a dict, the syntax is
// str.format, and "{now:%Y-%m-%d}" works because datetime.__format__ calls strftime. Go
// has neither piece - text/template is a different syntax, and time formatting uses
// reference layouts rather than strftime directives - so both the format parser and a
// strftime are written out here.
//
// It has to be exact rather than approximate. An archive name is what a retention policy
// and a restore both select on, so "%Y-%m-%d" producing anything other than 2026-08-17
// is not a cosmetic difference: it is a name that does not match what the user's other
// scripts look for, and on a repository shared with borg it is a name borg would have
// spelled differently.
package placeholders

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultTimeFormat is what {now} and {utcnow} produce with no format spec. It is borg's
// ISO_FORMAT_NO_USECS (src/borg/constants.py).
const DefaultTimeFormat = "%Y-%m-%dT%H:%M:%S"

// Values are the substitutions available to a template.
type Values struct {
	// Now is the local time and UTCNow the same instant in UTC. Both are taken once, so
	// every placeholder in one name agrees with the others - a name built from {now} and
	// {unixtime} must not straddle a second boundary.
	Now    time.Time
	UTCNow time.Time

	Hostname string
	FQDN     string
	User     string
	PID      int
	UUID4    string

	// Version is what {borgversion} expands to. borge's own version: a placeholder that
	// reported borg's would be a lie about what wrote the archive.
	Version string
}

// Default fills Values from the system.
//
// BORGE_HOSTNAME and BORGE_USERNAME override the detected values, falling back to
// BORG_HOSTNAME and BORG_USERNAME. borg has the same overrides, and they exist because a
// container's hostname is rarely the name a backup should be filed under.
func Default(version string) Values {
	now := time.Now()
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	fqdn := host
	if v := lookup("HOSTNAME"); v != "" {
		host = v
		fqdn = v
	}
	user := osUsername()
	if v := lookup("USERNAME"); v != "" {
		user = v
	}
	return Values{
		Now:      now,
		UTCNow:   now.UTC(),
		Hostname: host,
		FQDN:     fqdn,
		User:     user,
		PID:      os.Getpid(),
		UUID4:    uuid4(),
		Version:  version,
	}
}

func lookup(name string) string {
	if v, ok := os.LookupEnv("BORGE_" + name); ok && v != "" {
		return v
	}
	if v, ok := os.LookupEnv("BORG_" + name); ok && v != "" {
		return v
	}
	return ""
}

// Placeholder describes one substitution for the documentation.
type Placeholder struct {
	// Name is what goes between the braces.
	Name string
	// Syntax is what a user writes, which is "{name}" except where a format is accepted.
	Syntax string
	// Description is the user-facing explanation, as "borge help placeholders" prints it.
	Description string
	// TakesFormat is true for the two that accept a strftime format after a colon.
	TakesFormat bool
}

// All lists every placeholder, in the order the documentation presents them: the times
// first, because they are what an archive name is usually built from.
//
// This is the source. Names() is derived from it and the help topic renders it, so a
// placeholder cannot exist in one of the three and not the others.
func All() []Placeholder {
	return []Placeholder{
		{Name: "now", Syntax: "{now}", Description: "the current local time, as YYYY-MM-DDTHH:MM:SS", TakesFormat: true},
		{Name: "utcnow", Syntax: "{utcnow}", Description: "the same instant in UTC", TakesFormat: true},
		{Name: "now", Syntax: "{now:FORMAT}", Description: "the current local time in a chosen format", TakesFormat: true},
		{Name: "utcnow", Syntax: "{utcnow:FORMAT}", Description: "the same in UTC", TakesFormat: true},
		{Name: "unixtime", Syntax: "{unixtime}", Description: "seconds since the epoch"},
		{Name: "hostname", Syntax: "{hostname}", Description: "this machine's hostname (BORGE_HOSTNAME overrides it)"},
		{Name: "fqdn", Syntax: "{fqdn}", Description: "its fully qualified name"},
		{Name: "reverse-fqdn", Syntax: "{reverse-fqdn}", Description: "the same with the components reversed"},
		{Name: "user", Syntax: "{user}", Description: "the current user (BORGE_USERNAME overrides it)"},
		{Name: "pid", Syntax: "{pid}", Description: "this process's id"},
		{Name: "uuid4", Syntax: "{uuid4}", Description: "a random UUID"},
		{Name: "borgversion", Syntax: "{borgversion}", Description: "borge's version"},
		{Name: "borgmajor", Syntax: "{borgmajor}", Description: "its major part"},
		{Name: "borgminor", Syntax: "{borgminor}", Description: "its minor part"},
		{Name: "borgpatch", Syntax: "{borgpatch}", Description: "its patch part"},
	}
}

// Names lists every placeholder, sorted and without repeats, for error messages and
// documentation. It is derived from All so the two cannot disagree.
func Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range All() {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Expand substitutes the placeholders in text.
//
// An unknown placeholder is an error, not a literal. borg does the same, and the reason
// is what happens otherwise: "borge create '{hostnmae}-{now}'" would silently create an
// archive with a misspelling baked into its name, every night, until somebody looked.
func (v Values) Expand(text string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		if c != '{' && c != '}' {
			b.WriteByte(c)
			i++
			continue
		}
		// Doubled braces are literals, as in Python's format strings. This is the only
		// way to put a brace in an archive name.
		if i+1 < len(text) && text[i+1] == c {
			b.WriteByte(c)
			i += 2
			continue
		}
		if c == '}' {
			return "", fmt.Errorf("placeholders: unmatched } at offset %d in %q "+
				"(write }} for a literal brace)", i, text)
		}

		end := strings.IndexByte(text[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("placeholders: unmatched { at offset %d in %q "+
				"(write {{ for a literal brace)", i, text)
		}
		field := text[i+1 : i+end]
		i += end + 1

		expanded, err := v.field(field)
		if err != nil {
			return "", err
		}
		b.WriteString(expanded)
	}
	return b.String(), nil
}

// field expands one "{name}" or "{name:spec}".
func (v Values) field(field string) (string, error) {
	// Python's format also has a "!r" conversion. borg rejects it, and so does this: it
	// would be a Python repr, which means nothing here.
	if i := strings.IndexByte(field, '!'); i >= 0 {
		return "", fmt.Errorf("placeholders: %q has a conversion, which borge does not "+
			"support", "{"+field+"}")
	}

	name, spec, hasSpec := strings.Cut(field, ":")
	switch name {
	case "now":
		return strftime(v.Now, timeSpec(spec, hasSpec))
	case "utcnow":
		return strftime(v.UTCNow, timeSpec(spec, hasSpec))
	}

	if hasSpec && spec != "" {
		return "", fmt.Errorf("placeholders: {%s} takes no format, but %q was given "+
			"(only {now} and {utcnow} do)", name, spec)
	}

	switch name {
	case "hostname":
		return v.Hostname, nil
	case "fqdn":
		return v.FQDN, nil
	case "reverse-fqdn":
		return reverseFQDN(v.FQDN), nil
	case "user":
		return v.User, nil
	case "pid":
		return fmt.Sprint(v.PID), nil
	case "unixtime":
		return fmt.Sprint(v.UTCNow.Unix()), nil
	case "uuid4":
		return v.UUID4, nil
	case "borgversion":
		return v.Version, nil
	case "borgmajor":
		return versionPart(v.Version, 1), nil
	case "borgminor":
		return versionPart(v.Version, 2), nil
	case "borgpatch":
		return versionPart(v.Version, 3), nil
	case "":
		return "", fmt.Errorf("placeholders: {} is not a placeholder; " +
			"write {{}} for literal braces")
	default:
		return "", fmt.Errorf("placeholders: {%s} is not a placeholder. Available: %s",
			name, strings.Join(Names(), ", "))
	}
}

func timeSpec(spec string, hasSpec bool) string {
	if !hasSpec || spec == "" {
		return DefaultTimeFormat
	}
	return spec
}

func reverseFQDN(fqdn string) string {
	parts := strings.Split(fqdn, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

// versionPart returns the first n dot-separated components of a version.
//
// borge's version may be "dev" or carry a suffix, so the parts are taken as they are
// rather than parsed into numbers: {borgmajor} of "dev" is "dev", which is at least true.
func versionPart(version string, n int) string {
	parts := strings.SplitN(version, ".", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, ".")
}
