// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePkg makes a one-package directory from a map of file name to source.
func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const unitPkg = `package a

// Outer does the thing.
//
// This comment must not reach the reading step.
func Outer() int {
	helper()
	e := &Env{}
	e.method()
	elsewhere.Call()
	return 1
}

func helper() {}

func unrelated() {}

type Env struct{}

func (e *Env) method() { deeper() }

func deeper() {}
`

// TestExtractUnitTakesTheDeclarationAndItsDirectCallees. The unit is what the reading is
// built from, so what it leaves out is what the reading cannot know.
func TestExtractUnitTakesTheDeclarationAndItsDirectCallees(t *testing.T) {
	dir := writePkg(t, map[string]string{"a.go": unitPkg})
	u, err := ExtractUnit(dir, "Outer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.Source, "func Outer()") {
		t.Error("the unit does not contain the declaration it is for")
	}
	for _, want := range []string{"helper", "Env.method"} {
		if !contains(u.Callees, want) {
			t.Errorf("%s is called directly and is not in the unit: %v", want, u.Callees)
		}
	}
	// One level only. deeper() is called by method(), not by Outer, and pulling it in
	// would walk the package.
	if contains(u.Callees, "deeper") {
		t.Errorf("deeper is two calls away and was pulled in: %v", u.Callees)
	}
	if contains(u.Callees, "unrelated") {
		t.Errorf("unrelated is never called and was pulled in: %v", u.Callees)
	}
}

// TestExtractUnitDropsTheDocComment is the one that keeps the two-step honest.
//
// The whole value of reading the code blind is that the reading is not anchored on the
// claim. A unit that carried the doc comment would hand the model the sentence it is about
// to be asked to judge, and every verdict would be "supported".
func TestExtractUnitDropsTheDocComment(t *testing.T) {
	dir := writePkg(t, map[string]string{"a.go": unitPkg})
	u, err := ExtractUnit(dir, "Outer")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.Source, "must not reach the reading step") {
		t.Fatalf("the unit carries the declaration's doc comment:\n%s", u.Source)
	}
}

// TestExtractUnitSkipsTestFiles. A claim is about the program, not about its tests, and a
// test file would fill the budget with assertions.
func TestExtractUnitSkipsTestFiles(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"a.go":      unitPkg,
		"a_test.go": "package a\n\nfunc TestOuter() { Outer() }\n",
	})
	if _, err := ExtractUnit(dir, "TestOuter"); err == nil {
		t.Error("a function from a _test.go file was offered as a unit")
	}
}

// TestExtractUnitNamesAMissingDeclaration rather than returning an empty unit: an empty
// unit produces a confident reading of nothing.
func TestExtractUnitNamesAMissingDeclaration(t *testing.T) {
	dir := writePkg(t, map[string]string{"a.go": unitPkg})
	_, err := ExtractUnit(dir, "NoSuchFunction")
	if err == nil {
		t.Fatal("a declaration that does not exist produced a unit")
	}
	if !strings.Contains(err.Error(), "NoSuchFunction") {
		t.Errorf("the error does not name what was asked for: %v", err)
	}
}

// TestExtractUnitStopsAtTheBudget. The context is finite; a unit that overruns it is
// silently truncated by the server, and a reading of half a function is worse than none.
func TestExtractUnitStopsAtTheBudget(t *testing.T) {
	var big strings.Builder
	big.WriteString("package a\n\nfunc Outer() {\n")
	for i := range 40 {
		big.WriteString("\tfill")
		big.WriteString(string(rune('a' + i%26)))
		big.WriteString(string(rune('a' + i/26)))
		big.WriteString("()\n")
	}
	big.WriteString("}\n")
	for i := range 40 {
		big.WriteString("\nfunc fill")
		big.WriteString(string(rune('a' + i%26)))
		big.WriteString(string(rune('a' + i/26)))
		big.WriteString("() {\n")
		for range 20 {
			big.WriteString("\t_ = \"an unremarkable line of source that takes up room\"\n")
		}
		big.WriteString("}\n")
	}
	dir := writePkg(t, map[string]string{"a.go": big.String()})
	u, err := ExtractUnit(dir, "Outer")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Source) > UnitBudget {
		t.Errorf("the unit is %d bytes, over the %d budget", len(u.Source), UnitBudget)
	}
	if !u.Truncated {
		t.Error("the unit was cut short and does not say so")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
