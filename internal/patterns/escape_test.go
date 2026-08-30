// SPDX-License-Identifier: Apache-2.0

package patterns

import "testing"

// TestEscapedGroupCharactersAreLiterals is the behaviour R0 T9 chose, and the differential
// gate deliberately does not check it because borg disagrees (DIVERGENCES #63).
//
// A skipped case proves nothing on its own: the exception list in differential_test.go says
// borge differs, and this says how.
func TestEscapedGroupCharactersAreLiterals(t *testing.T) {
	for _, tc := range []struct {
		pat     string
		matches []string
		not     []string
	}{
		// The complaint R0.1 recorded: a filename with a parenthesis was unmatchable.
		{`a\(b`, []string{"a(b"}, []string{"ab", `a\(b`, "a)b"}},
		{`a\(b\)c`, []string{"a(b)c"}, []string{"abc", `a\b\c`}},
		{`\(`, []string{"("}, []string{"", `\(`}},
		{`a\|b`, []string{"a|b"}, []string{"a", "b", `a\b`}},
		{`song \(live\).mp3`, []string{"song (live).mp3"}, []string{"song live.mp3"}},

		// Backward compatibility. A backslash is a legal filename character on Linux, and
		// escaping only the three group characters keeps every other use of it unchanged.
		{`a\b`, []string{`a\b`}, []string{"ab"}},
		{`a\*b`, []string{`a\*b`}, []string{"axb"}},
		{`\\(`, []string{`\(`}, []string{"(", `\\(`}},

		// Unescaped, borg's passthrough is untouched: these are regex groups, which is
		// what makes {a,b} alternatives work after translateAlternatives.
		{`x{a,b}y`, []string{"xay", "xby"}, []string{"xy", "xaby"}},
	} {
		t.Run(tc.pat, func(t *testing.T) {
			re, err := Compile(tc.pat)
			if err != nil {
				t.Fatalf("compiling %q: %v", tc.pat, err)
			}
			for _, s := range tc.matches {
				if !re.MatchString(s) {
					t.Errorf("%q should match %q (regex %s)", tc.pat, s, re.String())
				}
			}
			for _, s := range tc.not {
				if re.MatchString(s) {
					t.Errorf("%q should not match %q (regex %s)", tc.pat, s, re.String())
				}
			}
		})
	}
}

// TestUnescapedParenthesisStillFailsLikeBorg pins what was *not* changed. An unescaped "("
// is passed through as regex syntax, so a lone one is still an unterminated group and still
// does not compile - in borge as in borg. Fixing that too would mean deciding what an
// unescaped parenthesis means, which would break the {a,b} alternatives that rely on the
// passthrough.
func TestUnescapedParenthesisStillFailsLikeBorg(t *testing.T) {
	if _, err := Compile("a(b"); err == nil {
		t.Error("a(b compiled; borg cannot compile it either and T9 did not change that")
	}
}
