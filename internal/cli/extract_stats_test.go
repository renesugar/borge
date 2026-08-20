// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extract --stdout, --continue and --stats, and the store statistics the last of them
// prints.
//
// The values cannot be compared against borg's - they are times, call counts and
// throughputs of two different implementations doing different amounts of I/O. What is
// compared is the *shape*: which keys, in which order, rendered in which units. That is
// what a frontend or a script reads. See DIVERGENCES.md #51.

// borgStreamsIn is borgStreams with a working directory, which is how borg is told where
// to extract: it has no -C, and extracts into the directory it was run in.
func borgStreamsIn(t *testing.T, r *borgRepo, dir string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("borg %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// borgeIn runs borge with -C, which is borge's own way of saying the same thing. The two
// are not the same mechanism, and that is the point of comparing them: borge's -C has to
// put the files where borg's working directory does.
func (r *borgRepo) borgeIn(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	return r.borge(t, append(args, "-C", dir)...)
}

// statsTree is a few files big enough to be chunked, so a store report has something in it.
func statsTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{
		"a.txt":     4000,
		"b.bin":     300000,
		"sub/c.txt": 120000,
	} {
		body := bytes.Repeat([]byte(name+" contents "), size/len(name+" contents ")+1)[:size]
		if err := os.WriteFile(filepath.Join(src, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// storeStatLines keeps the "Store ..." lines and strips the values, leaving the labels and
// the unit words: that is the part both tools must agree on.
func storeStatLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "Store ") {
			continue
		}
		label, value, ok := strings.Cut(line, ": ")
		if !ok {
			out = append(out, line)
			continue
		}
		// Keep the unit, drop the number: "0.003 seconds" -> "seconds", "6.59 kB" -> "kB",
		// "764.40 kB/s" -> "kB/s". A percentage and a bare count keep nothing.
		unit := ""
		if fields := strings.Fields(value); len(fields) == 2 {
			unit = " " + fields[1]
		}
		out = append(out, label+":"+unit)
	}
	return out
}

// TestExtractStatsShapeMatchesBorg: the store report is borg's, line for line.
func TestExtractStatsShapeMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := statsTree(t)
	r.mustRun("create", "-r", r.path, "src", src)

	borgDir, borgeDir := t.TempDir(), t.TempDir()
	_, wantErr := borgStreamsIn(t, r, borgDir, "extract", "-s", "-r", r.path, "src")
	want := storeStatLines(wantErr)
	if len(want) < 20 {
		t.Fatalf("borg printed %d store lines; the comparison would be thin:\n%s", len(want), wantErr)
	}

	_, gotErr, code := r.borgeIn(t, borgeDir, "extract", "-s", "-r", r.path, "src")
	if code != ExitOK {
		t.Fatalf("borge extract -s exited %d\n%s", code, gotErr)
	}
	got := storeStatLines(gotErr)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("store statistics\n got:\n%s\nwant:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// And the numbers have to be real: a report of all zeroes would have the right shape
	// and say nothing. The load count and volume are the ones an extraction must move.
	for _, want := range []string{"Store load calls:", "Store load volume:"} {
		line := ""
		for _, l := range strings.Split(gotErr, "\n") {
			if strings.HasPrefix(l, want) {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("borge printed no %q line", want)
		}
		if strings.HasSuffix(line, " 0") || strings.HasSuffix(line, " 0 B") {
			t.Errorf("%q - an extraction that loaded nothing", line)
		}
	}
}

// TestExtractStdoutMatchesBorg: the contents go to stdout, and nothing goes to disk.
func TestExtractStdoutMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := statsTree(t)
	r.mustRun("create", "-r", r.path, "src", src)

	// The whole archive, and then one path: both are borg's concatenation of the matched
	// files' contents in archive order.
	// The second case names one file with a shell pattern - a bare "**/b.bin" would be
	// taken as a path prefix by both tools, which is borg's default pattern style.
	for _, paths := range [][]string{nil, {"sh:**/b.bin"}} {
		name := "whole archive"
		if paths != nil {
			name = strings.Join(paths, " ")
		}
		t.Run(name, func(t *testing.T) {
			borgDir, borgeDir := t.TempDir(), t.TempDir()
			args := append([]string{"extract", "--stdout", "-r", r.path, "src"}, paths...)
			wantOut, _ := borgStreamsIn(t, r, borgDir, args...)
			if len(wantOut) == 0 {
				t.Fatal("borg wrote nothing to stdout; the comparison would be vacuous")
			}
			gotOut, stderr, code := r.borgeIn(t, borgeDir, args...)
			if code != ExitOK {
				t.Fatalf("borge extract --stdout exited %d\n%s", code, stderr)
			}
			if gotOut != wantOut {
				t.Errorf("stdout differs: got %d bytes, want %d", len(gotOut), len(wantOut))
			}
			// Neither tool may have created anything in the directory it ran in.
			for _, dir := range []string{borgDir, borgeDir} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("%s is not empty after --stdout: %v", dir, entries)
				}
			}
		})
	}
}

// TestExtractContinueMatchesBorg: what --continue skips, and what it refuses to skip.
//
// Compared by *outcome* rather than by counting reads: borg's store reports one load per
// pack range, so a skipped file need not change the count at all - which is how a first
// attempt at this test managed to show borg's --continue doing nothing.
func TestExtractContinueMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := statsTree(t)
	r.mustRun("create", "-r", r.path, "src", src)

	// corrupt replaces a file's contents with zeroes, keeping its size and mtime - which
	// is what an already-extracted file looks like to borg's same_item check.
	corrupt := func(t *testing.T, dir, name string) string {
		t.Helper()
		var found string
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() && filepath.Base(p) == name {
				found = p
			}
			return nil
		})
		if found == "" {
			t.Fatalf("%s was not extracted into %s", name, dir)
		}
		info, err := os.Stat(found)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(found, make([]byte, info.Size()), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(found, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		return found
	}
	isZeroes := func(t *testing.T, path string) bool {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range data {
			if b != 0 {
				return false
			}
		}
		return true
	}

	borgDir, borgeDir := t.TempDir(), t.TempDir()
	borgStreamsIn(t, r, borgDir, "extract", "-r", r.path, "src")
	if _, stderr, code := r.borgeIn(t, borgeDir, "extract", "-r", r.path, "src"); code != ExitOK {
		t.Fatalf("borge extract exited %d\n%s", code, stderr)
	}

	// 1. A file that is the right size with the right mtime is skipped, wrong contents
	//    and all. That is the bargain --continue makes, and borg makes it too.
	borgFile := corrupt(t, borgDir, "b.bin")
	borgeFile := corrupt(t, borgeDir, "b.bin")
	borgStreamsIn(t, r, borgDir, "extract", "--continue", "-r", r.path, "src")
	if _, stderr, code := r.borgeIn(t, borgeDir, "extract", "--continue", "-r", r.path, "src"); code != ExitOK {
		t.Fatalf("borge extract --continue exited %d\n%s", code, stderr)
	}
	if !isZeroes(t, borgFile) {
		t.Fatal("borg's --continue re-extracted a file whose size and mtime matched; " +
			"this test asserts the wrong behaviour")
	}
	if !isZeroes(t, borgeFile) {
		t.Error("borge's --continue re-extracted a file borg would have skipped")
	}

	// 2. Without the option, both re-extract it.
	borgStreamsIn(t, r, borgDir, "extract", "-r", r.path, "src")
	if _, stderr, code := r.borgeIn(t, borgeDir, "extract", "-r", r.path, "src"); code != ExitOK {
		t.Fatalf("borge extract exited %d\n%s", code, stderr)
	}
	if isZeroes(t, borgFile) {
		t.Fatal("borg left the file zeroed without --continue; the test's premise is wrong")
	}
	if isZeroes(t, borgeFile) {
		t.Error("borge left a corrupted file in place without --continue")
	}

	// 3. A truncated file is re-extracted even with --continue: the size check is what
	//    catches an extraction interrupted part way through a file.
	truncate := func(t *testing.T, path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 100); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	truncate(t, borgFile)
	truncate(t, borgeFile)
	borgStreamsIn(t, r, borgDir, "extract", "--continue", "-r", r.path, "src")
	if _, stderr, code := r.borgeIn(t, borgeDir, "extract", "--continue", "-r", r.path, "src"); code != ExitOK {
		t.Fatalf("borge extract --continue exited %d\n%s", code, stderr)
	}
	for tool, path := range map[string]string{"borg": borgFile, "borge": borgeFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 100 {
			t.Errorf("%s left a truncated file in place under --continue", tool)
		}
	}
}

// TestCreateJSONCarriesTheMeasurements: the three keys that used to be omitted.
func TestCreateJSONCarriesTheMeasurements(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := statsTree(t)

	read := func(stream string) map[string]any {
		var doc struct {
			Archive struct {
				Stats map[string]any `json:"stats"`
			} `json:"archive"`
		}
		if err := json.Unmarshal([]byte(stream), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, stream)
		}
		return doc.Archive.Stats
	}

	wantOut := r.mustRun("create", "-r", r.path, "by-borg", src, "--json")
	want := read(wantOut)
	gotOut, stderr, code := r.borge(t, "create", "by-borge", src, "--json")
	if code != ExitOK {
		t.Fatalf("borge create --json exited %d\n%s", code, stderr)
	}
	got := read(gotOut)

	for _, key := range []string{"hashing_time", "chunking_time"} {
		w, ok := want[key].(float64)
		if !ok {
			t.Fatalf("borg's %s is %T, not a number; this test asserts the wrong type", key, want[key])
		}
		g, ok := got[key].(float64)
		if !ok {
			t.Fatalf("borge's %s is %T, not a number", key, got[key])
		}
		// Not compared as values - two implementations, two machines' worth of noise -
		// but both must have measured something, since both hashed and chunked 400 kB.
		if w <= 0 {
			t.Fatalf("borg reported %s = %v; the archive was too small to time", key, w)
		}
		if g <= 0 {
			t.Errorf("borge reported %s = %v, which is the flat line the key was omitted to avoid", key, g)
		}
	}

	wantStore, _ := want["store_stats"].(map[string]any)
	gotStore, _ := got["store_stats"].(map[string]any)
	if len(wantStore) == 0 {
		t.Fatal("borg's create sent no store_stats")
	}
	if len(gotStore) != len(wantStore) {
		t.Errorf("store_stats has %d keys, borg sends %d", len(gotStore), len(wantStore))
	}
	for k := range wantStore {
		if _, ok := gotStore[k]; !ok {
			t.Errorf("store_stats is missing %q", k)
		}
	}
	// The load counters must be real here too.
	if v, _ := gotStore["store_calls"].(float64); v <= 0 {
		t.Errorf("store_stats.store_calls is %v after writing an archive", v)
	}
}
