// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
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
		{"rclone:remote:path/repo", "not implemented yet"},
		{"rest://host/srv/repo", "not implemented yet"},
		{"https://backup.example.com/repo", "not implemented yet"},
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
