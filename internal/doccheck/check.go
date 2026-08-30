// SPDX-License-Identifier: Apache-2.0

// Package doccheck asks whether borge's code contradicts borge's documentation.
//
// It is the advisory half of the documentation system: internal/docs says what is anchored
// and how it is verified, and this package attacks what is left - prose that no test
// reaches. It reads the anchored code with the doc comment withheld, then compares that
// reading with the prose, and reports the disagreements for a human to triage.
//
// It is never a gate. The answers move when the model moves, and a build that failed on
// them would fail for reasons nobody could reproduce.
//
// # Read the calibration score before believing anything here
//
// The set in testdata/calibration is thirteen labelled cases taken out of this
// repository's history, and ScoreRuns prints the score of a checker that answers the same
// label every time beside the real one. As of 2026-08-28 the 1.5B model this was built
// against does not beat that baseline. A checker with no known-answer set is a checker
// whose silence means nothing; this one has the set, and the set says so.
//
// plans/PORTING_PLAN.md 2.1.1 has the design, and
// plans/r2-documentation-system-20260828.md T5 has the measurements.
package doccheck

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// The contradiction check, in two steps.
//
// # Why two steps
//
// Showing the model the code and the claim together and asking "is this true?" anchors the
// reading on the claim: a model shown an assertion finds support for it. So the code is
// read first with the doc comment withheld (ExtractUnit removes it), and only then is the
// reading compared with the claim. The extra hop is lossy and buys independence, which is
// the whole value.
//
// # Why the claim is broken into statements
//
// Measured, not assumed. A whole paragraph judged in one call came back at the rate of a
// checker that answers the same label every time; the same model judging one short
// statement at a time answered correctly far more often. Documentation sentences here run
// to three clauses and a subordinate "so that", and a verdict on such a sentence is a
// verdict on whichever clause the model attended to.
//
// # Why the program's name is taken out
//
// Also measured. The model gets the polarity of statements about "borge" wrong - it is a
// word no training set contains - and gets the same statements about "the program" right.
// See TestNormaliseIsWhyTheNameGoes.

// Report is what one run produced.
type Report struct {
	Model    string
	Findings []Finding
	// Checked counts the blocks that reached the model, so a report of nothing found can
	// be told apart from a report of nothing tried.
	Checked int
}

// Finding is one claim's verdict.
type Finding struct {
	// Anchor is the //borge:help name, or the declaration when the block has none.
	Anchor  string
	File    string
	Line    int
	Unit    string
	Verdict Verdict
	// Reading is what the model said the code does, kept because a contradiction is only
	// worth triaging if the reading behind it can be judged too.
	Reading string
	// Disagreed lists the statements the reading denied, which is the part a human has
	// to look at.
	Disagreed []string
	// Truncated says the unit did not fit its budget, which devalues the verdict.
	Truncated bool
}

// Checker runs the two steps against a model.
type Checker struct {
	Model Model
}

const readSystem = "You are a careful Go reviewer. List what the code does when it runs, as " +
	"numbered short factual sentences. Say what is read, what the user is asked, what is " +
	"written and where, what happens on failure, and any exact counts or limits. Call the " +
	"subject \"the program\". Write only facts the code shows."

const splitSystem = "You rewrite documentation into a numbered list of simple statements about " +
	"what a program does. One fact per line, each a short complete sentence starting with " +
	"\"The program\". Keep negations exactly as written. Leave out anything that gives a " +
	"reason, a motivation, a history, or an effect on people rather than describing " +
	"behaviour. If nothing is left, write NONE."

const judgeSystem = "You answer questions about a program from a list of facts. Answer with " +
	"one word: YES or NO."

// yesNo constrains the judging step's answer, so a verdict is never a parse of prose.
const yesNo = `root ::= "YES" | "NO"`

// Read builds the blind reading of a unit.
func (c Checker) Read(ctx context.Context, u Unit) (string, error) {
	out, err := c.Model.Ask(ctx, readSystem,
		fmt.Sprintf("Go code:\n\n```go\n%s\n```\n\nList what %s does when it runs.",
			u.Source, u.Name), 320, "")
	if err != nil {
		return "", err
	}
	return Normalise(out), nil
}

// Statements breaks a claim into the assertions about behaviour it makes.
//
// Rationale is meant to fall out here: a paragraph that gives a reason and asserts nothing
// leaves no statements, and a claim with no statements is not determinable rather than
// wrong. That is the mechanism the "exclude rationale" rule is implemented by, rather than
// a list of blocks somebody remembered to exclude.
func (c Checker) Statements(ctx context.Context, claim string) ([]string, error) {
	out, err := c.Model.Ask(ctx, splitSystem,
		"DOCUMENTATION:\n"+Normalise(claim)+"\n\nSimple statements:", 280, "")
	if err != nil {
		return nil, err
	}
	return parseStatements(out), nil
}

var listMarker = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*])\s*`)

// MaxStatements caps one claim's decomposition.
//
// A small model asked to rewrite a long paragraph can fall into a loop: the first run of
// this over the placeholders topic produced "The program formats dates according to the
// machine's locale" twenty-two times, and every copy cost a judging call and a line in the
// triage list. A claim that genuinely makes more assertions than this is a claim that
// should be several fragments.
const MaxStatements = 12

// parseStatements takes the decomposition apart, dropping repeats.
//
// Duplicates are not merely noise. The verdict is "contradicted if any statement is
// denied", so one looped statement that the reading happens to deny counts once - but it
// crowds out the statements that would have been judged, because the cap is finite.
func parseStatements(out string) []string {
	var stmts []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(listMarker.ReplaceAllString(line, ""))
		if len(line) < 20 || strings.Contains(strings.ToUpper(line), "NONE") {
			continue
		}
		key := strings.ToLower(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		stmts = append(stmts, line)
		if len(stmts) == MaxStatements {
			break
		}
	}
	return stmts
}

// Judge asks whether the reading bears out one statement.
func (c Checker) Judge(ctx context.Context, reading, statement string) (bool, error) {
	answer, err := c.Model.Ask(ctx, judgeSystem,
		fmt.Sprintf("FACTS:\n%s\n\nQUESTION: According to the facts above, is this "+
			"statement true?\nSTATEMENT: %s\nANSWER:", reading, statement), 4, yesNo)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "YES"), nil
}

// CheckClaim runs both steps over one claim and its unit.
func (c Checker) CheckClaim(ctx context.Context, u Unit, claim string) (Verdict, string, []string, error) {
	reading, err := c.Read(ctx, u)
	if err != nil {
		return "", "", nil, err
	}
	return c.CheckAgainstReading(ctx, reading, claim)
}

// CheckAgainstReading is CheckClaim with the reading already made, so that several claims
// about one declaration are judged against the same reading rather than against several.
func (c Checker) CheckAgainstReading(ctx context.Context, reading, claim string) (Verdict, string, []string, error) {
	stmts, err := c.Statements(ctx, claim)
	if err != nil {
		return "", reading, nil, err
	}
	if len(stmts) == 0 {
		return VerdictNotDeterminable, reading, nil, nil
	}
	var denied []string
	for _, s := range stmts {
		ok, err := c.Judge(ctx, reading, s)
		if err != nil {
			return "", reading, nil, err
		}
		if !ok {
			denied = append(denied, s)
		}
	}
	if len(denied) > 0 {
		return VerdictContradicted, reading, denied, nil
	}
	return VerdictSupported, reading, nil, nil
}

var (
	borgePossessive = regexp.MustCompile(`(?i)\bborge's\b`)
	borgeWord       = regexp.MustCompile(`(?i)\bborge\b`)
)

// Normalise replaces the program's name with "the program".
//
// This is not cosmetic. Asked whether "borge prompts for a passphrase" follows from facts
// that say it prompts, the model answers no; asked the same about "the program", it answers
// yes. The name is a word no training corpus contains, and the model's handling of a
// sentence about an unknown proper noun is not reliable enough to build a verdict on.
func Normalise(text string) string {
	text = borgePossessive.ReplaceAllString(text, "the program's")
	return borgeWord.ReplaceAllString(text, "the program")
}
