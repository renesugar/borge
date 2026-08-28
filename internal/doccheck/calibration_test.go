// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCalibrationMatchesGit re-reads every case out of git.
//
// This is the guard that makes the set worth having. A calibration case is only evidence
// if it is a thing that happened; a case someone typed can be typed to agree with whatever
// the checker says, and a case can be quietly softened when the checker gets it wrong.
// So each case names a commit, a path and a line range, and the text it carries must be
// what git holds there.
//
// It also catches the failure that motivated the guard. docs/PORTING_PLAN.md 2.1.1 lists a
// placeholders before/after pair among the five calibration cases; the placeholders topic
// did not exist before the commit named, so there is no "before" text and no such pair.
// The table was written from memory and nothing checked it, which is the exact defect the
// whole of R2 is about.
func TestCalibrationMatchesGit(t *testing.T) {
	cases := loadForTest(t)
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			checkAgainstGit(t, "claim", c.Claim)
			for i, part := range c.Code {
				checkAgainstGit(t, fmt.Sprintf("code part %d", i+1), part)
			}
		})
	}
}

// checkAgainstGit asserts that a source's text is what the commit holds at that path.
//
// The line numbers are checked too when the source names a single file: text that is still
// present but has moved is a case whose provenance record has gone stale, and a stale
// record is how a set stops being evidence.
func checkAgainstGit(t *testing.T, what string, src Source) {
	t.Helper()
	content := gitShow(t, src.Commit, src.File)
	if !strings.Contains(content, src.Text) {
		t.Fatalf("%s: the text in this case is not in %s at %s\n--- case text ---\n%s",
			what, src.File, src.Commit, src.Text)
	}
	if src.FirstLine > 0 {
		lines := strings.SplitAfter(content, "\n")
		if src.LastLine > len(lines) {
			t.Fatalf("%s: %s at %s has %d lines, the case names %d",
				what, src.File, src.Commit, len(lines), src.LastLine)
		}
		got := strings.Join(lines[src.FirstLine-1:src.LastLine], "")
		if got != src.Text {
			t.Errorf("%s: lines %d-%d of %s at %s are not this case's text; the provenance "+
				"record has gone stale\n--- git ---\n%s\n--- case ---\n%s",
				what, src.FirstLine, src.LastLine, src.File, src.Commit, got, src.Text)
		}
	}
}

func gitShow(t *testing.T, commit, file string) string {
	t.Helper()
	out, err := exec.Command("git", "show", commit+":"+file).Output()
	if err != nil {
		t.Fatalf("git show %s:%s: %v", commit, file, err)
	}
	return string(out)
}

// TestCalibrationIsBalanced. A set that is nearly all one label makes a constant checker
// look good, which is how a useless checker gets believed.
func TestCalibrationIsBalanced(t *testing.T) {
	cases := loadForTest(t)
	counts := map[Verdict]int{}
	for _, c := range cases {
		counts[c.Verdict]++
	}
	for _, v := range []Verdict{VerdictSupported, VerdictContradicted, VerdictNotDeterminable} {
		if counts[v] < 3 {
			t.Errorf("only %d case(s) labelled %s; the set cannot measure that answer", counts[v], v)
		}
	}
	most := 0
	for _, n := range counts {
		if n > most {
			most = n
		}
	}
	if float64(most)/float64(len(cases)) > 0.5 {
		t.Errorf("the commonest label is %d of %d cases, so a constant checker scores %.0f%%",
			most, len(cases), 100*float64(most)/float64(len(cases)))
	}
}

// TestCalibrationSubjectsAreNotAllTheSame. Every prompting case can be got right by a
// checker that has learned one word. At least one pair has to be about something else.
func TestCalibrationSubjectsAreNotAllTheSame(t *testing.T) {
	cases := loadForTest(t)
	prompting := 0
	for _, c := range cases {
		if strings.Contains(strings.ToLower(c.ClaimProse()), "passphrase") {
			prompting++
		}
	}
	if prompting == len(cases) {
		t.Error("every case is about passphrases; the set cannot tell a checker from a keyword")
	}
}

func loadForTest(t *testing.T) []Case {
	t.Helper()
	cases, err := LoadCalibration("testdata/calibration")
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestModelIsCalibrated scores the model against the labelled cases.
//
// Skipped unless BORGE_DOCCHECK_URL names a running llama.cpp server, because the checker
// is advisory and a test that needs a GPU is not something "go test ./..." can require.
// When it does run it is a gate on the *checker*, not on the documentation: a model that
// cannot separate the before-and-after pairs is not ready, and its silence on the rest of
// the tree means nothing.
//
// The threshold is the constant-answer baseline. It is deliberately weak - beating "always
// say contradicted" is the least a checker can do - and passing it is not the same as being
// useful.
func TestModelIsCalibrated(t *testing.T) {
	url := os.Getenv("BORGE_DOCCHECK_URL")
	if url == "" {
		t.Skip("set BORGE_DOCCHECK_URL to score the model; see AGENTS.md for the server")
	}
	ctx := context.Background()
	model := NewLlamaServer(url)
	if err := model.Probe(ctx); err != nil {
		t.Fatalf("BORGE_DOCCHECK_URL is set and there is no server behind it: %v", err)
	}
	c := Checker{Model: model}
	cases := loadForTest(t)
	got := make([]Verdict, 0, len(cases))
	for _, tc := range cases {
		reading, err := c.Read(ctx, Unit{Name: tc.Unit, Source: tc.CodeText()})
		if err != nil {
			t.Fatal(err)
		}
		v, _, _, err := c.CheckAgainstReading(ctx, reading, tc.ClaimProse())
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
		t.Logf("want=%-17s got=%-17s %s", tc.Verdict, v, tc.ID)
	}
	score := ScoreRuns(cases, got)
	t.Logf("model %s\n%s", model.Name(), score.Format())
	if score.Correct <= score.Baseline {
		t.Errorf("this model scores %d/%d, which answering the commonest label every time "+
			"also does (%d/%d); its verdicts carry no information",
			score.Correct, score.Total, score.Baseline, score.Total)
	}
}
