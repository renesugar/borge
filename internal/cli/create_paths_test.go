// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRelativeSourcePathsMatchBorg closes docs/DIVERGENCES.md #21.
//
// borge resolved every source path to an absolute one before storing it, so
// "borge create A tree" run in /srv/work stored "srv/work/tree/..." where borg stores
// "tree/...". Same command, same tree, a different archive - and the difference only
// becomes visible during a restore, which is the worst time to find out.
//
// The stage 7 interoperability matrix never saw it because every row in it passes absolute
// paths. That is the lesson worth keeping: a gate that only ever exercises one shape of
// input is a gate with a blind spot, and this test exists to be the other shape.
func TestRelativeSourcePathsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	// The working directory the relative paths are relative to. It holds no repository
	// and no cache, so archiving "." is a bounded thing to do.
	work := t.TempDir()
	for path, content := range map[string]string{
		"here/tree/notes.txt":    "notes",
		"here/tree/sub/deep.txt": "deep",
		"sibling/s.txt":          "sibling",
	} {
		p := filepath.Join(work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(work, "here"))

	cases := []struct {
		name string
		arg  string
		want []string
	}{
		{"relative", "tree", []string{"tree", "tree/notes.txt", "tree/sub", "tree/sub/deep.txt"}},
		{"dot-slash", "./tree", []string{"tree", "tree/notes.txt", "tree/sub", "tree/sub/deep.txt"}},
		{"trailing-slash", "tree/", []string{"tree", "tree/notes.txt", "tree/sub", "tree/sub/deep.txt"}},
		{"dotdot-in-the-middle", "tree/../tree", []string{"tree", "tree/notes.txt", "tree/sub", "tree/sub/deep.txt"}},
		{"the working directory itself", ".", []string{".", "tree", "tree/notes.txt", "tree/sub", "tree/sub/deep.txt"}},
		// borg drops a leading "../" rather than refusing it, so what comes back out is a
		// tree that can be placed anywhere instead of one that climbs out of it.
		{"above the working directory", "../sibling", []string{"sibling", "sibling/s.txt"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun("create", "-r", r.path, "borg-"+c.name, c.arg)
			if _, stderr, code := r.borge(t, "create", "borge-"+c.name, c.arg); code != ExitOK {
				t.Fatalf("borge create %q exited %d\n%s", c.arg, code, stderr)
			}

			// Read as JSON, not as lines of "list --short": a stored path may contain a
			// space or even a newline, and the interop corpus has both.
			borgPaths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "borg-"+c.name, "--json-lines"))
			stdout, stderr, code := r.borge(t, "list", "--json-lines", "borge-"+c.name)
			if code != ExitOK {
				t.Fatalf("borge list exited %d\n%s", code, stderr)
			}
			borgePaths := sortedItemPaths(t, stdout)

			// Sorted on both sides: the two tools store a directory's entries in
			// different orders by design (docs/DIVERGENCES.md #23), and this test is
			// about which paths were stored.
			if strings.Join(borgPaths, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("borg stored\n  %s\nthis test expected\n  %s\n"+
					"- if borg changed, this expectation is what has to move",
					strings.Join(borgPaths, "\n  "), strings.Join(c.want, "\n  "))
			}
			if strings.Join(borgePaths, "\n") != strings.Join(borgPaths, "\n") {
				t.Errorf("borge stored\n  %s\nborg stored\n  %s",
					strings.Join(borgePaths, "\n  "), strings.Join(borgPaths, "\n  "))
			}
		})
	}
}

// TestEmptySourcePathIsRefused: an empty PATH would clean to "." and archive the working
// directory, which is never what an empty argument meant. borg rejects it before writing
// anything, with exit 2.
func TestEmptySourcePathIsRefused(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := r.borge(t, "create", "empty", "")
	if code != ExitError {
		t.Errorf("an empty path exited %d, want %d\n%s", code, ExitError, stderr)
	}
	// And it is refused even when a real path is given alongside it, rather than being
	// quietly dropped: the user typed something that does not mean anything.
	_, stderr, code = r.borge(t, "create", "empty2", src, "")
	if code != ExitError {
		t.Errorf("an empty path beside a real one exited %d, want %d\n%s", code, ExitError, stderr)
	}
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Errorf("a refused create left archives behind: %v", names)
	}
}

// sortedItemPaths is the set of stored paths in "list --json-lines" output.
func sortedItemPaths(t *testing.T, listing string) []string {
	t.Helper()
	var out []string
	for _, item := range parseJSONLines(t, listing) {
		p, _ := item["path"].(string)
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// TestSlashdotHackMatchesBorg closes docs/DIVERGENCES.md #24.
//
// A "/./" in a source path says where the stored path begins, the way rsync's does:
// "/srv/www/./site" reads from /srv/www/site and archives it as "site". borge cleaned the
// "." away and stored the whole path, so the same command produced a different archive
// layout in the two tools - visible, as with #21, only at restore.
func TestSlashdotHackMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	work := t.TempDir()
	for _, path := range []string{"a/b/c/d/f.txt", "a/b/g.txt"} {
		p := filepath.Join(work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)

	cases := []struct {
		name string
		arg  string
		want []string
	}{
		{"points at a subdirectory", "a/b/./c/d", []string{"c/d", "c/d/f.txt"}},
		{"points one level up", "a/b/./c", []string{"c", "c/d", "c/d/f.txt"}},
		{"nearer the root", "a/./b", []string{"b", "b/c", "b/c/d", "b/c/d/f.txt", "b/g.txt"}},
		// Only the first "/./" counts.
		{"twice", "a/./b/./c", []string{"b/c", "b/c/d", "b/c/d/f.txt"}},
		// A trailing "/./" points at the directory itself, which becomes ".".
		{"trailing", "a/b/./", []string{".", "c", "c/d", "c/d/f.txt", "g.txt"}},
		// A trailing "/." is not the hack at all: there is no "/./" in the string.
		{"trailing dot without a slash", "a/b/.", []string{"a/b", "a/b/c", "a/b/c/d", "a/b/c/d/f.txt", "a/b/g.txt"}},
		// The control: no hack, no change.
		{"no hack", "a/b/c", []string{"a/b/c", "a/b/c/d", "a/b/c/d/f.txt"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun("create", "-r", r.path, "borg-"+c.name, c.arg)
			if _, stderr, code := r.borge(t, "create", "borge-"+c.name, c.arg); code != ExitOK {
				t.Fatalf("borge create %q exited %d\n%s", c.arg, code, stderr)
			}
			borgPaths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "borg-"+c.name, "--json-lines"))
			stdout, stderr, code := r.borge(t, "list", "--json-lines", "borge-"+c.name)
			if code != ExitOK {
				t.Fatalf("borge list exited %d\n%s", code, stderr)
			}
			borgePaths := sortedItemPaths(t, stdout)

			if strings.Join(borgPaths, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("borg stored\n  %s\nthis test expected\n  %s",
					strings.Join(borgPaths, "\n  "), strings.Join(c.want, "\n  "))
			}
			if strings.Join(borgePaths, "\n") != strings.Join(borgPaths, "\n") {
				t.Errorf("borge stored\n  %s\nborg stored\n  %s",
					strings.Join(borgePaths, "\n  "), strings.Join(borgPaths, "\n  "))
			}
		})
	}
}

// TestPatternsMatchTheWalkedPathNotTheStoredOne is the half of the slashdot hack that is
// easy to get backwards.
//
// With a "/./" in the path, what is walked and what is stored are different strings. borg
// matches patterns against the walked one - an --exclude is written against the filesystem
// the user is looking at, not against the archive that does not exist yet - so an exclude
// naming the full path works and one naming the stored path does not.
func TestPatternsMatchTheWalkedPathNotTheStoredOne(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	work := t.TempDir()
	p := filepath.Join(work, "a", "b", "c", "d", "f.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	cases := []struct {
		name    string
		exclude string
		want    []string
	}{
		// The walked path: "a/b/c/d" relative to here. It excludes.
		{"the walked path", "pp:a/b/c/d", []string{"c"}},
		// The stored path: "d". It does not - and that is the assertion, because
		// matching on the stored path is the plausible wrong implementation.
		{"the stored path", "pp:d", []string{"c", "c/d", "c/d/f.txt"}},
		{"a suffix pattern, which reaches either way", "sh:**/f.txt", []string{"c", "c/d"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun("create", "-r", r.path, "--exclude", c.exclude, "borg-"+c.name, "a/b/./c")
			if _, stderr, code := r.borge(t, "create", "--exclude", c.exclude, "borge-"+c.name, "a/b/./c"); code != ExitOK {
				t.Fatalf("borge create exited %d\n%s", code, stderr)
			}
			borgPaths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "borg-"+c.name, "--json-lines"))
			stdout, _, _ := r.borge(t, "list", "--json-lines", "borge-"+c.name)
			borgePaths := sortedItemPaths(t, stdout)

			if strings.Join(borgPaths, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("borg stored %v, this test expected %v", borgPaths, c.want)
			}
			if strings.Join(borgePaths, "\n") != strings.Join(borgPaths, "\n") {
				t.Errorf("borge stored %v, borg stored %v", borgePaths, borgPaths)
			}
		})
	}
}
