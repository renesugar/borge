// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPositionalPatternStylesMatchBorg: a positional PATH may carry a style prefix, and
// borge has to select the same items borg does for each one.
//
// This exists because borge got it wrong in a way nothing would have reported. Positional
// paths were parsed as path prefixes unconditionally, so "sh:**/*.txt" was read as a
// literal path beginning with the characters "sh:" - which matches nothing. Not an error,
// not a warning: an empty result from list, extract, diff and export-tar alike, which
// looks exactly like a correct answer of "no such file".
func TestPositionalPatternStylesMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one")

	lines := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		sort.Strings(out)
		return out
	}

	// One pattern per style borg documents, plus a bare path and a deliberate non-match.
	for _, pattern := range positionalPatternStyles(r) {
		borgOut, err := r.runErr("list", "-r", r.path, "--short", "one", pattern)
		if err != nil {
			t.Fatalf("borg list %q failed: %v\n%s", pattern, err, borgOut)
		}
		borgeOut, stderr, code := r.borge(t, "list", "--short", "one", pattern)
		if code != ExitOK {
			t.Fatalf("borge list %q exited %d\n%s", pattern, code, stderr)
		}

		want, got := lines(borgOut), lines(borgeOut)
		if len(want) != len(got) {
			t.Errorf("%q: borg matched %d item(s), borge %d\n  borg:  %v\n  borge: %v",
				pattern, len(want), len(got), want, got)
			continue
		}
		for i := range want {
			if want[i] != got[i] {
				t.Errorf("%q: item %d differs\n  borg:  %s\n  borge: %s", pattern, i, want[i], got[i])
			}
		}
	}
}

// positionalPatternStyles builds one pattern per style borg documents, over the tree
// makeArchives archived.
//
// The path-anchored styles are derived from that tree rather than written out, because
// borg archives an absolute source path with the leading slash removed: the archived paths
// begin with wherever the test's temporary directory happens to be. Hard-coding a prefix
// made these patterns match whenever TMPDIR pointed at one particular disk and match
// nothing everywhere else - and "nothing" is not a failure here, since borg finds nothing
// too and the comparison passes. It was the emptiness guard below that caught it.
func positionalPatternStyles(r *borgRepo) []string {
	archived := strings.TrimPrefix(r.src, "/")
	root := archived
	if i := strings.Index(archived, "/"); i >= 0 {
		root = archived[:i]
	}
	return []string{
		"sh:**/file1.txt",               // shell, ** spanning directories
		"sh:**/*.txt",                   // shell, several matches
		"fm:*file1.txt",                 // fnmatch
		"re:file[12]\\.txt",             // regex
		"pp:" + root,                    // path prefix, explicit
		"pf:" + archived + "/file1.txt", // full path, exactly one item
		root,                            // bare path, the prefix fallback
		"sh:**/nothing-here",            // a deliberate non-match
	}
}

// TestPositionalPatternStylesAreNotAllEmpty guards the test above.
//
// Every assertion in it is "borge agrees with borg", and two empty lists agree. If the
// styles stopped matching in *both* tools the comparison would still pass, so at least one
// pattern of each shape is required to select something.
func TestPositionalPatternStylesAreNotAllEmpty(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one")

	archived := strings.TrimPrefix(r.src, "/")
	root := archived
	if i := strings.Index(archived, "/"); i >= 0 {
		root = archived[:i]
	}
	for _, pattern := range []string{
		"sh:**/file1.txt", "re:file1", "pp:" + root, "pf:" + archived + "/file1.txt", root,
	} {
		stdout, stderr, code := r.borge(t, "list", "--short", "one", pattern)
		if code != ExitOK {
			t.Fatalf("borge list %q exited %d\n%s", pattern, code, stderr)
		}
		if strings.TrimSpace(stdout) == "" {
			t.Errorf("%q matched nothing, so the comparison against borg proves nothing", pattern)
		}
	}
}

// TestPatternOptionOrderIsKept is docs/DIVERGENCES.md #26, at the level where it is cheap
// to see: the four pattern options share one list, and that list is in the order the user
// typed, not grouped by option.
//
// borge kept a slice per option and walked them in a fixed order, so "--exclude X
// --pattern +X" and "--pattern +X --exclude X" produced the same matcher. They must not:
// the first matching pattern decides.
func TestPatternOptionOrderIsKept(t *testing.T) {
	parse := func(args ...string) []patternSpec {
		t.Helper()
		e := &Env{Stdout: nopWriter{}, Stderr: nopWriter{}}
		fs := newFlagSet(e, "probe")
		var pf patternFlags
		pf.register(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parsing %v: %v", args, err)
		}
		return pf.specs
	}

	got := parse("--exclude", "a", "--pattern", "+b", "--exclude-from", "c",
		"--patterns-from", "d", "--exclude", "e")
	want := []patternSpec{
		{specExclude, "a"},
		{specPattern, "+b"},
		{specExcludeFrom, "c"},
		{specPatternsFrom, "d"},
		{specExclude, "e"},
	}
	if len(got) != len(want) {
		t.Fatalf("collected %d specs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("spec %d is %+v, want %+v", i, got[i], want[i])
		}
	}

	// The two orders have to differ, or nothing above is doing any work.
	one := parse("--exclude", "x", "--pattern", "+x")
	two := parse("--pattern", "+x", "--exclude", "x")
	if one[0] == two[0] {
		t.Error("the two orders produced the same first spec; the order is not being kept")
	}

	// And an option written after the paths still lands in the right place, because
	// permutation keeps the options' relative order (args.go).
	if got := parse("--exclude", "a", "--", "somepath"); len(got) != 1 || got[0] != (patternSpec{specExclude, "a"}) {
		t.Errorf("an option before a positional gave %v", got)
	}
}

// TestPatternOptionOrderMatchesBorg is the same thing measured against borg, because the
// unit test above only proves borge kept the order, not that borge uses it as borg does.
func TestPatternOptionOrderMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for _, name := range []string{"keep.txt", "drop.txt"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		args []string
	}{
		// The exclude comes first, so it wins and keep.txt is left out.
		{"exclude first", []string{"--exclude", "sh:**/keep.txt", "--pattern=+sh:**/keep.txt"}},
		// The include comes first, so keep.txt stays.
		{"include first", []string{"--pattern=+sh:**/keep.txt", "--exclude", "sh:**/keep.txt"}},
	}

	results := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun(append(append([]string{"create", "-r", r.path}, c.args...), "borg-"+c.name, src)...)
			if _, stderr, code := r.borge(t, append(append([]string{"create"}, c.args...), "borge-"+c.name, src)...); code != ExitOK {
				t.Fatalf("borge create exited %d\n%s", code, stderr)
			}
			borgPaths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "borg-"+c.name, "--json-lines"))
			stdout, _, _ := r.borge(t, "list", "--json-lines", "borge-"+c.name)
			borgePaths := sortedItemPaths(t, stdout)
			if strings.Join(borgePaths, "\n") != strings.Join(borgPaths, "\n") {
				t.Errorf("borge stored %v, borg stored %v", borgePaths, borgPaths)
			}
			results[c.name] = strings.Join(borgPaths, "\n")
		})
	}

	// If the two orders gave the same archive, both subtests above would pass while
	// proving nothing about order at all.
	if results["exclude first"] == results["include first"] {
		t.Errorf("both orders produced %q; this test cannot detect an ordering bug",
			results["exclude first"])
	}
}

// TestPatternRootsAreBackedUp closes docs/DIVERGENCES.md #25: an "R PATH" line in a
// patterns file is a path to back up. borge parsed them and threw them away, so a patterns
// file whose only root was an R line made borge refuse a command borg runs.
func TestPatternRootsAreBackedUp(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	base := t.TempDir()
	write := func(rel, content string) string {
		p := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("tree/keep.txt", "keep")
	write("tree/sub/deep.txt", "deep")
	write("other/o.txt", "other")
	tree := filepath.Join(base, "tree")
	other := filepath.Join(base, "other")

	oneRoot := write("pf1.txt", "R "+tree+"\n- sh:**/sub\n")
	twoRoots := write("pf2.txt", "R "+tree+"\nR "+other+"\n")

	cases := []struct {
		name string
		args []string
	}{
		{"a root and a rule, and no path on the command line", []string{"--patterns-from", oneRoot}},
		{"two roots", []string{"--patterns-from", twoRoots}},
		{"a root given as --pattern", []string{"--pattern=R " + other}},
		{"a root beside a command-line path", []string{"--pattern=R " + other}},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{}, c.args...)
			var tail []string
			if i == 3 {
				tail = []string{tree}
			}

			borgArgs := append(append([]string{"create", "-r", r.path}, args...), "borg-"+c.name)
			r.mustRun(append(borgArgs, tail...)...)
			borgeArgs := append(append([]string{"create"}, args...), "borge-"+c.name)
			if _, stderr, code := r.borge(t, append(borgeArgs, tail...)...); code != ExitOK {
				t.Fatalf("borge create exited %d\n%s", code, stderr)
			}

			borgPaths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "borg-"+c.name, "--json-lines"))
			stdout, _, _ := r.borge(t, "list", "--json-lines", "borge-"+c.name)
			borgePaths := sortedItemPaths(t, stdout)

			if len(borgPaths) == 0 {
				t.Fatal("borg archived nothing; the comparison would be vacuous")
			}
			if strings.Join(borgePaths, "\n") != strings.Join(borgPaths, "\n") {
				t.Errorf("borge stored\n  %s\nborg stored\n  %s",
					strings.Join(borgePaths, "\n  "), strings.Join(borgPaths, "\n  "))
			}
		})
	}
}

// TestCreateStillNeedsAPath: accepting roots must not turn "create NAME" with no paths at
// all into a command that archives nothing and reports success.
func TestCreateStillNeedsAPath(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	_, stderr, code := r.borge(t, "create", "empty")
	if code != ExitError {
		t.Errorf("\"create NAME\" with no paths exited %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "path") {
		t.Errorf("the message does not mention the missing path: %s", stderr)
	}
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Errorf("a refused create left archives behind: %v", names)
	}
}
