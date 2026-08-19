// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The JSON options are borg's frontend API. Its own documentation says so - "Borg does not
// have a public API on the Python level [...] it provides an API on a command-line level"
// (docs/internals/frontends.rst) - so a command that takes --json is a command a frontend
// may drive, and one that does not is a command it may not.
//
// That makes the *set* of commands taking --json part of the contract, not a formatting
// detail. borge registered --json on every repository command until 2026-08-18, so twelve
// commands accepted it and printed text anyway: "borge check --json" promised a machine
// answer and gave prose. Nothing caught it, because the option-coverage gate compares
// spellings and both tools spell it "--json".
//
// So this compares the surface itself, asking both tools rather than listing the answer
// here. It fails in both directions: an option borge offers where borg has none is as
// wrong as one it lacks, because a frontend probing for a JSON form gets a wrong answer
// either way.

var optionLine = regexp.MustCompile(`(^|\s)--?(log-json|json(-lines)?)\b`)

// jsonOptions is the set of json-ish options a help text offers, e.g. {"json-lines"}.
func jsonOptions(help string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(help, "\n") {
		// Only option lines, not the prose that describes them: argparse indents an
		// option by two spaces and Go's flag package by four, and both put the option
		// first on the line.
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		for _, m := range optionLine.FindAllStringSubmatch(trimmed, -1) {
			out[m[2]] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonSurfaceCommands are the commands compared. Subcommands are given as "benchmark cpu"
// because both tools carry --json there, not on the parent.
//
// The three command groups are here for --log-json alone. borg accepts it ahead of a
// subcommand and borge did not until 2026-08-19 - a gap this test could not see, because
// until then it only compared --json and --json-lines. --log-json is on every command, so
// it contributes nothing that distinguishes one command from another, and that is exactly
// why it went unchecked: the interesting cases are the ones where borge's structure differs
// from borg's, and those are the groups. See DIVERGENCES.md #41.
var jsonSurfaceCommands = []string{
	"analyze", "break-lock", "check", "compact", "create", "delete", "diff",
	"export-tar", "extract", "find", "import-tar", "info", "list", "prune",
	"recreate", "rename", "repo-compress", "repo-create", "repo-delete",
	"repo-info", "repo-list", "repo-space", "tag", "undelete", "version",
	"with-lock", "benchmark cpu", "benchmark crud",
	"debug", "key", "benchmark",
}

// "benchmark compression" is borge's own and has no borg counterpart, so it is not here:
// this test compares a shared surface, and a command only one tool has has nothing to
// compare against.

func TestJSONOptionSurfaceMatchesBorg(t *testing.T) {
	binary := borgBinary(t)

	var checked, withJSON int
	for _, name := range jsonSurfaceCommands {
		t.Run(strings.ReplaceAll(name, " ", "-"), func(t *testing.T) {
			args := append(strings.Fields(name), "--help")

			cmd := exec.Command(binary, args...)
			var borgOut bytes.Buffer
			cmd.Stdout = &borgOut
			cmd.Stderr = &borgOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("borg %s --help: %v\n%s", name, err, borgOut.String())
			}
			want := jsonOptions(borgOut.String())

			var stdout, stderr bytes.Buffer
			e := &Env{Stdout: &stdout, Stderr: &stderr, Getenv: func(string) (string, bool) { return "", false }}
			Run(e, args)
			got := jsonOptions(stdout.String() + stderr.String())

			checked++
			if len(want) > 0 {
				withJSON++
			}
			if len(want) != len(got) {
				t.Errorf("borg offers %v, borge offers %v", sortedKeys(want), sortedKeys(got))
				return
			}
			for k := range want {
				if !got[k] {
					t.Errorf("borg offers %v, borge offers %v", sortedKeys(want), sortedKeys(got))
					return
				}
			}
		})
	}

	// A regex that matched nothing, or a help text that failed to render, would make
	// every subtest pass by comparing two empty sets. borg has --log-json on every command
	// and --json or --json-lines on twelve of them; if the scan finds far fewer, it is the
	// scan that broke.
	if checked != len(jsonSurfaceCommands) {
		t.Fatalf("checked %d of %d commands", checked, len(jsonSurfaceCommands))
	}
	if withJSON < len(jsonSurfaceCommands) {
		t.Fatalf("found a JSON option on only %d of %d commands; borg has --log-json on "+
			"every one, so the help scan is broken", withJSON, len(jsonSurfaceCommands))
	}
}
