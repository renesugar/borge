// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --timestamp sets an archive's *nominal* time. start and end stay the real run times, so
// this is dating a backup, not pretending it ran then.
//
// The archive time is read back through borg rather than borge, so the assertion is about
// what was stored.
func TestTimestampMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A reference file with a known modification time, for the form that takes one.
	ref := filepath.Join(t.TempDir(), "ref")
	if err := os.WriteFile(ref, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	refTime := time.Date(2019, 7, 4, 12, 0, 0, 0, time.Local)
	if err := os.Chtimes(ref, refTime, refTime); err != nil {
		t.Fatal(err)
	}

	archiveTime := func(name string) string {
		t.Helper()
		return strings.TrimSpace(r.mustRun("repo-list", "-r", r.path, "-a", name, "--format", "{time}{NL}"))
	}

	cases := []struct{ name, value string }{
		{"naive", "2020-03-01T04:05:06"},
		{"with an offset", "2021-06-07T08:09:10+02:00"},
		{"a date alone", "2022-11-12"},
		{"a reference file", ref},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun("create", "-r", r.path, "--timestamp", c.value, "borg-"+c.name, src)
			if _, stderr, code := r.borge(t, "create", "--timestamp", c.value, "borge-"+c.name, src); code != ExitOK {
				t.Fatalf("borge create --timestamp %q exited %d\n%s", c.value, code, stderr)
			}
			got, want := archiveTime("borge-"+c.name), archiveTime("borg-"+c.name)
			if want == "" {
				t.Fatal("borg recorded no time; the comparison would be vacuous")
			}
			if got != want {
				t.Errorf("borge dated the archive %q, borg %q", got, want)
			}
		})
	}

	// import-tar takes it too.
	tarPath := filepath.Join(t.TempDir(), "t.tar")
	r.mustRun("export-tar", "-r", r.path, "borg-naive", tarPath)
	r.mustRun("import-tar", "-r", r.path, "--timestamp", "2016-06-06T06:06:06", "borg-tar", tarPath)
	if _, stderr, code := r.borge(t, "import-tar", "--timestamp", "2016-06-06T06:06:06", "borge-tar", tarPath); code != ExitOK {
		t.Fatalf("borge import-tar --timestamp exited %d\n%s", code, stderr)
	}
	if got, want := archiveTime("borge-tar"), archiveTime("borg-tar"); got != want {
		t.Errorf("import-tar dated the archive %q, borg %q", got, want)
	}
}

// TestRecreateTimestampNeedsARewrite is the subtlety worth pinning.
//
// "recreate --timestamp" on its own changes nothing, in either tool, because recreate skips
// an archive that needs no rewriting - and a test that only ran that form would pass while
// exercising none of the code it claims to test. Forcing a rewrite with --chunker-params
// is what makes the option observable.
func TestRecreateTimestampNeedsARewrite(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "--timestamp", "2020-03-01T04:05:06", "a", src)
	original := strings.TrimSpace(r.mustRun("repo-list", "-r", r.path, "-a", "a", "--format", "{time}{NL}"))

	// Without a reason to rewrite, the time is left alone.
	if _, stderr, code := r.borge(t, "recreate", "-a", "a", "--timestamp", "2015-05-05T05:05:05"); code != ExitOK {
		t.Fatalf("borge recreate exited %d\n%s", code, stderr)
	}
	if got := strings.TrimSpace(r.mustRun("repo-list", "-r", r.path, "-a", "a", "--format", "{time}{NL}")); got != original {
		t.Errorf("a recreate with nothing to do redated the archive: %q, was %q", got, original)
	}

	// With one, the new time is used - and borg agrees.
	r.mustRun("create", "-r", r.path, "--timestamp", "2020-03-01T04:05:06", "b", src)
	r.mustRun("recreate", "-r", r.path, "-a", "b", "--chunker-params", "fastcdc,19,23,20,2",
		"--timestamp", "2015-05-05T05:05:05")
	if _, stderr, code := r.borge(t, "recreate", "-a", "a", "--chunker-params", "fastcdc,19,23,20,2",
		"--timestamp", "2015-05-05T05:05:05"); code != ExitOK {
		t.Fatalf("borge recreate --chunker-params exited %d\n%s", code, stderr)
	}
	want := strings.TrimSpace(r.mustRun("repo-list", "-r", r.path, "-a", "b", "--format", "{time}{NL}"))
	got := strings.TrimSpace(r.mustRun("repo-list", "-r", r.path, "-a", "a", "--format", "{time}{NL}"))
	if want == original {
		t.Fatal("borg did not redate the rewritten archive; this test no longer proves anything")
	}
	if got != want {
		t.Errorf("borge dated the rewritten archive %q, borg %q", got, want)
	}
}

// TestTimestampRefusesRubbish: a value that is neither a file nor a timestamp is refused
// before the repository is opened, and an explicitly empty one - what "--timestamp $WHEN"
// expands to when WHEN is unset - is refused too rather than silently dating the archive
// now. See PORTING_PLAN.md §2.3.
func TestTimestampRefusesRubbish(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "not-a-time", "2020-13-45T99:99:99", "yesterday"} {
		_, stderr, code := r.borge(t, "create", "--timestamp", bad, "x", src)
		if code != ExitError {
			t.Errorf("--timestamp %q exited %d, want %d", bad, code, ExitError)
		}
		if !strings.Contains(stderr, "timestamp") {
			t.Errorf("--timestamp %q: unhelpful message %q", bad, stderr)
		}
	}
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Errorf("a refused --timestamp still created an archive: %v", names)
	}
}

// TestParseTimestampPrefersAFile pins borg's order: the argument is stat'ed before it is
// parsed, so a file whose name looks like a timestamp wins. It is observable, so it is
// reproduced rather than tidied - a user passing a reference file relies on that branch.
func TestParseTimestampPrefersAFile(t *testing.T) {
	dir := t.TempDir()
	name := "2001-01-01T01:01:01"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2018, 2, 3, 4, 5, 6, 0, time.Local)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	got, err := parseTimestamp(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(mtime) {
		t.Errorf("a reference file gave %s, want its mtime %s", got, mtime)
	}
	// The same text, with no such file, parses as the timestamp it looks like.
	parsed, err := parseTimestamp(name)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2001, 1, 1, 1, 1, 1, 0, time.Local); !parsed.Equal(want) {
		t.Errorf("the text parsed as %s, want %s", parsed, want)
	}
	if parsed.Equal(got) {
		t.Error("the two branches gave the same answer; the test cannot tell them apart")
	}
}
