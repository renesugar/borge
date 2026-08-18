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
