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
	for _, pattern := range []string{
		"sh:**/file1.txt",   // shell, ** spanning directories
		"sh:**/*.txt",       // shell, several matches
		"fm:*file1.txt",     // fnmatch
		"re:file[12]\\.txt", // regex
		"pp:media",          // path prefix, explicit
		"pf:media/renes",    // full path, which will not match
		"media",             // bare path, the prefix fallback
		"sh:**/nothing-here",
	} {
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

// TestPositionalPatternStylesAreNotAllEmpty guards the test above.
//
// Every assertion in it is "borge agrees with borg", and two empty lists agree. If the
// styles stopped matching in *both* tools the comparison would still pass, so at least one
// pattern is required to select something.
func TestPositionalPatternStylesAreNotAllEmpty(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one")

	for _, pattern := range []string{"sh:**/file1.txt", "re:file1", "pp:media"} {
		stdout, stderr, code := r.borge(t, "list", "--short", "one", pattern)
		if code != ExitOK {
			t.Fatalf("borge list %q exited %d\n%s", pattern, code, stderr)
		}
		if strings.TrimSpace(stdout) == "" {
			t.Errorf("%q matched nothing, so the comparison against borg proves nothing", pattern)
		}
	}
}
