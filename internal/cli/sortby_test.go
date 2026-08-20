// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --sort-by and --depth on list and diff, and the order of diff's output.
//
// The order is the reason these tests exist. borge sorted every diff by path, which is
// borg's "--sort-by path" output and not borg's default - so every line matched and the
// sequence did not. Nothing caught it: the diff tests all sorted both sides before
// comparing, which is exactly the shape of a test that cannot see an ordering bug.

// borgStreams runs borg and returns its two streams apart, which mustRun's CombinedOutput
// cannot: half of what is checked below is which stream something went to.
func borgStreams(t *testing.T, r *borgRepo, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("borg %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// sortTree builds a tree whose items differ in every field the sort keys read that an
// unprivileged test can vary: name, kind, mode, size and modification time.
//
// What it cannot vary is the owner. Every file a test creates belongs to the user running
// it, so "--sort-by user" and its three relatives are compared here as ties - the sort is
// exercised, the ordering of *distinct* owners is not. Said plainly rather than left as an
// unexamined pass.
func sortTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	files := []struct {
		name  string
		size  int
		mode  os.FileMode
		mtime time.Time
	}{
		{"zebra.txt", 300, 0o644, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"apple.txt", 10, 0o600, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"middle.txt", 2000, 0o755, time.Date(2020, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"beta.bin", 1, 0o400, time.Date(2022, 6, 15, 12, 0, 0, 0, time.UTC)},
	}
	for _, f := range files {
		p := filepath.Join(src, f.name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), f.size), f.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "deeper", "buried.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("apple.txt", filepath.Join(src, "alink")); err != nil {
		t.Fatal(err)
	}
	return src
}

// The item sort fields, and the diff sort fields, as borg lists them in its error message.
var (
	listSpecs = []string{
		"path", ">path", "size", ">size,path", "mode,path", "type,path",
		"mtime,path", ">mtime,path", "ctime,path", "atime,path",
		"user,path", "group,path", "uid,path", "gid,path",
		">size,>mtime,path",
	}
	diffSpecs = []string{
		"path", ">path", "size_added", ">size_added", "size_removed", ">size_removed",
		"size_diff", ">size_diff,path", "size", ">size", "mtime", ">mtime_diff",
		"ctime_diff", "user,path", "group,path", "uid,path", "gid,path",
	}
)

func TestListSortByMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := sortTree(t)
	r.mustRun("create", "-r", r.path, "src", src)

	const format = "{mode} {user} {group} {uid} {gid} {size} {mtime} {path}{NL}"
	unsorted, _ := borgStreams(t, r, "list", "-r", r.path, "src", "--format", format)
	if n := strings.Count(unsorted, "\n"); n < 8 {
		t.Fatalf("the archive has only %d items; the sorts below would be near-vacuous", n)
	}

	reordered := 0
	for _, spec := range listSpecs {
		t.Run(spec, func(t *testing.T) {
			want, _ := borgStreams(t, r, "list", "-r", r.path, "src", "--format", format, "--sort-by", spec)
			got, stderr, code := r.borge(t, "list", "-r", r.path, "src", "--format", format, "--sort-by", spec)
			if code != ExitOK {
				t.Fatalf("borge list --sort-by %q exited %d\n%s", spec, code, stderr)
			}
			if got != want {
				t.Errorf("borge list --sort-by %q\n got:\n%s\nwant:\n%s", spec, got, want)
			}
			if want != unsorted {
				reordered++
			}
		})
	}
	// A sort that never moves anything would compare equal however it was implemented.
	if reordered == 0 {
		t.Fatal("no --sort-by spec reordered the listing; the comparison proves nothing")
	}
}

func TestListDepthMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := sortTree(t)
	r.mustRun("create", "-r", r.path, "src", src)

	counts := map[string]int{}
	for _, depth := range []string{"-1", "0", "1", "2", "3", "20"} {
		t.Run("depth"+depth, func(t *testing.T) {
			want, _ := borgStreams(t, r, "list", "-r", r.path, "src", "--format", "{path}{NL}", "--depth", depth)
			got, stderr, code := r.borge(t, "list", "-r", r.path, "src", "--format", "{path}{NL}", "--depth", depth)
			if code != ExitOK {
				t.Fatalf("borge list --depth %s exited %d\n%s", depth, code, stderr)
			}
			if got != want {
				t.Errorf("borge list --depth %s\n got:\n%s\nwant:\n%s", depth, got, want)
			}
			counts[depth] = strings.Count(want, "\n")
		})
	}
	// --depth -1 excludes everything and a large depth excludes nothing; if every depth
	// gave the same count the option would not be under test.
	if counts["-1"] != 0 {
		t.Errorf("borg's --depth -1 listed %d paths, want none", counts["-1"])
	}
	if counts["20"] <= counts["0"] {
		t.Errorf("--depth 20 listed %d paths and --depth 0 listed %d; the filter is not biting",
			counts["20"], counts["0"])
	}

	// Omitting the option is not the same as passing its zero value.
	all, _ := borgStreams(t, r, "list", "-r", r.path, "src", "--format", "{path}{NL}")
	got, _, _ := r.borge(t, "list", "-r", r.path, "src", "--format", "{path}{NL}")
	if got != all {
		t.Errorf("without --depth\n got:\n%s\nwant:\n%s", got, all)
	}
	if strings.Count(all, "\n") == counts["0"] {
		t.Error("--depth 0 listed as much as no --depth at all")
	}
}

// makeDiffArchives builds two archives whose *stream* order is not their sorted order, and
// which differ by an addition, a removal and two modifications - the shape that makes the
// default diff order visible at all.
func makeDiffArchives(t *testing.T, r *borgRepo) {
	t.Helper()
	src := sortTree(t)
	// The roots are given out of alphabetical order and borg archives them in the order
	// given, so the item stream is not sorted by path.
	r.mustRun("create", "-r", r.path, "one",
		filepath.Join(src, "zebra.txt"), filepath.Join(src, "middle.txt"),
		filepath.Join(src, "apple.txt"), filepath.Join(src, "beta.bin"))

	if err := os.WriteFile(filepath.Join(src, "zebra.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "apple.txt"), bytes.Repeat([]byte("y"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "middle.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "aaa_new.txt"), []byte("new file"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "two",
		filepath.Join(src, "zebra.txt"), filepath.Join(src, "apple.txt"),
		filepath.Join(src, "beta.bin"), filepath.Join(src, "aaa_new.txt"))
}

// TestDiffDefaultOrderIsBorgs is the regression this cluster started from.
func TestDiffDefaultOrderIsBorgs(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	makeDiffArchives(t, r)

	want, _ := borgStreams(t, r, "diff", "-r", r.path, "one", "two")
	got, stderr, code := r.borge(t, "diff", "-r", r.path, "one", "two")
	if code != ExitOK {
		t.Fatalf("borge diff exited %d\n%s", code, stderr)
	}
	if n := strings.Count(want, "\n"); n < 4 {
		t.Fatalf("borg reported only %d changed paths; the order is not under test", n)
	}
	if got != want {
		t.Errorf("borge diff default order\n got:\n%s\nwant:\n%s", got, want)
	}

	// And it must not be the sorted order: that is what borge used to print, and a tree
	// whose stream order happened to be sorted would let the old behaviour pass.
	sorted, _ := borgStreams(t, r, "diff", "-r", r.path, "one", "two", "--sort-by", "path")
	if sorted == want {
		t.Fatal("borg's default order equals its --sort-by path order here, so this " +
			"comparison cannot tell them apart")
	}
}

func TestDiffSortByMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	makeDiffArchives(t, r)

	unsorted, _ := borgStreams(t, r, "diff", "-r", r.path, "one", "two")
	reordered := 0
	for _, spec := range diffSpecs {
		t.Run(spec, func(t *testing.T) {
			want, _ := borgStreams(t, r, "diff", "-r", r.path, "one", "two", "--sort-by", spec)
			got, stderr, code := r.borge(t, "diff", "-r", r.path, "one", "two", "--sort-by", spec)
			if code != ExitOK {
				t.Fatalf("borge diff --sort-by %q exited %d\n%s", spec, code, stderr)
			}
			if got != want {
				t.Errorf("borge diff --sort-by %q\n got:\n%s\nwant:\n%s", spec, got, want)
			}
			if want != unsorted {
				reordered++
			}
		})
	}
	if reordered == 0 {
		t.Fatal("no --sort-by spec reordered the diff; the comparison proves nothing")
	}
}

// TestDiffSameChunkerParams: the option, and the warning it silences.
//
// borg prints that warning unconditionally. borge printed it only under -v, which is the
// wrong way round - the run that needs it is the one whose every byte count silently
// becomes "(can't get size)", and that run is usually not a verbose one.
func TestDiffSameChunkerParams(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := sortTree(t)
	r.mustRun("create", "-r", r.path, "one", src)
	// The content has to change as well as the chunker parameters. With parameters that
	// differ, borg compares the bytes rather than the chunk ids - so two archives of an
	// unchanged tree report no differences at all, and the "(can't get size)" rendering
	// this test is about never appears. That is how the first version of it failed.
	if err := os.WriteFile(filepath.Join(src, "zebra.txt"), []byte("changed content"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "--chunker-params", "buzhash,10,23,16,4095", "-r", r.path, "two", src)

	wantOut, wantErr := borgStreams(t, r, "diff", "-r", r.path, "one", "two")
	if !strings.Contains(wantErr, "--same-chunker-params") {
		t.Fatalf("borg printed no chunker-params warning, so this test is not testing it:\n%s", wantErr)
	}
	if !strings.Contains(wantOut, "can't get size") {
		t.Fatalf("borg's diff shows sizes, so the archives were not chunked differently:\n%s", wantOut)
	}
	gotOut, gotErr, code := r.borge(t, "diff", "-r", r.path, "one", "two")
	if code != ExitOK {
		t.Fatalf("borge diff exited %d\n%s", code, gotErr)
	}
	if gotOut != wantOut {
		t.Errorf("stdout\n got:\n%s\nwant:\n%s", gotOut, wantOut)
	}
	if gotErr != wantErr {
		t.Errorf("stderr\n got:  %q\nwant: %q", gotErr, wantErr)
	}

	// With the override: no warning from either, and the comparison uses chunk ids.
	wantOut, wantErr = borgStreams(t, r, "diff", "-r", r.path, "one", "two", "--same-chunker-params")
	if wantErr != "" {
		t.Errorf("borg still warned with --same-chunker-params: %q", wantErr)
	}
	gotOut, gotErr, code = r.borge(t, "diff", "-r", r.path, "one", "two", "--same-chunker-params")
	if code != ExitOK {
		t.Fatalf("borge diff --same-chunker-params exited %d\n%s", code, gotErr)
	}
	if gotOut != wantOut {
		t.Errorf("stdout with --same-chunker-params\n got:\n%s\nwant:\n%s", gotOut, wantOut)
	}
	if gotErr != "" {
		t.Errorf("borge warned with --same-chunker-params: %q", gotErr)
	}

	// Under --log-json the same warning has to be ONE record at WARNING, as borg emits it.
	// borge wrote it to stderr as text, so the JSON logger split it on the newline and
	// produced two records at INFO: a frontend filtering on levelname saw no warning at
	// all. Only the shape is asserted - the "name" field differs between the two tools by
	// an older decision (DIVERGENCES #41).
	_, jsonErr := borgStreams(t, r, "diff", "--log-json", "-r", r.path, "one", "two")
	_, gotJSON, code := r.borge(t, "diff", "--log-json", "-r", r.path, "one", "two")
	if code != ExitOK {
		t.Fatalf("borge diff --log-json exited %d\n%s", code, gotJSON)
	}
	records := func(stream string) []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimRight(stream, "\n"), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, line)
			}
			out = append(out, rec)
		}
		return out
	}
	want, got := records(jsonErr), records(gotJSON)
	if len(want) != 1 {
		t.Fatalf("borg emitted %d records, want 1; this test asserts the wrong thing", len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("borge emitted %d records, want %d:\n%s", len(got), len(want), gotJSON)
	}
	if got[0]["message"] != want[0]["message"] {
		t.Errorf("message\n got: %q\nwant: %q", got[0]["message"], want[0]["message"])
	}
	if got[0]["levelname"] != want[0]["levelname"] {
		t.Errorf("levelname = %v, want %v", got[0]["levelname"], want[0]["levelname"])
	}
}

// TestSortSpecErrorsMatchBorg: a rejected spec has to be rejected by both, with the same
// exit code and the same reason, and before anything is read.
func TestSortSpecErrorsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := sortTree(t)
	r.mustRun("create", "-r", r.path, "one", src)
	r.mustRun("create", "-r", r.path, "two", src)

	cases := []struct {
		name string
		args []string
		spec string
		want string
	}{
		{"list-bogus", []string{"list", "-r", r.path, "one"}, "bogus",
			"unsupported sort field: bogus, supported: path, type, mode, user, uid, group, gid, size, mtime, ctime, atime"},
		{"list-empty", []string{"list", "-r", r.path, "one"}, "",
			"unsupported sort field: empty spec"},
		{"list-one-bad-of-two", []string{"list", "-r", r.path, "one"}, "path,nope",
			"unsupported sort field: nope"},
		{"diff-bogus", []string{"diff", "-r", r.path, "one", "two"}, "bogus",
			"unsupported sort field: bogus, supported: path, size_added, size_removed, size_diff, size, user, group, uid, gid, ctime, mtime, ctime_diff, mtime_diff"},
		{"diff-empty", []string{"diff", "-r", r.path, "one", "two"}, "",
			"unsupported sort field: empty spec"},
		{"diff-item-key", []string{"diff", "-r", r.path, "one", "two"}, "mode",
			"unsupported sort field: mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{}, c.args...), "--sort-by", c.spec)
			out, err := r.runErr(args...)
			if err == nil {
				t.Fatalf("borg accepted --sort-by %q:\n%s", c.spec, out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("borg's message changed; this test asserts the wrong text.\nwant %q in:\n%s", c.want, out)
			}
			stdout, stderr, code := r.borge(t, args...)
			if code != ExitError {
				t.Fatalf("borge exited %d for --sort-by %q, want %d\n%s", code, c.spec, ExitError, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("borge's message for --sort-by %q:\n got: %s\nwant it to contain: %s",
					c.spec, stderr, c.want)
			}
			if stdout != "" {
				t.Errorf("borge wrote %q to stdout before rejecting the spec", stdout)
			}
		})
	}
}

// TestDiffJSONLinesOrderIsBorgs: the JSON stream is ordered too, and nothing was checking
// it.
//
// The --json-lines tests compare a map keyed by path, which is the right shape for a schema
// comparison and blind to sequence by construction. A frontend reading the stream sees the
// order, so it is asserted here - by path alone, so that this test says nothing about the
// key order inside an object, which is json_lines_test.go's business.
func TestDiffJSONLinesOrderIsBorgs(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	makeDiffArchives(t, r)

	paths := func(stream string) []string {
		var out []string
		for _, line := range strings.Split(strings.TrimRight(stream, "\n"), "\n") {
			if line == "" {
				continue
			}
			var obj struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, line)
			}
			out = append(out, obj.Path)
		}
		return out
	}

	for _, spec := range []string{"", "path", ">size_added"} {
		name := spec
		if name == "" {
			name = "(default)"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"diff", "-r", r.path, "one", "two", "--json-lines"}
			if spec != "" {
				args = append(args, "--sort-by", spec)
			}
			wantOut, _ := borgStreams(t, r, args...)
			gotOut, stderr, code := r.borge(t, args...)
			if code != ExitOK {
				t.Fatalf("borge exited %d\n%s", code, stderr)
			}
			want, got := paths(wantOut), paths(gotOut)
			if len(want) < 4 {
				t.Fatalf("borg emitted %d objects; the order is not under test", len(want))
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("path order\n got: %v\nwant: %v", got, want)
			}
		})
	}
}
