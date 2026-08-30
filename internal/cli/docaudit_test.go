// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"path/filepath"
	"testing"

	"github.com/renesugar/borge/internal/docs"
)

// TestDocAuditIsClean runs the documentation audit over this repository.
//
// It lives here rather than in internal/docs because the topic list belongs to this
// package: the audit must ask the code which topics exist instead of keeping a list that
// can disagree with it. That is the same rule the coverage gates follow - ask the other
// tool, not a list you wrote.
//
// This is the gate. cmd/docaudit prints the same report for a human.
func TestDocAuditIsClean(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	set, err := docs.Parse(root)
	if err != nil {
		t.Fatalf("parsing anchors: %v", err)
	}
	// A parse that read nothing, or found no anchors, would report a clean audit - which
	// is the most misleading thing it could do.
	if set.Files < 50 {
		t.Fatalf("the audit scanned %d Go files, which is not this repository", set.Files)
	}
	if len(set.Blocks) == 0 {
		t.Fatal("the audit found no anchored documentation at all")
	}
	if len(set.Checks) == 0 {
		t.Fatal("the audit found no registered checks at all")
	}

	report := docs.Audit(set, HelpTopicNames(), EnumerationNames())
	for _, finding := range report.Errors() {
		t.Errorf("%s", finding)
	}
	// Warnings do not fail: a topic without an anchored fragment is a gap to close, not a
	// break. They are printed so the gap stays visible.
	for _, finding := range report.Findings {
		if finding.Severity == docs.SeverityWarning {
			t.Logf("%s", finding)
		}
	}
	t.Logf("\n%s", report.Format())
}
