// SPDX-License-Identifier: Apache-2.0

package patterns

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The patterns gate: for a corpus of patterns and paths, borge's matcher agrees with
// borg's on every pair.
//
// Match *results* are compared rather than the regexes, because the regexes cannot be
// identical - Go's RE2 rejects some escapes Python's re allows - and because what a user
// cares about is which files are selected.

type patternOracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startPatternOracle(t *testing.T) *patternOracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg pattern differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the pattern differential test")
	}

	cmd := exec.Command(py, "testdata/oracle.py")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return &patternOracle{stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20), stderr: &stderr}
}

func (o *patternOracle) ask(t *testing.T, format string, args ...any) string {
	t.Helper()
	req := fmt.Sprintf(format, args...)
	if _, err := io.WriteString(o.stdin, req+"\n"); err != nil {
		t.Fatalf("oracle write: %v (stderr: %s)", err, o.stderr)
	}
	line, err := o.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("oracle read: %v (stderr: %s)", err, o.stderr)
	}
	line = strings.TrimRight(line, "\n")
	if strings.HasPrefix(line, "ERR ") {
		t.Fatalf("%s: borg said: %s", req, strings.TrimPrefix(line, "ERR "))
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "OK"), " ")
}

func enc(s string) string {
	if s == "" {
		return "-"
	}
	return hex.EncodeToString([]byte(s))
}

// shellPatterns covers the constructs borg's translate() handles, plus the corners that
// are easy to get wrong: unterminated classes, a "]" as the first class member, escaped
// braces, nested alternatives, and braces with no comma.
var shellPatterns = []string{
	"", "*", "?", "**/", "**/x",
	"foo", "foo*", "foo?", "*foo", "*.txt", "foo/*", "foo/**/bar",
	"foo/**", "**/*.txt", "a*b*c",
	"[abc]", "[!abc]", "[]abc]", "[!]abc]", "[^abc]", "[a-z]", "[a-z0-9_]",
	"[", "[abc", "[]", "[?]", "[*]",
	"{a,b}", "{a,b}.txt", "{foo,bar}{baz,qux}", "{a}", "{a\\,b}", "{a\\,,b}",
	"{a,{b,c}}", "x{a,b}y",
	"a.b", "a+b", "a(b)c", "a|b", "a$b", "a^b",
	"with space", "ünïcodé", "tab\there",
	"foo.txt", "*/", "/absolute/path", "./relative",
	"a\\*b", "a\\?b", "\\[abc\\]",
}

// shellSubjects are the strings the patterns are matched against.
var shellSubjects = []string{
	"", "foo", "foo/", "foo/bar", "foo/bar/baz", "foobar", "foo.txt", "bar.txt",
	"a", "b", "c", "ab", "abc", "a/b", "a/b/c", "x", "xy", "xay", "xby",
	"]", "]abc", "?", "*", "[", "[abc]", "a*b", "a?b",
	"a.b", "a+b", "a(b)c", "a|b", "a$b", "a^b", "a-b", "_",
	"with space", "ünïcodé", "tab\there",
	"/absolute/path", "./relative", "absolute/path",
	"{a,b}", "{a}", "a,b",
	"deep/nested/path/file.txt", "nested/file.txt", "file.txt",
	"0", "9", "z", "Z",
}

// TestShellPatternMatchesBorg is the gate: every (pattern, subject) pair agrees.
func TestShellPatternMatchesBorg(t *testing.T) {
	o := startPatternOracle(t)

	var checked, differed int
	for _, pat := range shellPatterns {
		re, err := Compile(pat)
		if err != nil {
			// borg would raise here too; check that it does rather than assuming.
			t.Errorf("borge cannot compile %q: %v", pat, err)
			continue
		}
		for _, subject := range shellSubjects {
			want := o.ask(t, "M %s %s", enc(pat), enc(subject)) == "1"
			got := re.MatchString(subject)
			checked++
			if got != want {
				differed++
				t.Errorf("pattern %q vs %q: borge says %v, borg says %v\n  borge regex: %s",
					pat, subject, got, want, re.String())
			}
		}
	}
	t.Logf("compared %d (pattern, path) pairs, %d differed", checked, differed)
}

// namePatterns are the archive-name selectors.
var namePatterns = []string{
	"backup", "backup*", "sh:backup*", "sh:backup-{a,b}", "re:^backup.*$",
	"re:backup", "id:backup", "id:backup*", "sh:*", "re:.*",
	"sh:host-?/data", "sh:*.daily", "re:[0-9]{4}", "sh:{a,b}-*",
}

var nameSubjects = []string{
	"backup", "backup2", "backup-a", "backup-b", "backup*", "old-backup",
	"a-1", "b-2", "c-3", "host-1/data", "host-12/data",
	"2026", "20260", "x.daily", "daily", "",
}

// TestArchiveNamePatternMatchesBorg: selecting archives by name has to agree exactly,
// because the same selector is used by delete.
func TestArchiveNamePatternMatchesBorg(t *testing.T) {
	o := startPatternOracle(t)

	for _, pat := range namePatterns {
		re, err := CompileName(pat)
		if err != nil {
			t.Errorf("borge cannot compile name pattern %q: %v", pat, err)
			continue
		}
		for _, subject := range nameSubjects {
			want := o.ask(t, "N %s %s", enc(pat), enc(subject)) == "1"
			got := re.MatchString(subject)
			if got != want {
				t.Errorf("name pattern %q vs %q: borge says %v, borg says %v\n  borge regex: %s",
					pat, subject, got, want, re.String())
			}
		}
	}
}

// TestIdenticalIsTheDefault pins the thing most likely to be got wrong by a reader of
// the code: a bare archive name is an *exact* match, not a substring or a glob.
func TestIdenticalIsTheDefault(t *testing.T) {
	re, err := CompileName("backup")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("backup") {
		t.Error("the exact name does not match")
	}
	for _, other := range []string{"backup2", "old-backup", "backups", "Backup"} {
		if re.MatchString(other) {
			t.Errorf("%q matched a bare name pattern, which must be exact", other)
		}
	}
}

func TestErrorsAreNotSilent(t *testing.T) {
	if _, err := CompileName("re:[unclosed"); err == nil {
		t.Error("an invalid regular expression was accepted")
	}
}
