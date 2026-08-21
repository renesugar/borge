// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What -r accepts, and what it must refuse.
//
// borge resolved every repository with filepath.Abs until 2026-08-20, which means it did
// not reject a URL - it *joined* one to the working directory. "borge repo-create -r
// sftp://backup.example.com/srv/repo" made a local directory called "sftp:" and said
// "Repository created", so a user who pointed borge at their remote repository would have
// been backing up to a directory on the machine they were backing up. Measured before the
// fix, not imagined; see DIVERGENCES.md #56.

// TestRemoteLocationsAreRefusedNotJoined: a location borge cannot reach must fail, and must
// leave nothing behind.
func TestRemoteLocationsAreRefusedNotJoined(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")

	for _, c := range []struct {
		url  string
		says string
	}{
		{"sftp://backup.example.com/srv/repo", "not implemented yet"},
		{"s3:key:secret@http://localhost:4566/bucket/repo", "not implemented yet"},
		{"rest://host/srv/repo", "not implemented yet"},
		{"https://backup.example.com/repo", "not implemented yet"},
		// rclone is implemented, so this one gets as far as rclone, which does not know
		// the remote. What is being checked here is the same thing either way: the URL
		// was not quietly turned into a directory name.
		{"rclone:no-such-remote:path/repo", "rclone"},
		// ssh:// is not "later": it is borg 1.x, which is a §0.6 non-goal, and the
		// message has to say so rather than suggest waiting for a release.
		{"ssh://backup.example.com/srv/repo", "borg 1.x"},
	} {
		t.Run(c.url, func(t *testing.T) {
			work := t.TempDir()
			t.Chdir(work)

			_, stderr, code := r.borge(t, "repo-create", "-r", c.url, "-e", "none-sha256")
			if code == ExitOK {
				t.Fatalf("borge created a repository at %q", c.url)
			}
			if !strings.Contains(stderr, c.says) {
				t.Errorf("the refusal of %q does not say %q:\n%s", c.url, c.says, stderr)
			}
			// The point of the test: nothing was created under the working directory.
			entries, err := os.ReadDir(work)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("%q left %v in the working directory; the URL was taken for a path",
					c.url, names)
			}
		})
	}
}

// TestFileURLNamesTheSameRepository: "file://" plus an absolute path is the same repository
// as the path, in both tools, and both report it by the same name.
func TestFileURLNamesTheSameRepository(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	url := "file://" + r.path

	borg := parseKeyValues(r.mustRun("repo-info", "-r", url))
	stdout, stderr, code := r.borge(t, "repo-info", "-r", url)
	if code != ExitOK {
		t.Fatalf("borge repo-info through a file:// URL exited %d\n%s", code, stderr)
	}
	borge := parseKeyValues(stdout)

	// borg reports the canonical path, not the URL: two spellings, one repository.
	if borg["Location"] != r.path {
		t.Fatalf("borg reports Location %q for %q; this test's premise is wrong",
			borg["Location"], url)
	}
	if borge["Location"] != borg["Location"] {
		t.Errorf("through %q borg reports Location %q and borge %q",
			url, borg["Location"], borge["Location"])
	}
	if borge["Repository ID"] != borg["Repository ID"] {
		t.Errorf("the file:// URL opened a different repository: borg id %q, borge id %q",
			borg["Repository ID"], borge["Repository ID"])
	}

	// A file:// URL with a relative path is not a location at all - in either tool.
	if _, stderr, code := r.borge(t, "repo-info", "-r", "file://relative/repo"); code == ExitOK {
		t.Errorf("borge accepted a file:// URL with a relative path:\n%s", stderr)
	}
	if _, err := r.runErr("repo-info", "-r", "file://relative/repo"); err == nil {
		t.Error("borg accepted a file:// URL with a relative path; this test asserts it does not")
	}
}

// TestRelativeLocationsStillWork guards the change rather than the feature: the location
// type replaced a filepath.Abs that every command depended on, and a relative -r has to
// keep resolving against the working directory (DIVERGENCES #22).
func TestRelativeLocationsStillWork(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	work := filepath.Dir(r.path)
	t.Chdir(work)

	stdout, stderr, code := r.borge(t, "repo-info", "-r", filepath.Base(r.path))
	if code != ExitOK {
		t.Fatalf("borge could not open a relative repository path: %d\n%s", code, stderr)
	}
	if got := parseKeyValues(stdout)["Location"]; got != r.path {
		t.Errorf("a relative path resolved to %q, want the absolute %q", got, r.path)
	}
}

// requireRclone skips when rclone is not installed. The rclone tests that matter live in
// internal/store; this one is here because what it checks is a decision the command layer
// makes - which way to destroy a repository.
func requireRclone(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the rclone command tests in short mode")
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skipf("rclone is not installed: %v", err)
	}
}

// TestRepoDeleteThroughABackendDeletesTheRepository, and specifically not the working
// directory.
//
// Location.Path is empty for every location that is not a directory, and repo-delete used
// to hand it to a function that removes borge's namespaces from a path. Once rclone became
// openable that meant "borge repo-delete -r rclone:..." removed ./archives and ./packs from
// wherever it happened to be run - taking files that were not borge's with them - reported
// an error, and left the actual repository intact. Measured before the fix; DIVERGENCES #58.
func TestRepoDeleteThroughABackendDeletesTheRepository(t *testing.T) {
	requireRclone(t)
	r := newBorgRepo(t, "none-sha256")

	remote := filepath.Join(t.TempDir(), "remote")
	url := "rclone:" + remote
	if _, stderr, code := r.borge(t, "repo-create", "-r", url, "-e", "none-sha256"); code != ExitOK {
		t.Fatalf("repo-create through rclone exited %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borge(t, "create", "-r", url, "one", r.path); code != ExitOK {
		t.Fatalf("create through rclone exited %d\n%s", code, stderr)
	}

	// Decoys with the names of borge's own namespaces, in the directory the command runs
	// from. Nothing here belongs to the repository being deleted.
	work := t.TempDir()
	t.Chdir(work)
	for _, dir := range []string{"archives", "packs", "config"} {
		if err := os.MkdirAll(filepath.Join(work, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, dir, "mine.txt"), []byte("not borge's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, stderr, code := r.borge(t, "repo-delete", "-r", url, "--force"); code != ExitOK {
		t.Fatalf("repo-delete through rclone exited %d\n%s", code, stderr)
	}

	for _, dir := range []string{"archives", "packs", "config"} {
		if _, err := os.Stat(filepath.Join(work, dir, "mine.txt")); err != nil {
			t.Errorf("repo-delete removed %s from the working directory: %v", dir, err)
		}
	}
	// And the repository it was asked to delete is gone.
	if entries, err := os.ReadDir(remote); err == nil && len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the repository is still there after repo-delete: %v", names)
	}
}
