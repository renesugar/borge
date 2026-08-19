// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Which timestamps an archive stores: --atime, --noctime, --nobirthtime.
//
// The default is the half that matters. borge stored atime on every item and borg does
// not, so every borge archive carried a timestamp borg leaves out - bigger than borg's for
// the same tree, and *noisy*: atime moves when a file is merely read, so two backups of an
// unchanged tree produced different item metadata.
//
// The stage 7 comparator comes nowhere near this. It compares mtime and deliberately
// excludes atime and ctime from the restore contract, so no amount of interop testing
// would have found it. It was found by asking what the three options do.
func TestStoredTimestampsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// timeKeys reads the raw item metadata rather than a listing: the question is what
	// was *stored*, and every formatter falls back to mtime, which would hide the answer.
	timeKeys := func(dump string) []string {
		t.Helper()
		var doc struct {
			Items []map[string]any `json:"_items"`
		}
		if err := json.Unmarshal([]byte(dump), &doc); err != nil {
			t.Fatalf("debug dump-archive does not parse: %v", err)
		}
		for _, it := range doc.Items {
			path, _ := it["path"].(string)
			if !strings.HasSuffix(path, "f.txt") {
				continue
			}
			var keys []string
			for k := range it {
				if strings.Contains(k, "time") {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			return keys
		}
		t.Fatal("the file is not in the dump")
		return nil
	}

	cases := []struct {
		name  string
		flags []string
		want  []string
	}{
		{"default", nil, []string{"ctime", "mtime"}},
		{"atime", []string{"--atime"}, []string{"atime", "ctime", "mtime"}},
		{"noctime", []string{"--noctime"}, []string{"mtime"}},
		{"atime and noctime", []string{"--atime", "--noctime"}, []string{"atime", "mtime"}},
		// Accepted, and a no-op on Linux in both tools: birthtime is only reachable
		// through statx, which neither reads here.
		{"nobirthtime", []string{"--nobirthtime"}, []string{"ctime", "mtime"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.mustRun(append(append([]string{"create", "-r", r.path}, c.flags...), "borg-"+c.name, src)...)
			if _, stderr, code := r.borge(t, append(append([]string{"create"}, c.flags...), "borge-"+c.name, src)...); code != ExitOK {
				t.Fatalf("borge create %v exited %d\n%s", c.flags, code, stderr)
			}

			want := timeKeys(r.mustRun("debug", "dump-archive", "-r", r.path, "borg-"+c.name, "-"))
			stdout, stderr, code := r.borge(t, "debug", "dump-archive", "borge-"+c.name, "-")
			if code != ExitOK {
				t.Fatalf("borge debug dump-archive exited %d\n%s", code, stderr)
			}
			got := timeKeys(stdout)

			if strings.Join(want, ",") != strings.Join(c.want, ",") {
				t.Errorf("borg stored %v, this test expected %v - if borg changed, the "+
					"expectation is what has to move", want, c.want)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("borge stored %v, borg stored %v", got, want)
			}
		})
	}
}

// TestArchivesAreStableAcrossReads is why the default matters, stated as behaviour rather
// than as a byte count: reading a file must not change what the next backup stores.
func TestArchivesAreStableAcrossReads(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	path := filepath.Join(src, "f.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := r.borge(t, "create", "first", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}
	// Read the file, which moves its atime and nothing else.
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := r.borge(t, "create", "second", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	// Same content, same metadata: the two archives have the same id.
	ids := strings.Fields(r.mustRun("repo-list", "-r", r.path, "--short"))
	if len(ids) != 2 {
		t.Fatalf("expected two archives, got %v", ids)
	}
	if ids[0] != ids[1] {
		// Not necessarily a bug in itself - the archive metadata carries a timestamp -
		// so the real assertion is on the item stream below.
		t.Logf("archive ids differ, which the nominal time alone explains: %v", ids)
	}
	firstItems := itemDump(t, r, "first")
	secondItems := itemDump(t, r, "second")
	if firstItems != secondItems {
		t.Errorf("reading a file changed what the next backup stored:\n%s\n%s",
			firstItems, secondItems)
	}
}

// itemDump is an archive's item metadata with the archive-level noise removed.
func itemDump(t *testing.T, r *borgRepo, name string) string {
	t.Helper()
	stdout, stderr, code := r.borge(t, "debug", "dump-archive", name, "-")
	if code != ExitOK {
		t.Fatalf("borge debug dump-archive exited %d\n%s", code, stderr)
	}
	var doc struct {
		Items []map[string]any `json:"_items"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("dump does not parse: %v", err)
	}
	out, err := json.Marshal(doc.Items)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// timeKeysOf is the sorted timestamp keys of the first item in a dump, as a string.
func timeKeysOf(t *testing.T, dump string) string {
	t.Helper()
	var doc struct {
		Items []map[string]any `json:"_items"`
	}
	if err := json.Unmarshal([]byte(dump), &doc); err != nil {
		t.Fatalf("dump does not parse: %v", err)
	}
	for _, it := range doc.Items {
		var keys []string
		for k := range it {
			if strings.Contains(k, "time") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	t.Fatal("the dump has no items")
	return ""
}
