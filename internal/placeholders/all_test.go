// SPDX-License-Identifier: Apache-2.0

package placeholders

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fieldCase finds the names field() switches on, which is the set borge actually
// substitutes. The reverse direction has to be asked of the code rather than of a second
// list: a placeholder implemented and left out of All would work and be undiscoverable.
var fieldCase = regexp.MustCompile(`case "([a-z0-9-]+)":`)

func testValues() Values {
	return Values{
		Now:      time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		UTCNow:   time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		Hostname: "host",
		FQDN:     "host.example",
		User:     "someone",
		PID:      1234,
		UUID4:    "0d1b8f6e-0000-4000-8000-000000000000",
		Version:  "0.8.0",
	}
}

// TestAllCoversWhatExpands checks the documented table against the expander in both
// directions.
func TestAllCoversWhatExpands(t *testing.T) {
	documented := map[string]bool{}
	values := testValues()
	for _, p := range All() {
		if p.Description == "" {
			t.Errorf("placeholder %q has no description", p.Syntax)
		}
		if !strings.HasPrefix(p.Syntax, "{"+p.Name) {
			t.Errorf("placeholder %q does not write its own name %q", p.Syntax, p.Name)
		}
		documented[p.Name] = true

		// The documented syntax has to expand. The two that take a format are written
		// with a placeholder word in the table, so a real directive stands in for it.
		syntax := strings.Replace(p.Syntax, "FORMAT", "%Y", 1)
		got, err := values.Expand(syntax)
		if err != nil {
			t.Errorf("the documented placeholder %s does not expand: %v", p.Syntax, err)
			continue
		}
		if got == syntax || got == "" {
			t.Errorf("%s expanded to %q, which is not a substitution", p.Syntax, got)
		}
		if strings.ContainsAny(got, "{}") {
			t.Errorf("%s expanded to %q, which still contains braces", p.Syntax, got)
		}
	}

	src, err := os.ReadFile("placeholders.go")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range fieldCase.FindAllStringSubmatch(string(src), -1) {
		names[m[1]] = true
	}
	if len(names) < 10 {
		t.Fatalf("the source scan found %d placeholder cases, too few to be right: %v", len(names), names)
	}
	for name := range names {
		if !documented[name] {
			t.Errorf("field() expands {%s} and All() does not document it", name)
		}
	}

	// And the table must not promise a format where the expander refuses one.
	for _, p := range All() {
		if p.TakesFormat {
			continue
		}
		if _, err := values.Expand("{" + p.Name + ":%Y}"); err == nil {
			t.Errorf("{%s} accepts a format, but the table says it does not", p.Name)
		}
	}
}
