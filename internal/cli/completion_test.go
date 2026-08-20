// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func completionEnv() *Env {
	return &Env{Getenv: func(string) (string, bool) { return "", false }}
}

func generate(t *testing.T, shell string) string {
	t.Helper()
	var out, errOut strings.Builder
	e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}
	if code := Run(e, []string{"completion", shell}); code != ExitOK {
		t.Fatalf("completion %s exited %d\n%s", shell, code, errOut.String())
	}
	return out.String()
}

// TestCompletionSeesEveryCommandsFlags is the test the whole approach rests on.
//
// The option names are discovered by running each command with "-help" and collecting the
// FlagSet it built. That works only while every command registers its flags before calling
// Parse. If one ever stops - by parsing first, or by opening a repository before parsing -
// its options would silently vanish from the completions, and nothing else would notice.
func TestCompletionSeesEveryCommandsFlags(t *testing.T) {
	spec := describeCLI(completionEnv())
	if len(spec) < 25 {
		t.Fatalf("describeCLI found %d commands; the dispatch table has more", len(spec))
	}

	// The command groups build no FlagSet of their own; they dispatch straight to a
	// subcommand. "completion" takes a shell and nothing else. Everything else has at
	// least the repository flags, so an empty list means the probe stopped seeing them.
	groups := map[string]bool{"debug": true, "benchmark": true, "key": true}
	// "completion" takes a shell and "help" a topic, so neither has options of its own -
	// but both still carry --log-json, which every command registers because borg puts it
	// on every command. Spelled out rather than left as "no options" so that a future
	// option arriving on either is a failure rather than a silent pass.
	onlyLogJSON := map[string]bool{"completion": true, "help": true}
	for _, c := range spec {
		switch {
		case groups[c.Name]:
			if len(c.Flags) != 0 {
				t.Errorf("%s is a command group and should register no flags of its own, got %v",
					c.Name, c.Flags)
			}
			if len(c.Sub) == 0 {
				t.Errorf("%s is listed as a command group but has no subcommands", c.Name)
			}
			for _, sub := range c.Sub {
				if len(sub.Flags) == 0 && sub.Name != "info" {
					t.Errorf("%s %s registered no options; the probe is no longer reaching "+
						"the subcommand's flags", c.Name, sub.Name)
				}
			}
		case onlyLogJSON[c.Name]:
			if len(c.Flags) != 1 || c.Flags[0] != "--log-json" {
				t.Errorf("%s should register --log-json and nothing else, got %v", c.Name, c.Flags)
			}
		case len(c.Flags) == 0:
			t.Errorf("%s registered no options at all; either it really has none, or it "+
				"parses before registering and completion can no longer see them", c.Name)
		}
	}

	// A spot check with known answers, so a change that made flagsOf return something
	// plausible but wrong would still fail.
	byName := map[string]cmdSpec{}
	for _, c := range spec {
		byName[c.Name] = c
	}
	for name, want := range map[string][]string{
		"create":      {"-C", "-r", "--chunker-params", "--compression", "--files-cache", "--one-file-system"},
		"extract":     {"-C", "-r", "--dry-run", "--sparse", "--strip-components"},
		"prune":       {"-r", "--dry-run"},
		"repo-create": {"-e", "-r", "--encryption", "--key-location"},
	} {
		for _, flag := range want {
			if !contains(byName[name].Flags, flag) {
				t.Errorf("%s has no %s in its completion options: %v", name, flag, byName[name].Flags)
			}
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestCompletionProbeTouchesNothing.
//
// Building the completions runs every command. They are meant to stop at the flag parse,
// but "meant to" is not a guarantee - so this points the probe at a repository path and an
// output directory and checks that nothing at all happened to either.
func TestCompletionProbeTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	work := filepath.Join(dir, "work")
	for _, d := range []string{repo, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)

	e := &Env{Getenv: func(name string) (string, bool) {
		switch name {
		case "BORGE_REPO":
			return repo, true
		case "BORGE_PASSPHRASE":
			return "pw", true
		}
		return "", false
	}}
	describeCLI(e)

	for _, d := range []string{repo, work} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			t.Errorf("generating completions created something in %s: %v", d, names)
		}
	}
}

// TestCompletionArchiveListNamesRealCommands.
//
// archiveTakingCommands is maintained by hand, because the flag package has no notion of
// positional arguments. This catches the half of the drift that can be caught: a name that
// is no longer a command. A command that starts taking an archive and is not added here
// cannot be detected, which is why the list says so at its definition.
func TestCompletionArchiveListNamesRealCommands(t *testing.T) {
	real := map[string]bool{}
	for _, c := range commands() {
		real[c.name] = true
		for _, s := range subcommandsOf(c.name) {
			real[c.name+" "+s.name] = true
		}
	}
	for name := range archiveTakingCommands {
		if !real[name] {
			t.Errorf("archiveTakingCommands lists %q, which is not a command", name)
		}
	}
	for name := range archiveTakingSubcommands {
		if !real[name] {
			t.Errorf("archiveTakingSubcommands lists %q, which is not a subcommand", name)
		}
	}
}

// TestCompletionCoversEveryCommandGroup.
//
// subcommandsOf names the two groups explicitly. If a third is added and not listed there,
// its subcommands would be missing from every completion script - so this finds a group by
// what it does rather than by what the list says: a group prints "<command>" in its usage
// and exits successfully when run with no arguments.
func TestCompletionCoversEveryCommandGroup(t *testing.T) {
	for _, c := range commands() {
		var out, errOut strings.Builder
		e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}
		code := c.run(e, nil)
		// A group's usage line is "usage: borge <name> <command>". Matching "<command>"
		// anywhere was too loose: "help" prints it while pointing at other commands.
		looksLikeGroup := code == ExitOK &&
			strings.Contains(out.String(), "usage: borge "+c.name+" <command>")
		listed := subcommandsOf(c.name) != nil
		if looksLikeGroup && !listed {
			t.Errorf("%q looks like a command group but subcommandsOf does not know it, so its "+
				"subcommands are missing from the completions", c.name)
		}
		if listed && !looksLikeGroup {
			t.Errorf("%q is listed as a command group but does not behave like one", c.name)
		}
	}
}

// TestBashCompletionActuallyCompletes runs the generated script in bash and asks it to
// complete things.
//
// A syntax check would pass on a script that offers nothing. This drives the real
// completion function and checks what comes back, which is the only way to know the
// generated case statement is wired up correctly.
func TestBashCompletionActuallyCompletes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script := filepath.Join(t.TempDir(), "borge.bash")
	if err := os.WriteFile(script, []byte(generate(t, "bash")), 0o644); err != nil {
		t.Fatal(err)
	}

	complete := func(words ...string) []string {
		t.Helper()
		var quoted []string
		for _, w := range words {
			quoted = append(quoted, "'"+w+"'")
		}
		program := "source " + script + "\n" +
			"COMP_WORDS=(" + strings.Join(quoted, " ") + ")\n" +
			"COMP_CWORD=" + strconv.Itoa(len(words)-1) + "\n" +
			"COMPREPLY=()\n_borge\necho \"${COMPREPLY[*]}\"\n"
		out, err := exec.Command(bash, "-c", program).CombinedOutput()
		if err != nil {
			t.Fatalf("bash failed: %v\n%s", err, out)
		}
		return strings.Fields(string(out))
	}

	for _, tc := range []struct {
		words []string
		want  []string
	}{
		{[]string{"borge", "de"}, []string{"delete", "debug"}},
		{[]string{"borge", "repo-sp"}, []string{"repo-space"}},
		{[]string{"borge", "list", "--json-l"}, []string{"--json-lines"}},
		{[]string{"borge", "create", "--chunker"}, []string{"--chunker-params"}},
		{[]string{"borge", "debug", "dump-man"}, []string{"dump-manifest"}},
		{[]string{"borge", "benchmark", ""}, []string{"crud", "cpu"}},
	} {
		got := complete(tc.words...)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("completing %v gave %v, want %v", tc.words, got, tc.want)
		}
	}

	// And an unfinished command name must offer every command, not nothing.
	if all := complete("borge", ""); len(all) < 25 {
		t.Errorf("completing an empty command offered %d entries, want every command: %v",
			len(all), all)
	}
}

// TestZshAndFishCompletionMentionEveryCommand.
//
// zsh and fish are not installed on every machine that runs these tests, so the scripts
// cannot always be executed. What can always be checked is that every command and
// subcommand reached the output: a generator that silently skipped half the table would
// produce a valid script that completes almost nothing.
func TestZshAndFishCompletionMentionEveryCommand(t *testing.T) {
	for _, shell := range []string{"zsh", "fish"} {
		script := generate(t, shell)
		for _, c := range commands() {
			if !strings.Contains(script, c.name) {
				t.Errorf("the %s script does not mention the %q command", shell, c.name)
			}
			for _, s := range subcommandsOf(c.name) {
				if !strings.Contains(script, s.name) {
					t.Errorf("the %s script does not mention %q %q", shell, c.name, s.name)
				}
			}
		}
		// An option from deep in the table, to show the per-command options were emitted.
		if !strings.Contains(script, "chunker-params") {
			t.Errorf("the %s script has no per-command options", shell)
		}
		// help takes a topic rather than a subcommand, and the completion has to offer
		// those too - they are a fixed list in the first position, which is exactly what a
		// completion is for.
		for _, topic := range helpTopicNames() {
			if !strings.Contains(script, topic) {
				t.Errorf("the %s script does not offer the %q help topic", shell, topic)
			}
		}
	}

	// If the shells are installed, check the scripts parse.
	for _, tc := range []struct{ shell, flag string }{{"zsh", "-n"}, {"fish", "-n"}} {
		bin, err := exec.LookPath(tc.shell)
		if err != nil {
			t.Logf("%s not installed; the generated script was not syntax-checked", tc.shell)
			continue
		}
		path := filepath.Join(t.TempDir(), "script")
		if err := os.WriteFile(path, []byte(generate(t, tc.shell)), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, tc.flag, path).CombinedOutput(); err != nil {
			t.Errorf("%s rejects the generated script: %v\n%s", tc.shell, err, out)
		}
	}
}

// TestCompletionRejectsWhatItCannotGenerate.
func TestCompletionRejectsWhatItCannotGenerate(t *testing.T) {
	var out, errOut strings.Builder
	e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}

	if code := Run(e, []string{"completion", "csh"}); code != ExitError {
		t.Errorf("an unknown shell exited %d, want ExitError", code)
	}
	if !strings.Contains(errOut.String(), "bash") {
		t.Errorf("the error does not say which shells are supported: %q", errOut.String())
	}

	// tcsh gets its own message: borg supports it and borge deliberately does not, so
	// "unknown shell" would be a misleading answer.
	errOut.Reset()
	if code := Run(e, []string{"completion", "tcsh"}); code != ExitError {
		t.Errorf("tcsh exited %d, want ExitError", code)
	}
	if !strings.Contains(errOut.String(), "tcsh") {
		t.Errorf("the tcsh error does not explain itself: %q", errOut.String())
	}

	errOut.Reset()
	if code := Run(e, []string{"completion"}); code != ExitError {
		t.Errorf("completion with no shell exited %d, want ExitError", code)
	}
}

// TestHelpTextIsWellFormed: no command's help contains a rendering failure.
//
// flag.PrintDefaults builds a zero value of every Value type by reflection and calls
// String() on it; a Value that cannot survive that has its panic *recovered* and reported
// as a line of the help text. Nothing crashes, no test fails, and the line sits in the
// output of a command people run every day - "borge create --help" carried one until
// 2026-08-19, and the option gate could not see it because it only reads lines beginning
// with two spaces and a dash.
func TestHelpTextIsWellFormed(t *testing.T) {
	spec := describeCLI(completionEnv())
	if len(spec) < 20 {
		t.Fatalf("only %d commands; the probe is not reaching them", len(spec))
	}
	var checked int
	var check func(name string, args []string)
	check = func(name string, args []string) {
		var stdout, stderr bytes.Buffer
		e := &Env{Stdout: &stdout, Stderr: &stderr, Getenv: func(string) (string, bool) { return "", false }}
		Run(e, append(args, "--help"))
		text := stdout.String() + stderr.String()
		checked++
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s printed no help at all", name)
			return
		}
		for _, bad := range []string{"panic", "%!", "<nil>"} {
			if strings.Contains(text, bad) {
				for _, line := range strings.Split(text, "\n") {
					if strings.Contains(line, bad) {
						t.Errorf("%s --help contains %q:\n  %s", name, bad, line)
					}
				}
			}
		}
	}
	for _, c := range spec {
		check(c.Name, []string{c.Name})
		for _, sub := range c.Sub {
			check(c.Name+" "+sub.Name, []string{c.Name, sub.Name})
		}
	}
	if checked < 40 {
		t.Errorf("checked only %d help texts; commands and subcommands together are more", checked)
	}
}
