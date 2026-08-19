// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The surface test says which commands take --json. This says the documents agree.
//
// It compares *shapes*, not values: ids, times and paths differ between two runs by
// construction, and a test that required them equal would be comparing clocks. What must
// agree is the set of keys at every level, because that is what a frontend indexes into.
//
// borge's JSON was compared against borg's by counting commands before this existed, which
// found six commands "missing JSON" and called the other five done. Every one of the five
// emitted a document of a different shape. Counting is not comparing.

// shape renders a JSON value as its key structure: {"a":1,"b":{"c":[2]}} becomes
// "a, b{c[]}". Arrays collapse to the shape of their first element, since every row of a
// listing has the same keys.
func shape(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if inner := shape(t[k]); inner != "" {
				parts = append(parts, k+"{"+inner+"}")
			} else {
				parts = append(parts, k)
			}
		}
		return strings.Join(parts, ", ")
	case []any:
		if len(t) == 0 {
			return ""
		}
		return shape(t[0])
	default:
		return ""
	}
}

func mustJSON(t *testing.T, what, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%s did not produce JSON: %v\n%s", what, err, s)
	}
	return v
}

// TestJSONSchemaMatchesBorg: the documents borge prints have borg's shape.
func TestJSONSchemaMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("first", "second", "third")

	cases := []struct {
		name string
		borg []string
		// borge's arguments, minus the repository, which borgeEnv supplies.
		borge []string
	}{
		{
			name:  "repo-list",
			borg:  []string{"repo-list", "-r", r.path, "--json"},
			borge: []string{"repo-list", "--json"},
		},
		{
			name:  "repo-list with a format",
			borg:  []string{"repo-list", "-r", r.path, "--json", "--format", "{archive} {nfiles}"},
			borge: []string{"repo-list", "--json", "--format", "{archive} {nfiles}"},
		},
		{
			name:  "prune",
			borg:  []string{"prune", "-r", r.path, "--keep-daily", "1", "--dry-run", "--json"},
			borge: []string{"prune", "--keep-daily", "1", "--dry-run", "--json"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := shape(mustJSON(t, "borg "+c.name, r.mustRun(c.borg...)))
			stdout, stderr, code := r.borge(t, c.borge...)
			if code != ExitOK {
				t.Fatalf("borge %s exited %d\n%s", c.name, code, stderr)
			}
			got := shape(mustJSON(t, "borge "+c.name, stdout))
			if want == "" {
				t.Fatalf("borg's %s document has no keys; the comparison would pass on anything", c.name)
			}
			if got != want {
				t.Errorf("shapes differ\nborg : %s\nborge: %s", want, got)
			}
		})
	}
}

// TestCreateJSONSchemaMatchesBorg: "create --json" and "import-tar --json", which cannot
// use one repository for both tools because each has to write its own archive.
func TestCreateJSONSchemaMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("seed")

	// borg sends three stats keys borge does not; see PORTING_PLAN.md section 11.4.
	omitted := map[string]bool{"chunking_time": true, "hashing_time": true, "store_stats": true}

	// normalise removes the three keys borge does not send, and empties files_stats.
	// files_stats is a *value* map keyed by status character, so shaping it would compare
	// which characters the two runs happened to produce - and borg's import-tar sends it
	// empty where borge fills it in (DIVERGENCES.md #37). Reports whether it found the
	// omitted keys, so a stale omission list fails the test rather than weakening it.
	normalise := func(doc any) bool {
		m, ok := doc.(map[string]any)
		if !ok {
			return false
		}
		arch, ok := m["archive"].(map[string]any)
		if !ok {
			return false
		}
		stats, ok := arch["stats"].(map[string]any)
		if !ok {
			return false
		}
		found := 0
		for k := range omitted {
			if _, ok := stats[k]; ok {
				found++
			}
			delete(stats, k)
		}
		stats["files_stats"] = map[string]any{}
		return found == len(omitted)
	}

	// borge's own document goes through the same emptying of files_stats, and nothing
	// else: it never carries the three omitted keys.
	normaliseBorge := func(doc any) {
		if m, ok := doc.(map[string]any); ok {
			if arch, ok := m["archive"].(map[string]any); ok {
				if stats, ok := arch["stats"].(map[string]any); ok {
					stats["files_stats"] = map[string]any{}
				}
			}
		}
	}

	t.Run("create", func(t *testing.T) {
		borgDoc := mustJSON(t, "borg create", r.mustRun("create", "-r", r.path, "by-borg", r.src, "--json"))
		if !normalise(borgDoc) {
			t.Fatal("borg's create document is missing one of the keys borge omits; the omission list is stale")
		}
		want := shape(borgDoc)

		stdout, stderr, code := r.borge(t, "create", "by-borge", r.src, "--json")
		if code != ExitOK {
			t.Fatalf("borge create --json exited %d\n%s", code, stderr)
		}
		borgeDoc := mustJSON(t, "borge create", stdout)
		normaliseBorge(borgeDoc)
		got := shape(borgeDoc)
		if got != want {
			t.Errorf("shapes differ\nborg : %s\nborge: %s", want, got)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		want := shape(mustJSON(t, "borg create --dry-run",
			r.mustRun("create", "-r", r.path, "unused", r.src, "--json", "--dry-run")))
		stdout, stderr, code := r.borge(t, "create", "unused2", r.src, "--json", "--dry-run")
		if code != ExitOK {
			t.Fatalf("borge create --dry-run --json exited %d\n%s", code, stderr)
		}
		got := shape(mustJSON(t, "borge create --dry-run", stdout))
		// borg's dry-run document is a different shape from its real one, and that is the
		// point of testing it separately: no "archive", no "cache", and a "dry_run" key.
		if !strings.Contains(want, "dry_run") {
			t.Fatalf("borg's dry-run document has no dry_run key: %s", want)
		}
		if got != want {
			t.Errorf("shapes differ\nborg : %s\nborge: %s", want, got)
		}
	})

	t.Run("import-tar", func(t *testing.T) {
		tarPath := fmt.Sprintf("%s/from-borge.tar", t.TempDir())
		if _, stderr, code := r.borge(t, "export-tar", "seed", tarPath); code != ExitOK {
			t.Fatalf("borge export-tar exited %d\n%s", code, stderr)
		}
		borgDoc := mustJSON(t, "borg import-tar",
			r.mustRun("import-tar", "-r", r.path, "tar-by-borg", tarPath, "--json"))
		if !normalise(borgDoc) {
			t.Fatal("borg's import-tar document is missing one of the keys borge omits; the omission list is stale")
		}
		want := shape(borgDoc)

		stdout, stderr, code := r.borge(t, "import-tar", "tar-by-borge", tarPath, "--json")
		if code != ExitOK {
			t.Fatalf("borge import-tar --json exited %d\n%s", code, stderr)
		}
		borgeDoc := mustJSON(t, "borge import-tar", stdout)
		normaliseBorge(borgeDoc)
		got := shape(borgeDoc)
		if got != want {
			t.Errorf("shapes differ\nborg : %s\nborge: %s", want, got)
		}
	})
}

// TestCreateJSONStaysParseableWithList: --list must not write into the document.
//
// The same defect as export-tar's (DIVERGENCES.md #28): progress on stdout is invisible
// until something else needs stdout to be clean. A test that captured the two streams
// together would not see this one either, so it splits them.
func TestCreateJSONStaysParseableWithList(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("seed")

	stdout, stderr, code := r.borge(t, "create", "listed", r.src, "--json", "--list")
	if code != ExitOK {
		t.Fatalf("borge create --json --list exited %d\n%s", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("--list corrupted the document: %v\nstdout:\n%s", err, stdout)
	}
	if _, ok := doc["archive"]; !ok {
		t.Errorf("no archive block in the document: %v", doc)
	}
	// And the listing has to have gone somewhere, or this passes because --list did
	// nothing at all.
	if !strings.Contains(stderr, "A ") {
		t.Errorf("--list printed no item lines on stderr:\n%s", stderr)
	}
}
