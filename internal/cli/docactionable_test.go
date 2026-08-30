// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/doccheck"
)

// docactionable: can a reader turn this topic into a working command?
//
// plans/PORTING_PLAN.md 2.1.2 makes the argument. A true sentence can be useless - "borge
// supports patterns" is unfalsifiable and unactionable - so the useful test of help text is
// constructive: produce a command from the prose alone and run it. A topic that yields a
// working command is actionable. A topic that yields one which fails to parse, or does
// something else, is vague or wrong, and the generated attempt is itself the bug report:
// it shows what a careful reader concluded.
//
// This lives in internal/cli rather than in a command because the harness it needs is here:
// helpFixture builds a scratch repository with an archive named ARCHIVE, one matching
// sh:daily-*, one tagged temporary, and a source tree with a .cache to exclude and an
// invoice to find. Reusing it is the whole reason T7 came after T6.
//
// Advisory, and skipped unless BORGE_DOCCHECK_URL names a running model server.

// actionableCase is one labelled example: a topic as it stood at a commit, a task, and
// whether a reader following that prose lands on a working command.
type actionableCase struct {
	ID string `json:"id"`
	// Actionable is the answer: false for the three commands the topics documented and
	// that did not work, true for the same topics after correction.
	Actionable bool   `json:"actionable"`
	Task       string `json:"task"`
	Why        string `json:"why"`
	// Expect names what to look at. Exit status is not the assertion: the exclude case
	// exits in a way that looks like success and stores the data anyway.
	Expect string `json:"expect"`
	// Probe is the command the prose leads to, without the "borge " prefix, and
	// ProbeWorks whether it still works today. Empty where the prose documents no
	// command at all, which is its own kind of unactionable.
	Probe      string `json:"probe"`
	ProbeWorks bool   `json:"probe_works"`
	Topic      struct {
		Commit    string `json:"commit"`
		File      string `json:"file"`
		FirstLine int    `json:"first_line"`
		LastLine  int    `json:"last_line"`
		Text      string `json:"text"`
	} `json:"topic"`
}

func loadActionableCases(t *testing.T) []actionableCase {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("testdata", "actionable", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	var out []actionableCase
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var c actionableCase
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatal("no calibration cases in testdata/actionable")
	}
	return out
}

// TestActionableCalibrationMatchesGit re-reads every case out of git.
//
// The same guard as doccheck's, for the same reason: a case someone typed can be typed to
// agree with whatever the checker says. Here it also pins the line ranges, because a range
// that has drifted is how the mistake below happened.
func TestActionableCalibrationMatchesGit(t *testing.T) {
	for _, c := range loadActionableCases(t) {
		t.Run(c.ID, func(t *testing.T) {
			out, err := exec.Command("git", "show", c.Topic.Commit+":"+c.Topic.File).Output()
			if err != nil {
				t.Fatalf("git show %s:%s: %v", c.Topic.Commit, c.Topic.File, err)
			}
			lines := strings.SplitAfter(string(out), "\n")
			if c.Topic.LastLine > len(lines) {
				t.Fatalf("%s at %s has %d lines, the case names %d",
					c.Topic.File, c.Topic.Commit, len(lines), c.Topic.LastLine)
			}
			got := strings.Join(lines[c.Topic.FirstLine-1:c.Topic.LastLine], "")
			if got != c.Topic.Text {
				t.Errorf("lines %d-%d of %s at %s are not this case's text\n--- git ---\n%s",
					c.Topic.FirstLine, c.Topic.LastLine, c.Topic.File, c.Topic.Commit, got)
			}
			if !strings.Contains(got, "borge help ") {
				t.Errorf("the range does not start at a help topic; it reads:\n%s", got[:min(200, len(got))])
			}
		})
	}
}

// TestActionablePairsDiffer catches a vacuous calibration set.
//
// Written because the first version of this set had it: the match-archives before and
// after cases named line ranges from a file that had grown, so both halves extracted the
// same 1637 characters and neither contained the sentence the pair is about. A pair whose
// halves are identical measures nothing and passes every checker, which is the failure
// mode this whole exercise keeps finding by other routes.
func TestActionablePairsDiffer(t *testing.T) {
	byTask := map[string][]actionableCase{}
	for _, c := range loadActionableCases(t) {
		byTask[c.Task] = append(byTask[c.Task], c)
	}
	// Two, deliberately, and the plan says why: the other pairs 2.1.2 named stopped
	// discriminating when borge's flag handling was fixed. A small honest set beats a
	// larger one whose extra cases every checker gets right.
	if len(byTask) < 2 {
		t.Fatalf("%d distinct task(s); the set needs at least two", len(byTask))
	}
	for task, pair := range byTask {
		if len(pair) != 2 {
			t.Errorf("task %q has %d case(s), want a before and an after", task, len(pair))
			continue
		}
		if pair[0].Actionable == pair[1].Actionable {
			t.Errorf("task %q has two cases with the same answer; it is not a pair", task)
		}
		if pair[0].Topic.Text == pair[1].Topic.Text {
			t.Errorf("task %q: the before and after topics are byte-identical, so the "+
				"pair measures nothing", task)
		}
	}
}

// ---------------------------------------------------------------------------
// Generating a command from the prose, and running it
// ---------------------------------------------------------------------------

// generateSystem is deliberately plain. The point of the exercise is to measure the
// topic, so the instructions must not supply what the topic fails to: no borge
// vocabulary, no worked examples, no hints about option order.
const generateSystem = "You are a precise command-line assistant. Read the manual page and " +
	"write the single command line that performs the task. Reply with the command and " +
	"nothing else: no explanation, no markdown, no shell prompt."

// fenced strips a markdown code fence, which the model adds however firmly it is told not
// to. Tolerating it measures the prose rather than the model's formatting.
func fenced(out string) string {
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "```") {
		body := out[3:]
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = body[i+1:]
		}
		if i := strings.Index(body, "```"); i >= 0 {
			body = body[:i]
		}
		out = body
	}
	return firstCommandLine(out)
}

// The two rewrites a generated line gets, each with its reason, because every one of them
// is a step away from running what the model actually wrote.

// backticked strips the single backticks the model wraps a command in. Formatting, not
// content: a line inside backticks is still the command it proposed.
func backticked(line string) string {
	return strings.Trim(strings.TrimSpace(line), "`")
}

// deBorg rewrites a leading "borg " to "borge ".
//
// The model writes "borg" for the tool however plainly the manual page in front of it says
// "borge" - the same problem doccheck found, where a word no training corpus contains is
// the one the model will not reproduce. Without this rewrite every case fails on the
// missing letter and the set measures the tokenizer instead of the prose: the first run
// scored 3/6 with every case rejected before its command was ever executed.
//
// It rewrites only the program name at the start of the line. A "borg" appearing in an
// argument is left alone, because there it might be what the topic meant.
func deBorg(line string) string {
	if rest, ok := strings.CutPrefix(line, "borg "); ok {
		return "borge " + rest
	}
	// The same, after any leading NAME=VALUE assignments.
	if i := strings.Index(line, " borg "); i > 0 && envLeads(line) {
		return line[:i] + " borge " + line[i+len(" borg "):]
	}
	return line
}

// firstCommandLine takes the first line that looks like a borge invocation.
//
// A model that adds a sentence before the command has still produced a command, and
// scoring that as unactionable would blame the topic for the model's manners.
func firstCommandLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$"))
		line = deBorg(backticked(line))
		if strings.HasPrefix(line, "borge ") || envLeads(line) {
			return line
		}
	}
	return deBorg(backticked(strings.Split(strings.TrimSpace(out), "\n")[0]))
}

// envLeads reports whether a line is one or more NAME=VALUE assignments then borge.
func envLeads(line string) bool {
	fields := strings.Fields(line)
	for _, f := range fields {
		if envAssignment.MatchString(f) {
			continue
		}
		return f == "borge" || f == "borg"
	}
	return false
}

// attempt is what happened when a generated command was run.
type attempt struct {
	command  string
	args     []string
	parseErr error
	exit     int
	stdout   string
	stderr   string
}

// runGenerated substitutes the fixture's paths into a generated command and runs it.
//
// It runs in the fixture's working directory, as TestHelpExamplesRun does. Without that,
// "borge extract" writes into whatever directory the test process happens to be in - which
// is the package source directory, so the extraction succeeds, the files land in the tree,
// and the check that looks in the fixture finds nothing and reports the topic unactionable.
// That is exactly what happened on the first run of the report below.
func runGenerated(f *helpFixture, command string) attempt {
	f.t.Chdir(f.work)
	a := attempt{command: command}
	tokens, err := splitDocCommand(command)
	if err != nil {
		a.parseErr = err
		return a
	}
	// A leading NAME=VALUE is how the environment topic's examples are written, so a
	// command generated from that topic will carry them too.
	extra := map[string]string{}
	for len(tokens) > 0 && envAssignment.MatchString(tokens[0]) {
		name, value, _ := strings.Cut(tokens[0], "=")
		extra[name] = f.substitute(value)
		tokens = tokens[1:]
	}
	if len(tokens) == 0 || tokens[0] != "borge" {
		a.parseErr = errNotBorge
		return a
	}
	for _, tok := range tokens[1:] {
		a.args = append(a.args, f.substitute(tok))
	}
	a.stdout, a.stderr, a.exit = f.run(extra, a.args...)
	return a
}

var errNotBorge = fmt.Errorf("the generated line does not invoke borge")

// actionableChecks says what each task's command has to have done.
//
// Keyed by the task rather than by the case, so a pair's two halves are judged by exactly
// the same standard - which is the only way the pair means anything.
//
// None of these is an exit code. The exclude case is the reason: the wrong command exits
// non-zero *and* stores the data, and a checker reading only the status would call the
// broken form a failure for the right reason by luck, while a variant that exited 0 and
// stored it anyway would sail through.
var actionableChecks = map[string]func(f *helpFixture, a attempt) (bool, string){
	"patterns-find": func(f *helpFixture, a attempt) (bool, string) {
		if a.exit != ExitOK {
			return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
		}
		if !strings.Contains(a.stdout, "invoice-2026-01.pdf") {
			return false, "ran, but did not find the invoice: " + oneLine(a.stdout)
		}
		return true, "found the invoice"
	},
	"patterns-exclude": func(f *helpFixture, a attempt) (bool, string) {
		if a.exit != ExitOK {
			return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
		}
		// Whichever archive it made, no archive in the repository may hold a .cache
		// entry. Naming one would let a command that archived under a different name
		// pass by not being looked at.
		for _, name := range f.archiveNames() {
			for _, p := range f.archivePaths(name) {
				if strings.Contains(p, "/.cache/") {
					return false, "the archive " + name + " contains " + p
				}
			}
		}
		return true, "no .cache entry in any archive"
	},
	"environment-repo": func(f *helpFixture, a attempt) (bool, string) {
		if a.exit != ExitOK {
			return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
		}
		// The task is "without passing -r", so using it is not doing what was asked even
		// if the listing is right.
		for _, arg := range a.args {
			if arg == "-r" || arg == "--repo" {
				return false, "listed archives, but with -r rather than the environment"
			}
		}
		// Which repository the command names is the topic's business, not this check's:
		// the example the topic gives points at /backups/{hostname}, so requiring a
		// particular archive would fail the correct command. What matters is that it
		// listed something.
		if strings.TrimSpace(a.stdout) == "" {
			return false, "ran, but listed nothing"
		}
		return true, "listed archives from the repository named in the environment"
	},
	"match-archives-tag": func(f *helpFixture, a attempt) (bool, string) {
		if a.exit != ExitOK {
			return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
		}
		out, _, code := f.run(nil, "info", "-a", "ARCHIVE")
		if code != ExitOK {
			return false, "the command exited 0 but ARCHIVE cannot be read"
		}
		if !strings.Contains(out, "@PROT") {
			return false, "ran, but ARCHIVE does not carry @PROT"
		}
		return true, "ARCHIVE carries @PROT"
	},
}

// taskKey is a case id without its -before or -after half.
func taskKey(id string) string {
	for _, suffix := range []string{"-before", "-after"} {
		if strings.HasSuffix(id, suffix) {
			return strings.TrimSuffix(id, suffix)
		}
	}
	return id
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// TestEveryActionableCaseHasACheck. A case with no check would be scored by nothing and
// counted as if it had been measured.
func TestEveryActionableCaseHasACheck(t *testing.T) {
	for _, c := range loadActionableCases(t) {
		if actionableChecks[taskKey(c.ID)] == nil {
			t.Errorf("case %s has no check for task %q, so running it would prove nothing",
				c.ID, taskKey(c.ID))
		}
	}
}

// TestDocActionableIsCalibrated scores the model against the labelled topics.
//
// Skipped unless BORGE_DOCCHECK_URL names a running llama.cpp server; see AGENTS.md for
// the command. When it does run it is a gate on the *checker*, never on the documentation:
// a model that cannot tell the three broken topics from their corrections has nothing to
// say about the topics nobody has checked by hand.
//
// The baseline to beat is answering the same way every time, which scores 3 of 6 on a set
// built as pairs. That is a low bar and passing it is not the same as being useful.
func TestDocActionableIsCalibrated(t *testing.T) {
	url := os.Getenv("BORGE_DOCCHECK_URL")
	if url == "" {
		t.Skip("set BORGE_DOCCHECK_URL to score the model; see AGENTS.md for the server")
	}
	ctx := context.Background()
	model := doccheck.NewLlamaServer(url)
	if err := model.Probe(ctx); err != nil {
		t.Fatalf("BORGE_DOCCHECK_URL is set and there is no server behind it: %v", err)
	}

	cases := loadActionableCases(t)
	correct, agreed := 0, map[bool]int{}
	for _, c := range cases {
		check := actionableChecks[taskKey(c.ID)]
		if check == nil {
			t.Errorf("case %s has no check", c.ID)
			continue
		}
		generated, err := model.Ask(ctx, generateSystem,
			"MANUAL PAGE:\n"+c.Topic.Text+"\n\nTASK: "+c.Task+"\n\nCommand:", 120, "")
		if err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		command := fenced(generated)

		// A repository of its own per case, so one case cannot leave an archive or a tag
		// that decides the next one.
		f := newHelpFixture(t)
		a := runGenerated(f, command)
		got, why := false, ""
		switch {
		case a.parseErr != nil:
			why = a.parseErr.Error()
		default:
			got, why = check(f, a)
		}
		agreed[got]++
		mark := "XX"
		if got == c.Actionable {
			mark = "ok"
			correct++
		}
		t.Logf("%s want=actionable:%-5v got=actionable:%-5v %s\n    generated: %s\n    outcome:   %s",
			mark, c.Actionable, got, c.ID, command, why)
	}

	// A checker that answered one way for everything would score the number of cases
	// carrying that answer; the set is pairs, so that is half.
	baseline := len(cases) / 2
	t.Logf("docactionable: %d/%d correct (answering one way every time scores %d/%d)",
		correct, len(cases), baseline, len(cases))
	if agreed[true] == len(cases) || agreed[false] == len(cases) {
		t.Errorf("the checker answered the same way for all %d cases; it is measuring "+
			"nothing about the prose", len(cases))
	}
	if correct <= baseline {
		t.Errorf("this model scores %d/%d, which answering one way every time also does "+
			"(%d/%d); it cannot tell a broken topic from its correction",
			correct, len(cases), baseline, len(cases))
	}
}

// TestGeneratedLineIsCleanedUp pins the two rewrites, because a rewrite that went wrong
// would silently turn a working command into a rejected one - which is exactly what the
// first calibration run did, scoring the baseline with every case rejected unread.
func TestGeneratedLineIsCleanedUp(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"`borge tag --add @PROT ARCHIVE`", "borge tag --add @PROT ARCHIVE"},
		{"borg create -r REPO --exclude 'sh:**/.cache' archive ~",
			"borge create -r REPO --exclude 'sh:**/.cache' archive ~"},
		{"```sh\nborg find 'sh:**/invoice-*.pdf'\n```", "borge find 'sh:**/invoice-*.pdf'"},
		{"Here is the command:\n$ borge list ARCHIVE", "borge list ARCHIVE"},
		// A "borg" that is not the program name stays, because there the topic may have
		// meant it - borge's help talks about borg repositories.
		{"borge transfer --other-repo borg-repo", "borge transfer --other-repo borg-repo"},
	} {
		if got := fenced(tc.raw); got != tc.want {
			t.Errorf("fenced(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestActionableCasesStillDiscriminate runs each case's documented command against today's
// borge and requires it to behave the way the case says.
//
// This is the guard the set was missing, and it was missing it for exactly one day. The
// first version had three pairs, taken from plans/PORTING_PLAN.md 2.1.2 - and two of them
// were no longer pairs. "borge tag ARCHIVE --add @PROT" and "borge create ... ~ --exclude
// ..." were broken by the flag-order defect of DIVERGENCES #20, and args.go's permute has
// since fixed it, so both now work. The prose was corrected in 2026-08 and then the program
// caught up with the prose that had been wrong.
//
// That is a different decay from the one the doc anchors are about. There, prose goes stale
// while the code stays right. Here, a labelled example goes stale because the code was
// *fixed* - and a calibration case that has stopped discriminating scores the checker on
// nothing while looking exactly like a case that works.
//
// It needs no model, so it runs in the ordinary suite.
func TestActionableCasesStillDiscriminate(t *testing.T) {
	for _, c := range loadActionableCases(t) {
		if c.Probe == "" {
			// Prose that documents no command at all cannot be probed; that it
			// documents none is the finding, and TestActionablePairsDiffer holds it.
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			tokens, err := splitDocCommand(c.Probe)
			if err != nil {
				t.Fatalf("the probe does not parse: %v", err)
			}
			f := newHelpFixture(t)
			args := make([]string, 0, len(tokens))
			for _, tok := range tokens {
				args = append(args, f.substitute(tok))
			}
			_, stderr, exit := f.run(nil, args...)
			works := exit == ExitOK
			if works != c.ProbeWorks {
				t.Errorf("the case says %q %s today, and it %s (exit %d: %s)\n"+
					"This case no longer discriminates. Re-derive it or drop it; do not "+
					"leave it scoring the checker on nothing.",
					"borge "+c.Probe,
					map[bool]string{true: "works", false: "fails"}[c.ProbeWorks],
					map[bool]string{true: "works", false: "fails"}[works],
					exit, oneLine(stderr))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The advisory report over the topics as they stand
// ---------------------------------------------------------------------------

// topicTask is a thing a user would want to do, and what doing it has to achieve.
//
// One per topic, chosen to be what that topic exists to let someone do. The task is
// written the way a user would put it, in their words rather than borge's: naming the
// option would hand over the answer the topic is supposed to supply, and the whole
// question is whether the topic supplies it.
type topicTask struct {
	topic string
	task  string
	check func(f *helpFixture, a attempt) (bool, string)
}

func topicTasks() []topicTask {
	return []topicTask{
		{
			topic: "patterns",
			task:  "extract from the archive named ARCHIVE only the files whose names end in .txt",
			check: func(f *helpFixture, a attempt) (bool, string) {
				if a.exit != ExitOK {
					return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
				}
				got := regularFilesUnder(f.t, f.work)
				if len(got) == 0 {
					return false, "ran, but extracted nothing"
				}
				for _, p := range got {
					if !strings.HasSuffix(p, ".txt") {
						return false, "extracted " + p + ", which is not a .txt"
					}
				}
				return true, fmt.Sprintf("extracted %d file(s), all .txt", len(got))
			},
		},
		{
			topic: "match-archives",
			task:  "list only the archives whose names begin with daily-",
			check: func(f *helpFixture, a attempt) (bool, string) {
				if a.exit != ExitOK {
					return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
				}
				if !strings.Contains(a.stdout, "daily-2026-01-01") {
					return false, "ran, but did not list the daily archive"
				}
				for _, other := range []string{"tagged", "ARCHIVE"} {
					if strings.Contains(a.stdout, other) {
						return false, "listed " + other + ", which does not begin with daily-"
					}
				}
				return true, "listed the daily archive and nothing else"
			},
		},
		{
			topic: "placeholders",
			task:  "create an archive of the home directory whose name is the host name",
			check: func(f *helpFixture, a attempt) (bool, string) {
				if a.exit != ExitOK {
					return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
				}
				for _, name := range f.archiveNames() {
					// The fixture's own host-named archive ends in "-example", so a
					// bare host name can only have come from this command.
					if name == f.hostname {
						return true, "created an archive named " + name
					}
				}
				return false, "ran, but no archive is named after the host: " +
					strings.Join(f.archiveNames(), " ")
			},
		},
		{
			topic: "compression",
			task:  "create an archive of the home directory using zstd compression at level 10",
			check: func(f *helpFixture, a attempt) (bool, string) {
				if a.exit != ExitOK {
					return false, fmt.Sprintf("exit %d: %s", a.exit, oneLine(a.stderr))
				}
				// Exit status says nothing: an unrecognised -C would have been rejected,
				// but a command that quietly used the default would also exit 0. What is
				// checked is that an archive appeared that the fixture did not build.
				for _, name := range f.archiveNames() {
					if !fixtureArchive(f, name) {
						return true, "created " + name
					}
				}
				return false, "ran, but created no archive"
			},
		},
		{
			topic: "environment",
			task:  "list the archives in a repository without passing it on the command line",
			check: actionableChecks["environment-repo"],
		},
	}
}

// fixtureArchive reports whether an archive is one newHelpFixture made.
func fixtureArchive(f *helpFixture, name string) bool {
	for _, known := range []string{"ARCHIVE", "daily-2026-01-01", "tagged", f.hostname + "-example"} {
		if name == known {
			return true
		}
	}
	return false
}

// TestDocActionableReport is the advisory pass over the topics as they stand.
//
// It asks, for each topic, whether a reader given only that topic can produce a command
// that does the job - and it answers by running what comes back. A failure here is a
// finding about the documentation, or about the model, and telling those apart is the
// reader's work: the generated command is printed so it can be judged.
//
// It never fails the build. What it reports is not reliable enough to gate on, and
// TestDocActionableIsCalibrated is the measurement that says how much to trust it.
func TestDocActionableReport(t *testing.T) {
	url := os.Getenv("BORGE_DOCCHECK_URL")
	if url == "" {
		t.Skip("set BORGE_DOCCHECK_URL to run the actionability report; see AGENTS.md")
	}
	ctx := context.Background()
	model := doccheck.NewLlamaServer(url)
	if err := model.Probe(ctx); err != nil {
		t.Fatalf("BORGE_DOCCHECK_URL is set and there is no server behind it: %v", err)
	}

	topics := helpTopics()
	byName := map[string]string{}
	for _, topic := range topics {
		byName[topic.name] = topic.body
	}
	tasks := topicTasks()
	// Every topic gets a task. A topic quietly left out would be reported as nothing
	// wrong, which is the failure this whole plan keeps finding by new routes.
	if len(tasks) != len(topics) {
		t.Fatalf("%d task(s) for %d topic(s); every topic needs one", len(tasks), len(topics))
	}

	actionable := 0
	for _, tt := range tasks {
		body, ok := byName[tt.topic]
		if !ok {
			t.Fatalf("there is no topic named %q", tt.topic)
		}
		generated, err := model.Ask(ctx, generateSystem,
			"MANUAL PAGE:\n"+body+"\n\nTASK: "+tt.task+"\n\nCommand:", 120, "")
		if err != nil {
			t.Fatalf("%s: %v", tt.topic, err)
		}
		command := fenced(generated)
		f := newHelpFixture(t)
		a := runGenerated(f, command)
		got, why := false, ""
		if a.parseErr != nil {
			why = a.parseErr.Error()
		} else {
			got, why = tt.check(f, a)
		}
		if got {
			actionable++
		}
		t.Logf("%-15s %s\n    task:      %s\n    generated: %s\n    outcome:   %s",
			tt.topic, map[bool]string{true: "actionable", false: "NOT actionable"}[got],
			tt.task, command, why)
	}
	t.Logf("docactionable: %d of %d topic(s) yielded a working command", actionable, len(tasks))
}
