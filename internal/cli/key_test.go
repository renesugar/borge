// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The key commands are tested against borg in both directions throughout, because a key
// is the one thing in a repository that cannot be regenerated. A borge-written key that
// borg cannot read, or a borge import that quietly stores the wrong bytes, is data loss
// with no recovery - the repository is intact and unreadable.
//
// The library underneath was already gated at docs/PORTING_PLAN.md §1.3. What was never
// tested until now is the command: §1.3's gate text says "borge key export / borg key
// import cross-check in both directions", and that was true of a library test while
// `borge key` did not exist at all.

// keyEnv runs a borge command with an extra environment variable, for the new-passphrase
// variable the changing commands need.
func (r *borgRepo) keyEnv(t *testing.T, newPassphrase string, args ...string) (string, string, int) {
	t.Helper()
	return r.borgeWithEnv(t, map[string]string{"BORGE_NEW_PASSPHRASE": newPassphrase}, args...)
}

// TestKeyListMatchesBorg. The columns are borg's, so the output is compared as text.
func TestKeyListMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	want := r.mustRun("key", "list", "-r", r.path)
	if !strings.Contains(want, "KEY ID") || !strings.Contains(want, "admin") {
		t.Fatalf("borg's key list does not look like a listing:\n%s", want)
	}

	got, stderr, code := r.borge(t, "key", "list")
	if code != ExitOK {
		t.Fatalf("borge key list exited %d\n%s", code, stderr)
	}
	if got != want {
		t.Errorf("key list differs\n  borg:\n%s\n  borge:\n%s", want, got)
	}
	// The asterisk marks the key the passphrase opens, and getting that wrong would make
	// the listing useless for the one question it answers.
	if !strings.HasPrefix(strings.TrimLeft(got, "\n"), "  KEY ID") {
		t.Errorf("the header is not where borg puts it:\n%s", got)
	}
	if !strings.Contains(got, "* ") {
		t.Errorf("no key is marked as the current one:\n%s", got)
	}
}

// TestKeyExportMatchesBorg covers all three formats.
//
// The plain export and the QR HTML page have to be byte-identical: they are read back by
// the other tool. The paper key's *data* lines have to be identical for the same reason;
// its instruction line deliberately differs, and that difference is checked rather than
// ignored.
func TestKeyExportMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	out := t.TempDir()

	for _, tc := range []struct {
		name  string
		flags []string
		exact bool
	}{
		{"plain", nil, true},
		{"paper", []string{"--paper"}, false},
		{"qr-html", []string{"--qr-html"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantPath := filepath.Join(out, tc.name+".borg")
			gotPath := filepath.Join(out, tc.name+".borge")

			r.mustRun(append([]string{"key", "export", "-r", r.path}, append(tc.flags, wantPath)...)...)
			if _, stderr, code := r.borge(t, append([]string{"key", "export"},
				append(tc.flags, gotPath)...)...); code != ExitOK {
				t.Fatalf("borge key export %v exited %d\n%s", tc.flags, code, stderr)
			}

			want := readFileString(t, wantPath)
			got := readFileString(t, gotPath)
			if len(want) == 0 {
				t.Fatalf("borg's %s export is empty", tc.name)
			}
			if tc.exact {
				if got != want {
					t.Errorf("the %s export differs from borg's:\n%s", tc.name, firstDifference(want, got))
				}
				return
			}

			// The paper key: every line that carries key material must match. Only the
			// human instruction at the top may differ.
			wantData := paperDataLines(want)
			gotData := paperDataLines(got)
			if len(wantData) < 10 {
				t.Fatalf("borg's paper key has only %d data lines; this compares nothing", len(wantData))
			}
			if strings.Join(gotData, "\n") != strings.Join(wantData, "\n") {
				t.Errorf("the paper key data differs from borg's:\n%s",
					firstDifference(strings.Join(wantData, "\n"), strings.Join(gotData, "\n")))
			}
			if !strings.Contains(got, "borge key import --paper") {
				t.Errorf("borge's paper key does not say how to restore it with borge:\n%s", got)
			}
		})
	}
}

// paperDataLines keeps the lines that carry key material: the magic line, the id line and
// the numbered data lines. The prose at the top is what may differ.
func paperDataLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "BORG PAPER KEY") || strings.HasPrefix(trimmed, "id:") {
			out = append(out, trimmed)
			continue
		}
		// A data line is "<n>: <hex groups> - <checksum>".
		if i := strings.Index(trimmed, ":"); i > 0 && strings.Contains(trimmed, " - ") {
			if _, err := strconv.Atoi(strings.TrimSpace(trimmed[:i])); err == nil {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

// TestKeyImportRoundTripsWithBorg, in both directions.
//
// The failure this guards against is the worst one available: a repository whose key has
// been "restored" into a form the other tool cannot use. It would look like success until
// the day it is needed.
func TestKeyImportRoundTripsWithBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	work := t.TempDir()

	// borge exports; borg imports; borg still opens the repository.
	exported := filepath.Join(work, "by-borge.key")
	if _, stderr, code := r.borge(t, "key", "export", exported); code != ExitOK {
		t.Fatalf("borge key export exited %d\n%s", code, stderr)
	}
	r.mustRun("key", "import", "-r", r.path, exported)
	if out, err := r.runErr("repo-list", "-r", r.path); err != nil {
		t.Fatalf("borg cannot open the repository after importing borge's export: %v\n%s", err, out)
	}

	// borg exports; borge imports; borge still opens the repository.
	byBorg := filepath.Join(work, "by-borg.key")
	r.mustRun("key", "export", "-r", r.path, byBorg)
	if _, stderr, code := r.borge(t, "key", "import", byBorg); code != ExitOK {
		t.Fatalf("borge key import exited %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borge(t, "repo-list"); code != ExitOK {
		t.Fatalf("borge cannot open the repository after importing borg's export: %s", stderr)
	}

	// A key for a different repository is refused rather than installed. Importing the
	// wrong key would make the repository unopenable by anyone.
	other := newBorgRepo(t, "aes256-ocb")
	otherKey := filepath.Join(work, "other.key")
	other.mustRun("key", "export", "-r", other.path, otherKey)
	_, stderr, code := r.borge(t, "key", "import", otherKey)
	if code != ExitError {
		t.Errorf("importing another repository's key exited %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "not for repository") {
		t.Errorf("the error does not say the key belongs elsewhere: %q", stderr)
	}
}

// TestKeyPaperRoundTrip: the paper key is the last resort, so it is tested as one - the
// keyfile is taken away and the repository recovered from the printout alone.
func TestKeyPaperRoundTrip(t *testing.T) {
	// A keyfile repository, so that removing the keyfile really does lock it. A repokey
	// repository would keep working with its key in place and prove nothing.
	r := newKeyfileRepo(t)
	work := t.TempDir()
	paper := filepath.Join(work, "paper.txt")

	if _, stderr, code := r.borge(t, "key", "export", "--paper", paper); code != ExitOK {
		t.Fatalf("borge key export --paper exited %d\n%s", code, stderr)
	}

	// Take the keyfile away.
	away := filepath.Join(work, "keys-away")
	if err := os.Rename(r.keysDir, away); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := r.runErr("repo-list", "-r", r.path); err == nil {
		t.Fatalf("the repository is still readable with its keyfile gone:\n%s", out)
	}

	if _, stderr, code := r.borge(t, "key", "import", "--paper", paper); code != ExitOK {
		t.Fatalf("borge key import --paper exited %d\n%s", code, stderr)
	}
	if out, err := r.runErr("repo-list", "-r", r.path); err != nil {
		t.Fatalf("borg cannot open the repository restored from a paper key: %v\n%s", err, out)
	}
}

// newKeyfileRepo is newBorgRepo with the key kept outside the repository.
func newKeyfileRepo(t *testing.T) *borgRepo {
	t.Helper()
	r := newBorgRepo(t, "aes256-ocb")
	// repo-create already made a repokey repository; move the key out so the keyfile path
	// is what is under test.
	if _, stderr, code := r.borge(t, "key", "change-location", "keyfile"); code != ExitOK {
		t.Fatalf("borge key change-location keyfile exited %d\n%s", code, stderr)
	}
	return r
}

// TestKeyChangePassphrase: borg has to accept the new passphrase and refuse the old one.
func TestKeyChangePassphrase(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	if _, stderr, code := r.keyEnv(t, "a new secret", "key", "change-passphrase"); code != ExitOK {
		t.Fatalf("borge key change-passphrase exited %d\n%s", code, stderr)
	}

	if out, err := r.runErrEnv([]string{"BORG_PASSPHRASE=" + r.passphrase},
		"repo-list", "-r", r.path); err == nil {
		t.Errorf("the old passphrase still opens the repository:\n%s", out)
	}
	if out, err := r.runErrEnv([]string{"BORG_PASSPHRASE=a new secret"},
		"repo-list", "-r", r.path); err != nil {
		t.Errorf("borg does not accept the new passphrase: %v\n%s", err, out)
	}
}

// TestKeyChangePassphraseNeedsANewOne.
//
// borge does not prompt, so an unset variable has to be an error. Proceeding with an empty
// passphrase would leave the repository unprotected while reporting success, which is the
// one outcome worse than refusing.
func TestKeyChangePassphraseNeedsANewOne(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	_, stderr, code := r.borge(t, "key", "change-passphrase")
	if code != ExitError {
		t.Errorf("change-passphrase without a new passphrase exited %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "BORGE_NEW_PASSPHRASE") {
		t.Errorf("the error does not say what to set: %q", stderr)
	}
	// And the old passphrase must still work, since nothing should have happened.
	if _, _, code := r.borge(t, "repo-list"); code != ExitOK {
		t.Error("the repository stopped opening after a refused passphrase change")
	}
}

// TestKeyAddAndRemove.
func TestKeyAddAndRemove(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	if _, stderr, code := r.keyEnv(t, "second secret", "key", "add", "--label", "shared"); code != ExitOK {
		t.Fatalf("borge key add exited %d\n%s", code, stderr)
	}

	// borg sees it, and the second passphrase opens the repository.
	listing := r.mustRun("key", "list", "-r", r.path)
	if !strings.Contains(listing, "shared") {
		t.Errorf("borg does not see the key borge added:\n%s", listing)
	}
	if out, err := r.runErrEnv([]string{"BORG_PASSPHRASE=second secret"},
		"repo-list", "-r", r.path); err != nil {
		t.Errorf("the added passphrase does not open the repository: %v\n%s", err, out)
	}

	// A label may not be reused: two keys with one name cannot both be selected.
	if _, stderr, code := r.keyEnv(t, "third", "key", "add", "--label", "shared"); code != ExitError {
		t.Errorf("adding a second key labelled 'shared' exited %d, want ExitError\n%s", code, stderr)
	}

	// Remove it again; borg agrees it is gone and the passphrase stops working.
	if _, stderr, code := r.borge(t, "key", "remove", "--label", "shared"); code != ExitOK {
		t.Fatalf("borge key remove exited %d\n%s", code, stderr)
	}
	if listing := r.mustRun("key", "list", "-r", r.path); strings.Contains(listing, "shared") {
		t.Errorf("borg still sees the removed key:\n%s", listing)
	}
	if out, err := r.runErrEnv([]string{"BORG_PASSPHRASE=second secret"},
		"repo-list", "-r", r.path); err == nil {
		t.Errorf("the removed passphrase still opens the repository:\n%s", out)
	}

	// The last key cannot be removed: that would make the repository unreadable with no
	// way back, which is not something a command should do on request.
	_, stderr, code := r.borge(t, "key", "remove", "--label", "admin")
	if code != ExitError {
		t.Errorf("removing the only key exited %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "only key") && !strings.Contains(stderr, "protected") {
		t.Errorf("the refusal does not explain itself: %q", stderr)
	}
}

// TestKeyChangeLocation moves the key both ways and checks borg after each.
func TestKeyChangeLocation(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	keyfilesIn := func(dir string) int {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	if n := keyfilesIn(r.keysDir); n != 0 {
		t.Fatalf("the repository starts with %d keyfile(s); it should be a repokey repository", n)
	}

	if _, stderr, code := r.borge(t, "key", "change-location", "keyfile"); code != ExitOK {
		t.Fatalf("change-location keyfile exited %d\n%s", code, stderr)
	}
	if n := keyfilesIn(r.keysDir); n != 1 {
		t.Errorf("after moving to keyfile there are %d keyfiles, want 1", n)
	}
	if out, err := r.runErr("repo-list", "-r", r.path); err != nil {
		t.Fatalf("borg cannot open the repository from the keyfile: %v\n%s", err, out)
	}
	if listing := r.mustRun("key", "list", "-r", r.path); !strings.Contains(listing, "keyfile") {
		t.Errorf("borg does not report the key as a keyfile:\n%s", listing)
	}

	// And back again.
	if _, stderr, code := r.borge(t, "key", "change-location", "repokey"); code != ExitOK {
		t.Fatalf("change-location repokey exited %d\n%s", code, stderr)
	}
	if n := keyfilesIn(r.keysDir); n != 0 {
		t.Errorf("after moving back to repokey there are still %d keyfiles", n)
	}
	if out, err := r.runErr("repo-list", "-r", r.path); err != nil {
		t.Fatalf("borg cannot open the repository from the repokey: %v\n%s", err, out)
	}

	// Asking for where it already is does nothing and says so.
	stdout, _, code := r.borge(t, "key", "change-location", "repokey")
	if code != ExitOK {
		t.Errorf("a no-op change-location exited %d, want ExitOK", code)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("a no-op change-location does not say so:\n%s", stdout)
	}
}

// TestKeyUsage.
func TestKeyUsage(t *testing.T) {
	var out, errOut strings.Builder
	e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}

	if code := Run(e, []string{"key"}); code != ExitOK {
		t.Errorf("bare 'key' exited %d, want ExitOK", code)
	}
	for _, name := range []string{"export", "import", "change-passphrase", "add", "remove", "list"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("the key usage does not list %q:\n%s", name, out.String())
		}
	}

	errOut.Reset()
	if code := Run(e, []string{"key", "no-such-thing"}); code != ExitError {
		t.Errorf("an unknown key command exited %d, want ExitError", code)
	}
}

// runErrEnv is runErr with extra environment variables, for the tests that need borg to
// try a different passphrase than the repository's default.
func (r *borgRepo) runErrEnv(extra []string, args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = append(r.env(), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
