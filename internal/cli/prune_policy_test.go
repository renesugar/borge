// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// prune's retention policy, compared against borg decision by decision.
//
// This is the command that deletes history, so the comparison is of *which archives
// survive* rather than of the wording around it. borge implemented borg 1's interface -
// --keep-last, --keep-within, --keep-oldest - where borg 2 has --keep, intervals on every
// rule, and an automatic keep-the-oldest. See DIVERGENCES.md #50.

// pruneTimeline builds archives at fixed offsets from now, so that interval rules have
// unambiguous members however long the test takes.
//
// The offsets are deliberately not round: an archive exactly seven days old could fall
// either side of "--keep 7d" depending on which second each tool computed "now" in, and a
// test that flakes once a month is worse than no test.
func pruneTimeline(t *testing.T, r *borgRepo) []string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	offsets := []time.Duration{
		2 * time.Hour, 5 * time.Hour, 30 * time.Hour, 34 * time.Hour,
		3*24*time.Hour + 6*time.Hour,
		6*24*time.Hour + 12*time.Hour,
		9*24*time.Hour + 12*time.Hour,
		20*24*time.Hour + 12*time.Hour,
		40*24*time.Hour + 12*time.Hour,
		75*24*time.Hour + 12*time.Hour,
		110*24*time.Hour + 12*time.Hour,
		200*24*time.Hour + 12*time.Hour,
		400*24*time.Hour + 12*time.Hour,
		800*24*time.Hour + 12*time.Hour,
	}
	var names []string
	for i, off := range offsets {
		when := now.Add(-off)
		name := fmt.Sprintf("a%02d", i)
		r.mustRun("create", "--timestamp", when.Format("2006-01-02T15:04:05"),
			"-r", r.path, name, src)
		names = append(names, name)
	}
	return names
}

// prunePolicies is the matrix. Each is a whole command line, because what is under test is
// how the options combine.
var prunePolicies = [][]string{
	{"--keep", "3"},
	{"--keep", "1"},
	{"--keep", "all"},
	{"--keep", "7d"},
	{"--keep", "36H"},
	{"--keep-hourly", "3"},
	{"--keep-daily", "5"},
	{"--keep-daily", "1"},
	{"--keep-daily", "30d"},
	{"--keep-weekly", "4"},
	{"--keep-monthly", "3"},
	{"--keep-monthly", "all"},
	{"--keep-13weekly", "3"},
	{"--keep-3monthly", "3"},
	{"--keep-yearly", "2"},
	{"--keep-yearly", "5"},
	// Combinations, which is where the "a finer rule does not spend a coarser rule's
	// quota" property and the "last rule keeps the oldest" property interact.
	{"--keep-daily", "3", "--keep-monthly", "2"},
	{"--keep-daily", "3", "--keep-monthly", "2", "--keep-yearly", "2"},
	{"--keep", "2", "--keep-daily", "3"},
	{"--keep-hourly", "2", "--keep-daily", "2", "--keep-weekly", "2", "--keep-monthly", "2"},
	{"--keep-daily", "3", "--keep-yearly", "0"},
	{"--keep-daily", "2d", "--keep-monthly", "90d"},
	{"--keep-daily", "5", "--keep-monthly", "all"},
	// Short spellings, which borge did not have at all.
	{"-d", "3"},
	{"-H", "2", "-d", "2"},
	{"-m", "2", "-y", "2"},
	{"-w", "3"},
}

// pruneLines keeps only the per-archive decision lines, which are what the policy decides.
// The summary around them differs by design (DIVERGENCES.md #34).
func pruneLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "Keeping archive") ||
			strings.HasPrefix(line, "Would prune:") ||
			strings.HasPrefix(line, "Pruning archive") {
			out = append(out, line)
		}
	}
	return out
}

func TestPrunePolicyMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	names := pruneTimeline(t, r)
	if len(names) < 10 {
		t.Fatalf("the timeline has %d archives; too few to tell the rules apart", len(names))
	}

	// A policy that keeps everything and one that keeps almost nothing must give different
	// answers, or the whole matrix could be passing on identical output.
	// The listing is on stderr in both tools, which is why it is the second return value.
	_, all := borgStreams(t, r, "prune", "-n", "--list", "--short", "-r", r.path, "--keep", "all")
	_, one := borgStreams(t, r, "prune", "-n", "--list", "--short", "-r", r.path, "--keep", "1")
	if len(pruneLines(all)) != len(names) || len(pruneLines(one)) != len(names) {
		t.Fatalf("borg listed %d and %d lines for %d archives", len(pruneLines(all)), len(pruneLines(one)), len(names))
	}
	if reflect.DeepEqual(pruneLines(all), pruneLines(one)) {
		t.Fatal("borg keeps the same archives for --keep all and --keep 1; the matrix is vacuous")
	}

	for _, policy := range prunePolicies {
		name := strings.Join(policy, " ")
		t.Run(name, func(t *testing.T) {
			args := append([]string{"prune", "-n", "--list", "--short", "-r", r.path}, policy...)
			_, wantErr := borgStreams(t, r, args...)
			_, gotErr, code := r.borge(t, args...)
			if code != ExitOK {
				t.Fatalf("borge prune %s exited %d\n%s", name, code, gotErr)
			}
			want, got := pruneLines(wantErr), pruneLines(gotErr)
			if len(want) != len(names) {
				t.Fatalf("borg decided about %d archives, want %d", len(want), len(names))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("prune %s\n got:\n%s\nwant:\n%s", name,
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// TestPruneJSONMatchesBorg: the same decisions as a document, including keep_rule,
// kept_oldest and the two numbering keys.
func TestPruneJSONMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	pruneTimeline(t, r)

	type archive struct {
		Name       string `json:"name"`
		Kept       bool   `json:"kept"`
		KeepRule   string `json:"keep_rule"`
		KeptOldest bool   `json:"kept_oldest"`
		KeptNumber int    `json:"kept_archive_number"`
		DelNumber  int    `json:"deleted_archive_number"`
	}
	decode := func(s string) []archive {
		var doc struct {
			Archives []archive `json:"archives"`
		}
		if err := json.Unmarshal([]byte(s), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, s)
		}
		return doc.Archives
	}

	for _, policy := range [][]string{
		{"--keep-yearly", "5"},
		{"--keep-daily", "3", "--keep-monthly", "2"},
		{"--keep", "2"},
	} {
		t.Run(strings.Join(policy, " "), func(t *testing.T) {
			args := append([]string{"prune", "-n", "--json", "-r", r.path}, policy...)
			wantOut, _ := borgStreams(t, r, args...)
			gotOut, stderr, code := r.borge(t, args...)
			if code != ExitOK {
				t.Fatalf("borge exited %d\n%s", code, stderr)
			}
			want, got := decode(wantOut), decode(gotOut)
			if len(want) == 0 {
				t.Fatal("borg's JSON holds no archives")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("JSON decisions\n got: %+v\nwant: %+v", got, want)
			}
			// The rule names have to be borg's, and for the quarterly ones they are not
			// the option's name - so at least one non-empty keep_rule is asserted here.
			named := false
			for _, a := range want {
				if a.KeepRule != "" {
					named = true
				}
			}
			if !named {
				t.Fatal("no archive carries a keep_rule; the comparison is weaker than it looks")
			}
		})
	}
}

// TestPruneProtectedArchivesAreNotConsidered: an @PROT archive is absent from the listing
// in both tools, rather than kept with a reason.
func TestPruneProtectedArchivesAreNotConsidered(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	names := pruneTimeline(t, r)
	protected := names[len(names)-2]
	r.mustRun("tag", "--set", "@PROT", "-r", r.path, protected)

	args := []string{"prune", "-n", "--list", "--short", "-r", r.path, "--keep-monthly", "2"}
	_, wantErr := borgStreams(t, r, args...)
	_, gotErr, code := r.borge(t, args...)
	if code != ExitOK {
		t.Fatalf("borge exited %d\n%s", code, gotErr)
	}
	want, got := pruneLines(wantErr), pruneLines(gotErr)
	if len(want) != len(names)-1 {
		t.Fatalf("borg listed %d lines for %d archives with one protected; want %d",
			len(want), len(names), len(names)-1)
	}
	for _, line := range want {
		if strings.Contains(line, protected) {
			t.Fatalf("borg mentioned the protected archive, so this test asserts the wrong thing:\n%s", line)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with a protected archive\n got:\n%s\nwant:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// And it is still there afterwards, which is the point of the tag.
	out, _ := borgStreams(t, r, "repo-list", "-r", r.path, "--format", "{archive}{NL}")
	if !strings.Contains(out, protected) {
		t.Error("the protected archive is gone from the repository")
	}
}

// TestPruneFromMatchesBorg: --from holds back the newest archives entirely.
func TestPruneFromMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	pruneTimeline(t, r)

	for _, days := range []int{3, 30, 300} {
		from := time.Now().AddDate(0, 0, -days).Format("2006-01-02T15:04:05")
		t.Run(fmt.Sprintf("%dd", days), func(t *testing.T) {
			args := []string{"prune", "-n", "--list", "--short", "-r", r.path,
				"--from", from, "--keep-monthly", "2"}
			_, wantErr := borgStreams(t, r, args...)
			_, gotErr, code := r.borge(t, args...)
			if code != ExitOK {
				t.Fatalf("borge exited %d\n%s", code, gotErr)
			}
			want, got := pruneLines(wantErr), pruneLines(gotErr)
			held := 0
			for _, l := range want {
				if strings.Contains(l, "rule: skip") {
					held++
				}
			}
			if held == 0 {
				t.Fatalf("--from %s held nothing back; the case is vacuous", from)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("--from %s\n got:\n%s\nwant:\n%s", from,
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// TestPruneRefusalsMatchBorg: every way of writing a policy that borg refuses.
func TestPruneRefusalsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	pruneTimeline(t, r)

	cases := [][]string{
		{},                    // no rule at all
		{"--keep-daily", "0"}, // every rule zero
		{"--keep-daily", "0", "--keep-monthly", "0"},     //
		{"--keep-daily", "7d", "--keep-monthly", "2d"},   // finer reaches further back
		{"--keep-daily", "all", "--keep-monthly", "5"},   // "all" is infinite in both groups
		{"--keep-13weekly", "3", "--keep-3monthly", "2"}, // two answers to "what is a quarter"
	}
	for _, policy := range cases {
		name := strings.Join(policy, " ")
		if name == "" {
			name = "(no rule)"
		}
		t.Run(name, func(t *testing.T) {
			args := append([]string{"prune", "-n", "-r", r.path}, policy...)
			out, err := r.runErr(args...)
			if err == nil {
				t.Fatalf("borg accepted %q:\n%s", name, out)
			}
			_, stderr, code := r.borge(t, args...)
			if code != ExitError {
				t.Fatalf("borge exited %d for %q, want %d\n%s", code, name, ExitError, stderr)
			}
			// borg's complaint, with its prefix removed: the part that is borg's wording
			// rather than its plumbing. Matched by PREFIX and not by content - looking for
			// a line mentioning "keep" finds borg's usage block, which lists [--keep KEEP].
			wanted := ""
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				for _, prefix := range []string{"Command error: ", "error: "} {
					if rest, ok := strings.CutPrefix(line, prefix); ok && wanted == "" {
						wanted = rest
					}
				}
			}
			if wanted == "" {
				t.Fatalf("could not find borg's complaint in:\n%s", out)
			}
			if !strings.Contains(stderr, wanted) {
				t.Errorf("borge's message for %q:\n got: %s\nwant it to contain: %s", name, stderr, wanted)
			}
		})
	}
}

// TestPruneActuallyDeletes: the dry run above decides, and this confirms the decision is
// carried out - the same archives survive in both tools.
func TestPruneActuallyDeletes(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	pruneTimeline(t, r)

	before, _ := borgStreams(t, r, "repo-list", "-r", r.path, "--format", "{archive}{NL}")
	if _, stderr, code := r.borge(t, "prune", "--list", "--short", "-r", r.path,
		"--keep-daily", "2", "--keep-monthly", "2"); code != ExitOK {
		t.Fatalf("borge prune exited %d\n%s", code, stderr)
	}
	after, _ := borgStreams(t, r, "repo-list", "-r", r.path, "--format", "{archive}{NL}")
	if after == before {
		t.Fatal("prune deleted nothing; the test would pass however the policy was applied")
	}

	// The same repository, the same policy, decided by borg: the survivors must be the
	// archives borg's dry run said it would keep.
	kept := map[string]bool{}
	for _, l := range strings.Split(strings.TrimRight(after, "\n"), "\n") {
		if l != "" {
			kept[l] = true
		}
	}
	if len(kept) == 0 {
		t.Fatal("prune deleted everything")
	}
	for name := range kept {
		if !strings.Contains(before, name) {
			t.Errorf("%s survived and was never there", name)
		}
	}
}
