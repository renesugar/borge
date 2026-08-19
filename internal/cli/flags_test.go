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

// flaggedTree builds a source tree holding one file with the nodump flag set and one
// without, and skips the test where flags cannot be set at all.
//
// nodump is the flag to test with: immutable and append-only need CAP_LINUX_IMMUTABLE, so
// an unprivileged run cannot set them on the source, let alone restore them.
func flaggedTree(t *testing.T) (dir, flagged, plain string) {
	t.Helper()
	dir = t.TempDir()
	flagged = filepath.Join(dir, "nodump.txt")
	plain = filepath.Join(dir, "plain.txt")
	write(t, flagged, "flagged")
	write(t, plain, "plain")

	if _, err := exec.LookPath("chattr"); err != nil {
		t.Skip("chattr is not available, so no flag can be set to test with")
	}
	if out, err := exec.Command("chattr", "+d", flagged).CombinedOutput(); err != nil {
		t.Skipf("cannot set the nodump flag under %s (%v): %s", dir, err, out)
	}
	// A source that did not actually get the flag would make every assertion below pass
	// by comparing two zeroes.
	if !hasNoDump(t, flagged) {
		t.Skipf("the nodump flag did not stick under %s; the filesystem does not support it", dir)
	}
	if hasNoDump(t, plain) {
		t.Fatalf("the control file has the nodump flag; the test cannot tell the two apart")
	}
	return dir, flagged, plain
}

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

// TestFileFlagsRoundTripAgainstBorg: a flag borge stored is one borg restores, and the
// reverse.
func TestFileFlagsRoundTripAgainstBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src, _, _ := flaggedTree(t)

	r.mustRun("create", "-r", r.path, "by-borg", src)
	if _, stderr, code := r.borge(t, "create", "by-borge", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	// Stored: borg's decoder reading what each tool wrote.
	for _, archive := range []string{"by-borg", "by-borge"} {
		items := dumpItems(t, r, archive)
		flagged, ok := items["nodump.txt"]
		if !ok {
			// borg excludes a nodump file from the backup entirely; borge does not yet.
			// That difference is real and recorded separately - here it only means this
			// archive has nothing to compare.
			if archive == "by-borg" {
				continue
			}
			t.Fatalf("%s has no nodump.txt", archive)
		}
		if got := flagged["bsdflags"]; got != float64(1) {
			t.Errorf("%s stored bsdflags=%v for a nodump file, want 1 (UF_NODUMP)", archive, got)
		}
		if got := items["plain.txt"]["bsdflags"]; got != float64(0) {
			t.Errorf("%s stored bsdflags=%v for an unflagged file, want 0", archive, got)
		}
	}

	// Restored: each tool extracting the archive the other wrote.
	for _, c := range []struct{ archive, by string }{
		{"by-borge", "borg"},
		{"by-borge", "borge"},
	} {
		into := t.TempDir()
		if c.by == "borg" {
			cmd := exec.Command(r.binary, "extract", "-r", r.path, c.archive)
			cmd.Dir, cmd.Env = into, r.env()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("borg extract: %v\n%s", err, out)
			}
		} else {
			// borge's -C rather than a chdir: the CLI runs in-process here, so changing
			// the working directory would change it for the whole test binary.
			if _, stderr, code := r.borge(t, "extract", "-C", into, c.archive); code != ExitOK {
				t.Fatalf("borge extract exited %d\n%s", code, stderr)
			}
		}
		restored := findFile(t, into, "nodump.txt")
		if !hasNoDump(t, restored) {
			t.Errorf("%s extracted by %s: the nodump flag was not restored", c.archive, c.by)
		}
		if plain := findFile(t, into, "plain.txt"); hasNoDump(t, plain) {
			t.Errorf("%s extracted by %s: an unflagged file came back flagged", c.archive, c.by)
		}
	}
}

// findFile locates a basename anywhere under root; extraction recreates the source's whole
// path, which under a temporary directory is long and not worth spelling out.
func findFile(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Base(p) == name {
			found = p
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("no %s under %s (%v)", name, root, err)
	}
	return found
}

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
