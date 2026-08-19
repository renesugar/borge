// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os/exec"
	"strings"
	"testing"
)

// Archiving a stream: a "-" among the paths, and --content-from-command.
//
// There is no file on disk, so every piece of metadata is either given on the command line
// or invented - which makes this the place where a port and its original drift silently.
// The whole item is compared against borg's, not just the content.
func TestStdinContentMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	const itemFormat = "{path}|{mode}|{user}|{group}|{uid}|{gid}|{size}|{type}{NL}"
	describe := func(name string, borge bool) string {
		t.Helper()
		if borge {
			stdout, stderr, code := r.borge(t, "list", "--format", itemFormat, name)
			if code != ExitOK {
				t.Fatalf("borge list exited %d\n%s", code, stderr)
			}
			return stdout
		}
		return r.mustRun("list", "-r", r.path, name, "--format", itemFormat)
	}

	t.Run("defaults", func(t *testing.T) {
		r.mustRunStdin("hello stream\n", "create", "-r", r.path, "borg-d", "-")
		if _, stderr, code := r.borgeStdin(t, "hello stream\n", "create", "borge-d", "-"); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		want, got := describe("borg-d", false), describe("borge-d", true)
		if !strings.HasPrefix(want, "stdin|-rw-rw----|") {
			t.Fatalf("borg's defaults are not what this test assumes: %q", want)
		}
		if got != want {
			t.Errorf("borge %q, borg %q", got, want)
		}
	})

	t.Run("named and owned", func(t *testing.T) {
		// The current user, because borg refuses a name it cannot resolve and a test that
		// invented one would only ever exercise the error path.
		me := currentUserName(t)
		args := []string{"--stdin-name", "db.sql", "--stdin-mode", "0644",
			"--stdin-user", me, "--stdin-group", me}
		r.mustRunStdin("data\n", append(append([]string{"create", "-r", r.path}, args...), "borg-n", "-")...)
		if _, stderr, code := r.borgeStdin(t, "data\n", append(append([]string{"create"}, args...), "borge-n", "-")...); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		want, got := describe("borg-n", false), describe("borge-n", true)
		if !strings.Contains(want, "db.sql|-rw-r--r--|"+me) {
			t.Fatalf("borg did not apply the options: %q", want)
		}
		if got != want {
			t.Errorf("borge %q, borg %q", got, want)
		}
	})

	t.Run("content from a command", func(t *testing.T) {
		args := []string{"--content-from-command", "--stdin-name", "out.txt"}
		r.mustRun(append(append([]string{"create", "-r", r.path}, args...), "borg-c", "--", "echo", "from-a-command")...)
		if _, stderr, code := r.borge(t, append(append([]string{"create"}, args...), "borge-c", "--", "echo", "from-a-command")...); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		want, got := describe("borg-c", false), describe("borge-c", true)
		if !strings.Contains(want, "out.txt") {
			t.Fatalf("borg did not use --stdin-name: %q", want)
		}
		if got != want {
			t.Errorf("borge %q, borg %q", got, want)
		}
	})

	// The timestamps are the invented part: borg's process_pipe sets all three to the
	// moment of the backup, whatever --atime and --noctime say, because those options are
	// about copying a file's inode and a pipe has none.
	t.Run("timestamps", func(t *testing.T) {
		r.mustRunStdin("x\n", "create", "-r", r.path, "--noctime", "borg-t", "-")
		if _, stderr, code := r.borgeStdin(t, "x\n", "create", "--noctime", "borge-t", "-"); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		want := timeKeysOf(t, r.mustRun("debug", "dump-archive", "-r", r.path, "borg-t", "-"))
		stdout, _, _ := r.borge(t, "debug", "dump-archive", "borge-t", "-")
		if got := timeKeysOf(t, stdout); got != want {
			t.Errorf("borge stored %s, borg stored %s", got, want)
		}
		if want != "atime,ctime,mtime" {
			t.Errorf("borg stored %s; --noctime is expected not to reach a pipe", want)
		}
	})
}

// TestStdinContentRefusesNonsense covers the cases that must not silently produce an
// archive holding the wrong thing.
func TestStdinContentRefusesNonsense(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	// borg refuses a user it cannot resolve rather than storing a name with no id behind
	// it; an archive whose ownership cannot be restored is worse than none.
	if _, stderr, code := r.borgeStdin(t, "x\n", "create", "--stdin-user", "definitely-no-such-user", "a", "-"); code != ExitError {
		t.Errorf("an unknown --stdin-user exited %d\n%s", code, stderr)
	}
	if _, _, code := r.borge(t, "create", "--content-from-command", "a"); code != ExitError {
		t.Error("--content-from-command with no command was accepted")
	}
	// A command that fails must fail the backup: a truncated dump stored as a complete
	// one is the worst outcome this feature has.
	if _, stderr, code := r.borge(t, "create", "--content-from-command", "a", "--", "sh", "-c", "echo half; exit 3"); code == ExitOK {
		t.Errorf("a failing command produced an archive anyway\n%s", stderr)
	}
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Errorf("a refused stream still created an archive: %v", names)
	}
}

func currentUserName(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Skipf("cannot determine the current user: %v", err)
	}
	return strings.TrimSpace(string(out))
}
