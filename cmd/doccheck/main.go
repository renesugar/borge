// SPDX-License-Identifier: Apache-2.0

// Command doccheck asks a local model whether the code contradicts the documentation.
//
// It is advisory and it is not part of "make check". The reading it produces can be wrong,
// the verdicts move when the model changes, and a build that failed on either would fail
// for reasons nobody could reproduce. What it emits is a triage list: claim, anchor,
// verdict, and the reading that disagreed.
//
//	doccheck -calibrate     score the model against the labelled cases from git
//	doccheck                check the //borge:doc user blocks in this tree
//
// Run -calibrate first. A checker with no known-answer set is a checker whose silence
// means nothing, and this one is silent most of the time.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/renesugar/borge/internal/doccheck"
	"github.com/renesugar/borge/internal/docs"
)

func main() {
	root := flag.String("root", ".", "directory to read anchors from")
	url := flag.String("url", envOr("BORGE_DOCCHECK_URL", doccheck.DefaultURL),
		"llama.cpp server to ask")
	calibrate := flag.Bool("calibrate", false,
		"score the model against the labelled cases instead of checking the tree")
	only := flag.String("only", "", "check only anchors containing this substring")
	flag.Parse()

	ctx := context.Background()
	model := doccheck.NewLlamaServer(*url)
	if err := model.Probe(ctx); err != nil {
		// Not an error exit that looks like a finding: no server means the check did not
		// run, which is different from the check having found nothing.
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
		fmt.Fprintf(os.Stderr, "doccheck: start one with the command in AGENTS.md, or set "+
			"BORGE_DOCCHECK_URL\n")
		os.Exit(2)
	}
	checker := doccheck.Checker{Model: model}

	if *calibrate {
		if err := runCalibration(ctx, checker, *root); err != nil {
			fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	set, err := docs.Parse(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
		os.Exit(1)
	}
	targets := doccheck.Targets(set)
	if *only != "" {
		targets = filterTargets(targets, *only)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "doccheck: no //borge:doc user blocks on functions to check")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "doccheck: reading %d claim(s) with %s\n", len(targets), model.Name())
	report, err := checker.Run(ctx, *root, targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(report.Format())
}

func filterTargets(in []doccheck.Target, sub string) []doccheck.Target {
	var out []doccheck.Target
	for _, t := range in {
		if strings.Contains(t.Anchor, sub) || strings.Contains(t.Decl, sub) {
			out = append(out, t)
		}
	}
	return out
}

// runCalibration scores the model against the labelled cases and says what the score means.
func runCalibration(ctx context.Context, c doccheck.Checker, root string) error {
	dir := filepath.Join(root, "internal", "doccheck", "testdata", "calibration")
	cases, err := doccheck.LoadCalibration(dir)
	if err != nil {
		return err
	}
	got := make([]doccheck.Verdict, 0, len(cases))
	for _, tc := range cases {
		// The case carries its own code text, pinned to the commit it came from, so the
		// unit extractor is not in the loop: this measures the model and the prompts,
		// which is what a score is wanted for.
		reading, err := c.Read(ctx, doccheck.Unit{Name: tc.Unit, Source: tc.CodeText()})
		if err != nil {
			return err
		}
		verdict, _, _, err := c.CheckAgainstReading(ctx, reading, tc.ClaimProse())
		if err != nil {
			return err
		}
		mark := "XX"
		if verdict == tc.Verdict {
			mark = "ok"
		}
		fmt.Printf("%s  want=%-17s got=%-17s %s\n", mark, tc.Verdict, verdict, tc.ID)
		got = append(got, verdict)
	}
	score := doccheck.ScoreRuns(cases, got)
	fmt.Printf("\nmodel %s\n%s", c.Model.Name(), score.Format())
	if score.Correct <= score.Baseline {
		fmt.Println("\nThis model does not beat answering the commonest label every time. " +
			"Its verdicts carry no information and its silence carries none either.")
	}
	return nil
}

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}
