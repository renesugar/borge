// SPDX-License-Identifier: Apache-2.0

package cli

import (
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
