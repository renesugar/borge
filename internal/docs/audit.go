// SPDX-License-Identifier: Apache-2.0

package docs

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Severity says whether a finding fails the audit.
type Severity string

const (
	// SeverityError is a broken anchor: a topic that does not exist, a claim nothing
	// checks, a directive nothing reads. Each is a documentation promise with no
	// mechanism behind it.
	SeverityError Severity = "error"
	// SeverityWarning is a gap worth seeing but not worth failing on, such as a topic
	// that carries no anchored fragment yet.
	SeverityWarning Severity = "warning"
)

// Finding is one problem with the anchors.
type Finding struct {
	Severity Severity
	Rule     string
	Message  string
	File     string
	Line     int
}

func (f Finding) String() string {
	where := f.File
	if f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if where == "" {
		return fmt.Sprintf("%s: %s: %s", f.Severity, f.Rule, f.Message)
	}
	return fmt.Sprintf("%s: %s: %s: %s", where, f.Severity, f.Rule, f.Message)
}

// Grade is how well a fragment of documentation is verified. The order matters: it is the
// order of how much a reader can rely on it, and "unverified" is permitted but counted.
type Grade string

const (
	// GradeExecuted: the prose carries examples that the test suite runs. This is the
	// grade users actually rely on, because they copy the example.
	GradeExecuted Grade = "executed"
	// GradeGenerated: the text is produced from the code, so it cannot drift.
	GradeGenerated Grade = "generated"
	// GradeClaimed: prose linked by id to a check that exists.
	GradeClaimed Grade = "claimed"
	// GradeUnverified: everything else. Rationale belongs here and is fine here.
	GradeUnverified Grade = "unverified"
)

// Grades in report order.
var Grades = []Grade{GradeExecuted, GradeGenerated, GradeClaimed, GradeUnverified}

// ExamplesClaimSuffix is the claim id convention that earns a topic the executed grade:
// a claim named "<topic>/examples" is checked by the test that runs the topic's own
// examples. It is a convention rather than a sixth directive because the mechanism is
// exactly a claim and a check - inventing a directive would mean a second thing to keep
// registered.
const ExamplesClaimSuffix = "/examples"

// TopicReport is the grade breakdown for one help topic.
type TopicReport struct {
	Topic     string
	Fragments int
	// Sections counts the fragments anchored to a named section (//borge:help
	// topic/section) rather than to the topic as a whole. A topic with none is graded in
	// one lump, which flatters it: see the topic-anchored-as-a-whole finding.
	Sections         int
	ByGrade          map[Grade]int
	ExamplesExecuted bool
}

// Report is the whole audit.
type Report struct {
	Findings []Finding
	Topics   []TopicReport
	Blocks   int
	ByGrade  map[Grade]int
	Files    int
}

// Errors returns only the findings that fail the audit.
func (r Report) Errors() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}

// UnverifiedShare is the fraction of anchored fragments that nothing verifies, as a
// percentage. The point of the whole exercise is that this number exists.
func (r Report) UnverifiedShare() float64 {
	if r.Blocks == 0 {
		return 0
	}
	return 100 * float64(r.ByGrade[GradeUnverified]) / float64(r.Blocks)
}

// topicOf strips the section from a "topic/section" anchor value.
func topicOf(anchor string) string {
	if i := strings.Index(anchor, "/"); i >= 0 {
		return anchor[:i]
	}
	return anchor
}

// gradeOf grades one block. checked names the claims that have a registered check.
func gradeOf(b Block, checked map[string]bool) Grade {
	for _, claim := range b.Claims {
		if strings.HasSuffix(claim, ExamplesClaimSuffix) && checked[claim] {
			return GradeExecuted
		}
	}
	if len(b.Enumerates) > 0 {
		return GradeGenerated
	}
	for _, claim := range b.Claims {
		if checked[claim] {
			return GradeClaimed
		}
	}
	return GradeUnverified
}

// isExamplesFragment reports whether a block is a topic's examples.
//
// Those are exempt from needing code to point at. An examples block is a list of commands
// with no single implementation, and it is already the best-verified kind of fragment
// there is: TestHelpExamplesRun executes every line of it. Asking a model whether a reading
// of some function contradicts a command line would add nothing to that.
func isExamplesFragment(b Block) bool {
	for _, topic := range b.Topics {
		if strings.HasSuffix(topic, ExamplesClaimSuffix) {
			return true
		}
	}
	return false
}

// blockName is the fragment's help anchor, or its declaration when it has none.
func blockName(b Block) string {
	if len(b.Topics) > 0 {
		return "//borge:help " + b.Topics[0]
	}
	return b.Decl
}

// Audit checks a parsed set against the topics and generated lists that actually exist.
//
// Both lists are passed in rather than read here: they live in the command layer and this
// package is a leaf, so the caller - the tool, or a test in that layer - asks the code
// itself what exists. A list written here would be a second place for them to disagree,
// which is the class of bug this whole package exists to remove.
func Audit(set *Set, topics, enumerations []string) Report {
	report := Report{ByGrade: map[Grade]int{}, Blocks: len(set.Blocks), Files: set.Files}
	known := map[string]bool{}
	for _, t := range topics {
		known[t] = true
	}
	anchoredHelp := map[string]Block{}
	knownEnum := map[string]bool{}
	for _, e := range enumerations {
		knownEnum[e] = true
	}
	anchoredEnum := map[string]bool{}

	for _, d := range set.Malformed {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError,
			Rule:     "unknown-directive",
			Message: fmt.Sprintf("//borge:%s is not an anchor; the vocabulary is %s",
				d.Name, strings.Join(sortedKeys(knownDirectives), ", ")),
			File: d.File, Line: d.Line,
		})
	}

	checked := map[string]bool{}
	claimed := map[string]Block{}
	for _, c := range set.Checks {
		if c.Claim == "" {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: "directive-missing-argument",
				Message: "//borge:checks names no claim", File: c.File, Line: c.Line,
			})
			continue
		}
		if !c.IsFun {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: "checks-on-non-function",
				Message: fmt.Sprintf("//borge:checks %s is on %s, which is not a function",
					c.Claim, c.Func),
				File: c.File, Line: c.Line,
			})
			continue
		}
		checked[c.Claim] = true
	}

	for _, b := range set.Blocks {
		if b.Audience != "" && b.Audience != "user" {
			// "api" is the one worth naming: it was in the design, and marking a comment
			// with it would do nothing at all, because nothing renders that subset. An
			// audience no generator reads is a silent no-op, which is the failure this
			// whole package exists to remove.
			hint := "the only subset is user"
			if b.Audience == "api" {
				hint = "the api subset was declined in R2 T4: go doc already serves " +
					"internal/, so nothing renders it"
			}
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: "invalid-audience",
				Message: fmt.Sprintf("//borge:doc %q: %s", b.Audience, hint),
				File:    b.File, Line: b.Line,
			})
		}
		for _, anchor := range b.Topics {
			if anchor == "" {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "directive-missing-argument",
					Message: "//borge:help names no topic", File: b.File, Line: b.Line,
				})
				continue
			}
			if !known[topicOf(anchor)] {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "unknown-topic",
					Message: fmt.Sprintf("//borge:help %s names a topic that does not exist (%s on %s)",
						anchor, topicOf(anchor), b.Decl),
					File: b.File, Line: b.Line,
				})
				continue
			}
			if first, dup := anchoredHelp[anchor]; dup {
				// Two fragments claiming one place in a document is exactly the
				// ambiguity the templates exist to remove: the generator would have to
				// pick one, and it would pick by source order.
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "duplicate-help-anchor",
					Message: fmt.Sprintf("//borge:help %s is also anchored at %s:%d; a section has one source",
						anchor, first.File, first.Line),
					File: b.File, Line: b.Line,
				})
				continue
			}
			anchoredHelp[anchor] = b
			if b.Audience == "" {
				// A help fragment with no audience is invisible to the generator, which
				// emits the "user" subset. It would vanish from the topic in silence.
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "help-without-audience",
					Message: fmt.Sprintf("//borge:help %s has no //borge:doc, so nothing would render it",
						anchor),
					File: b.File, Line: b.Line,
				})
			}
		}
		for _, name := range b.Enumerates {
			if name == "" {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "directive-missing-argument",
					Message: "//borge:enumerates names no list", File: b.File, Line: b.Line,
				})
				continue
			}
			if !knownEnum[name] {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "unknown-enumeration",
					Message: fmt.Sprintf("//borge:enumerates %s names a list the code does not define (on %s)",
						name, b.Decl),
					File: b.File, Line: b.Line,
				})
				continue
			}
			anchoredEnum[name] = true
		}
		for _, about := range b.About {
			if about == "" {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "directive-missing-argument",
					Message: "//borge:about names no declaration", File: b.File, Line: b.Line,
				})
				continue
			}
			// A Set built by hand rather than by Parse has no declaration index, and a
			// check that cannot resolve a name must not invent a finding about it.
			dir := filepath.Dir(b.File)
			if decls, indexed := set.Decls[dir]; indexed && !decls[about] {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "unknown-declaration",
					Message: fmt.Sprintf("//borge:about %s names no function in %s; a "+
						"pointer at nothing is worse than none, because it reads as a link",
						about, dir),
					File: b.File, Line: b.Line,
				})
			}
		}
		// A user-facing fragment that neither sits on a function nor names one is prose
		// the contradiction checker cannot reach: it has no code to read. That is a gap
		// rather than a break, so it is a warning - but an unreported one would make
		// doccheck silently check nothing, which is what it did until 2026-08-28.
		if b.Audience == "user" && !b.IsFunc && len(b.About) == 0 && !isExamplesFragment(b) {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: "fragment-without-code",
				Message: fmt.Sprintf("%s is user-facing prose on %s, which is not a "+
					"function and names none with //borge:about; nothing can check it "+
					"against code", blockName(b), b.Decl),
				File: b.File, Line: b.Line,
			})
		}
		for _, claim := range b.Claims {
			if claim == "" {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "directive-missing-argument",
					Message: "//borge:claim names no id", File: b.File, Line: b.Line,
				})
				continue
			}
			if first, dup := claimed[claim]; dup {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "duplicate-claim",
					Message: fmt.Sprintf("claim %s is also made at %s:%d; one check cannot answer for two claims",
						claim, first.File, first.Line),
					File: b.File, Line: b.Line,
				})
				continue
			}
			claimed[claim] = b
			if !checked[claim] {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: "claim-without-check",
					Message: fmt.Sprintf("claim %s has no //borge:checks anywhere; the prose asserts something nothing verifies",
						claim),
					File: b.File, Line: b.Line,
				})
			}
			if b.Audience == "" && len(b.Enumerates) == 0 {
				// A block carrying //borge:enumerates is user-facing whether or not it
				// says so: its entries are rendered verbatim into a topic. Anything else
				// with a claim and no audience is prose the checkers will never read.
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityWarning, Rule: "claim-without-audience",
					Message: fmt.Sprintf("claim %s is on a comment with no //borge:doc, so the prose checkers will not read it",
						claim),
					File: b.File, Line: b.Line,
				})
			}
		}
		report.ByGrade[gradeOf(b, checked)]++
	}

	for _, c := range set.Checks {
		if c.Claim == "" || !c.IsFun {
			continue
		}
		if _, ok := claimed[c.Claim]; !ok {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: "check-without-claim",
				Message: fmt.Sprintf("%s checks claim %s, which no documentation makes; the check outlived its prose",
					c.Func, c.Claim),
				File: c.File, Line: c.Line,
			})
		}
	}

	for _, name := range enumerations {
		if !anchoredEnum[name] {
			// The list is generated, so it cannot be wrong - but nothing says which
			// documentation it appears in, so the audit cannot report it under a topic.
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: "enumeration-not-anchored",
				Message: fmt.Sprintf("the %s list is generated but no //borge:enumerates names it, "+
					"so no topic is credited with it", name),
			})
		}
	}

	report.Topics = topicReports(set, topics, checked)
	for _, t := range report.Topics {
		if t.Fragments == 0 {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: "topic-without-fragments",
				Message: fmt.Sprintf("the %s topic has no anchored fragment, so nothing ties it to the code", t.Topic),
			})
			continue
		}
		if t.Sections == 0 {
			// One anchor over a whole topic grades the whole topic by its best part. The
			// examples in it are executed; the paragraphs around them are not separately
			// graded, and a reader of "unverified 0" would conclude otherwise.
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: "topic-anchored-as-a-whole",
				Message: fmt.Sprintf("the %s topic is anchored in one piece, so its grade covers all of it at once; "+
					"anchor its sections (//borge:help %s/<section>) for a breakdown that means something",
					t.Topic, t.Topic),
			})
		}
		if !t.ExamplesExecuted {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: "topic-without-executed-examples",
				Message: fmt.Sprintf("the %s topic has no %s%s claim, so nothing runs what it tells users to type",
					t.Topic, t.Topic, ExamplesClaimSuffix),
			})
		}
	}

	sortFindings(report.Findings)
	return report
}

func topicReports(set *Set, topics []string, checked map[string]bool) []TopicReport {
	byTopic := map[string]*TopicReport{}
	for _, t := range topics {
		byTopic[t] = &TopicReport{Topic: t, ByGrade: map[Grade]int{}}
	}
	for _, b := range set.Blocks {
		seen := map[string]bool{}
		for _, anchor := range b.Topics {
			name := topicOf(anchor)
			report, ok := byTopic[name]
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			report.Fragments++
			if strings.Contains(anchor, "/") {
				report.Sections++
			}
			report.ByGrade[gradeOf(b, checked)]++
		}
	}
	for name, report := range byTopic {
		if checked[name+ExamplesClaimSuffix] {
			report.ExamplesExecuted = true
		}
	}
	out := make([]TopicReport, 0, len(byTopic))
	for _, t := range topics {
		out = append(out, *byTopic[t])
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}

// Format renders the report the way the tool prints it.
func (r Report) Format() string {
	var b strings.Builder
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "%s\n", f)
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "topics\n")
	for _, t := range r.Topics {
		examples := "no executed examples"
		if t.ExamplesExecuted {
			examples = "examples executed"
		}
		granularity := fmt.Sprintf("%d section(s)", t.Sections)
		if t.Sections == 0 {
			granularity = "whole-topic grade"
		}
		fmt.Fprintf(&b, "  %-16s %2d fragment(s)  %-18s %s  %s\n",
			t.Topic, t.Fragments, granularity, gradeLine(t.ByGrade), examples)
	}
	fmt.Fprintf(&b, "\n%d anchored fragment(s) in %d file(s): %s\n",
		r.Blocks, r.Files, gradeLine(r.ByGrade))
	fmt.Fprintf(&b, "unverified share: %.0f%%\n", r.UnverifiedShare())
	errs := len(r.Errors())
	fmt.Fprintf(&b, "%d error(s), %d warning(s)\n", errs, len(r.Findings)-errs)
	return b.String()
}

func gradeLine(counts map[Grade]int) string {
	var parts []string
	for _, g := range Grades {
		parts = append(parts, fmt.Sprintf("%s %d", g, counts[g]))
	}
	return strings.Join(parts, ", ")
}
