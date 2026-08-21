// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRelativeRepositoryPathMatchesBorg closes docs/DIVERGENCES.md #22.
//
// "borg repo-create -r REPO" works; borge answered "store: path must be absolute" and
// stopped. borg resolves a relative repository path against the working directory and
// reports the absolute form as the repository's Location, and now so does borge.
func TestRelativeRepositoryPathMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	// borge creates a repository at a relative path, and borg opens it.
	if _, stderr, code := r.borge(t, "repo-create", "-r", "sub/G", "-e", "none-sha256"); code != ExitOK {
		t.Fatalf("borge repo-create -r sub/G exited %d\n%s", code, stderr)
	}
	byBorg := parseKeyValues(r.mustRun("repo-info", "-r", filepath.Join(work, "sub", "G")))
	if byBorg["Repository ID"] == "" {
		t.Fatalf("borg could not read the repository borge made at a relative path")
	}

	// Both tools report the same repository, at an absolute Location.
	stdout, stderr, code := r.borge(t, "repo-info", "-r", "sub/G")
	if code != ExitOK {
		t.Fatalf("borge repo-info -r sub/G exited %d\n%s", code, stderr)
	}
	byBorge := parseKeyValues(stdout)
	if byBorge["Repository ID"] != byBorg["Repository ID"] {
		t.Errorf("the two tools disagree about which repository sub/G is:\n%s\n%s",
			byBorge["Repository ID"], byBorg["Repository ID"])
	}
	want := filepath.Join(work, "sub", "G")
	if byBorge["Location"] != want {
		t.Errorf("borge reports Location %q, want the absolute %q", byBorge["Location"], want)
	}
	if byBorg["Location"] != want {
		t.Errorf("borg reports Location %q; this test's expectation is what has to move",
			byBorg["Location"])
	}

	// A relative BORGE_REPO works too - it is the same code path, and it is the one a
	// user sets once in a profile.
	stdout, stderr, code = r.borgeWithEnv(t, map[string]string{"BORGE_REPO": "sub/G"}, "repo-info")
	if code != ExitOK {
		t.Fatalf("borge repo-info with a relative BORGE_REPO exited %d\n%s", code, stderr)
	}
	if got := parseKeyValues(stdout)["Location"]; got != want {
		t.Errorf("a relative BORGE_REPO resolved to %q, want %q", got, want)
	}

	// And the other direction: borg creates at a relative path, borge reads it. mustRun
	// inherits this test's working directory, which t.Chdir has set.
	r.mustRun("repo-create", "-r", "sub/B", "-e", "none-sha256")
	stdout, stderr, code = r.borge(t, "repo-info", "-r", "sub/B")
	if code != ExitOK {
		t.Fatalf("borge could not read the repository borg made at a relative path: %d\n%s", code, stderr)
	}
	if got := parseKeyValues(stdout)["Location"]; got != filepath.Join(work, "sub", "B") {
		t.Errorf("borge resolved sub/B to %q", got)
	}

	// The whole path, not just the opening: archive through the relative path and read
	// the result back through the absolute one.
	if _, stderr, code := r.borge(t, "create", "-r", "sub/G", "arch", "f.txt"); code != ExitOK {
		t.Fatalf("borge create through a relative repository path exited %d\n%s", code, stderr)
	}
	stdout, stderr, code = r.borge(t, "list", "-r", filepath.Join(work, "sub", "G"), "--short", "arch")
	if code != ExitOK {
		t.Fatalf("borge list exited %d\n%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "f.txt" {
		t.Errorf("the archive made through a relative repository path holds %q", stdout)
	}

	// Resolution is against the working directory and nothing else. From one level down,
	// the same relative path names a repository that does not exist - which is what borg
	// does, and what proves the resolution is not a search.
	t.Chdir(filepath.Join(work, "sub"))
	if _, _, code := r.borge(t, "repo-info", "-r", "sub/G"); code == ExitOK {
		t.Error("\"-r sub/G\" opened a repository from a directory it is not relative to")
	}
}

// TestResolveRepoExpandsBeforeResolving pins the order: a placeholder is substituted
// first, and the result is then made absolute. The other order would turn
// "-r {hostname}/repo" into a path containing a literal brace.
func TestResolveRepoExpandsBeforeResolving(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{Stdout: os.Stderr, Stderr: os.Stderr, Getenv: func(string) (string, bool) { return "", false }}

	got, err := e.resolveRepo("{hostname}/repo")
	if err != nil {
		t.Fatalf("resolveRepo: %v", err)
	}
	if want := filepath.Join(work, host, "repo"); got.Path != want {
		t.Errorf("resolveRepo(%q) = %q, want %q", "{hostname}/repo", got.Path, want)
	}
	// The unexpanded form is kept beside the expanded one, because that is what a command
	// re-expands when it needs the location as of a particular time.
	if got.Raw != "{hostname}/repo" {
		t.Errorf("resolveRepo kept raw=%q, want the location as the user wrote it", got.Raw)
	}

	// An absolute path is left where it is.
	if got, err := e.resolveRepo("/backups/here"); err != nil || got.Path != "/backups/here" {
		t.Errorf("resolveRepo(\"/backups/here\") = %v, %v", got, err)
	}
	// And no repository at all is still an error rather than the working directory.
	if _, err := e.resolveRepo(""); err == nil {
		t.Error("resolveRepo(\"\") returned a path; an unset repository must be an error")
	}
}
