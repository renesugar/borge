// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// stdout carries a command's data; stderr carries everything it says about the work.
//
// borg draws that line and borge did not: "borg check -v" writes 405 bytes to stderr and
// nothing to stdout, while borge wrote its whole report to stdout. Invisible until stdout
// has to be clean for something else - which is what --json and --stdout are - and by then
// it is a corrupted document rather than a cosmetic difference. See DIVERGENCES.md #46.
//
// The commands here are the ones that report without producing data. A command whose
// output *is* data (list, info, analyze, repo-info, repo-space, repo-list) is not in this
// list, and borg puts those on stdout too.
func TestReportingCommandsWriteToStderr(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("first", "second")

	// A soft-deleted archive, so compact has something to report.
	if _, stderr, code := r.borge(t, "delete", "-a", "first"); code != ExitOK {
		t.Fatalf("borge delete exited %d\n%s", code, stderr)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"check", []string{"check", "-v"}},
		{"compact", []string{"compact", "-v"}},
		{"repo-compress", []string{"repo-compress", "-v", "-C", "zstd,1"}},
		{"break-lock", []string{"break-lock"}},
		{"recreate", []string{"recreate", "-v", "-a", "second", "--chunker-params", "fastcdc,18,22,20,2"}},
		{"extract --list", []string{"extract", "-C", t.TempDir(), "--list", "second"}},
	}

	var spoke int
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, code := r.borge(t, c.args...)
			if code != ExitOK {
				t.Fatalf("exited %d\n%s", code, stderr)
			}
			if stdout != "" {
				t.Errorf("wrote to stdout, which belongs to the command's data:\n%s", stdout)
			}
			if strings.TrimSpace(stderr) != "" {
				spoke++
			}
		})
	}
	// Every command staying silent would pass this without testing anything.
	if spoke < 4 {
		t.Errorf("only %d of %d commands said anything at all; the test is not "+
			"exercising the reports it is about", spoke, len(cases))
	}
}

// TestRepoCreateWritesToStderr: the same rule, on the command that has no repository yet.
func TestRepoCreateWritesToStderr(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	base := t.TempDir()

	var stdout, stderr string
	var code int
	func() {
		t.Setenv("BORGE_REPO", filepath.Join(base, "fresh"))
		stdout, stderr, code = r.borge(t, "repo-create", "-r", filepath.Join(base, "fresh"), "-e", "none-sha256")
	}()
	if code != ExitOK {
		t.Fatalf("repo-create exited %d\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("repo-create wrote to stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Repository created") {
		t.Errorf("repo-create said nothing about creating a repository:\n%s", stderr)
	}
}

// TestExtractStdoutStaysData: --list must not write into --stdout's file contents.
//
// This is the failure the rule exists to prevent, and the one that was live: extract wrote
// its listing to the same stream the file contents go to, so asking for both gave a file
// with its own name interleaved into it.
func TestExtractStdoutStaysData(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	const body = "the whole content and nothing else\n"
	write(t, filepath.Join(src, "only.txt"), body)
	if _, stderr, code := r.borge(t, "create", "one", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	stdout, stderr, code := r.borge(t, "extract", "--stdout", "--list", "one")
	if code != ExitOK {
		// borge has no --stdout yet: it is one of extract's three missing options
		// (PORTING_PLAN table row 3). Skipped rather than deleted, so this starts
		// testing the moment the option lands - which is exactly when the listing being
		// on the wrong stream would start corrupting file contents.
		if strings.Contains(stderr, "not defined") {
			t.Skip("borge has no extract --stdout yet; this activates when it does")
		}
		t.Fatalf("borge extract --stdout exited %d\n%s", code, stderr)
	}
	if stdout != body {
		t.Errorf("stdout is not the file's content alone:\n%q", stdout)
	}
	if !strings.Contains(stderr, "only.txt") {
		t.Errorf("--list named nothing on stderr:\n%s", stderr)
	}
}
