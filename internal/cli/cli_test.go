// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The stage 5e gate: borge's read-only commands produce what borg's do.
//
// The text output is compared field by field rather than byte for byte - borg pads some
// columns to widths that depend on the data, and reproducing that exactly would be
// pinning a detail nobody depends on. The machine-readable output is compared as data,
// which is what a script actually consumes.

type borgRepo struct {
	t          *testing.T
	binary     string
	path       string
	keysDir    string
	configDir  string
	passphrase string
	src        string
}

func newBorgRepo(t *testing.T, encryption string) *borgRepo {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg CLI gate in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, ".venv-borg2", "bin", "borg")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the CLI gate")
	}

	base := t.TempDir()
	r := &borgRepo{
		t:          t,
		binary:     binary,
		path:       filepath.Join(base, "repo"),
		keysDir:    filepath.Join(base, "keys"),
		configDir:  filepath.Join(base, "config"),
		passphrase: "cli gate",
	}
	for _, d := range []string{r.keysDir, r.configDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Env.Getenv covers the CLI's own lookups, but the layers below it - the key
	// manager's search path, the KDF weakening - read the process environment directly,
	// as they must at runtime. So both are set, and the test would be lying if it only
	// set the first.
	t.Setenv("BORGE_KEYS_DIR", r.keysDir)
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	t.Setenv("BORGE_KEY_FILE", "")
	t.Setenv("BORG_KEY_FILE", "")

	r.mustRun("repo-create", "-r", r.path, "-e", encryption)
	return r
}

func (r *borgRepo) env() []string {
	return append(os.Environ(),
		"BORG_KEYS_DIR="+r.keysDir,
		"BORG_CONFIG_DIR="+r.configDir,
		"BORG_CACHE_DIR="+filepath.Join(r.configDir, "cache"),
		"BORG_TESTONLY_WEAKEN_KDF=1",
		"BORG_PASSPHRASE="+r.passphrase,
		"BORG_KEY_FILE=",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
	)
}

func (r *borgRepo) mustRun(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("borg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// borgeEnv builds the Env borge's commands run in, wired to the same repository and key
// directory borg used.
func (r *borgRepo) borgeEnv(stdout, stderr *bytes.Buffer) *Env {
	vars := map[string]string{
		"BORGE_REPO":                r.path,
		"BORGE_KEYS_DIR":            r.keysDir,
		"BORGE_PASSPHRASE":          r.passphrase,
		"BORGE_TESTONLY_WEAKEN_KDF": "1",
	}
	return &Env{
		Stdout: stdout,
		Stderr: stderr,
		Getenv: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		},
	}
}

func (r *borgRepo) borge(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(r.borgeEnv(&stdout, &stderr), args)
	return stdout.String(), stderr.String(), code
}

// makeArchives builds a small source tree and archives it several times.
func (r *borgRepo) makeArchives(names ...string) {
	r.t.Helper()
	src := r.t.TempDir()
	r.src = src
	for i := 0; i < 3; i++ {
		p := filepath.Join(src, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("content ", 10+i)), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink("file0.txt", filepath.Join(src, "link")); err != nil {
		r.t.Fatal(err)
	}
	for _, name := range names {
		r.mustRun("create", "-r", r.path, name, src)
	}
}

func TestRepoListMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("first", "second", "third")

	// The machine-readable form is compared as data.
	type row struct {
		Name     string `json:"name"`
		ID       string `json:"id"`
		Hostname string `json:"hostname"`
		Username string `json:"username"`
	}
	type doc struct {
		Archives []row `json:"archives"`
	}

	var wantDoc, gotDoc doc
	if err := json.Unmarshal([]byte(r.mustRun("repo-list", "-r", r.path, "--json")), &wantDoc); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := r.borge(t, "repo-list", "--json")
	if code != ExitOK {
		t.Fatalf("borge repo-list --json exited %d\n%s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &gotDoc); err != nil {
		t.Fatalf("borge's JSON does not parse: %v\n%s", err, stdout)
	}
	if len(gotDoc.Archives) != len(wantDoc.Archives) {
		t.Fatalf("borge listed %d archives, borg %d", len(gotDoc.Archives), len(wantDoc.Archives))
	}
	for i := range wantDoc.Archives {
		if gotDoc.Archives[i] != wantDoc.Archives[i] {
			t.Errorf("archive %d differs\n  borge: %+v\n  borg:  %+v", i, gotDoc.Archives[i], wantDoc.Archives[i])
		}
	}

	// The text form is compared by fields, so column padding is not part of the contract.
	wantLines := nonEmptyLines(r.mustRun("repo-list", "-r", r.path))
	gotOut, _, code := r.borge(t, "repo-list")
	if code != ExitOK {
		t.Fatalf("borge repo-list exited %d", code)
	}
	gotLines := nonEmptyLines(gotOut)
	if len(gotLines) != len(wantLines) {
		t.Fatalf("borge printed %d lines, borg %d\nborge:\n%s\nborg:\n%s",
			len(gotLines), len(wantLines), gotOut, strings.Join(wantLines, "\n"))
	}
	for i := range wantLines {
		if strings.Join(strings.Fields(gotLines[i]), " ") != strings.Join(strings.Fields(wantLines[i]), " ") {
			t.Errorf("line %d differs\n  borge: %q\n  borg:  %q", i, gotLines[i], wantLines[i])
		}
	}

	// --short is exactly the names, so it can be compared literally.
	if got, want := r.mustRun("repo-list", "-r", r.path, "--short"), ""; want == "" {
		gotShort, _, _ := r.borge(t, "repo-list", "-short")
		if gotShort != got {
			t.Errorf("--short differs\n  borge: %q\n  borg:  %q", gotShort, got)
		}
	}
}

func TestListMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	r.makeArchives("only")

	// json-lines, compared as data.
	wantItems := parseJSONLines(t, r.mustRun("list", "-r", r.path, "only", "--json-lines"))
	stdout, stderr, code := r.borge(t, "list", "-json-lines", "only")
	if code != ExitOK {
		t.Fatalf("borge list exited %d\n%s", code, stderr)
	}
	gotItems := parseJSONLines(t, stdout)

	if len(gotItems) != len(wantItems) {
		t.Fatalf("borge listed %d items, borg %d", len(gotItems), len(wantItems))
	}
	for i := range wantItems {
		for _, field := range []string{"path", "mode", "type", "user", "group", "uid", "gid", "size", "target", "mtime", "hlid"} {
			w, g := wantItems[i][field], gotItems[i][field]
			if fmt.Sprint(w) != fmt.Sprint(g) {
				t.Errorf("item %d (%v): %s differs (borg %v, borge %v)",
					i, wantItems[i]["path"], field, w, g)
			}
		}
	}

	// The text listing, by fields.
	wantLines := nonEmptyLines(r.mustRun("list", "-r", r.path, "only"))
	gotOut, _, _ := r.borge(t, "list", "only")
	gotLines := nonEmptyLines(gotOut)
	if len(gotLines) != len(wantLines) {
		t.Fatalf("borge printed %d lines, borg %d", len(gotLines), len(wantLines))
	}
	for i := range wantLines {
		w := strings.Join(strings.Fields(wantLines[i]), " ")
		g := strings.Join(strings.Fields(gotLines[i]), " ")
		if w != g {
			t.Errorf("line %d differs\n  borge: %q\n  borg:  %q", i, g, w)
		}
	}
}

// TestListPatternsSelectTheSameItems: the pattern options have to narrow a listing the
// same way, because the same options narrow an extraction.
func TestListPatternsSelectTheSameItems(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	r.makeArchives("patterned")

	cases := []struct{ borgArgs, borgeArgs []string }{
		{[]string{"--exclude", "*.txt"}, []string{"-exclude", "*.txt"}},
		{[]string{"--exclude", "sub/"}, []string{"-exclude", "sub/"}},
		{[]string{"--pattern", "- sh:**/file1.txt"}, []string{"-pattern", "- sh:**/file1.txt"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.borgArgs, " "), func(t *testing.T) {
			args := append([]string{"list", "-r", r.path, "patterned", "--short"}, tc.borgArgs...)
			want := nonEmptyLines(r.mustRun(args...))

			borgeArgs := append([]string{"list", "-short"}, tc.borgeArgs...)
			borgeArgs = append(borgeArgs, "patterned")
			gotOut, stderr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge exited %d\n%s", code, stderr)
			}
			got := nonEmptyLines(gotOut)

			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("selection differs\n  borge (%d):\n%s\n  borg (%d):\n%s",
					len(got), strings.Join(got, "\n"), len(want), strings.Join(want, "\n"))
			}
		})
	}
}

func TestInfoMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("described")

	want := parseKeyValues(r.mustRun("info", "-r", r.path, "-a", "described"))
	gotOut, stderr, code := r.borge(t, "info", "-a", "described")
	if code != ExitOK {
		t.Fatalf("borge info exited %d\n%s", code, stderr)
	}
	got := parseKeyValues(gotOut)

	// Compare the fields both tools report and that are not derived from borg's own
	// accounting of a backup it performed.
	for _, field := range []string{
		"Archive name", "Archive fingerprint", "Comment", "Hostname", "Username",
		"Time (nominal)", "Time (start)", "Time (end)", "Command line",
		"Working Directory", "Number of files",
	} {
		w, ok := want[field]
		if !ok {
			t.Errorf("borg did not report %q; the comparison is checking the wrong thing", field)
			continue
		}
		if g := got[field]; g != w {
			t.Errorf("%s differs\n  borge: %q\n  borg:  %q", field, g, w)
		}
	}
}

// TestExtractCommandMatchesBorg drives extraction through the command line rather than
// the library, so the flag wiring is covered too.
func TestExtractCommandMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("restored")

	borgDir := t.TempDir()
	cmd := exec.Command(r.binary, "extract", "-r", r.path, "restored")
	cmd.Env = r.env()
	cmd.Dir = borgDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("borg extract: %v\n%s", err, out)
	}

	borgeDir := t.TempDir()
	_, stderr, code := r.borge(t, "extract", "-C", borgeDir, "restored")
	if code != ExitOK {
		t.Fatalf("borge extract exited %d\n%s", code, stderr)
	}

	rel := strings.TrimPrefix(filepath.ToSlash(r.src), "/")
	wantRoot := filepath.Join(borgDir, filepath.FromSlash(rel))
	gotRoot := filepath.Join(borgeDir, filepath.FromSlash(rel))

	wantNames := walkNames(t, wantRoot)
	gotNames := walkNames(t, gotRoot)
	if strings.Join(gotNames, "\n") != strings.Join(wantNames, "\n") {
		t.Errorf("extracted trees differ\n  borge: %v\n  borg:  %v", gotNames, wantNames)
	}
	if len(wantNames) == 0 {
		t.Fatal("borg extracted nothing; the comparison is vacuous")
	}
}

func TestErrorsAreReportedNotPanicked(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	r.makeArchives("exists")

	cases := [][]string{
		{"list", "no-such-archive"},
		{"info", "-a", "no-such-archive"},
		{"list"},
		{"extract"},
		{"nonsense"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stderr, code := r.borge(t, args...)
			if code == ExitOK {
				t.Errorf("succeeded, want a failure")
			}
			if stderr == "" {
				t.Error("failed without saying why")
			}
		})
	}
}

// TestNoRepositoryIsAClearError: the most common mistake should produce a sentence that
// says what to do, not a stack of internals.
func TestNoRepositoryIsAClearError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &Env{Stdout: &stdout, Stderr: &stderr, Getenv: func(string) (string, bool) { return "", false }}
	if code := Run(e, []string{"repo-list"}); code != ExitError {
		t.Errorf("exited %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "BORGE_REPO") {
		t.Errorf("the error does not say how to give a repository: %q", stderr.String())
	}
}

// ---------------------------------------------------------------- helpers

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, " \t"))
		}
	}
	return out
}

func parseJSONLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("could not parse a JSON line: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func parseKeyValues(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func walkNames(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
