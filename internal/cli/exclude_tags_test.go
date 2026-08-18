// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Tag-based directory exclusion: --exclude-caches, --exclude-if-present and
// --keep-exclude-tags.
//
// The three belong together because the third only means anything with one of the first
// two, and because the interesting behaviour is what is *not* stored: without
// --keep-exclude-tags a tagged directory contributes nothing at all, not even its own
// entry. A test that only checked the excluded file was gone would pass on an
// implementation that still stored the directory.
func TestExcludeTagsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A real cache directory, by the CACHEDIR.TAG specification.
	write("cachedir/CACHEDIR.TAG", "Signature: 8a477f597d28d172789f06886806bc55\nfree text\n")
	write("cachedir/junk.bin", "junk")
	// A file of the right name and the wrong contents. It must NOT be excluded, and it is
	// the row that separates "checks the signature" from "checks the name".
	write("fakecache/CACHEDIR.TAG", "not the signature\n")
	write("fakecache/keep.bin", "keep")
	// A directory carrying a user-chosen marker.
	write("marked/.nobackup", "")
	write("marked/x.bin", "data")
	write("normal/ok.txt", "keep")

	borgPaths := func(archive string) []string {
		return relPaths(t, src, sortedItemPaths(t, r.mustRun("list", "-r", r.path, archive, "--json-lines")))
	}
	borgePaths := func(archive string) []string {
		stdout, stderr, code := r.borge(t, "list", "--json-lines", archive)
		if code != ExitOK {
			t.Fatalf("borge list exited %d\n%s", code, stderr)
		}
		return relPaths(t, src, sortedItemPaths(t, stdout))
	}

	cases := []struct {
		name  string
		flags []string
		// want is what must survive, relative to the source root, sorted.
		want []string
	}{
		{"no options", nil, []string{
			".", "cachedir", "cachedir/CACHEDIR.TAG", "cachedir/junk.bin",
			"fakecache", "fakecache/CACHEDIR.TAG", "fakecache/keep.bin",
			"marked", "marked/.nobackup", "marked/x.bin", "normal", "normal/ok.txt"}},
		// The tagged directory contributes nothing, not even its entry. The one with the
		// wrong signature is untouched.
		{"exclude-caches", []string{"--exclude-caches"}, []string{
			".", "fakecache", "fakecache/CACHEDIR.TAG", "fakecache/keep.bin",
			"marked", "marked/.nobackup", "marked/x.bin", "normal", "normal/ok.txt"}},
		// With --keep-exclude-tags the directory and the tag file come back, and only
		// those: junk.bin stays out.
		{"exclude-caches and keep", []string{"--exclude-caches", "--keep-exclude-tags"}, []string{
			".", "cachedir", "cachedir/CACHEDIR.TAG",
			"fakecache", "fakecache/CACHEDIR.TAG", "fakecache/keep.bin",
			"marked", "marked/.nobackup", "marked/x.bin", "normal", "normal/ok.txt"}},
		{"exclude-if-present", []string{"--exclude-if-present", ".nobackup"}, []string{
			".", "cachedir", "cachedir/CACHEDIR.TAG", "cachedir/junk.bin",
			"fakecache", "fakecache/CACHEDIR.TAG", "fakecache/keep.bin",
			"normal", "normal/ok.txt"}},
		{"exclude-if-present and keep", []string{"--exclude-if-present", ".nobackup", "--keep-exclude-tags"}, []string{
			".", "cachedir", "cachedir/CACHEDIR.TAG", "cachedir/junk.bin",
			"fakecache", "fakecache/CACHEDIR.TAG", "fakecache/keep.bin",
			"marked", "marked/.nobackup", "normal", "normal/ok.txt"}},
		{"both", []string{"--exclude-caches", "--exclude-if-present", ".nobackup"}, []string{
			".", "fakecache", "fakecache/CACHEDIR.TAG", "fakecache/keep.bin",
			"normal", "normal/ok.txt"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun(append(append([]string{"create", "-r", r.path}, c.flags...), "borg-"+c.name, src)...)
			if _, stderr, code := r.borge(t, append(append([]string{"create"}, c.flags...), "borge-"+c.name, src)...); code != ExitOK {
				t.Fatalf("borge create %v exited %d\n%s", c.flags, code, stderr)
			}
			got, want := borgePaths("borge-"+c.name), borgPaths("borg-"+c.name)

			if strings.Join(want, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("borg stored\n  %s\nthis test expected\n  %s\n"+
					"- if borg changed, the expectation is what has to move",
					strings.Join(want, "\n  "), strings.Join(c.want, "\n  "))
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("borge stored\n  %s\nborg stored\n  %s",
					strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// relPaths makes an archive listing readable by cutting the scratch root off the front.
func relPaths(t *testing.T, root string, stored []string) []string {
	t.Helper()
	prefix := strings.TrimPrefix(filepath.ToSlash(root), "/")
	var out []string
	for _, p := range stored {
		rel := strings.TrimPrefix(strings.TrimPrefix(p, prefix), "/")
		if rel == "" {
			rel = "."
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}
