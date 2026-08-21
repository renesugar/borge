// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// recreate's exclusion group, and the positional argument that meant two different things.
//
// The options are compared against borg decision by decision. The positional is the reason
// this file is longer than four options deserve: borg's recreate takes "[PATH ...]" and no
// archive name, borge read the first positional as an archive name, and the same command
// line therefore did opposite things in the two tools. See DIVERGENCES.md #54.

const cacheTagSignatureLine = "Signature: 8a477f597d28d172789f06886806bc55\n"

// taggedTree holds a real cache directory, a decoy that only looks like one, a directory
// marked with a plain file, and something outside all of them.
func taggedTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, dir := range []string{"cache", "keep", "marked"} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cache/CACHEDIR.TAG", cacheTagSignatureLine+"this directory is a cache\n")
	write("cache/data.bin", "cached")
	// The decoy: named CACHEDIR.TAG, without the signature. Excluding it would mean the
	// content was never read.
	write("keep/CACHEDIR.TAG", "not a real tag\n")
	write("keep/data.bin", "keep me")
	write("marked/.nobackup", "")
	write("marked/data.bin", "marked")
	write("top.txt", "top")
	return src
}

// recreateLines keeps the per-item decision lines, which is what the options decide.
// borge's dry run also prints a one-line summary that borg has no equivalent of; see #54.
func recreateLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 2 && line[1] == ' ' && strings.IndexByte("Adsfbc?+-", line[0]) >= 0 {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

func archivePathsOf(t *testing.T, r *borgRepo, name string) []string {
	t.Helper()
	out, _ := borgStreams(t, r, "list", "-r", r.path, name, "--format", "{path}{NL}")
	var paths []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			paths = append(paths, l)
		}
	}
	sort.Strings(paths)
	return paths
}

// TestRecreateExclusionGroupMatchesBorg: the four options, decision by decision.
func TestRecreateExclusionGroupMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := taggedTree(t)

	cases := []struct {
		name string
		args []string
	}{
		{"exclude-caches", []string{"--exclude-caches"}},
		{"exclude-if-present", []string{"--exclude-if-present", ".nobackup"}},
		{"keep-exclude-tags", []string{"--exclude-caches", "--keep-exclude-tags"}},
		{"both", []string{"--exclude-caches", "--exclude-if-present", ".nobackup"}},
		{"both keeping tags", []string{"--exclude-caches", "--exclude-if-present", ".nobackup",
			"--keep-exclude-tags"}},
		{"filter A", []string{"--exclude-caches", "--filter", "A"}},
		{"filter d", []string{"--exclude-caches", "--filter", "d"}},
		{"paths", []string{"keep"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			borgName, borgeName := "b-"+c.name, "e-"+c.name
			r.mustRun("create", "-r", r.path, borgName, src)
			r.mustRun("create", "-r", r.path, borgeName, src)

			borgArgs := append([]string{"recreate", "--list", "-a", borgName, "-r", r.path}, c.args...)
			borgeArgs := append([]string{"recreate", "--list", "-a", borgeName, "-r", r.path}, c.args...)
			_, wantErr := borgStreams(t, r, borgArgs...)
			_, gotErr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge recreate exited %d\n%s", code, gotErr)
			}

			want, got := recreateLines(wantErr), recreateLines(gotErr)
			if len(want) == 0 {
				t.Fatalf("borg decided about nothing; the case is vacuous:\n%s", wantErr)
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("decisions differ\n got:\n%s\nwant:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}

			// And what actually survived, which is what the user cares about.
			wantPaths := archivePathsOf(t, r, borgName)
			gotPaths := archivePathsOf(t, r, borgeName)
			if strings.Join(gotPaths, "\n") != strings.Join(wantPaths, "\n") {
				t.Errorf("survivors differ\n got:  %v\nwant: %v", gotPaths, wantPaths)
			}
		})
	}
}

// TestRecreateExcludesTheRealCacheOnly: the signature is read from the archive, not guessed
// from the name.
//
// This is the assertion the differential comparison above cannot make on its own: two tools
// that both excluded every CACHEDIR.TAG directory would agree with each other and be wrong.
func TestRecreateExcludesTheRealCacheOnly(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := taggedTree(t)
	r.mustRun("create", "-r", r.path, "tagged", src)

	if _, stderr, code := r.borge(t, "recreate", "-a", "tagged", "-r", r.path, "--exclude-caches"); code != ExitOK {
		t.Fatalf("borge recreate exited %d\n%s", code, stderr)
	}
	paths := strings.Join(archivePathsOf(t, r, "tagged"), "\n")

	if strings.Contains(paths, "/cache/") || strings.HasSuffix(paths, "/cache") {
		t.Errorf("the real cache directory survived --exclude-caches:\n%s", paths)
	}
	// The decoy has the name and not the signature, so its content had to be read.
	if !strings.Contains(paths, "/keep/data.bin") {
		t.Errorf("a directory whose CACHEDIR.TAG lacks the signature was excluded anyway:\n%s", paths)
	}
	// And a directory marked with a different file is untouched by --exclude-caches.
	if !strings.Contains(paths, "/marked/data.bin") {
		t.Errorf("--exclude-caches excluded a directory marked with .nobackup:\n%s", paths)
	}
}

// TestRecreateKeepExcludeTagsKeepsTheTag: the marker survives so the restored tree can be
// excluded the same way again.
func TestRecreateKeepExcludeTagsKeepsTheTag(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := taggedTree(t)
	r.mustRun("create", "-r", r.path, "kept", src)

	if _, stderr, code := r.borge(t, "recreate", "-a", "kept", "-r", r.path,
		"--exclude-caches", "--keep-exclude-tags"); code != ExitOK {
		t.Fatalf("borge recreate exited %d\n%s", code, stderr)
	}
	paths := strings.Join(archivePathsOf(t, r, "kept"), "\n")

	if !strings.Contains(paths, "cache/CACHEDIR.TAG") {
		t.Errorf("--keep-exclude-tags dropped the tag file itself:\n%s", paths)
	}
	if strings.Contains(paths, "cache/data.bin") {
		t.Errorf("--keep-exclude-tags kept the cache's contents:\n%s", paths)
	}
}

// TestRecreatePositionalsArePaths is the dangerous one.
//
// borg's recreate takes "[PATH ...]" and selects archives with -a alone. borge read the
// first positional as an archive name, so "borge recreate ARCHIVE" kept the whole archive
// while "borg recreate ARCHIVE" recreated EVERY archive in the repository keeping only
// paths matching "ARCHIVE" - which empties all of them.
func TestRecreatePositionalsArePaths(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := taggedTree(t)
	r.mustRun("create", "-r", r.path, "one", src)
	r.mustRun("create", "-r", r.path, "two", src)

	// The stored paths are the source tree's, with the leading slash removed, so a
	// positional has to name the stored form: a bare "keep" is a path-prefix pattern and
	// matches nothing. Taken from the archive rather than built from the temp directory's
	// name, so the test cannot disagree with what was actually stored.
	keepPath := strings.TrimPrefix(src, "/") + "/keep"
	if paths := archivePathsOf(t, r, "one"); len(paths) == 0 || !strings.Contains(strings.Join(paths, "\n"), keepPath) {
		t.Fatalf("the archive does not hold %q, so the positional would match nothing:\n%v", keepPath, paths)
	}

	// A positional naming a path keeps that subtree and nothing else, in both tools.
	r.mustRun("recreate", "-a", "one", "-r", r.path, keepPath)
	if _, stderr, code := r.borge(t, "recreate", "-a", "two", "-r", r.path, keepPath); code != ExitOK {
		t.Fatalf("borge recreate exited %d\n%s", code, stderr)
	}
	want := archivePathsOf(t, r, "one")
	got := archivePathsOf(t, r, "two")
	if len(want) == 0 {
		t.Fatal("borg kept nothing at all; the comparison would pass on anything")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("a positional path\n got:  %v\nwant: %v", got, want)
	}
	for _, p := range got {
		if !strings.Contains(p, "/keep") {
			t.Errorf("%q survived a recreate restricted to the keep subtree", p)
		}
	}

	// And the archive is NOT selected by a positional: an archive name there is a path
	// that matches nothing, which is why borge must not treat it as a selector.
	r.mustRun("create", "-r", r.path, "three", src)
	if _, stderr, code := r.borge(t, "recreate", "-a", "three", "-r", r.path, "three"); code != ExitOK {
		t.Fatalf("borge recreate exited %d\n%s", code, stderr)
	}
	if paths := archivePathsOf(t, r, "three"); len(paths) != 0 {
		t.Errorf("a positional matching no path left %v; borg empties the archive here", paths)
	}
}
