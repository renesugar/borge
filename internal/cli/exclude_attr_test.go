// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// borg does not archive a file the filesystem itself marks as "do not back up": the nodump
// flag, macOS's Time Machine exclusion xattr, or the XDG backup xattr set to "false".
// borge archived all of them until 2026-08-19, which is a difference in the *contents* of
// the archive rather than in an item's fields. See DIVERGENCES.md #39.
//
// Compared as sets of stored paths against borg, because that is the question: not "did
// borge print a dash" but "is the file in the archive".

// setXAttr sets an extended attribute, skipping the test where the filesystem will not
// take one.
//
// Note what cannot be tested this way: Linux allows only the user, security, system and
// trusted namespaces, and borg's Apple marker
// ("com.apple.metadata:com_apple_backup_excludeItem") is in none of them, so it cannot be
// set on a Linux filesystem at all. That rule is unreachable through create here, and is
// covered by a unit test on the rule itself in internal/archive instead. It still matters
// in practice - the attribute arrives on archives made on macOS, and on anything imported
// from a tar that carries it.
func setXAttr(t *testing.T, path, name, value string) {
	t.Helper()
	if err := unix.Setxattr(path, name, []byte(value), 0); err != nil {
		t.Skipf("cannot set the xattr %s under %s: %v", name, filepath.Dir(path), err)
	}
}

// storedNames is the set of item basenames in an archive, read through borg.
func storedNames(t *testing.T, r *borgRepo, archive string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range sortedItemPaths(t, r.mustRun("list", "-r", r.path, archive, "--json-lines")) {
		out[filepath.Base(p)] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s lists no items at all", archive)
	}
	return out
}

func sortedKeysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestBackupExcludeAttrsMatchBorg: every marker, and the cases that must NOT exclude.
func TestBackupExcludeAttrsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()

	// The Apple marker is absent from this test on purpose; see setXAttr.
	//
	// Exactly "false" excludes. This attribute exists to say "yes, back this up", so
	// anything else - including "true" - leaves the file in, and getting that backwards
	// would silently drop files somebody asked to keep.
	xdgFalse := filepath.Join(src, "xdg-false.txt")
	write(t, xdgFalse, "excluded by the xdg attribute")
	setXAttr(t, xdgFalse, "user.xdg.robots.backup", "false")

	xdgTrue := filepath.Join(src, "xdg-true.txt")
	write(t, xdgTrue, "kept, and the attribute says so")
	setXAttr(t, xdgTrue, "user.xdg.robots.backup", "true")

	// A value that is neither, to catch a truthiness test standing in for the comparison.
	xdgOther := filepath.Join(src, "xdg-other.txt")
	write(t, xdgOther, "kept")
	setXAttr(t, xdgOther, "user.xdg.robots.backup", "0")

	plain := filepath.Join(src, "plain.txt")
	write(t, plain, "kept")

	r.mustRun("create", "-r", r.path, "by-borg", src)
	if _, stderr, code := r.borge(t, "create", "by-borge", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	borg := storedNames(t, r, "by-borg")
	borge := storedNames(t, r, "by-borge")

	excluded := []string{"xdg-false.txt"}
	kept := []string{"xdg-true.txt", "xdg-other.txt", "plain.txt"}

	for _, name := range excluded {
		if borg[name] {
			t.Fatalf("borg archived %s; the premise of this test is wrong", name)
		}
		if borge[name] {
			t.Errorf("borge archived %s, which borg excludes", name)
		}
	}
	for _, name := range kept {
		if !borg[name] {
			t.Fatalf("borg did not archive %s; the premise of this test is wrong", name)
		}
		if !borge[name] {
			t.Errorf("borge did not archive %s, which borg keeps", name)
		}
	}
	// And the whole sets agree, so a file neither list names cannot go missing unnoticed.
	if got, want := sortedKeysOf(borge), sortedKeysOf(borg); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("archives differ\nborg : %v\nborge: %v", want, got)
	}
}

// TestNodumpMatchesBorg: the flag excludes a file, and takes a directory's subtree with it.
func TestNodumpMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()

	if _, err := exec.LookPath("chattr"); err != nil {
		t.Skip("chattr is not available, so no nodump flag can be set to test with")
	}

	flagged := filepath.Join(src, "nodump.txt")
	write(t, flagged, "marked do-not-dump")
	plain := filepath.Join(src, "plain.txt")
	write(t, plain, "kept")

	// A whole directory, to check that the subtree goes with it rather than only the
	// directory entry.
	dir := filepath.Join(src, "nodump-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	buried := filepath.Join(dir, "buried.txt")
	write(t, buried, "inside an excluded directory")

	for _, p := range []string{flagged, dir} {
		if out, err := exec.Command("chattr", "+d", p).CombinedOutput(); err != nil {
			t.Skipf("cannot set the nodump flag under %s (%v): %s", src, err, out)
		}
	}
	if !hasNoDump(t, flagged) {
		t.Skipf("the nodump flag did not stick under %s", src)
	}

	r.mustRun("create", "-r", r.path, "by-borg", src)
	if _, stderr, code := r.borge(t, "create", "by-borge", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	borg := storedNames(t, r, "by-borg")
	borge := storedNames(t, r, "by-borge")

	for _, name := range []string{"nodump.txt", "nodump-dir", "buried.txt"} {
		if borg[name] {
			t.Fatalf("borg archived %s; the premise of this test is wrong", name)
		}
		if borge[name] {
			t.Errorf("borge archived %s, which borg excludes", name)
		}
	}
	if !borge["plain.txt"] {
		t.Error("borge dropped plain.txt, which carries no marker at all")
	}
	if got, want := sortedKeysOf(borge), sortedKeysOf(borg); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("archives differ\nborg : %v\nborge: %v", want, got)
	}
}

// TestDryRunDoesNotApplyAttributeExclusion: borge reports "+" for a file it would not
// actually store, because borg does.
//
// This is borg being inconsistent with itself - its dry run never collects the extended
// attributes, so it cannot know - and borge matches rather than improving on it, because
// "create --dry-run --list" is output the two tools are compared on directly. Recorded in
// DIVERGENCES.md #39 so the inconsistency is not mistaken for borge's own.
func TestDryRunDoesNotApplyAttributeExclusion(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	marked := filepath.Join(src, "xdg-false.txt")
	write(t, marked, "excluded on a real run")
	setXAttr(t, marked, "user.xdg.robots.backup", "false")

	borgOut := r.mustRun("create", "-r", r.path, "unused", src, "--dry-run", "--list")
	_, stderr, code := r.borge(t, "create", "unused2", src, "--dry-run", "--list")
	if code != ExitOK {
		t.Fatalf("borge create --dry-run exited %d\n%s", code, stderr)
	}

	for _, c := range []struct{ who, out string }{{"borg", borgOut}, {"borge", stderr}} {
		var status string
		for _, line := range strings.Split(c.out, "\n") {
			if strings.Contains(line, "xdg-false.txt") && len(line) > 1 {
				status = line[:1]
			}
		}
		if status == "" {
			t.Fatalf("%s did not list xdg-false.txt at all:\n%s", c.who, c.out)
		}
		if status != "+" {
			t.Errorf("%s reported %q for a dry run; both tools report \"+\" here even "+
				"though a real run excludes the file", c.who, status)
		}
	}
}
