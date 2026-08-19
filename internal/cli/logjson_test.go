// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --log-json is the half of borg's frontend API that carries what a command *said* rather
// than what it returned. Its contract is stronger than "some lines are JSON": a frontend
// reads stderr line by line and parses each one, so one plain-text line in the middle is a
// parse error. These tests hold that property rather than spot-checking a message.

// logLines parses a stderr stream as JSON lines, failing on the first line that is not an
// object. That failure is the point: it is exactly what a frontend would hit.
func logLines(t *testing.T, who, stream string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimRight(stream, "\n"), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("%s wrote a line --log-json cannot parse (line %d): %v\n%s",
				who, i+1, err, line)
		}
		if _, ok := obj["type"]; !ok {
			t.Errorf("%s wrote an object with no type: %s", who, line)
		}
		out = append(out, obj)
	}
	return out
}

func typesIn(objs []map[string]any) map[string]int {
	out := map[string]int{}
	for _, o := range objs {
		t, _ := o["type"].(string)
		out[t]++
	}
	return out
}

// TestLogJSONIsAlwaysParseable: every command's stderr survives line-by-line parsing.
//
// The commands are chosen to cover the different ways borge writes to stderr: a listing, a
// summary line, a "Done." hint, a warning, and an error. Any of them left as plain text
// breaks the stream, which is why this runs several rather than one.
func TestLogJSONIsAlwaysParseable(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("first", "second")

	cases := []struct {
		name string
		args []string
	}{
		{"create with a listing", []string{"create", "third", r.src, "--list"}},
		{"delete with a hint", []string{"delete", "-a", "second"}},
		{"prune with a summary", []string{"prune", "--keep-daily", "1", "--dry-run", "--list"}},
		{"check", []string{"check", "-v"}},
		{"an error", []string{"list", "nosucharchive"}},
	}

	var sawAny bool
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{}, c.args...), "--log-json")
			_, stderr, _ := r.borge(t, args...)
			if strings.TrimSpace(stderr) == "" {
				return // nothing said is not a failure; some commands are quiet
			}
			sawAny = true
			objs := logLines(t, "borge "+c.name, stderr)
			if len(objs) == 0 {
				t.Errorf("stderr was not empty but produced no objects:\n%s", stderr)
			}
		})
	}
	// If every command happened to say nothing, the whole test would pass having parsed
	// nothing at all.
	if !sawAny {
		t.Fatal("no command wrote anything to stderr; the test parsed nothing")
	}
}

// TestLogJSONLevels: an error is reported as one, not as INFO.
//
// The wrapped-writer approach makes every plain write an INFO message, so the risk is that
// errors and warnings quietly become INFO too and a frontend cannot tell a failure from a
// remark.
func TestLogJSONLevels(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("only")

	_, stderr, code := r.borge(t, "list", "nosucharchive", "--log-json")
	if code == ExitOK {
		t.Fatal("listing a missing archive succeeded; there is no error to check")
	}
	objs := logLines(t, "borge list", stderr)
	var levels []string
	for _, o := range objs {
		if l, ok := o["levelname"].(string); ok {
			levels = append(levels, l)
		}
	}
	if len(levels) == 0 {
		t.Fatalf("no log_message carried a levelname:\n%s", stderr)
	}
	var sawError bool
	for _, l := range levels {
		if l == "ERROR" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("a failing command reported levels %v, none of them ERROR", levels)
	}
}

// TestLogJSONFileStatusMatchesBorg: create --list emits file_status objects, as borg does,
// with the same statuses and the same paths.
func TestLogJSONFileStatusMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives()

	_, borgErr := r.runSplit("create", "-r", r.path, "by-borg", r.src, "--list", "--log-json")
	borgObjs := logLines(t, "borg", borgErr)

	_, borgeErr, code := r.borge(t, "create", "by-borge", r.src, "--list", "--log-json")
	if code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, borgeErr)
	}
	borgeObjs := logLines(t, "borge", borgeErr)

	if n := typesIn(borgObjs)["file_status"]; n == 0 {
		t.Fatalf("borg emitted no file_status objects; the premise is wrong:\n%s", borgErr)
	}

	// Compared as "status path" pairs. borg reports the walked path, not the stored one,
	// which is the same string for both tools only because borge was changed to match on
	// 2026-08-19; see the note on walker.report.
	collect := func(objs []map[string]any) []string {
		var out []string
		for _, o := range objs {
			if o["type"] != "file_status" {
				continue
			}
			status, _ := o["status"].(string)
			path, _ := o["path"].(string)
			out = append(out, status+" "+path)
		}
		sort.Strings(out)
		return out
	}
	borgList, borgeList := collect(borgObjs), collect(borgeObjs)
	if strings.Join(borgList, "\n") != strings.Join(borgeList, "\n") {
		t.Errorf("file_status objects differ\nborg :\n%s\nborge:\n%s",
			strings.Join(borgList, "\n"), strings.Join(borgeList, "\n"))
	}
}

// TestLogJSONParseErrorStaysPlainText: borg's documented caveat, asserted for both tools.
//
// "JSON logging requires successful argument parsing. Even with --log-json specified, a
// parsing error will be printed in plain text, because logging set-up happens after all
// arguments are parsed" (frontends.rst). A frontend has to be ready for that, so this
// records it as shared behaviour rather than letting borge drift either way.
func TestLogJSONParseErrorStaysPlainText(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	// Not runSplit: a parse error exits non-zero, which is the case under test, and
	// runSplit treats a non-zero exit as a failure of the test itself.
	borgErr := r.stderrOf("list", "-r", r.path, "--nosuchoption", "--log-json")
	_, borgeErr, code := r.borge(t, "list", "--nosuchoption", "--log-json")
	if code == ExitOK {
		t.Fatal("borge accepted an option that does not exist")
	}

	for _, c := range []struct{ who, out string }{{"borg", borgErr}, {"borge", borgeErr}} {
		if strings.TrimSpace(c.out) == "" {
			t.Fatalf("%s said nothing about an unknown option", c.who)
		}
		first := strings.SplitN(strings.TrimSpace(c.out), "\n", 2)[0]
		var obj map[string]any
		if json.Unmarshal([]byte(first), &obj) == nil {
			t.Errorf("%s reported a parse error as JSON; borg's contract says plain text: %s",
				c.who, first)
		}
	}
}

// TestCreateListPathMatchesBorg: the listing names the path borge read, not the one it
// stored.
//
// The two are the same string for a relative source, which is how the difference survived:
// every existing test used one. With an absolute source borg prints "/srv/data/f" where
// the archive holds "srv/data/f".
func TestCreateListPathMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives()

	listed := func(who string, out string) []string {
		var paths []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if len(line) > 2 && (line[0] == 'A' || line[0] == 'd' || line[0] == 's') {
				paths = append(paths, line[2:])
			}
		}
		sort.Strings(paths)
		if len(paths) == 0 {
			t.Fatalf("%s listed nothing:\n%s", who, out)
		}
		return paths
	}

	_, borgErr := r.runSplit("create", "-r", r.path, "abs-borg", r.src, "--list")
	_, borgeErr, code := r.borge(t, "create", "abs-borge", r.src, "--list")
	if code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, borgeErr)
	}
	borgPaths, borgePaths := listed("borg", borgErr), listed("borge", borgeErr)

	// The premise: an absolute source, so the walked and stored paths differ.
	if !filepath.IsAbs(borgPaths[0]) {
		t.Fatalf("borg listed a relative path %q for an absolute source; the case this "+
			"test exists for is not being exercised", borgPaths[0])
	}
	if strings.Join(borgPaths, "\n") != strings.Join(borgePaths, "\n") {
		t.Errorf("listed paths differ\nborg :\n%s\nborge:\n%s",
			strings.Join(borgPaths, "\n"), strings.Join(borgePaths, "\n"))
	}
}

// stderrOf runs borg and returns its stderr whatever the exit status, for the cases where
// a non-zero exit is what is being tested.
func (r *borgRepo) stderrOf(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	return stderr.String()
}
