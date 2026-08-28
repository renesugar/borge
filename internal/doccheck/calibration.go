// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The calibration set.
//
// A checker with no known-answer set is a checker whose silence means nothing, and this
// one is non-deterministic, so its silence is the only thing it produces most of the time.
// The cases are real claims from this repository's history: prose that was true, went
// false, and was corrected, paired with the code it is about.
//
// They are extracted from git by scripts/build-doccheck-calibration.py and verified
// against git by TestCalibrationMatchesGit, so a case cannot be edited into agreeing with
// the checker and cannot describe a change that never happened. That second guard is not
// hypothetical: the table in docs/PORTING_PLAN.md 2.1.1 listed a placeholders before/after
// pair, and git has no trace of it. Building the set from history is what found that.

// Verdict is what the checker can say about a claim.
type Verdict string

const (
	// VerdictSupported - the reading of the code says what the claim says.
	VerdictSupported Verdict = "supported"
	// VerdictContradicted - the reading denies the claim. "Look at this", not "this is
	// wrong": the reading can be wrong too.
	VerdictContradicted Verdict = "contradicted"
	// VerdictNotDeterminable - the reading neither confirms nor denies it. Rationale
	// lands here by design, and so does everything the checker is unsure of.
	VerdictNotDeterminable Verdict = "not-determinable"
)

// Source is a piece of text with the commit and path it came from.
type Source struct {
	Commit    string `json:"commit"`
	File      string `json:"file"`
	FirstLine int    `json:"first_line"`
	LastLine  int    `json:"last_line"`
	Text      string `json:"text"`
}

// Case is one labelled example: a claim, the code it is about, and the answer.
type Case struct {
	ID      string  `json:"id"`
	Verdict Verdict `json:"verdict"`
	// Why is the argument for the label, so a disagreement with the checker is a
	// disagreement about something stated rather than about an unexplained answer.
	Why   string `json:"why"`
	Claim Source `json:"claim"`
	// Code is the unit the claim is judged against, in the order it is shown. It is a
	// list because a unit sometimes spans files - a function and the caller that does
	// not reach it - and every part keeps its own commit, path and line range so that
	// all of it can be checked against git.
	Code []Source `json:"code"`
	Unit string   `json:"unit"`
}

// CodeText is the unit as one piece of source.
func (c Case) CodeText() string {
	parts := make([]string, 0, len(c.Code))
	for _, s := range c.Code {
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, "\n")
}

// ClaimProse is the claim with Go comment markers removed.
//
// The checker is given prose, not comment syntax; the raw text is kept in Claim.Text
// because that is what git can be asked to confirm.
func (c Case) ClaimProse() string {
	return StripCommentMarkers(c.Claim.Text)
}

var commentMarker = regexp.MustCompile(`(?m)^[ \t]*//[ \t]?`)

// StripCommentMarkers removes the leading "// " from every line that has one.
func StripCommentMarkers(text string) string {
	return strings.TrimSpace(commentMarker.ReplaceAllString(text, ""))
}

// LoadCalibration reads the case files under dir.
func LoadCalibration(dir string) ([]Case, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []Case
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var c Case
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if c.ID == "" || c.Verdict == "" || c.Claim.Text == "" || len(c.Code) == 0 {
			return nil, fmt.Errorf("%s: incomplete case", name)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no calibration cases in %s", dir)
	}
	return out, nil
}

// Score is how a run of the checker did against the calibration set.
type Score struct {
	Total   int
	Correct int
	// Confusion counts want -> got, so a checker that answers one label for everything
	// is visible as such rather than as an accuracy number.
	Confusion map[[2]Verdict]int
	// Baseline is the score of always answering the set's most common label. A checker
	// that does not beat it has learned nothing.
	Baseline int
}

// ScoreRuns builds a Score from the answers a checker gave, in case order.
func ScoreRuns(cases []Case, got []Verdict) Score {
	s := Score{Total: len(cases), Confusion: map[[2]Verdict]int{}}
	counts := map[Verdict]int{}
	for i, c := range cases {
		counts[c.Verdict]++
		if i >= len(got) {
			continue
		}
		s.Confusion[[2]Verdict{c.Verdict, got[i]}]++
		if got[i] == c.Verdict {
			s.Correct++
		}
	}
	for _, n := range counts {
		if n > s.Baseline {
			s.Baseline = n
		}
	}
	return s
}

// Format renders a score for a person.
func (s Score) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d correct (always answering the commonest label scores %d/%d)\n",
		s.Correct, s.Total, s.Baseline, s.Total)
	keys := make([][2]Verdict, 0, len(s.Confusion))
	for k := range s.Confusion {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	b.WriteString("want -> got:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-17s -> %-17s %d\n", k[0], k[1], s.Confusion[k])
	}
	return b.String()
}
