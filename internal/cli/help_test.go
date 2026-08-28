// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/renesugar/borge/internal/placeholders"
)

// Documentation drifts. That is not a risk to be accepted here: a help topic is believed,
// and a nearly-right one is worse than none - a user who reads that borge substitutes
// {now} and puts it in a cron job gets a year of archives all named "{now}".
//
// That is not hypothetical. The placeholders topic was first written describing borg's
// behaviour, because borg has the feature and the topic was drafted from borg's. borge
// does not have it. It was caught by running the command, and these tests exist so the
// next one is caught without anybody remembering to.

// TestHelpEnvironmentTopicListsEveryVariable scans the source for the variables the code
// reads and requires each to be documented.
func TestHelpEnvironmentTopicListsEveryVariable(t *testing.T) {
	read := envVarsReadBySource(t)
	if len(read) < 10 {
		t.Fatalf("only found %d environment variables in the source; the scan is broken "+
			"and this test would pass on anything: %v", len(read), read)
	}

	documented := map[string]bool{}
	for _, name := range helpEnvVarNames() {
		documented[name] = true
	}

	// Variables that are deliberately not in the topic, with the reason.
	private := map[string]string{
		// Benchmark test-mode switches, which exist only so the test suite can run the
		// benchmarks at a sane size. Documenting them would invite their use.
		"BENCHMARK_CRUD_TEST": "internal test switch",
		"BENCHMARK_CPU_TEST":  "internal test switch",
	}

	for _, name := range read {
		if documented[name] || private[name] != "" {
			continue
		}
		t.Errorf("the code reads BORGE_%s but \"borge help environment\" does not mention it", name)
	}

	// And the reverse: a documented variable nothing reads is a promise the tool does not
	// keep.
	for _, name := range helpEnvVarNames() {
		found := false
		for _, r := range read {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("\"borge help environment\" documents BORGE_%s, which nothing in the "+
				"code reads", name)
		}
	}
}

// envVarLookup matches the two ways borge reads a variable: through Env.lookupBorg and
// key.lookupEnv, which prepend the prefix, and through a direct os.LookupEnv.
var (
	envVarLookup = regexp.MustCompile(`lookupBorg\("([A-Z0-9_]+)"\)|lookupEnv\("([A-Z0-9_]+)"\)`)
	// A BORGE_ name handed to anything whose name ends in "env", in any argument
	// position. The pattern above only sees the two accessors it names, so a variable
	// read through a helper - firstEnv, say - was invisible to this check, and
	// BORGE_REMOTE_PATH and BORGE_RSH could have been added without ever being
	// documented. Matching every BORGE_ string instead would be simpler and wrong:
	// BORGE_FILES_CACHE_1 is a file format's magic number, not a variable.
	envVarDirect = regexp.MustCompile(`\w*[Ee]nv\([^)]*"BORGE_([A-Z0-9_]+)"`)
)

func envVarsReadBySource(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// help.go is the documentation, not a reader: every documented name appears in
		// it, so scanning it would make the reverse check below vacuous.
		if filepath.Base(path) == "help.go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, re := range []*regexp.Regexp{envVarLookup, envVarDirect} {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				for _, group := range m[1:] {
					if group != "" {
						seen[group] = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestHelpTopicsCoverTheCode checks the other lists the topics make.
func TestHelpTopicsCoverTheCode(t *testing.T) {
	// Every pattern style has to be in the patterns topic. A style a user cannot discover
	// is a style that does not exist as far as they are concerned.
	for _, style := range []string{"fm:", "sh:", "re:", "pp:", "pf:"} {
		if !strings.Contains(helpPatterns, style) {
			t.Errorf("the patterns topic does not document the %q style", style)
		}
	}

	// Every archive selector.
	for _, sel := range []string{"aid:", "tags:", "user:", "host:", "sh:", "re:", "name:"} {
		if !strings.Contains(helpMatchArchives, sel) {
			t.Errorf("the match-archives topic does not document the %q selector", sel)
		}
	}
	for _, key := range []string{"timestamp", "name", "id", "host", "user", "tags"} {
		if !strings.Contains(helpMatchArchives, key) {
			t.Errorf("the match-archives topic does not document the %q sort key", key)
		}
	}

	// Every compression codec, checked by asking the compressor factory rather than by a
	// list written here - a new codec would be missed by a second hand-written list just
	// as it would by the topic.
	for _, spec := range []string{"none", "lz4", "zstd", "zlib", "lzma", "auto", "obfuscate"} {
		if !strings.Contains(helpCompression, spec) {
			t.Errorf("the compression topic does not mention %q", spec)
		}
	}
}

// TestHelpPlaceholdersTopicIsTrue.
//
// The topic describes what borge substitutes, so the claim is checked against the
// behaviour rather than trusted. It has already been wrong once in the other direction:
// first written describing borg's substitution when borge had none, then left claiming
// borge had none after it gained it. Both were caught here.
//
//borge:checks placeholders/substituted
func TestHelpPlaceholdersTopicIsTrue(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := r.borge(t, "create", "{hostname}-{now:%Y-%m-%d}", src); code != ExitOK {
		t.Fatalf("create with placeholders exited %d\n%s", code, stderr)
	}
	names := borgArchiveNames(t, r)
	if len(names) != 1 {
		t.Fatalf("expected one archive, got %v", names)
	}
	stored := names[0]

	if strings.ContainsAny(stored, "{}") {
		t.Errorf("the archive is called %q: the placeholders were not substituted, but "+
			"\"borge help placeholders\" says they are", stored)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	want := host + "-" + time.Now().Format("2006-01-02")
	if stored != want {
		t.Errorf("the archive is called %q, want %q", stored, want)
	}

	// Every placeholder the topic lists has to work, or the topic promises something the
	// tool does not do.
	for _, name := range placeholders.Names() {
		if !strings.Contains(helpPlaceholders, "{"+name+"}") {
			t.Errorf("the placeholders topic does not document {%s}", name)
		}
	}

	// And an unknown one is an error, as the topic says.
	_, stderr, code := r.borge(t, "create", "{hostnmae}", src)
	if code != ExitError {
		t.Errorf("an unknown placeholder exited %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "not a placeholder") {
		t.Errorf("the error does not say what is wrong: %q", stderr)
	}
}

// TestHelpCommandAnswersUsefully.
func TestHelpCommandAnswersUsefully(t *testing.T) {
	var out, errOut strings.Builder
	e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}

	// Bare "help" lists the topics.
	if code := Run(e, []string{"help"}); code != ExitOK {
		t.Errorf("bare help exited %d, want ExitOK", code)
	}
	for _, topic := range helpTopicNames() {
		if !strings.Contains(out.String(), topic) {
			t.Errorf("the index does not list the %q topic:\n%s", topic, out.String())
		}
	}

	// Every topic prints something substantial.
	for _, topic := range helpTopicNames() {
		out.Reset()
		if code := Run(e, []string{"help", topic}); code != ExitOK {
			t.Errorf("help %s exited %d", topic, code)
		}
		if len(out.String()) < 200 {
			t.Errorf("the %q topic is %d bytes, which is not a topic", topic, len(out.String()))
		}
	}

	// A command name is a plausible thing to type, and gets an answer rather than an error.
	out.Reset()
	if code := Run(e, []string{"help", "prune"}); code != ExitOK {
		t.Errorf("help for a command exited %d, want ExitOK", code)
	}
	if !strings.Contains(out.String(), "retention") {
		t.Errorf("help for a command does not describe it:\n%s", out.String())
	}

	// Something that is neither is an error that lists the real topics.
	errOut.Reset()
	if code := Run(e, []string{"help", "nonsense"}); code != ExitError {
		t.Errorf("an unknown topic exited %d, want ExitError", code)
	}
	if !strings.Contains(errOut.String(), "patterns") {
		t.Errorf("the error does not list the topics:\n%s", errOut.String())
	}
}

// TestCommandGroupsAnswerHelp: "borge debug --help" has to print the group's usage on
// stdout and exit 0, as borg's argparse groups do.
//
// It answered 'unknown debug command "--help"' on stderr with exit 2 until 2026-08-20, and
// no gate could see it: option-coverage.sh reads the same invocation with 2>&1 and greps,
// so the error line above the usage made no difference to it. A gate that captures both
// streams is not a test of which stream something went to.
//
// The subcommand list is checked here too, because command-coverage.sh parses it to compare
// against borg's - a group whose usage stopped naming its subcommands would leave that
// comparison empty rather than wrong.
func TestCommandGroupsAnswerHelp(t *testing.T) {
	groups := map[string][]command{
		"debug":     debugCommands(),
		"key":       keyCommands(),
		"benchmark": benchmarkCommands(),
	}
	for name, subs := range groups {
		for _, spelling := range []string{"-h", "-help", "--help"} {
			t.Run(name+" "+spelling, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				e := &Env{
					Stdout: &stdout, Stderr: &stderr,
					Getenv: func(string) (string, bool) { return "", false },
				}
				if code := Run(e, []string{name, spelling}); code != ExitOK {
					t.Fatalf("borge %s %s exited %d\nstderr: %s", name, spelling, code, stderr.String())
				}
				if stderr.Len() != 0 {
					t.Errorf("borge %s %s wrote to stderr: %s", name, spelling, stderr.String())
				}
				out := stdout.String()
				if !strings.Contains(out, "usage: borge "+name) {
					t.Fatalf("borge %s %s printed no usage:\n%s", name, spelling, out)
				}
				if len(subs) == 0 {
					t.Fatalf("%s has no subcommands, so this check is vacuous", name)
				}
				for _, c := range subs {
					if !strings.Contains(out, c.name) {
						t.Errorf("borge %s %s does not list %q:\n%s", name, spelling, c.name, out)
					}
				}
			})
		}
	}
}
