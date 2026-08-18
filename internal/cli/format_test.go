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
