// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// findLines runs find and returns its output lines.
func findLines(t *testing.T, r *borgRepo, args ...string) []string {
	t.Helper()
	stdout, stderr, code := r.borge(t, append([]string{"find"}, args...)...)
	if code != ExitOK {
		t.Fatalf("borge find exited %d\n%s", code, stderr)
	}
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestFindReportsEveryArchiveContainingAPath: the whole point of the command is that a
// path present in three archives is reported three times, once per archive.
//
// A find that reported only the newest would answer "yes it is backed up" while hiding
// which backups actually have it, which is the question people ask before deleting
// something.
func TestFindReportsEveryArchiveContainingAPath(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one", "two", "three")

	lines := findLines(t, r, "--short", "sh:**/file1.txt")
	if len(lines) != 3 {
		t.Fatalf("find reported %d match(es) for a path in all three archives, want 3:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}

	// Every line has to name a *different* archive: three lines from one archive would
	// have the same count and mean something quite different.
	seen := map[string]bool{}
	for _, line := range lines {
		id, _, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("--short line has no archive id: %q", line)
		}
		if seen[id] {
			t.Errorf("archive %s reported twice, so the matches are not one per archive", id)
		}
		seen[id] = true
	}
}

// TestFindNewestFirst checks the ordering, and that --reverse turns it around.
func TestFindNewestFirst(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one", "two", "three")

	// The archive names come back in the JSON, which is where the order is visible.
	order := func(args ...string) []string {
		stdout, stderr, code := r.borge(t, append([]string{"find", "-json-lines"}, args...)...)
		if code != ExitOK {
			t.Fatalf("borge find exited %d\n%s", code, stderr)
		}
		var names []string
		for _, line := range strings.Split(stdout, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			// borg's key, inside the flat item object rather than an envelope of
			// borge's own; see DIVERGENCES.md #43.
			var f struct {
				ArchiveName string `json:"archivename"`
			}
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatalf("find JSON does not parse: %v\n%s", err, line)
			}
			names = append(names, f.ArchiveName)
		}
		return names
	}

	forward := order("sh:**/file1.txt")
	if len(forward) != 3 {
		t.Fatalf("want 3 matches, got %d: %v", len(forward), forward)
	}
	if forward[0] != "three" || forward[2] != "one" {
		t.Errorf("find is not newest-first: %v", forward)
	}

	reversed := order("--reverse", "sh:**/file1.txt")
	if len(reversed) != 3 {
		t.Fatalf("want 3 matches with --reverse, got %d: %v", len(reversed), reversed)
	}
	if reversed[0] != "one" || reversed[2] != "three" {
		t.Errorf("--reverse did not reverse the order: %v", reversed)
	}
}

// TestFindHonoursArchiveSelectors: --last narrows which archives are searched.
func TestFindHonoursArchiveSelectors(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one", "two", "three")

	lines := findLines(t, r, "--last", "2", "--short", "sh:**/file1.txt")
	if len(lines) != 2 {
		t.Errorf("--last 2 searched %d archive(s), want 2:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// TestFindRefusesToMatchEverything: with no path and no pattern the command would print
// every item of every archive, which is list's job and is never what was meant.
func TestFindRefusesToMatchEverything(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one")

	stdout, stderr, code := r.borge(t, "find")
	if code != ExitError {
		t.Fatalf("find with no arguments exited %d, want ExitError (%d)\n%s%s",
			code, ExitError, stdout, stderr)
	}
	if !strings.Contains(stderr, "path or a pattern") {
		t.Errorf("the error does not say what is missing: %q", stderr)
	}
}

// TestFindMatchesNothingCleanly: a path in no archive is not an error, it is an answer.
func TestFindMatchesNothingCleanly(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one")

	stdout, stderr, code := r.borge(t, "find", "--short", "sh:**/no-such-file")
	if code != ExitOK {
		t.Fatalf("find exited %d for a path that is simply not there, want ExitOK\n%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("find printed matches for a path that does not exist:\n%s", stdout)
	}
}
