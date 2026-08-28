// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/docs"
)

// fakeModel answers from a script, so the pipeline can be tested without a server. Every
// prompt it is given is recorded, which is how the blindness of the reading step is
// asserted.
type fakeModel struct {
	reading string
	// yes decides the judging step: a statement containing one of these substrings is
	// answered YES.
	yes []string
	// split is what the decomposition step returns.
	split  string
	prompt []string
}

func (m *fakeModel) Name() string { return "fake" }

func (m *fakeModel) Ask(_ context.Context, system, user string, _ int, _ string) (string, error) {
	m.prompt = append(m.prompt, user)
	switch {
	case strings.HasPrefix(system, "You are a careful Go reviewer"):
		return m.reading, nil
	case strings.HasPrefix(system, "You rewrite documentation"):
		return m.split, nil
	default:
		for _, y := range m.yes {
			if strings.Contains(user, y) {
				return "YES", nil
			}
		}
		return "NO", nil
	}
}

// TestTheReadingStepNeverSeesTheClaim is the property the two-step design exists for.
//
// A model shown an assertion finds support for it, so the reading has to be made with the
// documentation withheld. If this ever fails, every verdict the checker produces is
// "supported" and the tool is worse than nothing, because it looks like it is working.
func TestTheReadingStepNeverSeesTheClaim(t *testing.T) {
	m := &fakeModel{
		reading: "1. The program prompts for a passphrase.",
		split:   "1. The program does not prompt for a passphrase.",
	}
	c := Checker{Model: m}
	claim := "the program does not prompt for a passphrase, a distinctive phrase"
	unit := Unit{Name: "Env.unlock", Source: "func (e *Env) unlock() {}"}
	if _, _, _, err := c.CheckClaim(context.Background(), unit, claim); err != nil {
		t.Fatal(err)
	}
	if len(m.prompt) == 0 {
		t.Fatal("the model was never asked anything")
	}
	if strings.Contains(m.prompt[0], "a distinctive phrase") {
		t.Fatalf("the reading prompt carries the claim:\n%s", m.prompt[0])
	}
}

// TestAClaimWithNoAssertionsIsNotDeterminable. Rationale is excluded by falling out of the
// decomposition, not by a list of blocks somebody remembered to skip - so an empty
// decomposition has to end the check rather than being treated as agreement.
func TestAClaimWithNoAssertionsIsNotDeterminable(t *testing.T) {
	m := &fakeModel{reading: "1. The program prompts.", split: "NONE"}
	v, _, _, err := Checker{Model: m}.CheckAgainstReading(context.Background(),
		"1. The program prompts.", "This exists because the key type is not known early.")
	if err != nil {
		t.Fatal(err)
	}
	if v != VerdictNotDeterminable {
		t.Errorf("a claim that asserts nothing came back %s, want %s", v, VerdictNotDeterminable)
	}
}

// TestOneDeniedStatementContradictsTheWholeClaim. A paragraph with one false sentence in it
// is a paragraph to look at; averaging it away is how the false sentence survives.
func TestOneDeniedStatementContradictsTheWholeClaim(t *testing.T) {
	m := &fakeModel{
		reading: "1. The program prompts three times.",
		split:   "1. The program prompts.\n2. The program prompts ten times.",
		yes:     []string{"The program prompts."},
	}
	v, _, denied, err := Checker{Model: m}.CheckAgainstReading(context.Background(),
		"reading", "claim")
	if err != nil {
		t.Fatal(err)
	}
	if v != VerdictContradicted {
		t.Fatalf("verdict %s, want %s", v, VerdictContradicted)
	}
	if len(denied) != 1 || !strings.Contains(denied[0], "ten times") {
		t.Errorf("the report does not name the statement that was denied: %v", denied)
	}
}

// TestNormaliseIsWhyTheNameGoes records the measurement the substitution rests on.
//
// Asked whether "borge prompts for a passphrase" follows from facts that say the program
// prompts, the 1.5B model answers no; asked the same about "the program", it answers yes.
// The name is a word no training corpus has seen. Without this the checker's answers on
// borge's own documentation are close to noise, so the rewriting is load-bearing rather
// than cosmetic.
func TestNormaliseIsWhyTheNameGoes(t *testing.T) {
	got := Normalise("borge does not prompt, and borge's prompt is on stderr. Borge asks.")
	want := "the program does not prompt, and the program's prompt is on stderr. the program asks."
	if got != want {
		t.Errorf("Normalise gave %q, want %q", got, want)
	}
	// A word that merely contains the name is left alone; rewriting BORGE_PASSPHRASE
	// would change what a claim about the environment says.
	if got := Normalise("BORGE_PASSPHRASE and borgebackup"); got != "BORGE_PASSPHRASE and borgebackup" {
		t.Errorf("Normalise rewrote inside a word: %q", got)
	}
}

// TestParseStatementsTakesListsApart, because the decomposition comes back as prose and a
// statement lost here is a statement never judged.
func TestParseStatementsTakesListsApart(t *testing.T) {
	got := parseStatements("1. The program reads the environment first.\n" +
		"2) The program prompts at the terminal.\n" +
		"- The program writes the prompt to stderr.\n" +
		"ok\n\n")
	if len(got) != 3 {
		t.Fatalf("parsed %d statement(s), want 3: %v", len(got), got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "1.") || strings.HasPrefix(s, "-") {
			t.Errorf("the list marker is still on the statement: %q", s)
		}
	}
}

// TestNoneMeansNoStatements. The decomposition is told to write NONE for prose that asserts
// nothing, and a NONE read as a statement would be judged and produce a verdict.
func TestNoneMeansNoStatements(t *testing.T) {
	if got := parseStatements("NONE"); len(got) != 0 {
		t.Errorf("NONE parsed as %v", got)
	}
}

// TestTargetsFollowAboutOffACarrier is the bug this package shipped with for an afternoon.
//
// borge's user-facing prose lives on "var _ = helpText" carriers, not on functions, so a
// target list built from IsFunc alone is empty - and an empty list produces a clean report
// over nothing at all. That is the worst failure a checker can have, because it looks
// exactly like success.
func TestTargetsFollowAboutOffACarrier(t *testing.T) {
	set := &docs.Set{Blocks: []docs.Block{{
		File: "internal/patterns/pattern.go", Line: 30, Decl: "_",
		Audience: "user", Topics: []string{"patterns/intro"},
		About: []string{"Matcher.Match"}, Prose: "Which files a command acts on.",
	}}}
	got := Targets(set)
	if len(got) != 1 {
		t.Fatalf("%d target(s) from a carrier with //borge:about, want 1", len(got))
	}
	if got[0].Decl != "Matcher.Match" {
		t.Errorf("the target reads %q, want the declaration the fragment is about", got[0].Decl)
	}
	if got[0].Anchor != "patterns/intro" {
		t.Errorf("the target is labelled %q, not by its help anchor", got[0].Anchor)
	}
}

// TestTargetsSkipWhatHasNoCode. A user fragment with no function and no //borge:about has
// nothing to read; the audit warns about it, and inventing a unit for it here would
// produce a verdict about the wrong code.
func TestTargetsSkipWhatHasNoCode(t *testing.T) {
	set := &docs.Set{Blocks: []docs.Block{
		{File: "a.go", Line: 1, Decl: "_", Audience: "user",
			Topics: []string{"compression/intro"}, Prose: "Prose with no code behind it."},
		{File: "a.go", Line: 9, Decl: "Helper", Audience: "user", IsFunc: true,
			Prose: "Prose on the function itself."},
		{File: "a_test.go", Line: 3, Decl: "TestHelper", Audience: "user", IsFunc: true,
			IsTest: true, Prose: "A claim is about the program, not about its tests."},
	}}
	got := Targets(set)
	if len(got) != 1 || got[0].Decl != "Helper" {
		t.Fatalf("targets = %+v; want only the function-attached fragment", got)
	}
}

// TestParseStatementsDropsRepeats. The first real run over the placeholders topic produced
// one sentence twenty-two times: a small model rewriting a long paragraph can loop, and
// every copy costs a judging call and a line in the triage list.
func TestParseStatementsDropsRepeats(t *testing.T) {
	one := "The program formats dates according to the machine's locale."
	got := parseStatements(strings.Repeat(one+"\n", 22))
	if len(got) != 1 {
		t.Fatalf("parsed %d statement(s) from one sentence repeated, want 1", len(got))
	}
}

// TestParseStatementsStopsAtTheCap, so a decomposition that runs away cannot fill the
// report on its own.
func TestParseStatementsStopsAtTheCap(t *testing.T) {
	var b strings.Builder
	for i := range MaxStatements * 3 {
		fmt.Fprintf(&b, "%d. The program does a distinct thing numbered %d.\n", i+1, i+1)
	}
	if got := parseStatements(b.String()); len(got) != MaxStatements {
		t.Errorf("parsed %d statement(s), want the cap of %d", len(got), MaxStatements)
	}
}
