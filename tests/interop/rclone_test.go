// SPDX-License-Identifier: Apache-2.0

//go:build linux

package interop

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rclone backend, both directions.
//
// The claim being tested is not "borge can drive rclone" - the unit tests cover that - but
// that a repository borge writes *through* rclone is the repository borg reads through
// rclone, and the other way round. Nothing about the format changes when the bytes travel
// through a different backend, and this is what says so out loud.
//
// A local rclone remote needs no service, no credentials and no network, which is why these
// rows can run everywhere rather than only where a cloud account exists.

func requireRcloneTool(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the rclone interop rows in short mode")
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skipf("rclone is not installed: %v", err)
	}
}

func TestRcloneBothDirections(t *testing.T) {
	requireRcloneTool(t)
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)

	for _, c := range []struct {
		name           string
		writer, reader string
	}{
		{"borg writes, borge reads", tl.borg, tl.borge},
		{"borge writes, borg reads", tl.borge, tl.borg},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "remote")
			url := "rclone:" + dir

			if _, err := tl.run(c.writer, "", "repo-create", "-r", url, "-e", "aes256-ocb"); err != nil {
				t.Fatalf("repo-create through rclone: %v", err)
			}
			if _, err := tl.run(c.writer, "", "create", "-r", url, "one", src); err != nil {
				t.Fatalf("create through rclone: %v", err)
			}

			// The other tool, through the same rclone remote.
			out, err := tl.run(c.reader, "", "repo-list", "-r", url, "--format", "{archive}{NL}")
			if err != nil {
				t.Fatalf("repo-list through rclone: %v", err)
			}
			if strings.TrimSpace(out) != "one" {
				t.Errorf("the other tool lists %q through rclone", strings.TrimSpace(out))
			}

			// And the whole archive, extracted by the reader and compared against the
			// tree that went in.
			dest := t.TempDir()
			if c.reader == tl.borg {
				_, err = tl.run(c.reader, dest, "extract", "-r", url, "one")
			} else {
				_, err = tl.run(c.reader, "", "extract", "-r", url, "-C", dest, "one")
			}
			if err != nil {
				t.Fatalf("extract through rclone: %v", err)
			}
			checkTrees(t, src, filepath.Join(dest,
				filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)

			// The same repository, opened as a plain directory: an rclone remote that is
			// a local path must produce exactly the layout a local repository has, or the
			// two backends are writing two different formats.
			//
			// borg warns first. It records the location a repository was last opened
			// under, and "rclone:/x" and "/x" are two spellings with one canonical form
			// each, so to borg the repository has *moved* - the same warning it gives for
			// a repository that really was moved, answered here by the environment
			// variable the harness sets. borge keeps no such record (#53), so it says
			// nothing. The archive name is the last line either way.
			out, err = tl.run(tl.borg, "", "repo-list", "-r", dir, "--format", "{archive}{NL}")
			if err != nil {
				t.Fatalf("borg could not read the rclone-written repository as a directory: %v", err)
			}
			if lastLine(out) != "one" {
				t.Errorf("read as a directory, the repository lists %q", lastLine(out))
			}
		})
	}
}

// lastLine is the final non-empty line of some output, which is how a result is read out
// from under a warning.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
