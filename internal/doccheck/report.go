// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/renesugar/borge/internal/docs"
)

// What gets checked, and what the run says afterwards.
//
// Only //borge:doc user blocks: rationale is not entailed by any code and never will be,
// so checking it produces permanent noise that teaches everyone to ignore the report.
// Rationale stays unmarked and unchecked.
//
// The output is a triage list, never a gate. The check is non-deterministic in the sense
// that matters - a different model gives different answers - and a build that fails on it
// would be a build that fails for reasons nobody can reproduce. A contradicted verdict
// means "look at this".

// Target is one block to check: a claim, and the declaration it is anchored to.
type Target struct {
	Anchor string
	File   string
	Line   int
	Decl   string
	Prose  string
}

// Targets picks the blocks worth checking out of a parsed anchor set.
//
// A block is skipped when it is not in the user subset, when its prose is empty, or when
// there is no code to read it against.
//
// # Where the code comes from
//
// A doc comment on the function that implements a sentence needs nothing extra: the
// attachment is the link. But almost none of borge's user-facing prose is written that
// way. gofmt moves //borge: directives to the end of a comment, so the fragments live on
// "var _ = helpText" carriers beside the code instead - which put the prose in the right
// file and left nothing saying which function it is about.
//
// That is why //borge:about exists, and it is not a detail: with the IsFunc test alone
// this function returned nothing at all, and doccheck reported a clean tree by checking
// none of it. The audit now warns about a user fragment with neither, so the same silence
// cannot come back.
//
// A block that names several declarations becomes several targets: each is judged against
// its own reading, and a claim about two functions is worth asking about twice.
func Targets(set *docs.Set) []Target {
	var out []Target
	for _, b := range set.Blocks {
		if b.Audience != "user" || strings.TrimSpace(b.Prose) == "" || b.IsTest {
			continue
		}
		decls := b.About
		if len(decls) == 0 && b.IsFunc {
			decls = []string{b.Decl}
		}
		anchor := b.Decl
		if len(b.Topics) > 0 {
			anchor = b.Topics[0]
		}
		for _, decl := range decls {
			out = append(out, Target{
				Anchor: anchor, File: b.File, Line: b.Line, Decl: decl, Prose: b.Prose,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// Run checks every target and returns the triage list.
//
// One reading per declaration, reused across the claims anchored to it: the reading is the
// expensive half, and two readings of the same function that disagree with each other would
// make the report harder to trust rather than easier.
func (c Checker) Run(ctx context.Context, root string, targets []Target) (Report, error) {
	rep := Report{Model: c.Model.Name()}
	readings := map[string]string{}
	units := map[string]Unit{}
	for _, t := range targets {
		dir := filepath.Dir(filepath.Join(root, t.File))
		key := dir + "\x00" + t.Decl
		if _, done := readings[key]; !done {
			u, err := ExtractUnit(dir, t.Decl)
			if err != nil {
				// A declaration the extractor cannot find is a gap in the extractor,
				// not a finding about the prose. It is reported and skipped.
				rep.Findings = append(rep.Findings, Finding{
					Anchor: t.Anchor, File: t.File, Line: t.Line, Unit: t.Decl,
					Verdict: VerdictNotDeterminable,
					Reading: "no unit: " + err.Error(),
				})
				continue
			}
			reading, err := c.Read(ctx, u)
			if err != nil {
				return rep, err
			}
			units[key] = u
			readings[key] = reading
		}
		verdict, reading, denied, err := c.CheckAgainstReading(ctx, readings[key], t.Prose)
		if err != nil {
			return rep, err
		}
		rep.Checked++
		rep.Findings = append(rep.Findings, Finding{
			Anchor: t.Anchor, File: t.File, Line: t.Line, Unit: t.Decl,
			Verdict: verdict, Reading: reading, Disagreed: denied,
			Truncated: units[key].Truncated,
		})
	}
	return rep, nil
}

// Contradictions is the part of a report a person has to read.
func (r Report) Contradictions() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Verdict == VerdictContradicted {
			out = append(out, f)
		}
	}
	return out
}

// Format renders a report.
//
// The counts come first and the header says the check is advisory, because a list of
// "contradicted" lines with no framing reads like a list of bugs, and most of them will not
// be.
func (r Report) Format() string {
	var b strings.Builder
	counts := map[Verdict]int{}
	for _, f := range r.Findings {
		counts[f.Verdict]++
	}
	fmt.Fprintf(&b, "doccheck (advisory) - model %s\n", r.Model)
	fmt.Fprintf(&b, "%d claim(s) checked: %d supported, %d contradicted, %d not determinable\n",
		r.Checked, counts[VerdictSupported], counts[VerdictContradicted],
		counts[VerdictNotDeterminable])
	if len(r.Contradictions()) == 0 {
		b.WriteString("\nNothing to triage. This is not evidence the documentation is " +
			"right; see the calibration score for what this model's silence is worth.\n")
		return b.String()
	}
	b.WriteString("\nTo look at. A contradiction is the model's reading disagreeing with " +
		"the prose; the reading can be the wrong one.\n")
	for _, f := range r.Contradictions() {
		fmt.Fprintf(&b, "\n%s:%d  %s  (%s)\n", f.File, f.Line, f.Anchor, f.Unit)
		if f.Truncated {
			b.WriteString("  the unit did not fit the budget, so the reading is partial\n")
		}
		for _, s := range f.Disagreed {
			fmt.Fprintf(&b, "  claim:   %s\n", s)
		}
		fmt.Fprintf(&b, "  reading: %s\n", indent(f.Reading, "           "))
	}
	return b.String()
}

// indent puts prefix in front of every line after the first.
func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
