// SPDX-License-Identifier: Apache-2.0

package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write lays out a synthetic package. The parser is tested against source it can see in
// full rather than against borge's own tree: a test that reads the repository passes or
// fails for reasons that have nothing to do with the parser.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func parse(t *testing.T, files map[string]string) *Set {
	t.Helper()
	set, err := Parse(write(t, files))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if set.Files == 0 {
		t.Fatal("the parser read no files, so every assertion below would be vacuous")
	}
	return set
}

func TestParseFindsAnchorsOnEveryKindOfDeclaration(t *testing.T) {
	set := parse(t, map[string]string{
		"a.go": `// Package a is the package comment.
//
//borge:doc user
//borge:help patterns
package a

// Thing is a type.
//
//borge:doc api
//borge:claim thing-exists
type Thing struct{}

// Text is a constant.
//
//borge:doc user
//borge:help patterns/styles
const Text = "x"

// Do does something.
//
//borge:doc user
//borge:claim do-does
func Do() {}

// Method on a pointer receiver.
//
//borge:doc user
//borge:enumerates a.Kinds
func (t *Thing) Method() {}
`,
	})

	if len(set.Blocks) != 5 {
		t.Fatalf("found %d anchored blocks, want 5: %+v", len(set.Blocks), set.Blocks)
	}
	byDecl := map[string]Block{}
	for _, b := range set.Blocks {
		byDecl[b.Decl] = b
	}
	for _, decl := range []string{"package a", "Thing", "Text", "Do", "Thing.Method"} {
		if _, ok := byDecl[decl]; !ok {
			t.Errorf("no block for %q; found %v", decl, keysOf(byDecl))
		}
	}
	if got := byDecl["Text"].Topics; len(got) != 1 || got[0] != "patterns/styles" {
		t.Errorf("the const's help anchor is %v, want [patterns/styles]", got)
	}
	if got := byDecl["Thing.Method"].Enumerates; len(got) != 1 || got[0] != "a.Kinds" {
		t.Errorf("the method's enumerates anchor is %v, want [a.Kinds]", got)
	}
	if !byDecl["Do"].IsFunc {
		t.Error("Do is not recorded as a function")
	}
	if byDecl["Text"].IsFunc {
		t.Error("a const is recorded as a function")
	}
}

// TestParseKeepsDirectivesOutOfTheProse. Go strips directives from a rendered comment, so
// the generator must see the same text a reader does - otherwise "//borge:doc user" would
// appear in the help output.
func TestParseKeepsDirectivesOutOfTheProse(t *testing.T) {
	set := parse(t, map[string]string{
		"a.go": `package a

// Do is documented.
//
// The second paragraph.
//
//borge:doc user
//borge:claim do-does
func Do() {}
`,
	})
	if len(set.Blocks) != 1 {
		t.Fatalf("found %d blocks, want 1", len(set.Blocks))
	}
	prose := set.Blocks[0].Prose
	if strings.Contains(prose, "borge:") {
		t.Errorf("the prose still contains a directive:\n%s", prose)
	}
	if !strings.Contains(prose, "The second paragraph.") {
		t.Errorf("the prose lost its content:\n%s", prose)
	}
}

// TestParseReportsNearMisses. "// borge:help x" has a space after the slashes, so Go does
// not treat it as a directive and it would render into the documentation as prose while
// registering nothing. It reads exactly like an anchor to a human, which is what makes it
// worth reporting rather than ignoring.
func TestParseReportsNearMisses(t *testing.T) {
	set := parse(t, map[string]string{
		"a.go": `package a

// Do is documented.
//
// borge:help patterns
//borge:Claim do-does
//borge:claims do-does
//borge:doc user
func Do() {}
`,
	})
	if len(set.Malformed) != 3 {
		t.Fatalf("found %d malformed directives, want 3: %+v", len(set.Malformed), set.Malformed)
	}
	names := map[string]bool{}
	for _, d := range set.Malformed {
		names[d.Name] = true
	}
	for _, want := range []string{"help", "Claim", "claims"} {
		if !names[want] {
			t.Errorf("the near-miss %q was not reported; got %v", want, keysOfBool(names))
		}
	}
	// The well-formed anchor on the same comment is still read.
	if len(set.Blocks) != 1 || set.Blocks[0].Audience != "user" {
		t.Errorf("the valid anchor beside the near-misses was not read: %+v", set.Blocks)
	}
}

func TestParseCollectsChecksFromTestFiles(t *testing.T) {
	set := parse(t, map[string]string{
		"a.go": `package a

// Do is documented.
//
//borge:doc user
//borge:claim do-does
func Do() {}
`,
		"a_test.go": `package a

// TestDo checks it.
//
//borge:checks do-does
func TestDo() {}
`,
	})
	if len(set.Checks) != 1 {
		t.Fatalf("found %d checks, want 1: %+v", len(set.Checks), set.Checks)
	}
	c := set.Checks[0]
	if c.Claim != "do-does" || c.Func != "TestDo" || !c.IsFun {
		t.Errorf("check is %+v, want claim do-does on function TestDo", c)
	}
	if !strings.HasSuffix(c.File, "_test.go") {
		t.Errorf("check file is %q, want the test file", c.File)
	}
	// A comment that only registers a check is not a documentation fragment.
	for _, b := range set.Blocks {
		if b.Decl == "TestDo" {
			t.Errorf("the check's comment was recorded as a documentation block: %+v", b)
		}
	}
}

func TestParseIgnoresUnanchoredComments(t *testing.T) {
	set := parse(t, map[string]string{
		"a.go": `package a

// Do is documented in the ordinary way, with no anchors at all.
func Do() {}
`,
	})
	if len(set.Blocks) != 0 {
		t.Errorf("an unanchored comment produced %d blocks: %+v", len(set.Blocks), set.Blocks)
	}
	if len(set.Malformed) != 0 {
		t.Errorf("an unanchored comment produced malformed directives: %+v", set.Malformed)
	}
}

func TestParseSkipsVendoredAndGeneratedDirectories(t *testing.T) {
	set := parse(t, map[string]string{
		"a.go": `package a

// Do is documented.
//
//borge:doc user
//borge:claim do-does
func Do() {}
`,
		"vendor/b/b.go": `package b

// Other is somebody else's.
//
//borge:doc user
//borge:claim not-ours
func Other() {}
`,
		"testdata/c.go": `package c

// Fixture is a fixture.
//
//borge:doc user
//borge:claim fixture
func Fixture() {}
`,
	})
	for _, b := range set.Blocks {
		if strings.Contains(b.File, "vendor") || strings.Contains(b.File, "testdata") {
			t.Errorf("scanned a directory it should skip: %s", b.File)
		}
	}
	if len(set.Blocks) != 1 {
		t.Errorf("found %d blocks, want 1: %+v", len(set.Blocks), set.Blocks)
	}
}

func keysOf(m map[string]Block) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
