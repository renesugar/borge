// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// parseSpecCase finds the names parseSpec switches on. Reading the source is how the
// reverse direction is asked of the parser rather than of a second list: a codec added to
// the switch and left out of SpecDocs would otherwise be accepted by borge and appear in
// no documentation at all.
var parseSpecCase = regexp.MustCompile(`case ("[a-z0-9]+"(?:, "[a-z0-9]+")*):`)

// TestSpecDocsCoverTheParser checks the documented specifications against the parser in
// both directions.
func TestSpecDocsCoverTheParser(t *testing.T) {
	documented := map[string]bool{}
	for _, doc := range SpecDocs() {
		if doc.Description == "" || doc.Syntax == "" {
			t.Errorf("specification %q is missing its syntax or description", doc.Name)
		}
		documented[doc.Name] = true

		// Every documented name has to parse. The optional parts are filled in with the
		// smallest legal value, since this checks the name rather than the levels.
		spec := doc.Name
		switch doc.Name {
		case "auto":
			spec = "auto,lz4"
		case "obfuscate":
			spec = "obfuscate,1,lz4"
		}
		if _, err := ParseSpec(spec); err != nil {
			t.Errorf("the documented specification %q does not parse: %v", spec, err)
		}
	}

	src, err := os.ReadFile("spec.go")
	if err != nil {
		t.Fatal(err)
	}
	// parseSpec's switch is the first one in the file that lists codec names; the
	// helpers below it switch on the same names, so a set union is what is wanted.
	found := map[string]bool{}
	for _, m := range parseSpecCase.FindAllStringSubmatch(string(src), -1) {
		for _, name := range strings.Split(m[1], ", ") {
			found[strings.Trim(name, `"`)] = true
		}
	}
	if len(found) < 5 {
		t.Fatalf("the source scan found %d codec names, which is too few to be right: %v",
			len(found), found)
	}
	for name := range found {
		if !documented[name] {
			t.Errorf("parseSpec accepts %q and SpecDocs does not document it", name)
		}
	}
}
