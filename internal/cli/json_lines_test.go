// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The --json-lines commands were never compared against borg as data. TestJSONSchema-
// MatchesBorg covers the eight --json commands; list, find and diff produce a stream
// rather than a document and had no gate at all, which is how three separate differences
// survived: find emitted an envelope of borge's own, diff renamed every key in a change,
// and the item object was not driven by --format. See DIVERGENCES.md #43.

// jsonLinesOf runs a command on both tools and returns the parsed streams.
func jsonLinesOf(t *testing.T, r *borgRepo, borgArgs, borgeArgs []string) (borgOut, borgeOut []map[string]any) {
	t.Helper()
	parse := func(who, s string) []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
			if line == "" {
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Fatalf("%s produced an unparseable line: %v\n%s", who, err, line)
			}
			out = append(out, obj)
		}
		if len(out) == 0 {
			t.Fatalf("%s produced no objects at all", who)
		}
		return out
	}
	stdout, _ := r.runSplit(borgArgs...)
	borgOut = parse("borg", stdout)

	stdout, stderr, code := r.borge(t, borgeArgs...)
	if code != ExitOK {
		t.Fatalf("borge %v exited %d\n%s", borgeArgs, code, stderr)
	}
	borgeOut = parse("borge", stdout)
	return borgOut, borgeOut
}

func jsonKeysOf(objs []map[string]any) []string {
	seen := map[string]bool{}
	for _, o := range objs {
		for k := range o {
			seen[k] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestJSONLinesSchemasMatchBorg: list, find and diff emit borg's objects.
func TestJSONLinesSchemasMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("first")

	// A second archive that differs, so diff has changes of several kinds to report.
	extra := filepath.Join(r.src, "added.txt")
	write(t, extra, "added in the second archive")
	if err := os.Chmod(filepath.Join(r.src, "file0.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "second", r.src)

	cases := []struct {
		name        string
		borg, borge []string
	}{
		{"list", []string{"list", "-r", r.path, "first", "--json-lines"},
			[]string{"list", "first", "--json-lines"}},
		{"list with a format", []string{"list", "-r", r.path, "first", "--json-lines", "--format", "{path}"},
			[]string{"list", "first", "--json-lines", "--format", "{path}"}},
		{"find", []string{"find", "-r", r.path, "--json-lines", "-a", "first", "sh:**"},
			[]string{"find", "--json-lines", "-a", "first", "sh:**"}},
		{"diff", []string{"diff", "-r", r.path, "first", "second", "--json-lines"},
			[]string{"diff", "first", "second", "--json-lines"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			borgObjs, borgeObjs := jsonLinesOf(t, r, c.borg, c.borge)
			want, got := jsonKeysOf(borgObjs), jsonKeysOf(borgeObjs)
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("keys differ\nborg : %v\nborge: %v", want, got)
			}
		})
	}

	// The format case has to actually drop keys, or it is the same test twice.
	full, _ := jsonLinesOf(t, r,
		[]string{"list", "-r", r.path, "first", "--json-lines"},
		[]string{"list", "first", "--json-lines"})
	trimmed, _ := jsonLinesOf(t, r,
		[]string{"list", "-r", r.path, "first", "--json-lines", "--format", "{path}"},
		[]string{"list", "first", "--json-lines", "--format", "{path}"})
	if len(jsonKeysOf(trimmed)) >= len(jsonKeysOf(full)) {
		t.Errorf("--format '{path}' did not reduce the key set: %v vs %v",
			jsonKeysOf(trimmed), jsonKeysOf(full))
	}
}

// TestDiffJSONValuesMatchBorg: the change objects agree, not only their key names.
func TestDiffJSONValuesMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("v1")

	write(t, filepath.Join(r.src, "added.txt"), "new")
	if err := os.Remove(filepath.Join(r.src, "file1.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.src, "file0.txt"), "rewritten with different content")
	if err := os.Chmod(filepath.Join(r.src, "file2.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "v2", r.src)

	borgObjs, borgeObjs := jsonLinesOf(t, r,
		[]string{"diff", "-r", r.path, "v1", "v2", "--json-lines"},
		[]string{"diff", "v1", "v2", "--json-lines"})

	byPath := func(objs []map[string]any) map[string]string {
		out := map[string]string{}
		for _, o := range objs {
			path, _ := o["path"].(string)
			changes, _ := o["changes"].([]any)
			var kinds []string
			for _, c := range changes {
				m, _ := c.(map[string]any)
				// Rendered whole, so a differing item1/added value fails too and not only
				// a differing type.
				b, _ := json.Marshal(m)
				kinds = append(kinds, string(b))
			}
			sort.Strings(kinds)
			out[path] = strings.Join(kinds, " ")
		}
		return out
	}
	want, got := byPath(borgObjs), byPath(borgeObjs)

	var kinds int
	for path, w := range want {
		kinds += strings.Count(w, "\"type\"")
		g, ok := got[path]
		if !ok {
			t.Errorf("borge reported no change for %s", path)
			continue
		}
		if w != g {
			t.Errorf("%s:\n  borg : %s\n  borge: %s", path, w, g)
		}
	}
	// Several change kinds, or this passes on a diff that found one thing.
	if kinds < 5 {
		t.Errorf("borg reported only %d changes; the comparison is too thin", kinds)
	}
}

// TestNonUnicodePathsMatchBorg: a filename that is not valid UTF-8 survives every JSON
// form, in whichever representation borg uses there.
//
// borg uses two. The item and file_status objects carry an approximation with each bad
// byte as "?" plus a base64 copy of the original bytes; diff and "debug dump-*" carry
// Python's surrogate escapes. borge produced neither - Go's encoder had replaced the bad
// bytes with U+FFFD, so the path was mangled with no way to recover it.
func TestNonUnicodePathsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()

	// Written through the byte-oriented API: this name cannot be spelled as a Go string
	// literal that survives a round trip through anything unicode-aware.
	bad := append([]byte(src+"/bad"), 0xff, 0xfe)
	bad = append(bad, []byte("name.txt")...)
	if err := os.WriteFile(string(bad), []byte("content"), 0o644); err != nil {
		t.Skipf("cannot create a non-unicode filename under %s: %v", src, err)
	}
	write(t, filepath.Join(src, "good.txt"), "ok")

	r.mustRun("create", "-r", r.path, "by-borg", src)
	if _, stderr, code := r.borge(t, "create", "by-borge", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	// The item form: "?" plus path_b64, and the base64 must decode to the original bytes.
	borgObjs, borgeObjs := jsonLinesOf(t, r,
		[]string{"list", "-r", r.path, "by-borg", "--json-lines"},
		[]string{"list", "by-borge", "--json-lines"})

	find := func(objs []map[string]any) map[string]any {
		for _, o := range objs {
			if p, _ := o["path"].(string); strings.Contains(p, "bad") {
				return o
			}
		}
		return nil
	}
	wantItem, gotItem := find(borgObjs), find(borgeObjs)
	if wantItem == nil {
		t.Fatal("borg did not list the non-unicode path; the premise is wrong")
	}
	if gotItem == nil {
		t.Fatal("borge did not list the non-unicode path")
	}
	for _, key := range []string{"path", "path_b64"} {
		if wantItem[key] != gotItem[key] {
			t.Errorf("%s: borg %v, borge %v", key, wantItem[key], gotItem[key])
		}
	}
	if !strings.Contains(gotItem["path"].(string), "??") {
		t.Errorf("the approximation does not show the bad bytes as ?: %v", gotItem["path"])
	}
	raw, err := base64.StdEncoding.DecodeString(gotItem["path_b64"].(string))
	if err != nil {
		t.Fatalf("path_b64 is not base64: %v", err)
	}
	if !strings.HasSuffix(string(raw), "name.txt") || !strings.Contains(string(raw), "\xff\xfe") {
		t.Errorf("path_b64 does not decode to the original bytes: %q", raw)
	}

	// The surrogate form, which diff uses instead.
	borgDiff, _ := r.runSplit("diff", "-r", r.path, "by-borg", "by-borg", "--json-lines")
	_ = borgDiff
	borgDump := filepath.Join(t.TempDir(), "borg.json")
	r.mustRun("debug", "dump-archive", "-r", r.path, "by-borg", borgDump)
	borgeDump := filepath.Join(t.TempDir(), "borge.json")
	if _, stderr, code := r.borge(t, "debug", "dump-archive", "by-borge", borgeDump); code != ExitOK {
		t.Fatalf("borge debug dump-archive exited %d\n%s", code, stderr)
	}
	borgText, err := os.ReadFile(borgDump)
	if err != nil {
		t.Fatal(err)
	}
	borgeText, err := os.ReadFile(borgeDump)
	if err != nil {
		t.Fatal(err)
	}
	const escaped = `bad\udcff\udcfename.txt`
	if !strings.Contains(string(borgText), escaped) {
		t.Fatalf("borg's dump does not use surrogate escapes; the premise is wrong")
	}
	if !strings.Contains(string(borgeText), escaped) {
		t.Errorf("borge's dump does not carry the path as surrogate escapes")
	}
}
