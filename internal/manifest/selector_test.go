// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/patterns"
)

// matchPrefix finds the prefixes applyMatch dispatches on. The reverse direction has to be
// asked of the code: a selector implemented and left out of Selectors would work and be
// undiscoverable, which is how "borge help" comes to describe a smaller tool than the one
// that shipped.
var matchPrefix = regexp.MustCompile(`strings\.HasPrefix\(match, "([a-z]+:)"\)`)

// TestSelectorsCoverApplyMatch checks the documented selectors against the matcher in both
// directions.
func TestSelectorsCoverApplyMatch(t *testing.T) {
	documented := map[string]bool{}
	for _, s := range Selectors() {
		if s.Syntax == "" || s.Description == "" {
			t.Errorf("selector %+v is missing its syntax or description", s)
		}
		if s.Prefix != "" && !strings.HasPrefix(s.Syntax, s.Prefix) {
			t.Errorf("selector %q does not begin with its own prefix %q", s.Syntax, s.Prefix)
		}
		documented[s.Prefix] = true
	}
	if !documented[""] {
		t.Error("no selector documents a bare archive name, which is the common case")
	}

	src, err := os.ReadFile("archives.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, m := range matchPrefix.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}
	if len(found) < 4 {
		t.Fatalf("the source scan found %d selector prefixes, too few to be right: %v", len(found), found)
	}
	for prefix := range found {
		if !documented[prefix] {
			t.Errorf("applyMatch accepts %q and Selectors() does not document it", prefix)
		}
	}
	// The name-pattern styles are the other half of the answer: applyMatch's default
	// branch hands what is left to patterns.CompileName, so "sh:", "re:" and "id:" are
	// accepted there rather than here. Both sources are asked, because a selector
	// documented against neither is a line describing something that does not work.
	//
	// This is how id: was found: it had been accepted since the archive filters were
	// written and appeared in no help topic.
	for _, style := range []string{patterns.StyleIdentical, patterns.StyleShell, patterns.StyleRegex} {
		found[style+":"] = true
	}
	for prefix := range documented {
		if prefix == "" || prefix == "name:" {
			// A bare name and its explicit spelling are the fallthrough case rather than
			// a prefix anything dispatches on.
			continue
		}
		if !found[prefix] {
			t.Errorf("Selectors() documents %q and nothing accepts it", prefix)
		}
	}

	// And every accepted prefix is documented, which is the direction that found id:.
	for prefix := range found {
		if !documented[prefix] {
			t.Errorf("archives can be selected with %q and Selectors() does not document it", prefix)
		}
	}
}
