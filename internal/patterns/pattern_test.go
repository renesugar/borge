// SPDX-License-Identifier: Apache-2.0

package patterns

import "testing"

// TestStylesCoverTheParser checks the documented style list against the parser in both
// directions.
//
// A list written for the documentation is a second place the styles live, so it is
// checked against the first: every documented prefix has to parse, and every prefix the
// parser accepts has to be documented. Without the second direction a new style would be
// implemented, accepted, and invisible to every user.
func TestStylesCoverTheParser(t *testing.T) {
	documented := map[string]bool{}
	for _, style := range Styles() {
		documented[style.Prefix] = true
		if style.Description == "" {
			t.Errorf("style %q has no description", style.Prefix)
		}
		if _, err := ParsePattern(style.Prefix+":some/path", StyleFnmatch, true); err != nil {
			t.Errorf("the documented style %q does not parse: %v", style.Prefix, err)
		}
	}
	if len(documented) != len(Styles()) {
		t.Fatalf("Styles() repeats a prefix: %v", Styles())
	}

	// The reverse: no undocumented two-letter prefix may be read as a style. Enumerating
	// the alphabet is crude, and it is the only way to ask the parser what it accepts
	// rather than comparing one hand-written list against another.
	//
	// An unknown prefix is an error today, which is what makes the loop quiet. The
	// assertion is the case where it is not: a style implemented and left undocumented
	// parses, and a parsed style has its prefix stripped, so String() no longer matches
	// what was written. That is the difference this looks for.
	for a := 'a'; a <= 'z'; a++ {
		for b := 'a'; b <= 'z'; b++ {
			prefix := string([]rune{a, b})
			if documented[prefix] {
				continue
			}
			p, err := ParsePattern(prefix+":some/path", StyleFnmatch, true)
			if err != nil {
				continue
			}
			// A pattern with an undocumented prefix must have been read as a literal
			// path in the fallback style, not as a style of its own.
			if p.String() != prefix+":some/path" {
				t.Errorf("the parser accepts the undocumented style %q", prefix)
			}
		}
	}
}
