// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// --list on delete, undelete and export-tar, and the reporting rules that go with it.
//
// borg prints per-archive lines *only* under --list: its -v is a log level and produces
// exactly what a plain run does. borge printed its own line under -v and a different one
// under --dry-run, so one event had three shapes and none of them was borg's.

// normaliseArchiveLine makes borg's and borge's per-archive lines comparable by replacing
// the parts that cannot match: the archive id and the timestamp.
var (
	someID   = regexp.MustCompile(`\[[0-9a-f]{64}\]`)
	someTime = regexp.MustCompile(`[A-Z][a-z]{2}, [0-9-]+ [0-9:]+ [-+][0-9]{4}`)
)

func normaliseArchiveLines(s string) string {
	s = someID.ReplaceAllString(s, "[ID]")
	s = someTime.ReplaceAllString(s, "TIME")
	// borg names its own binary in the compact hint; borge names borge's.
	s = strings.ReplaceAll(s, `Run "borg compact"`, `Run "borge compact"`)
	// The two tools work on differently-named archives of equal length, so the names are
	// folded together and the padding still has to match.
	s = strings.ReplaceAll(s, "aa-", "xx-")
	s = strings.ReplaceAll(s, "bb-", "xx-")
	return s
}

// runSplit runs borg with stdout and stderr kept apart.
//
// r.mustRun uses CombinedOutput, which merges them - and that is how a differential test
// of --list passed while borge was writing its listing to the wrong stream. borg puts
// every listing and progress line on stderr, keeping stdout for the command's data, and a
// helper that cannot see the difference cannot check it.
func (r *borgRepo) runSplit(args ...string) (string, string) {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("borg %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func TestDeleteAndUndeleteListMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Each tool acts on its own archives so the two runs do not interfere, and the names
	// are the *same length* on purpose: the template pads the name to 36 columns, so
	// names of different lengths would make every line differ in whitespace and the
	// comparison would be about the fixture rather than the format.
	for _, n := range []string{"aa-1", "aa-2", "bb-1", "bb-2"} {
		r.mustRun("create", "-r", r.path, n, src)
	}

	// Both sides are read from stderr, which is where borg puts a listing: stdout is for
	// the command's data, and delete has none.
	compare := func(t *testing.T, borgArgs, borgeArgs []string) {
		t.Helper()
		borgOut, borgErr := r.runSplit(borgArgs...)
		want := normaliseArchiveLines(borgErr)
		stdout, stderr, code := r.borge(t, borgeArgs...)
		if code != ExitOK {
			t.Fatalf("borge %v exited %d\n%s", borgeArgs, code, stderr)
		}
		if strings.TrimSpace(want) == "" {
			t.Fatal("borg printed nothing on stderr; the comparison would be vacuous")
		}
		if strings.TrimSpace(borgOut) != "" || strings.TrimSpace(stdout) != "" {
			t.Errorf("something reached stdout: borg %q, borge %q", borgOut, stdout)
		}
		if normaliseArchiveLines(stderr) != want {
			t.Errorf("borge:\n%q\nborg:\n%q", normaliseArchiveLines(stderr), want)
		}
	}

	t.Run("delete --list --dry-run", func(t *testing.T) {
		// borge adds a summary line that borg does not print at all (DIVERGENCES #31),
		// so the archive lines are compared and the summary is checked separately below.
		_, borgErr := r.runSplit("delete", "-r", r.path, "--list", "--dry-run", "-a", "sh:aa-*")
		want := normaliseArchiveLines(borgErr)
		// --force because borge refuses a multi-archive delete without it, which is
		// borge's own safety rule and not what this test is about.
		stdout, stderr, code := r.borge(t, "delete", "--list", "--dry-run", "--force", "-a", "sh:bb-*")
		if code != ExitOK {
			t.Fatalf("borge delete --list --dry-run exited %d\n%s", code, stderr)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("a dry run wrote to stdout:\n%s", stdout)
		}
		got := normaliseArchiveLines(stderr)
		summary := "would delete 2 archive(s); nothing was changed\n"
		if !strings.HasSuffix(got, summary) {
			t.Errorf("no dry-run summary:\n%q", got)
		}
		if strings.TrimSuffix(got, summary) != want {
			t.Errorf("the archive lines differ\nborge:\n%q\nborg:\n%q",
				strings.TrimSuffix(got, summary), want)
		}
	})
	t.Run("delete --list", func(t *testing.T) {
		compare(t,
			[]string{"delete", "-r", r.path, "--list", "-a", "sh:aa-1"},
			[]string{"delete", "--list", "-a", "sh:bb-1"})
	})
	t.Run("undelete --list", func(t *testing.T) {
		compare(t,
			[]string{"undelete", "-r", r.path, "--list", "-a", "sh:aa-1"},
			[]string{"undelete", "--list", "-a", "sh:bb-1"})
	})
	t.Run("delete without --list", func(t *testing.T) {
		compare(t,
			[]string{"delete", "-r", r.path, "-a", "sh:aa-2"},
			[]string{"delete", "-a", "sh:bb-2"})
	})
}

// TestVerboseDoesNotListArchives: -v is a log level in borg, not a listing. borge printed
// per-archive lines under it, which is why this is asserted rather than assumed - the
// change removed output people may have been reading.
func TestVerboseDoesNotListArchives(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "one", src)

	stdout, stderr, code := r.borge(t, "delete", "-v", "-a", "sh:one")
	if code != ExitOK {
		t.Fatalf("borge delete -v exited %d", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("delete wrote to stdout, which is for a command's data:\n%s", stdout)
	}
	stdout = stderr
	// Checked against the archive's id rather than its name: "Done." contains "one", and
	// a substring test on the name passed the wrong way round until that showed up.
	if regexp.MustCompile(`\b[0-9a-f]{8}\b`).MatchString(stdout) {
		t.Errorf("-v named an archive; borg prints those only under --list:\n%s", stdout)
	}
	// The compact hint is what a plain run says, and -v must say the same thing.
	if !strings.Contains(stdout, "compact") {
		t.Errorf("-v did not print the compact hint:\n%s", stdout)
	}

	// A dry run without --list says what it would do, where borg says nothing at all.
	// Silence is an answer nobody can act on: the point of a dry run is to decide
	// something from what it says. DIVERGENCES #31.
	r.mustRun("undelete", "-r", r.path, "-a", "sh:one")
	stdout, stderr, code = r.borge(t, "delete", "--dry-run", "-a", "sh:one")
	if code != ExitOK {
		t.Fatalf("borge delete --dry-run exited %d", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a dry run wrote to stdout:\n%s", stdout)
	}
	stdout = stderr
	if !strings.Contains(stdout, "would delete 1 archive(s); nothing was changed") {
		t.Errorf("a dry run without --list did not say what it would do:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--list") {
		t.Errorf("a dry run without --list did not say how to see which:\n%s", stdout)
	}
	// And it still names no archive, because that is what --list is for.
	if regexp.MustCompile(`\b[0-9a-f]{8}\b`).MatchString(stdout) {
		t.Errorf("a dry run without --list named an archive:\n%s", stdout)
	}
}

// TestExportTarListMatchesBorg: the same paths, in the two tools' own orders.
//
// Sibling order differs by design - borge walks a directory sorted and borg takes readdir
// order (DIVERGENCES #23) - and an archive's item order carries that difference into every
// listing made from it. So the comparison is of sets.
func TestExportTarListMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	for _, rel := range []string{"f.txt", "sub/g.txt"} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.mustRun("create", "-r", r.path, "one", src)

	lines := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		sort.Strings(out)
		return out
	}

	_, borgErr := r.runSplit("export-tar", "-r", r.path, "--list", "one", filepath.Join(t.TempDir(), "b.tar"))
	want := lines(borgErr)
	stdout, stderr, code := r.borge(t, "export-tar", "--list", "one", filepath.Join(t.TempDir(), "g.tar"))
	if code != ExitOK {
		t.Fatalf("borge export-tar --list exited %d\n%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("the listing reached stdout, where the tar goes:\n%s", stdout)
	}
	got := lines(stderr)
	if len(want) < 3 {
		t.Fatalf("borg listed %d items; the comparison would be vacuous", len(want))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("borge listed\n  %s\nborg listed\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// Without --list it says nothing either.
	stdout, _, code = r.borge(t, "export-tar", "one", filepath.Join(t.TempDir(), "quiet.tar"))
	if code != ExitOK || strings.TrimSpace(stdout) != "" {
		t.Errorf("export-tar without --list printed %q (exit %d)", stdout, code)
	}
}

// TestExportTarToStdoutStaysATar is the regression for the defect the test above could not
// see: with --list writing to stdout, "export-tar --list ARCHIVE -" put the listing inside
// the tar stream and produced an archive tar(1) rejects.
func TestExportTarToStdoutStaysATar(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "one", src)

	stdout, stderr, code := r.borge(t, "export-tar", "--list", "one", "-")
	if code != ExitOK {
		t.Fatalf("borge export-tar --list - exited %d\n%s", code, stderr)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("--list printed nothing; this test would pass on a listing that never ran")
	}

	// The bytes on stdout have to be a tar and nothing else. A tar header starts with the
	// file name, so a listing mixed in shows up as a path where a header belongs - which
	// is exactly what happened.
	tarPath := filepath.Join(t.TempDir(), "out.tar")
	if err := os.WriteFile(tarPath, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-tf", tarPath).CombinedOutput(); err != nil {
		t.Errorf("tar rejected what borge wrote to stdout: %v\n%s", err, out)
	}
}
