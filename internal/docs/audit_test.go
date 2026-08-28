// SPDX-License-Identifier: Apache-2.0

package docs

import (
	"strings"
	"testing"
)

// The topics the synthetic sources below anchor into. Real topic names, so a reader of a
// failure recognises them, but supplied by the test rather than read from the CLI: this
// package is a leaf and must not know what borge's topics are.
var testTopics = []string{"patterns", "environment"}

// clean is a source set with nothing wrong: every claim checked, every topic anchored,
// examples executed. Every case below starts from it and breaks exactly one thing, so a
// finding can only come from what the case did.
func cleanFiles() map[string]string {
	return map[string]string{
		"help.go": `package a

// Patterns is the patterns topic.
//
//borge:doc user
//borge:help patterns
//borge:claim patterns/examples
const Patterns = "..."

// PatternStyles is one section of it.
//
//borge:doc user
//borge:help patterns/styles
//borge:enumerates a.Styles
const PatternStyles = "..."

// Environment is the environment topic.
//
//borge:doc user
//borge:help environment
//borge:claim environment/examples
//borge:claim prompts-only-on-tty
const Environment = "..."

// EnvironmentPassphrases is one section of it.
//
//borge:doc user
//borge:help environment/passphrases
//borge:claim passphrase-prompted-once
const EnvironmentPassphrases = "..."
`,
		"help_test.go": `package a

// TestExamplesRun runs the examples in every topic.
//
//borge:checks patterns/examples
//borge:checks environment/examples
func TestExamplesRun() {}

// TestPrompting checks the prompting claims.
//
//borge:checks prompts-only-on-tty
//borge:checks passphrase-prompted-once
func TestPrompting() {}
`,
	}
}

func auditFiles(t *testing.T, files map[string]string) Report {
	t.Helper()
	return Audit(parse(t, files), testTopics)
}

// findingRules is the multiset of rules a report produced, for comparison.
func findingRules(r Report) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Rule)
	}
	return out
}

func hasRule(r Report, rule string) bool {
	for _, f := range r.Findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

// TestAuditIsQuietOnACleanSet is the baseline every other case depends on. Without it a
// case that "detects" a problem might be detecting the fixture.
func TestAuditIsQuietOnACleanSet(t *testing.T) {
	report := auditFiles(t, cleanFiles())
	if len(report.Findings) != 0 {
		t.Fatalf("the clean set produced findings: %v", findingRules(report))
	}
	if report.Blocks != 4 {
		t.Fatalf("audited %d blocks, want 4; the fixture is not what the cases think", report.Blocks)
	}
	for _, topic := range report.Topics {
		if !topic.ExamplesExecuted {
			t.Errorf("the %s topic is not graded as executing its examples", topic.Topic)
		}
		if topic.ByGrade[GradeExecuted] != 1 {
			t.Errorf("the %s topic has %d executed fragment(s), want 1",
				topic.Topic, topic.ByGrade[GradeExecuted])
		}
	}
}

// Each case damages the clean set in one way. The rule it must produce is named, so a
// case that fails through some other check counts as a failure: the check it is about
// would still be unproven.
func TestAuditDetects(t *testing.T) {
	cases := []struct {
		name   string
		damage func(map[string]string)
		rule   string
	}{
		{
			"a help anchor naming a topic that does not exist",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:help patterns\n", "//borge:help paterns\n", 1)
			},
			"unknown-topic",
		},
		{
			"a claim nothing checks",
			func(f map[string]string) {
				f["help_test.go"] = strings.Replace(f["help_test.go"],
					"//borge:checks prompts-only-on-tty\n", "", 1)
			},
			"claim-without-check",
		},
		{
			"a check whose claim no longer exists",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:claim prompts-only-on-tty\n", "", 1)
			},
			"check-without-claim",
		},
		{
			"the same claim id made twice",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:claim patterns/examples\n",
					"//borge:claim patterns/examples\n//borge:claim prompts-only-on-tty\n", 1)
			},
			"duplicate-claim",
		},
		{
			"a directive that is not in the vocabulary",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:claim prompts-only-on-tty",
					"//borge:claims prompts-only-on-tty\n//borge:claim prompts-only-on-tty", 1)
			},
			"unknown-directive",
		},
		{
			"an audience that is not a subset",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:doc user\n//borge:help patterns",
					"//borge:doc users\n//borge:help patterns", 1)
			},
			"invalid-audience",
		},
		{
			"a help fragment with no audience, which nothing would render",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:doc user\n//borge:help patterns", "//borge:help patterns", 1)
			},
			"help-without-audience",
		},
		{
			"a directive with no argument",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"],
					"//borge:claim prompts-only-on-tty", "//borge:claim", 1)
				f["help_test.go"] = strings.Replace(f["help_test.go"],
					"//borge:checks prompts-only-on-tty\n", "", 1)
			},
			"directive-missing-argument",
		},
		{
			"a check registered on something that is not a function",
			func(f map[string]string) {
				f["help_test.go"] = strings.Replace(f["help_test.go"],
					"func TestPrompting() {}", "var TestPrompting = 1", 1)
			},
			"checks-on-non-function",
		},
		{
			"a topic nothing anchors",
			func(f map[string]string) {
				// Both of the environment topic's fragments go, along with the checks
				// that would then have no claim.
				f["help.go"] = strings.Replace(f["help.go"], "//borge:help environment\n", "", 1)
				f["help.go"] = strings.Replace(f["help.go"], "//borge:help environment/passphrases\n", "", 1)
				f["help.go"] = strings.Replace(f["help.go"], "//borge:claim environment/examples\n", "", 1)
				f["help.go"] = strings.Replace(f["help.go"], "//borge:claim passphrase-prompted-once\n", "", 1)
				f["help_test.go"] = strings.Replace(f["help_test.go"], "//borge:checks environment/examples\n", "", 1)
				f["help_test.go"] = strings.Replace(f["help_test.go"], "//borge:checks passphrase-prompted-once\n", "", 1)
			},
			"topic-without-fragments",
		},
		{
			"a topic whose examples nothing runs",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"], "//borge:claim environment/examples\n", "", 1)
				f["help_test.go"] = strings.Replace(f["help_test.go"], "//borge:checks environment/examples\n", "", 1)
			},
			"topic-without-executed-examples",
		},
		{
			"a topic anchored in one piece, whose grade flatters the whole text",
			func(f map[string]string) {
				f["help.go"] = strings.Replace(f["help.go"], "//borge:help patterns/styles", "//borge:help patterns", 1)
			},
			"topic-anchored-as-a-whole",
		},
		{
			"a claim on a comment with no audience, which the prose checkers will not read",
			func(f map[string]string) {
				f["help.go"] += `
// Helper is rationale, not documentation.
//
//borge:claim helper-claim
func Helper() {}
`
				f["help_test.go"] += `
// TestHelper checks it.
//
//borge:checks helper-claim
func TestHelper() {}
`
			},
			"claim-without-audience",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := cleanFiles()
			before := len(files["help.go"]) + len(files["help_test.go"])
			tc.damage(files)
			if len(files["help.go"])+len(files["help_test.go"]) == before {
				t.Fatal("the damage changed nothing, so this case tests nothing")
			}
			report := auditFiles(t, files)
			if !hasRule(report, tc.rule) {
				t.Fatalf("no %s finding; got %v", tc.rule, findingRules(report))
			}
		})
	}
}

// TestAuditGradesFragments checks the four grades and the number the whole exercise is
// for: the share of documentation that nothing verifies.
func TestAuditGradesFragments(t *testing.T) {
	files := cleanFiles()
	files["more.go"] = `package a

// Generated lists what the code defines.
//
//borge:doc user
//borge:help patterns/styles
//borge:enumerates a.Styles
const Generated = "..."

// Claimed says something a test checks.
//
//borge:doc user
//borge:help patterns/paths
//borge:claim paths-are-relative
const Claimed = "..."

// Unverified is rationale in the user subset: true, useful, and checked by nothing.
//
//borge:doc user
//borge:help patterns/why
const Unverified = "..."
`
	files["more_test.go"] = `package a

// TestPaths checks it.
//
//borge:checks paths-are-relative
func TestPaths() {}
`
	report := auditFiles(t, files)
	if errs := report.Errors(); len(errs) != 0 {
		t.Fatalf("the fixture has errors, so the grades below are not what is being tested: %v", errs)
	}
	want := map[Grade]int{GradeExecuted: 2, GradeGenerated: 2, GradeClaimed: 2, GradeUnverified: 1}
	for grade, count := range want {
		if report.ByGrade[grade] != count {
			t.Errorf("%d fragment(s) graded %s, want %d", report.ByGrade[grade], grade, count)
		}
	}
	if got := report.UnverifiedShare(); got < 13 || got > 15 {
		t.Errorf("unverified share is %.1f%%, want one seventh", got)
	}
	// The patterns topic gained three fragments from more.go.
	for _, topic := range report.Topics {
		if topic.Topic != "patterns" {
			continue
		}
		if topic.Fragments != 5 {
			t.Errorf("the patterns topic has %d fragments, want 5", topic.Fragments)
		}
	}
}

// TestReportFormatSaysWhatItFound. The report is read by a person; a report that omits
// the number it exists to produce is not a report.
func TestReportFormatSaysWhatItFound(t *testing.T) {
	report := auditFiles(t, cleanFiles())
	out := report.Format()
	for _, want := range []string{"patterns", "environment", "unverified share", "0 error(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
}
