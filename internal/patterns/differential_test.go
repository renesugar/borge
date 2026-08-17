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

// ---------------------------------------------------------------- file patterns

// filePatterns are the shapes users actually write in an exclude file, plus the corners:
// a trailing slash (contents but not the directory), a leading slash (stripped), "..",
// and patterns that only differ between the fnmatch and shell dialects.
var filePatterns = []string{
	"foo", "foo/", "/foo", "/foo/", "foo/bar", "foo/*", "foo/**", "*.txt", "**/*.txt",
	"*", "**", "?", "a?c", "[abc]", "[!abc]", "[a-z]*.txt",
	"home/*/.cache", "home/*/.cache/", "**/.cache", "**/node_modules/**",
	".git", ".git/", "*.pyc", "__pycache__/",
	"a/b/c", "a//b", "a/./b", "./a",
	"{a,b}.txt", "with space", "ünïcodé",
}

// filePaths are archived paths: relative, no leading slash.
var filePaths = []string{
	"foo", "foo/bar", "foo/bar/baz", "foobar", "barfoo",
	"a", "a/b", "a/b/c", "a/bc", "abc", "ac",
	"x.txt", "dir/x.txt", "deep/dir/x.txt", "x.pyc", "dir/x.pyc",
	"home/alice/.cache", "home/alice/.cache/thing", "home/alice/.cachex",
	"home/bob/.cache/deep/thing", ".cache", "sub/.cache",
	".git", ".git/config", "src/.git/config",
	"__pycache__", "__pycache__/x.pyc", "src/__pycache__/y.pyc",
	"node_modules", "node_modules/pkg/index.js", "app/node_modules/pkg/index.js",
	"a.txt", "b.txt", "c.txt", "with space", "ünïcodé",
}

// TestFilePatternsMatchBorg is the stage 5d gate: every (style, pattern, path) triple
// agrees with borg. A pattern that selects a different set is how a backup silently omits
// files, so this runs over the whole cross product rather than a sample.
func TestFilePatternsMatchBorg(t *testing.T) {
	o := startPatternOracle(t)

	styles := []string{StyleFnmatch, StyleShellPath, StylePathPrefix, StylePathFull}
	var checked, differed int

	for _, style := range styles {
		for _, pat := range filePatterns {
			p, err := NewPattern(style, pat, true)
			if err != nil {
				t.Errorf("%s:%s does not compile: %v", style, pat, err)
				continue
			}
			for _, path := range filePaths {
				want := o.ask(t, "P %s %s %s", style, enc(pat), enc(path)) == "1"
				got := p.Match(path)
				checked++
				if got != want {
					differed++
					t.Errorf("%s:%q vs %q: borge says %v, borg says %v", style, pat, path, got, want)
				}
			}
		}
	}
	t.Logf("compared %d (style, pattern, path) triples, %d differed", checked, differed)
}

// TestRegexFilePatternsMatchBorg covers "re:", which is searched rather than anchored and
// keeps a leading slash.
func TestRegexFilePatternsMatchBorg(t *testing.T) {
	o := startPatternOracle(t)

	regexes := []string{
		"foo", "^foo", "foo$", ".*\\.txt$", "^home/[^/]+/\\.cache",
		"\\.git(/|$)", "node_modules", "^a/b$", "[0-9]+",
	}
	for _, pat := range regexes {
		p, err := NewPattern(StyleRegexPath, pat, true)
		if err != nil {
			t.Errorf("re:%s does not compile: %v", pat, err)
			continue
		}
		for _, path := range filePaths {
			want := o.ask(t, "P re %s %s", enc(pat), enc(path)) == "1"
			if got := p.Match(path); got != want {
				t.Errorf("re:%q vs %q: borge says %v, borg says %v", pat, path, got, want)
			}
		}
	}
}

// matcherCases are whole include/exclude sequences, in the syntax of a --patterns-from
// file. The ordering cases are the point: the first match wins, so an include after an
// exclude does nothing.
var matcherCases = [][]string{
	{"- *.pyc"},
	{"! *.pyc"},
	{"- .git"},
	{"! .git"},
	{"+ home/alice/.cache/thing", "- home/alice/.cache"},
	{"- home/alice/.cache", "+ home/alice/.cache/thing"},
	{"+ sh:**/*.txt", "- sh:**"},
	{"- sh:**/node_modules/**"},
	{"! sh:**/node_modules/**"},
	{"- fm:*.pyc", "- fm:__pycache__/"},
	{"+ pf:a/b/c", "- pp:a"},
	{"- pp:home", "+ pf:home/alice/.cache"},
	{"P re", "- ^a/"},
	{"P fm", "- *.txt"},
}

// TestMatcherAgreesWithBorg checks the decision *and* the recursion flag, which is what
// decides whether an excluded directory is walked into at all.
func TestMatcherAgreesWithBorg(t *testing.T) {
	o := startPatternOracle(t)

	for _, spec := range matcherCases {
		name := strings.Join(spec, " ; ")
		t.Run(name, func(t *testing.T) {
			m := NewMatcher(true)
			fallback := StyleShellPath
			for _, line := range spec {
				e, err := ParseInclExclCommand(line, fallback)
				if err != nil {
					t.Fatalf("%q: %v", line, err)
				}
				switch e.Cmd {
				case CmdPatternStyle:
					fallback = e.Value
				case CmdRootPath:
				default:
					m.Add(e.Pattern, e.Cmd)
				}
			}

			var encoded []string
			for _, line := range spec {
				encoded = append(encoded, enc(line))
			}
			joined := strings.Join(encoded, ",")

			for _, path := range filePaths {
				resp := o.ask(t, "X %s %s", joined, enc(path))
				fields := strings.Split(resp, "/")
				if len(fields) != 2 {
					t.Fatalf("malformed oracle response %q", resp)
				}
				wantMatch := fields[0] == "1"
				wantRecurse := fields[1] == "1"

				gotMatch := m.Match(path)
				gotRecurse := m.RecurseDir()
				if gotMatch != wantMatch {
					t.Errorf("%q: borge says %v, borg says %v", path, gotMatch, wantMatch)
				}
				if gotRecurse != wantRecurse {
					t.Errorf("%q: borge recurses %v, borg %v", path, gotRecurse, wantRecurse)
				}
			}
		})
	}
}

// TestPatternFileParsing checks the file forms rather than the matching.
func TestPatternFileParsing(t *testing.T) {
	content := `
# a comment
R /home/alice
P re
- ^\.cache/
P sh
+ **/keep.txt
! node_modules
`
	entries, roots, err := LoadPatternFile(strings.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != "/home/alice" {
		t.Errorf("roots are %v, want [/home/alice]", roots)
	}
	if len(entries) != 3 {
		t.Fatalf("parsed %d entries, want 3", len(entries))
	}
	if entries[0].Cmd != CmdExclude || entries[1].Cmd != CmdInclude || entries[2].Cmd != CmdExcludeNoRecurse {
		t.Errorf("commands are %v %v %v", entries[0].Cmd, entries[1].Cmd, entries[2].Cmd)
	}
	// The "P re" line has to have changed the style for the line after it, not for the
	// whole file: "+ **/keep.txt" comes after "P sh" and is a shell pattern.
	if !entries[1].Pattern.Match("sub/keep.txt") {
		t.Error("the shell pattern after 'P sh' does not match")
	}
	if !entries[0].Pattern.Match(".cache/x") {
		t.Error("the regex pattern after 'P re' does not match")
	}

	for _, bad := range []string{"", "x foo", "-", "+   ", "P nonsense"} {
		if _, err := ParseInclExclCommand(bad, StyleShellPath); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestExcludeFileParsing(t *testing.T) {
	content := "# comment\n\n*.pyc\nsh:**/node_modules\n"
	pats, err := LoadExcludeFile(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(pats) != 2 {
		t.Fatalf("parsed %d patterns, want 2", len(pats))
	}
	// --exclude patterns do not recurse, which is what makes excluding a big directory
	// cheap: borge does not walk into it at all.
	for i, p := range pats {
		if p.RecurseDir() {
			t.Errorf("pattern %d recurses; --exclude patterns must not", i)
		}
	}
	if !pats[0].Match("x.pyc") || !pats[0].Match("dir/x.pyc") {
		t.Error("the default fnmatch pattern does not match as borg's would")
	}
}

// TestIncludePathsFlipTheFallback: naming paths on a command line means not wanting
// anything else.
func TestIncludePathsFlipTheFallback(t *testing.T) {
	m := NewMatcher(true)
	if !m.Match("anything") {
		t.Error("an empty matcher should include everything")
	}
	if err := m.AddIncludePaths([]string{"home/alice"}); err != nil {
		t.Fatal(err)
	}
	if !m.Match("home/alice/doc.txt") {
		t.Error("a path below an include path was not matched")
	}
	if m.Match("home/bob/doc.txt") {
		t.Error("a path outside every include path was matched")
	}
	if len(m.UnmatchedIncludePatterns()) != 0 {
		t.Error("an include pattern that matched is reported as unmatched")
	}

	m2 := NewMatcher(true)
	if err := m2.AddIncludePaths([]string{"nowhere"}); err != nil {
		t.Fatal(err)
	}
	m2.Match("home/alice")
	if len(m2.UnmatchedIncludePatterns()) != 1 {
		t.Error("an include pattern that never matched is not reported")
	}
}
