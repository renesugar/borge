// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoListFormatMatchesBorg is the differential for --format.
//
// The default is included on purpose, and it is the row that matters most: borge's column
// layout used to be a Printf, and the comment above it *quoted* borg's default template as
// if the two were the same thing. Now the default is that template, so the columns a user
// sees without --format are produced by the same code path as the ones they see with it,
// and a drift between them is impossible rather than merely unlikely.
func TestRepoListFormatMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A comment longer than the default's 40-character truncation, and a tag, so the
	// columns that only appear when there is something in them are exercised.
	r.mustRun("create", "-r", r.path, "--comment",
		"a fairly long comment that will be truncated at forty characters", "one", src)
	r.mustRun("create", "-r", r.path, "two", src)
	r.mustRun("tag", "-r", r.path, "--add", "T", "one")

	cases := []struct {
		name  string
		flags []string
	}{
		{"the default", nil},
		{"--short", []string{"--short"}},
		{"a bare key", []string{"--format", "{archive}{NL}"}},
		{"left padding", []string{"--format", "{archive:<20}|{NL}"}},
		{"right padding", []string{"--format", "{archive:>20}|{NL}"}},
		{"a fill character", []string{"--format", "{archive:.>20}|{NL}"}},
		{"precision", []string{"--format", "{id:.12} {comment:.10}|{NL}"}},
		// A number pads right by default where a string pads left; getting this backwards
		// would still produce plausible-looking columns.
		{"a numeric key", []string{"--format", "{size:12}|{nfiles:4}|{NL}"}},
		{"several keys", []string{"--format", "{time} {username}@{hostname} {tags}{NL}"}},
		{"literal braces", []string{"--format", "{{{archive}}}{NL}"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := r.mustRun(append([]string{"repo-list", "-r", r.path}, c.flags...)...)
			stdout, stderr, code := r.borge(t, append([]string{"repo-list"}, c.flags...)...)
			if code != ExitOK {
				t.Fatalf("borge repo-list %v exited %d\n%s", c.flags, code, stderr)
			}
			if strings.TrimSpace(want) == "" {
				t.Fatalf("borg printed nothing; the comparison would be vacuous")
			}
			if stdout != want {
				t.Errorf("borge:\n%q\nborg:\n%q", stdout, want)
			}
		})
	}
}

// TestRepoListFormatEnvironment: BORGE_REPO_LIST_FORMAT replaces the default, and --format
// beats it, which is borg's precedence.
func TestRepoListFormatEnvironment(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "only", src)

	fromEnv, _, code := r.borgeWithEnv(t, map[string]string{"BORGE_REPO_LIST_FORMAT": "env:{archive}{NL}"}, "repo-list")
	if code != ExitOK || strings.TrimSpace(fromEnv) != "env:only" {
		t.Errorf("BORGE_REPO_LIST_FORMAT gave %q (exit %d)", fromEnv, code)
	}

	// --format wins over the environment.
	explicit, _, code := r.borgeWithEnv(t, map[string]string{"BORGE_REPO_LIST_FORMAT": "env:{archive}{NL}"},
		"repo-list", "--format", "flag:{archive}{NL}")
	if code != ExitOK || strings.TrimSpace(explicit) != "flag:only" {
		t.Errorf("--format did not beat the environment: %q (exit %d)", explicit, code)
	}

	// And a bad format is refused rather than printing a listing with a hole in it.
	_, stderr, code := r.borge(t, "repo-list", "--format", "{nosuchkey}{NL}")
	if code != ExitError {
		t.Errorf("an unknown key exited %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "nosuchkey") {
		t.Errorf("the error does not name the key: %q", stderr)
	}
}

// TestListFormatMatchesBorg is the item formatter's differential. The keys are borg's
// ItemFormatter set, and the interesting ones are not the obvious ones.
func TestListFormatMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../f.txt", filepath.Join(src, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "one", src)

	cases := []struct {
		name   string
		format string
	}{
		{"the default", ""},
		{"a bare key", "{path}{NL}"},
		{"the stat keys", "{type}|{mode}|{user}|{group}|{uid}|{gid}|{NL}"},
		{"sizes", "{size:10}|{num_chunks}|{NL}"},
		// borg's format_time falls back to the modification time for all three, and borg
		// does not store atime unless asked - so "{atime}" is a date in borg and would be
		// empty in a port that read the field and stopped there. It was.
		{"timestamps", "{mtime}|{ctime}|{atime}|{NL}"},
		{"iso timestamps", "{isomtime}|{isoctime}|{isoatime}|{NL}"},
		// A symlink has a target, so {extra} is the arrow; everything else has neither.
		{"link keys", "{path}|{target}|{extra}|{hlid}|{NL}"},
		{"archive keys", "{archivename}/{archiveid:.8} {path}{NL}"},
		{"padding", "{user:>10}|{group:<10}|{path:.12}|{NL}"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"list"}
			borgArgs := []string{"list", "-r", r.path}
			if c.format != "" {
				args = append(args, "--format", c.format)
				borgArgs = append(borgArgs, "--format", c.format)
			}
			want := r.mustRun(append(borgArgs, "one")...)
			stdout, stderr, code := r.borge(t, append(args, "one")...)
			if code != ExitOK {
				t.Fatalf("borge list %v exited %d\n%s", args, code, stderr)
			}
			if strings.TrimSpace(want) == "" {
				t.Fatal("borg printed nothing; the comparison would be vacuous")
			}
			if stdout != want {
				t.Errorf("borge:\n%q\nborg:\n%q", stdout, want)
			}
		})
	}

	// An unknown key is refused before the archive is read, and the message lists what
	// there is - a listing with a blank column would be the alternative.
	_, stderr, code := r.borge(t, "list", "--format", "{nosuchkey}{NL}", "one")
	if code != ExitError || !strings.Contains(stderr, "nosuchkey") {
		t.Errorf("an unknown item key exited %d: %s", code, stderr)
	}
	// And the environment default works, as it does for repo-list.
	out, _, code := r.borgeWithEnv(t, map[string]string{"BORGE_LIST_FORMAT": "env:{path}{NL}"}, "list", "one")
	if code != ExitOK || !strings.Contains(out, "env:") {
		t.Errorf("BORGE_LIST_FORMAT was not used: %q", out)
	}
}

// TestFindFormatMatchesBorg: find shares the item keys but has its own default, which
// carries the archive id and name because a find crosses archives.
func TestFindFormatMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "one", src)
	r.mustRun("create", "-r", r.path, "two", src)

	// borge prints a summary line that borg does not; it is dropped before comparing
	// rather than removed, because it is a borge feature and not a difference in the
	// listing itself.
	dropSummary := func(s string) string {
		var kept []string
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "match(es) in") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}

	for _, format := range []string{"", "{path}{NL}", "{archivename}|{path}|{size}{NL}"} {
		t.Run(format, func(t *testing.T) {
			args := []string{"find"}
			borgArgs := []string{"find", "-r", r.path}
			if format != "" {
				args = append(args, "--format", format)
				borgArgs = append(borgArgs, "--format", format)
			}
			want := r.mustRun(append(borgArgs, "sh:**/f.txt")...)
			stdout, stderr, code := r.borge(t, append(args, "sh:**/f.txt")...)
			if code != ExitOK {
				t.Fatalf("borge find exited %d\n%s", code, stderr)
			}
			if strings.TrimSpace(want) == "" {
				t.Fatal("borg found nothing; the comparison would be vacuous")
			}
			if dropSummary(stdout) != dropSummary(want) {
				t.Errorf("borge:\n%q\nborg:\n%q", dropSummary(stdout), dropSummary(want))
			}
		})
	}
}
