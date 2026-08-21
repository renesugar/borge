// SPDX-License-Identifier: Apache-2.0

package location

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The oracle for this package is borg's own Location, run out of the pinned venv.
//
// A location parser is exactly the kind of code that looks obviously right and is quietly
// wrong at the edges - "ftp://host/x" is a directory, "rest:///srv/repo" has no host and a
// path with no leading slash, an unbracketed IPv6 address is not a location at all. None of
// those are guesses here: every case below is decided by running borg's parser.

// borgLocations parses every text with borg's Location and returns what it made of it.
func borgLocations(t *testing.T, texts []string) []map[string]any {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the Location differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the Location differential test")
	}

	const script = `
import json, sys
from borg.helpers.parseformat import Location

out = []
for text in json.load(sys.stdin):
    try:
        loc = Location(text)
    except ValueError as err:
        out.append({"valid": False, "error": str(err)})
        continue
    out.append({
        "valid": True,
        "proto": loc.proto,
        "user": loc.user,
        "host": loc._host,
        "port": loc.port,
        "path": loc.path,
        "canonical": loc.canonical_path(),
    })
json.dump(out, sys.stdout)
`
	in, err := json.Marshal(texts)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = strings.NewReader(string(in))
	// The working directory decides what a relative location resolves to, so the oracle
	// has to run in the same one the Go side is using.
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("borg's Location parser failed: %v", err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("oracle output is not JSON: %v\n%s", err, out)
	}
	if len(parsed) != len(texts) {
		t.Fatalf("oracle returned %d results for %d locations", len(parsed), len(texts))
	}
	return parsed
}

// corpus is every shape this parser has to decide about, including the ones that look like
// mistakes and are not.
var corpus = []string{
	// Local paths, which is what most locations are.
	"repo",
	"./repo",
	"../up/repo",
	"sub/dir/repo",
	"/abs/repo",
	"/abs/./repo/../repo",
	"repo:with:colons",
	"~/backups",       // no tilde expansion in either tool: a directory named "~"
	"ftp://host/repo", // not a scheme borg knows, so it is a path
	"unknownscheme:repo",
	"s3nonsense/repo", // starts with "s3" but not with "s3:"

	// file://
	"file:///abs/repo",
	"file:///abs/./repo/..",
	"file://relative", // invalid: file:// demands an absolute path
	"file:///",        // invalid: nothing after the slash
	"file://",

	// rest://
	"rest:///srv/repo",
	"rest://host/repo",
	"rest://user@host/repo",
	"rest://user@host:8080/repo",
	"rest://host:8080/srv/repo",
	"rest://[fe80::1]/repo",
	"rest://[fe80::1]:9000/repo",
	"rest://user@[::1]:9000/repo",
	"rest://host/",        // invalid: empty path
	"rest://host",         // invalid: no separator
	"rest://host:port/x",  // invalid: a port is digits
	"rest://host:22:33/x", // invalid: one colon
	"rest:////double",

	// ssh:// - borg 1.x only, but it parses, and borge has to know that it did.
	"ssh://host/repo",
	"ssh://user@host:2222/path/repo",
	"ssh://[::1]:22/repo",
	"ssh://::1/repo", // invalid: an IPv6 address needs brackets
	"ssh:///repo",    // invalid: ssh:// needs a host

	// The borgstore schemes, which borg detects and does not parse.
	"sftp://borge-sftp-test/repo",
	"sftp://user@host:2222/repo",
	"s3:bucket/repo",
	"s3:profile@bucket/repo",
	"s3:key:secret@http://localhost:4566/bucket/repo",
	"b2:key:secret@bucket/repo",
	"rclone:remote:path/repo",
	"rclone:/abs/path/repo",
	"http://host/repo",
	"https://user:pass@host/repo",
	"s3:",
	"rclone:",

	// Neither a path nor a URL.
	"//double/slash",
}

func TestLocationMatchesBorg(t *testing.T) {
	want := borgLocations(t, corpus)
	for i, text := range corpus {
		w := want[i]
		got, err := Parse(text)
		valid, _ := w["valid"].(bool)
		if !valid {
			if err == nil {
				t.Errorf("%q: borg rejected it (%v), borge parsed it as %s",
					text, w["error"], got.Canonical())
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: borg parsed it as proto=%v path=%v, borge rejected it: %v",
				text, w["proto"], w["path"], err)
			continue
		}
		if s, _ := w["proto"].(string); s != got.Proto {
			t.Errorf("%q: borg says proto=%q, borge says %q", text, s, got.Proto)
		}
		if s, _ := w["user"].(string); s != got.User {
			t.Errorf("%q: borg says user=%q, borge says %q", text, s, got.User)
		}
		if s, _ := w["host"].(string); s != got.Host {
			t.Errorf("%q: borg says host=%q, borge says %q", text, s, got.Host)
		}
		port := 0
		if f, ok := w["port"].(float64); ok {
			port = int(f)
		}
		if port != got.Port {
			t.Errorf("%q: borg says port=%d, borge says %d", text, port, got.Port)
		}
		if s, _ := w["path"].(string); s != got.Path {
			t.Errorf("%q: borg says path=%q, borge says %q", text, s, got.Path)
		}
		if s, _ := w["canonical"].(string); s != got.Canonical() {
			t.Errorf("%q: borg's canonical path is %q, borge's is %q", text, s, got.Canonical())
		}
	}
}

// TestCorpusIsNotVacuous: a corpus that every case rejected, or that never reached the
// interesting protos, would agree with borg while measuring nothing.
func TestCorpusIsNotVacuous(t *testing.T) {
	seen := map[string]int{}
	var invalid int
	for _, text := range corpus {
		loc, err := Parse(text)
		if err != nil {
			invalid++
			continue
		}
		seen[loc.Proto]++
	}
	for _, proto := range []string{"file", "rest", "ssh", "sftp", "s3", "b2", "rclone", "http", "https"} {
		if seen[proto] == 0 {
			t.Errorf("the corpus has no %s location", proto)
		}
	}
	if invalid < 5 {
		t.Errorf("the corpus has only %d rejected locations; the rejecting half is untested", invalid)
	}
}
