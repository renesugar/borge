// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsAreCurrent regenerates the help topics in memory and compares.
//
// This is the standard Go generated-code freshness check, and it is what makes colocation
// safe: editing a doc comment without regenerating fails here rather than shipping a
// binary whose help text disagrees with its own source.
//
// It runs the generator rather than shelling out to it, so a defect in the generator fails
// this test too.
func TestDocsAreCurrent(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	source, orphans, err := GenerateHelpFile(root)
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	for _, orphan := range orphans {
		t.Errorf("//borge:help %s is anchored but no topic asks for it, so it is written "+
			"and never shown", orphan)
	}

	path := filepath.Join(root, "internal", "cli", "help_generated.go")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(existing) == source {
		return
	}

	// Report the first line that differs rather than the whole file: the topics are
	// thousands of characters and a full dump buries the change.
	want := strings.Split(source, "\n")
	got := strings.Split(string(existing), "\n")
	for i := 0; i < len(want) || i < len(got); i++ {
		var w, g string
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		if w != g {
			t.Fatalf("help_generated.go is out of date; run \"make docgen\".\n"+
				"first difference at line %d:\n  generated: %q\n  checked in: %q", i+1, w, g)
		}
	}
	t.Fatal("help_generated.go differs from what the generator produces; run \"make docgen\"")
}

// TestGeneratedTopicsAreSubstantial guards against the failure that would make every other
// help test pass while the topics are empty: a generated map that rendered nothing.
func TestGeneratedTopicsAreSubstantial(t *testing.T) {
	if len(helpGeneratedTopics) != len(helpTemplates()) {
		t.Fatalf("%d generated topics for %d templates", len(helpGeneratedTopics), len(helpTemplates()))
	}
	for _, tmpl := range helpTemplates() {
		body := helpGeneratedTopics[tmpl.name]
		if len(body) < 400 {
			t.Errorf("the %s topic is %d bytes, which is not a topic", tmpl.name, len(body))
		}
		if !strings.HasPrefix(body, "borge help "+tmpl.name+"\n") {
			t.Errorf("the %s topic does not begin with its own name: %q", tmpl.name, first(body))
		}
		if strings.Contains(body, "{{enum:") || strings.Contains(body, "//borge:") {
			t.Errorf("the %s topic still contains generator syntax:\n%s", tmpl.name, body)
		}
	}
}

func first(body string) string {
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		return body[:i]
	}
	return body
}
