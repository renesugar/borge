// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --paths-from-stdin, --paths-from-command, --paths-from-shell-command and
// --paths-delimiter.
//
// These are not just another way to name paths. borg's promise is "all control is
// external: it will back up all files given - no more, no less", and two consequences fall
// out of it that a port can easily miss: a directory in the list is archived *without its
// contents*, and the include/exclude patterns are not applied at all. Both are measured
// against borg below rather than taken from the sentence.
func TestPathsFromMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for _, rel := range []string{"f.txt", "h.txt", "sub/g.txt"} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// rel cuts the scratch root off the front. The root itself is stored without a
	// trailing slash, so it needs its own case - it became a full path in the expectation
	// until that showed up.
	rel := func(paths []string) []string {
		root := strings.TrimPrefix(filepath.ToSlash(src), "/")
		var out []string
		for _, p := range paths {
			switch {
			case p == root:
				out = append(out, ".")
			default:
				out = append(out, strings.TrimPrefix(p, root+"/"))
			}
		}
		sort.Strings(out)
		return out
	}
	borgPaths := func(name string) []string {
		return rel(sortedItemPaths(t, r.mustRun("list", "-r", r.path, name, "--json-lines")))
	}
	borgePaths := func(name string) []string {
		stdout, stderr, code := r.borge(t, "list", "--json-lines", name)
		if code != ExitOK {
			t.Fatalf("borge list exited %d\n%s", code, stderr)
		}
		return rel(sortedItemPaths(t, stdout))
	}
	compare := func(t *testing.T, borgName, borgeName string, want []string) {
		t.Helper()
		got, ref := borgePaths(borgeName), borgPaths(borgName)
		if strings.Join(ref, ",") != strings.Join(want, ",") {
			t.Errorf("borg archived %v, this test expected %v", ref, want)
		}
		if strings.Join(got, ",") != strings.Join(ref, ",") {
			t.Errorf("borge archived %v, borg archived %v", got, ref)
		}
	}

	// A directory and a file. The directory contributes its entry and nothing under it:
	// "no more, no less".
	list := filepath.Join(src) + "\n" + filepath.Join(src, "f.txt") + "\n"
	t.Run("stdin", func(t *testing.T) {
		r.mustRunStdin(list, "create", "-r", r.path, "--paths-from-stdin", "borg-stdin")
		if _, stderr, code := r.borgeStdin(t, list, "create", "--paths-from-stdin", "borge-stdin"); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		compare(t, "borg-stdin", "borge-stdin", []string{".", "f.txt"})
	})

	// The exclusion is ignored, which is the half that looks like a bug until you read
	// borg's sentence.
	t.Run("patterns do not apply", func(t *testing.T) {
		two := filepath.Join(src, "f.txt") + "\n" + filepath.Join(src, "h.txt") + "\n"
		r.mustRunStdin(two, "create", "-r", r.path, "--paths-from-stdin", "--exclude", "sh:**/h.txt", "borg-excl")
		if _, stderr, code := r.borgeStdin(t, two, "create", "--paths-from-stdin", "--exclude", "sh:**/h.txt", "borge-excl"); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		compare(t, "borg-excl", "borge-excl", []string{"f.txt", "h.txt"})
	})

	// A NUL delimiter, which is what find -print0 produces and the only safe separator
	// for paths that may contain a newline.
	t.Run("nul delimiter", func(t *testing.T) {
		nul := filepath.Join(src, "f.txt") + "\x00" + filepath.Join(src, "h.txt") + "\x00"
		r.mustRunStdin(nul, "create", "-r", r.path, "--paths-from-stdin", "--paths-delimiter", `\0`, "borg-nul")
		if _, stderr, code := r.borgeStdin(t, nul, "create", "--paths-from-stdin", "--paths-delimiter", `\0`, "borge-nul"); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		compare(t, "borg-nul", "borge-nul", []string{"f.txt", "h.txt"})
	})

	// The command forms. "--" is needed in both tools, because the command's own options
	// would otherwise be read as the backup program's.
	t.Run("from a command", func(t *testing.T) {
		r.mustRun("create", "-r", r.path, "--paths-from-command", "borg-cmd", "--", "find", src, "-name", "*.txt")
		if _, stderr, code := r.borge(t, "create", "--paths-from-command", "borge-cmd", "--", "find", src, "-name", "*.txt"); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		compare(t, "borg-cmd", "borge-cmd", []string{"f.txt", "h.txt", "sub/g.txt"})
	})
	t.Run("from a shell command", func(t *testing.T) {
		script := "echo " + filepath.Join(src, "f.txt")
		r.mustRun("create", "-r", r.path, "--paths-from-shell-command", "borg-sh", "--", script)
		if _, stderr, code := r.borge(t, "create", "--paths-from-shell-command", "borge-sh", "--", script); code != ExitOK {
			t.Fatalf("borge exited %d\n%s", code, stderr)
		}
		compare(t, "borg-sh", "borge-sh", []string{"f.txt"})
	})
}

// mustRunStdin runs borg with something on standard input.
func (r *borgRepo) mustRunStdin(input string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("borg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestPathsFromRefusesNonsense: the combinations borg rejects, and the two it accepts
// silently while doing nothing - where borge says so instead.
func TestPathsFromRefusesNonsense(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A PATH beside --paths-from-stdin: borg errors, and so does borge.
	if _, stderr, code := r.borgeStdin(t, "x\n", "create", "--paths-from-stdin", "a", src); code != ExitError {
		t.Errorf("a PATH beside --paths-from-stdin exited %d\n%s", code, stderr)
	}
	// A command form with no command.
	if _, stderr, code := r.borge(t, "create", "--paths-from-command", "a"); code != ExitError {
		t.Errorf("--paths-from-command with no command exited %d\n%s", code, stderr)
	}
	// Two sources at once.
	if _, _, code := r.borgeStdin(t, "x\n", "create", "--paths-from-stdin", "--paths-from-command", "a", "--", "true"); code != ExitError {
		t.Errorf("two path sources exited %d", code)
	}
	// An empty list archives nothing, so it is refused rather than producing an empty
	// archive that looks like a successful backup.
	if _, stderr, code := r.borgeStdin(t, "", "create", "--paths-from-stdin", "a"); code != ExitError {
		t.Errorf("an empty list exited %d\n%s", code, stderr)
	}

	// borg accepts these two silently and ignores them. borge says so, on stderr, and
	// carries on - see PORTING_PLAN.md §2.3.
	_, stderr, code := r.borge(t, "create", "--paths-delimiter", `\0`, "warn1", src)
	if code != ExitOK {
		t.Fatalf("--paths-delimiter without a source exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "--paths-delimiter does nothing") {
		t.Errorf("no warning for a delimiter with no source: %q", stderr)
	}
	_, stderr, code = r.borgeStdin(t, filepath.Join(src, "f.txt")+"\n",
		"create", "--paths-from-stdin", "--exclude", "sh:**/nothing", "warn2")
	if code != ExitOK {
		t.Fatalf("--exclude with a path list exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "do not apply") {
		t.Errorf("no warning that the patterns are ignored: %q", stderr)
	}
}
