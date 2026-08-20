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
		// borgOnly are top-level keys borg sends and borge deliberately does not. Each is
		// asserted to be present in borg's document before it is removed, so a stale
		// exemption fails rather than quietly weakening the comparison.
		borgOnly []string
	}{
		{
			name:  "repo-list",
			borg:  []string{"repo-list", "-r", r.path, "--json"},
			borge: []string{"repo-list", "--json"},
		},
		{
			name:  "repo-info",
			borg:  []string{"repo-info", "-r", r.path, "--json"},
			borge: []string{"repo-info", "--json"},
			// borg keeps a per-repository security directory holding the manifest and
			// nonce it last saw; borge has no such thing, so the key is omitted rather
			// than pointed at a path that does not exist.
			borgOnly: []string{"security_dir"},
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
		{
			name:  "info",
			borg:  []string{"info", "-r", r.path, "-a", "first", "--json"},
			borge: []string{"info", "-a", "first", "--json"},
		},
		{
			name:  "analyze",
			borg:  []string{"analyze", "-r", r.path, "--json"},
			borge: []string{"analyze", "--json"},
		},
		{
			name:  "analyze by name",
			borg:  []string{"analyze", "-r", r.path, "--by-name", "--json"},
			borge: []string{"analyze", "--by-name", "--json"},
		},
		{
			name:  "version",
			borg:  []string{"version", "--json"},
			borge: []string{"version", "--json"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			borgDoc := mustJSON(t, "borg "+c.name, r.mustRun(c.borg...))
			for _, key := range c.borgOnly {
				m, ok := borgDoc.(map[string]any)
				if !ok {
					t.Fatalf("borg's %s document is not an object", c.name)
				}
				if _, present := m[key]; !present {
					t.Fatalf("borg's %s document has no %q; the exemption is stale",
						c.name, key)
				}
				delete(m, key)
			}
			want := shape(borgDoc)
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

	// No omission list any more. borge used to send three of borg's six stats keys, and
	// this test deleted chunking_time, hashing_time and store_stats from borg's document
	// before comparing. borge measures all three since 2026-08-20 (DIVERGENCES #51), so
	// the shapes are compared whole.
	//
	// normalise empties files_stats, which stays out of the shape comparison for a
	// different reason: it is a *value* map keyed by status character, so comparing it
	// would compare which characters the two runs happened to produce - and borg's
	// import-tar sends it empty where borge fills it in (DIVERGENCES.md #37).
	normalise := func(doc any) {
		m, ok := doc.(map[string]any)
		if !ok {
			return
		}
		arch, ok := m["archive"].(map[string]any)
		if !ok {
			return
		}
		stats, ok := arch["stats"].(map[string]any)
		if !ok {
			return
		}
		stats["files_stats"] = map[string]any{}
	}

	t.Run("create", func(t *testing.T) {
		borgDoc := mustJSON(t, "borg create", r.mustRun("create", "-r", r.path, "by-borg", r.src, "--json"))
		normalise(borgDoc)
		want := shape(borgDoc)

		stdout, stderr, code := r.borge(t, "create", "by-borge", r.src, "--json")
		if code != ExitOK {
			t.Fatalf("borge create --json exited %d\n%s", code, stderr)
		}
		borgeDoc := mustJSON(t, "borge create", stdout)
		normalise(borgeDoc)
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
		normalise(borgDoc)
		want := shape(borgDoc)

		stdout, stderr, code := r.borge(t, "import-tar", "tar-by-borge", tarPath, "--json")
		if code != ExitOK {
			t.Fatalf("borge import-tar --json exited %d\n%s", code, stderr)
		}
		borgeDoc := mustJSON(t, "borge import-tar", stdout)
		normalise(borgeDoc)
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
