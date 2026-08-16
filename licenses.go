// SPDX-License-Identifier: Apache-2.0

// Package borge embeds the license texts borge must ship with every binary.
//
// BSD-3-Clause clause 2 (borg) and BSD-2-Clause clause 2 (restic) require binary
// redistributions to reproduce the upstream copyright notices and disclaimers. A
// static Go binary has no "documentation or other materials" of its own, so the
// texts are embedded here and printed by "borge --license". See docs/LICENSING.md.
package borge

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed LICENSE NOTICE licenses
var licenseFS embed.FS

// License returns the Apache-2.0 text borge as a whole is licensed under.
func License() string { return mustRead("LICENSE") }

// Notice returns the NOTICE file required by Apache-2.0 section 4(d).
func Notice() string { return mustRead("NOTICE") }

// UpstreamLicense returns one upstream license text by its path relative to the
// licenses/ directory, e.g. "borg/LICENSE".
func UpstreamLicense(name string) (string, error) {
	b, err := licenseFS.ReadFile(path.Join("licenses", name))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UpstreamLicenses lists the available upstream license paths, relative to
// licenses/, in sorted order.
func UpstreamLicenses() []string {
	var names []string
	_ = fs.WalkDir(licenseFS, "licenses", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, strings.TrimPrefix(p, "licenses/"))
		return nil
	})
	sort.Strings(names)
	return names
}

// WriteAll writes the complete license disclosure: borge's own license, the NOTICE
// file, and every upstream license text, each under a banner naming its source.
func WriteAll(w io.Writer) error {
	sections := []struct{ title, body string }{
		{"borge is licensed under the Apache License, Version 2.0", License()},
		{"NOTICE", Notice()},
	}
	for _, name := range UpstreamLicenses() {
		body, err := UpstreamLicense(name)
		if err != nil {
			return err
		}
		sections = append(sections, struct{ title, body string }{"licenses/" + name, body})
	}
	for i, s := range sections {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s\n%s\n\n%s\n", s.title, strings.Repeat("=", len(s.title)), strings.TrimRight(s.body, "\n")); err != nil {
			return err
		}
	}
	return nil
}

func mustRead(name string) string {
	b, err := licenseFS.ReadFile(name)
	if err != nil {
		// Unreachable: the files are embedded at build time, so a failure here means
		// the binary was built without them, which the licenses_test.go guards against.
		panic("borge: embedded license missing: " + err.Error())
	}
	return string(b)
}
