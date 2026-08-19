// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// File flags and extended attributes, compared against borg as stored bytes rather than as
// output. Both are item fields borge did not write at all until 2026-08-19: flags were
// never captured, so a borge backup silently dropped them, and both keys were omitted when
// empty, which in borg's schema means something else entirely. See DIVERGENCES.md #8.

// dumpItems reads an archive's raw items through borg, which is the point: borg's own
// decoder is what has to make sense of what borge wrote.
func dumpItems(t *testing.T, r *borgRepo, archive string) map[string]map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), archive+".json")
	r.mustRun("debug", "dump-archive", "-r", r.path, archive, out)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading borg's dump: %v", err)
	}
	var doc struct {
		Items []map[string]any `json:"_items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("borg's dump is not JSON: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, it := range doc.Items {
		path, _ := it["path"].(string)
		if path == "" {
			continue
		}
		byName[filepath.Base(path)] = it
	}
	if len(byName) == 0 {
		t.Fatalf("borg's dump of %s has no items", archive)
	}
	return byName
}

// hasNoDump reports whether a path carries the nodump flag, read through lsattr so that
// the check is independent of the code under test.
func hasNoDump(t *testing.T, path string) bool {
	t.Helper()
	out, err := exec.Command("lsattr", "-d", path).Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return false
	}
	return strings.Contains(fields[0], "d")
}

// The flag round trip is NOT tested here, and the reason is worth stating rather than
// leaving as an absence.
//
// Testing it needs a file whose flags borge stores. Unprivileged, the only flag that can
// be set on a source file is nodump - immutable and append-only need CAP_LINUX_IMMUTABLE -
// and a nodump file is one neither tool archives at all (DIVERGENCES.md #39). So no
// unprivileged "borge create" can produce an archive carrying a flag, and this test used
// to pass only because borge did not yet implement the exclusion.
//
// What replaces it: TestExaminedAttributesAreRecorded below shows that the flags are read
// and stored (bsdflags=0 appears, and disappears under --noflags); the exclusion tests in
// exclude_attr_test.go show a real flag value being read, since borge can only exclude a
// nodump file by having read the flag; and internal/archive's TestFlagsSurviveExtraction
// builds an archive holding a flagged item directly - which is how such an archive arises
// in practice, from a privileged backup or another machine - and extracts it.

// TestExaminedAttributesAreRecorded: the presence of the key is the statement that borge
// looked, and it has to match borg's in all three states.
//
// This is the half that is easy to mistake for cosmetic. An archive that omits "xattrs"
// when there are none is indistinguishable from one taken with --noxattrs, so "checked,
// found none" and "not recorded" become the same answer.
func TestExaminedAttributesAreRecorded(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	write(t, filepath.Join(src, "plain.txt"), "plain")

	cases := []struct {
		name       string
		args       []string
		wantXAttrs bool
		wantFlags  bool
	}{
		{"default", nil, true, true},
		{"noflags", []string{"--noflags"}, true, false},
		{"noxattrs", []string{"--noxattrs"}, false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			borgArgs := append([]string{"create", "-r", r.path, "b-" + c.name, src}, c.args...)
			r.mustRun(borgArgs...)
			borgeArgs := append([]string{"create", "e-" + c.name, src}, c.args...)
			if _, stderr, code := r.borge(t, borgeArgs...); code != ExitOK {
				t.Fatalf("borge create %v exited %d\n%s", c.args, code, stderr)
			}

			for _, prefix := range []string{"b-", "e-"} {
				item := dumpItems(t, r, prefix+c.name)["plain.txt"]
				if item == nil {
					t.Fatalf("%s%s has no plain.txt", prefix, c.name)
				}
				_, hasXAttrs := item["xattrs"]
				_, hasFlags := item["bsdflags"]
				if hasXAttrs != c.wantXAttrs {
					t.Errorf("%s%s: xattrs present = %v, want %v", prefix, c.name, hasXAttrs, c.wantXAttrs)
				}
				if hasFlags != c.wantFlags {
					t.Errorf("%s%s: bsdflags present = %v, want %v", prefix, c.name, hasFlags, c.wantFlags)
				}
			}
		})
	}
}
