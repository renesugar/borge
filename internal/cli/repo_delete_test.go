// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo-delete is the one irreversible command in borge. Everything else - delete, prune,
// even check --repair - leaves something to recover from until a compaction runs. This
// removes the archives, the chunks and the keys together, so the tests are mostly about
// what it *refuses* to do.

// borgeStdin runs a borge command with something on standard input, for the confirmation.
func (r *borgRepo) borgeStdin(t *testing.T, input string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr strings.Builder
	e := &Env{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader(input),
		Getenv: r.borgeEnv(nil, nil).Getenv,
	}
	code := Run(e, args)
	return stdout.String(), stderr.String(), code
}

// TestRepoDeleteRefusesWithoutConfirmation.
func TestRepoDeleteRefusesWithoutConfirmation(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("one")

	for _, answer := range []string{"", "no\n", "yes\n", "Y\n", "YES please\n"} {
		stdout, _, code := r.borgeStdin(t, answer, "repo-delete", "-r", r.path)
		if code == ExitOK {
			t.Errorf("answering %q deleted the repository", answer)
		}
		if !strings.Contains(stdout, "nothing was deleted") {
			t.Errorf("answering %q did not say the repository was kept:\n%s", answer, stdout)
		}
		if _, err := os.Stat(filepath.Join(r.path, "config", "readme")); err != nil {
			t.Fatalf("answering %q destroyed the repository: %v", answer, err)
		}
	}

	// The summary has to say what is at stake before the prompt, or the prompt is
	// meaningless.
	stdout, _, _ := r.borgeStdin(t, "no\n", "repo-delete", "-r", r.path)
	for _, needle := range []string{"DELETE", "Repository ID:", "Location:", "Archives:"} {
		if !strings.Contains(stdout, needle) {
			t.Errorf("the confirmation does not mention %q:\n%s", needle, stdout)
		}
	}
}

// TestRepoDeleteRemovesTheRepository, with each of the three ways of confirming.
func TestRepoDeleteRemovesTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		stdin string
		args  []string
	}{
		{"YES on stdin", nil, "YES\n", nil},
		{"--force", nil, "", []string{"--force"}},
		{"environment override", map[string]string{"BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING": "YES"}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBorgRepo(t, "aes256-ocb")
			r.makeArchives("one")

			args := append([]string{"repo-delete", "-r", r.path}, tc.args...)
			var code int
			var stdout string
			if tc.env != nil {
				stdout, _, code = r.borgeWithEnv(t, tc.env, args...)
			} else {
				stdout, _, code = r.borgeStdin(t, tc.stdin, args...)
			}
			if code != ExitOK {
				t.Fatalf("repo-delete exited %d\n%s", code, stdout)
			}
			if !strings.Contains(stdout, "deleted") {
				t.Errorf("repo-delete did not say what it did:\n%s", stdout)
			}
			if _, err := os.Stat(r.path); !os.IsNotExist(err) {
				t.Errorf("the repository directory is still there: %v", err)
			}
			// borg has to agree it is gone, rather than finding a half-removed repository.
			if out, err := r.runErr("repo-info", "-r", r.path); err == nil {
				t.Errorf("borg still opens the deleted repository:\n%s", out)
			}
		})
	}
}

// TestRepoDeleteDryRunChangesNothing.
func TestRepoDeleteDryRunChangesNothing(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("one", "two")

	stdout, stderr, code := r.borge(t, "repo-delete", "-r", r.path, "--dry-run", "--force", "--list")
	if code != ExitOK {
		t.Fatalf("repo-delete --dry-run exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("the dry run does not say it was one:\n%s", stdout)
	}
	// --list has to name the archives, since seeing them is the point of running it.
	for _, name := range []string{"one", "two"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("--list does not mention the archive %q:\n%s", name, stdout)
		}
	}
	if out, err := r.runErr("repo-info", "-r", r.path); err != nil {
		t.Fatalf("the dry run damaged the repository: %v\n%s", err, out)
	}
	if names := borgArchiveNames(t, r); len(names) != 2 {
		t.Errorf("the dry run left %d archives, want 2: %v", len(names), names)
	}
}

// TestRepoDeleteRefusesANonRepository is the safety property that matters most.
//
// "borge repo-delete -r ~" must not be a way to lose a home directory to a mistyped path,
// so the target is opened as a repository first and nothing is removed if it is not one.
func TestRepoDeleteRefusesANonRepository(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	notARepo := t.TempDir()
	precious := filepath.Join(notARepo, "precious.txt")
	if err := os.WriteFile(precious, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := r.borgeWithEnv(t, map[string]string{"BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING": "YES"},
		"repo-delete", "-r", notARepo, "--force")
	if code != ExitError {
		t.Errorf("deleting a non-repository exited %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "repository") {
		t.Errorf("the error does not say the path is not a repository: %q", stderr)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("deleting a non-repository removed a file in it: %v", err)
	}
}

// TestRepoDeleteLeavesForeignFiles.
//
// A repository created inside a directory that also holds other things must not take them
// with it. borg's store.destroy() removes the directory outright; borge removes only the
// namespaces it owns and warns about what is left. See docs/DIVERGENCES.md #18.
func TestRepoDeleteLeavesForeignFiles(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	stranger := filepath.Join(r.path, "not-borges-file.txt")
	if err := os.WriteFile(stranger, []byte("somebody else's"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := r.borgeWithEnv(t, map[string]string{"BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING": "YES"},
		"repo-delete", "-r", r.path)
	if code != ExitWarning {
		t.Errorf("deleting a repository that shares its directory exited %d, want ExitWarning (%d)",
			code, ExitWarning)
	}
	if !strings.Contains(stdout, "deleted") {
		t.Errorf("the repository was not reported as deleted:\n%s", stdout)
	}
	if !strings.Contains(stderr, "not-borges-file.txt") {
		t.Errorf("the warning does not name what was left behind: %q", stderr)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("the foreign file was removed anyway: %v", err)
	}
	// The repository itself really is gone, though: this is a warning, not a refusal.
	for _, ns := range repoNamespaces {
		if _, err := os.Stat(filepath.Join(r.path, ns)); !os.IsNotExist(err) {
			t.Errorf("the %s namespace survived: %v", ns, err)
		}
	}
}

// TestRepoDeleteCacheOnly removes this client's cache and leaves the repository.
func TestRepoDeleteCacheOnly(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("BORGE_CACHE_DIR", cacheDir)
	r := newBorgRepo(t, "aes256-ocb")

	// A create fills the files cache, so there is something to delete.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := r.borgeWithEnv(t, map[string]string{"BORGE_CACHE_DIR": cacheDir},
		"create", "cached", src); code != ExitOK {
		t.Fatalf("create exited %d\n%s", code, stderr)
	}
	repoCache := filepath.Join(cacheDir, repoIDOf(t, r))
	if _, err := os.Stat(repoCache); err != nil {
		t.Fatalf("no cache was written, so this test would delete nothing: %v", err)
	}

	stdout, stderr, code := r.borgeWithEnv(t, map[string]string{"BORGE_CACHE_DIR": cacheDir},
		"repo-delete", "-r", r.path, "--cache-only")
	if code != ExitOK {
		t.Fatalf("repo-delete --cache-only exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "cache") {
		t.Errorf("--cache-only does not say what it deleted:\n%s", stdout)
	}
	if _, err := os.Stat(repoCache); !os.IsNotExist(err) {
		t.Errorf("the cache is still there: %v", err)
	}
	// And the repository is untouched, which is the whole distinction.
	if out, err := r.runErr("repo-info", "-r", r.path); err != nil {
		t.Fatalf("--cache-only damaged the repository: %v\n%s", err, out)
	}
}

// repoIDOf reads the repository id, which is what names its cache directory.
func repoIDOf(t *testing.T, r *borgRepo) string {
	t.Helper()
	id, err := os.ReadFile(filepath.Join(r.path, "config", "id"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(id))
}
