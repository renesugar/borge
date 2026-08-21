// SPDX-License-Identifier: Apache-2.0

package location

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The two forms of a location, and why they are two.
//
// A location that carries a credential has to be usable without publishing it. borg keeps
// the same pair - the processed URL opens the repository, canonical_path() is what may be
// printed - and the whole value of the split is lost if the printing path can reach the
// other one by accident. So String() is the canonical form: fmt cannot print the secret.

const secretURL = "s3:AKIAEXAMPLE:sup3rs3cret@http://localhost:4566/bucket/repo"

func TestCanonicalDropsCredentialsAndOpenableKeepsThem(t *testing.T) {
	loc, err := Parse(secretURL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(loc.Canonical(), "sup3rs3cret") {
		t.Errorf("the canonical form publishes the secret key: %s", loc.Canonical())
	}
	if !strings.Contains(loc.Openable(), "sup3rs3cret") {
		t.Errorf("the openable form lost the credentials, so nothing could open it: %s",
			loc.Openable())
	}

	// The one that matters in practice: an error message, a log line, a JSON field.
	if printed := fmt.Sprintf("%s|%v", loc, loc); strings.Contains(printed, "sup3rs3cret") {
		t.Errorf("printing a location printed its secret: %s", printed)
	}
}

func TestOpenableIsAbsoluteForALocalRepository(t *testing.T) {
	// A child process may change directory, so what it is handed must not depend on the
	// one it starts in.
	loc, err := Parse("sub/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(loc.Openable()) {
		t.Errorf("Openable() = %q for a relative location; it must be absolute", loc.Openable())
	}
	if loc.Openable() != loc.Path {
		t.Errorf("Openable() = %q but Path = %q", loc.Openable(), loc.Path)
	}
	if loc.Raw != "sub/repo" {
		t.Errorf("Raw = %q, want the location as written", loc.Raw)
	}
}

func TestLocalIsTheSameAsParsingAPath(t *testing.T) {
	made := MustLocal("sub/repo")
	parsed, err := Parse("sub/repo")
	if err != nil {
		t.Fatal(err)
	}
	if made.Proto != parsed.Proto || made.Path != parsed.Path {
		t.Errorf("Local(%q) = %s %q, Parse gives %s %q",
			"sub/repo", made.Proto, made.Path, parsed.Proto, parsed.Path)
	}
	if !made.IsLocal() {
		t.Error("Local() did not produce a local location")
	}
}

func TestRedact(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"s3:key:secret@bucket/repo", "s3:bucket/repo"},
		{"b2:key:secret@bucket/repo", "b2:bucket/repo"},
		{"s3:profile@bucket/repo", "s3:bucket/repo"},
		{"https://user:pass@host/repo", "https://host/repo"},
		{"sftp://user@host/repo", "sftp://host/repo"},
		// Nothing to redact, and nothing may be invented.
		{"rclone:remote:path/repo", "rclone:remote:path/repo"},
		{"/srv/repo", "/srv/repo"},
	} {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUnreachableSchemesStillParse: the point of parsing a location borge cannot open is
// that the refusal can name what it is. A scheme that failed to parse would be reported as
// a bad path instead.
func TestUnreachableSchemesStillParse(t *testing.T) {
	for url, proto := range map[string]string{
		"ssh://host/repo":     "ssh",
		"sftp://host/repo":    "sftp",
		"rest:///srv/repo":    "rest",
		"s3:bucket/repo":      "s3",
		"rclone:remote:x/y":   "rclone",
		"https://host/repo":   "https",
		"b2:bucket/repo":      "b2",
		"http://host:80/repo": "http",
	} {
		loc, err := Parse(url)
		if err != nil {
			t.Errorf("%q did not parse: %v", url, err)
			continue
		}
		if loc.Proto != proto {
			t.Errorf("%q parsed as %q, want %q", url, loc.Proto, proto)
		}
		if loc.IsLocal() {
			t.Errorf("%q was taken for a local path", url)
		}
	}
}
