// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/renesugar/borge/internal/version"
)

// sleepBriefly is the poll interval for waiting on another process to take a lock.
func sleepBriefly() { time.Sleep(10 * time.Millisecond) }

// TestBreakLockOnAnUnlockedRepository says so rather than pretending it broke something.
func TestBreakLockOnAnUnlockedRepository(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	// break-lock reports on stderr, where borg puts everything it says about the work;
	// stdout is for a command's data and break-lock has none (DIVERGENCES.md #46).
	_, stderr, code := r.borge(t, "break-lock")
	if code != ExitOK {
		t.Fatalf("break-lock exited %d on an unlocked repository\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "no locks") {
		t.Errorf("break-lock did not report that there was nothing to break:\n%s", stderr)
	}
}

// TestBreakLockRemovesALiveLockAndWarns.
//
// The lock is written by borg, not by borge, so this also checks that borge reads borg's
// lock records - the failure mode being that borge sees no locks, reports "nothing to
// break", and leaves the repository stuck.
func TestBreakLockRemovesALiveLockAndWarns(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	// borg's with-lock holds the lock for as long as the command runs. A command that
	// waits for a file to appear gives a lock that is live and under this test's control.
	if runtime.GOOS == "windows" {
		t.Skip("the helper command is a shell script")
	}
	release := filepath.Join(t.TempDir(), "release")
	script := "while [ ! -e " + release + " ]; do sleep 0.05; done"

	done := make(chan string, 1)
	go func() {
		out, err := r.runErr("with-lock", "-r", r.path, "sh", "-c", script)
		if err != nil {
			done <- "borg with-lock failed: " + err.Error() + "\n" + out
			return
		}
		done <- ""
	}()
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		if msg := <-done; msg != "" {
			t.Log(msg) // the helper is torn down after the assertions; a failure here is not the point
		}
	}()

	// Wait for the lock to actually exist, rather than assuming borg got there first.
	locks := filepath.Join(r.path, "locks")
	found := false
	for i := 0; i < 400 && !found; i++ {
		entries, err := os.ReadDir(locks)
		if err == nil && len(entries) > 0 {
			found = true
			break
		}
		sleepBriefly()
	}
	if !found {
		t.Skip("borg did not take a lock in time; nothing to break")
	}

	// break-lock reports on stderr, where borg puts everything it says about the work;
	// stdout is for a command's data and break-lock has none (DIVERGENCES.md #46).
	_, stderr, code := r.borge(t, "break-lock")
	if code != ExitWarning {
		t.Errorf("break-lock on a live lock exited %d, want ExitWarning (%d)\n%s%s",
			code, ExitWarning, "", stderr)
	}
	if !strings.Contains(stderr, "live") {
		t.Errorf("break-lock did not warn that the lock was live:\n%s", stderr)
	}
	if !strings.Contains(stderr, "lock(s) broken") {
		t.Errorf("break-lock did not report what it broke:\n%s", stderr)
	}

	entries, err := os.ReadDir(locks)
	if err == nil && len(entries) > 0 {
		t.Errorf("the lock is still there after break-lock: %v", entries)
	}
}

// TestWithLockRunsTheCommandAndPassesItsExitCode.
//
// Both halves matter. If the exit code were swallowed, "borge with-lock rsync ... &&
// echo done" would print done after a failed copy.
func TestWithLockRunsTheCommandAndPassesItsExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper command is a shell script")
	}
	r := newBorgRepo(t, "aes256-ocb")

	marker := filepath.Join(t.TempDir(), "ran")
	stdout, stderr, code := r.borge(t, "with-lock", "sh", "-c", "touch "+marker+"; echo hello")
	if code != ExitOK {
		t.Fatalf("with-lock exited %d for a command that succeeded\n%s", code, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the command did not run: %v", err)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("the command's output did not reach stdout:\n%s", stdout)
	}

	_, _, code = r.borge(t, "with-lock", "sh", "-c", "exit 7")
	if code != 7 {
		t.Errorf("with-lock returned %d for a command that exited 7", code)
	}
}

// TestWithLockActuallyHoldsTheLock: the command runs with a lock present, which is the
// only reason the wrapper exists.
func TestWithLockActuallyHoldsTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper command is a shell script")
	}
	r := newBorgRepo(t, "aes256-ocb")

	out := filepath.Join(t.TempDir(), "locks-seen")
	// ls exits non-zero if the directory is empty in some shells, so count instead.
	script := "ls -1 " + filepath.Join(r.path, "locks") + " | wc -l > " + out
	if _, stderr, code := r.borge(t, "with-lock", "sh", "-c", script); code != ExitOK {
		t.Fatalf("with-lock exited %d\n%s", code, stderr)
	}
	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(seen)) == "0" {
		t.Error("no lock was present while the command ran, so with-lock locked nothing")
	}
}

// TestWithLockReportsAMissingCommand rather than reporting success for a command that
// never ran.
func TestWithLockReportsAMissingCommand(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	_, stderr, code := r.borge(t, "with-lock", "no-such-command-exists-here")
	if code == ExitOK {
		t.Fatal("with-lock reported success for a command that does not exist")
	}
	if !strings.Contains(stderr, "no-such-command-exists-here") {
		t.Errorf("the error does not name the command: %q", stderr)
	}
}

// TestVersionPrintsClientAndServer checks the shape borg uses, and the JSON form.
func TestVersionPrintsClientAndServer(t *testing.T) {
	var stdout, stderr strings.Builder
	e := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(e, []string{"version"}); code != ExitOK {
		t.Fatalf("version exited %d\n%s", code, stderr.String())
	}
	// "<client> / <server>", as borg prints it: scripts parse this shape.
	line := strings.TrimSpace(stdout.String())
	before, after, ok := strings.Cut(line, " / ")
	if !ok {
		t.Fatalf("version did not print '<client> / <server>': %q", line)
	}
	if before == "" || after == "" {
		t.Errorf("version printed an empty half: %q", line)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(e, []string{"version", "-json"}); code != ExitOK {
		t.Fatalf("version -json exited %d\n%s", code, stderr.String())
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &v); err != nil {
		t.Fatalf("version -json does not parse: %v\n%s", err, stdout.String())
	}
	if v["client"] == "" || v["server"] == "" {
		t.Errorf("version -json has an empty version: %+v", v)
	}
	// Two keys and no more: --json is borg's API and borg sends exactly these, so an
	// extra field here is one a frontend iterating the object would not expect
	// (DIVERGENCES.md #42).
	if len(v) != 2 {
		t.Errorf("version -json sent %d keys, want client and server only: %+v", len(v), v)
	}

	// The borg pin still has to be reported somewhere: it is what says which repositories
	// this build was actually tested against. It moved to "version --long", which is
	// borge's own output rather than borg's document.
	stdout.Reset()
	stderr.Reset()
	if code := Run(e, []string{"version", "-long"}); code != ExitOK {
		t.Fatalf("version -long exited %d\n%s", code, stderr.String())
	}
	long := stdout.String()
	if !strings.Contains(long, version.BorgUpstreamCommit[:12]) {
		t.Errorf("version -long does not name the borg commit it was built against:\n%s", long)
	}
	if !strings.Contains(long, "version 4") {
		t.Errorf("version -long does not name the repository format version:\n%s", long)
	}
}

// TestWithLockGivesTheChildAnOpenableRepository: the command runs with BORG_REPO and
// BORGE_REPO set to a location it can actually open.
//
// borg sets neither - the child simply inherits the environment - so this is borge's
// addition, and the value has to be the absolute path rather than the "-r sub/repo" the
// user typed: the child is a command like rsync or a shell script, and it is allowed to
// change directory. It also must not be the canonical form, which for a remote location has
// had its credentials removed (DIVERGENCES #56).
func TestWithLockGivesTheChildAnOpenableRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper command is a shell script")
	}
	r := newBorgRepo(t, "none-sha256")
	t.Chdir(filepath.Dir(r.path))

	seen := filepath.Join(t.TempDir(), "seen")
	script := `cd / && printf '%s\n%s\n' "$BORGE_REPO" "$BORG_REPO" > ` + seen
	if _, stderr, code := r.borge(t, "with-lock", "-r", filepath.Base(r.path),
		"sh", "-c", script); code != ExitOK {
		t.Fatalf("with-lock exited %d\n%s", code, stderr)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		if line != r.path {
			t.Errorf("the child was given %q; from another directory that names no repository, "+
				"the absolute %q does", line, r.path)
		}
	}
}
