// SPDX-License-Identifier: Apache-2.0

package borge

import (
	"bytes"
	"strings"
	"testing"
)

// The license obligations in docs/LICENSING.md are only met if these texts actually
// ship in the binary. These tests are the mechanism that keeps that true.

func TestLicenseIsApache2(t *testing.T) {
	l := License()
	for _, want := range []string{
		"Apache License",
		"Version 2.0, January 2004",
		"http://www.apache.org/licenses/",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("LICENSE does not look like Apache-2.0: missing %q", want)
		}
	}
}

func TestNoticeCreditsUpstream(t *testing.T) {
	n := Notice()
	for _, want := range []string{
		"BorgBackup",
		"The Borg Collective",
		"Jonas Borgström",
		"BSD 3-Clause",
		"restic",
		"BSD 2-Clause",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("NOTICE is missing required attribution %q", want)
		}
	}
}

func TestUpstreamLicensesArePresent(t *testing.T) {
	// These exact paths are what NOTICE and README.md point readers at.
	want := map[string]string{
		"borg/LICENSE":   "Redistribution and use in source and binary forms",
		"borg/AUTHORS":   "Borg",
		"restic/LICENSE": "BSD 2-Clause License",
		// borghash and borgstore were split out of borg into separate packages and
		// have a different copyright holder, so they need their own notices even
		// though they carry the same BSD-3-Clause license. See LICENSING.md section 6.
		"upstream-python/borghash.LICENSE.rst":  "Thomas Waldmann",
		"upstream-python/borgstore.LICENSE.rst": "Thomas Waldmann",
	}
	for name, marker := range want {
		body, err := UpstreamLicense(name)
		if err != nil {
			t.Fatalf("licenses/%s not embedded: %v", name, err)
		}
		if !strings.Contains(body, marker) {
			t.Errorf("licenses/%s does not contain %q", name, marker)
		}
	}

	got := UpstreamLicenses()
	for name := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("UpstreamLicenses() = %v, missing %q", got, name)
		}
	}
}

// The borg LICENSE must be reproduced verbatim; clause 3 forbids using the upstream
// name as an endorsement, so it is the clause most likely to be quietly dropped.
func TestBorgLicenseIsVerbatim(t *testing.T) {
	body, err := UpstreamLicense("borg/LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Copyright (C) 2015-2026 The Borg Collective",
		"Copyright (C) 2010-2014 Jonas Borgström",
		"The name of the author may not be used to endorse or promote",
		"THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("borg LICENSE is not verbatim: missing %q", want)
		}
	}
}

func TestWriteAllIncludesEverything(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAll(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Apache License",
		"NOTICE",
		"licenses/borg/LICENSE",
		"licenses/restic/LICENSE",
		"licenses/upstream-python/borghash.LICENSE.rst",
		"licenses/upstream-python/borgstore.LICENSE.rst",
		"Jonas Borgström",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteAll output missing %q", want)
		}
	}
}
