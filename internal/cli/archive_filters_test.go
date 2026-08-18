// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The relative archive filters: --older, --newer, --oldest and --newest.
//
// They were missing from eight of borge's commands at once, because borg registers them
// as one shared group and borge had no equivalent. That is why they are tested here rather
// than per command: one implementation, one test, and a separate check that every command
// which should offer them does.
//
// The archives are created *by borg*, with --timestamp, because that is the only way to
// get archives at controlled times - and it makes this a real differential test over one
// repository rather than two tools each filtering their own.

// filterFixture builds a repository holding archives at known ages.
type filterFixture struct {
	r    *borgRepo
	ages map[string]int // archive name -> age in days
}

func newFilterFixture(t *testing.T) *filterFixture {
	t.Helper()
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &filterFixture{r: r, ages: map[string]int{
		"age-100": 100,
		"age-040": 40,
		"age-010": 10,
		"age-001": 1,
	}}
	now := time.Now().UTC()
	for name, days := range f.ages {
		ts := now.AddDate(0, 0, -days).Format("2006-01-02T15:04:05+00:00")
		r.mustRun("create", "-r", r.path, "--timestamp", ts, name, src)
	}
	return f
}

// names lists what a repo-list reports, from either tool, as a sorted set.
func (f *filterFixture) borgNames(t *testing.T, args ...string) []string {
	t.Helper()
	out := f.r.mustRun(append([]string{"repo-list", "-r", f.r.path, "--json"}, args...)...)
	return archiveNamesFromJSON(t, out)
}

func (f *filterFixture) borgeNames(t *testing.T, args ...string) []string {
	t.Helper()
	stdout, stderr, code := f.r.borge(t, append([]string{"repo-list", "--json"}, args...)...)
	if code != ExitOK {
		t.Fatalf("borge repo-list %v exited %d\n%s", args, code, stderr)
	}
	return archiveNamesFromJSON(t, stdout)
}

func archiveNamesFromJSON(t *testing.T, out string) []string {
	t.Helper()
	var doc struct {
		Archives []struct {
			Name string `json:"name"`
		} `json:"archives"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("repo-list --json does not parse: %v\n%s", err, out)
	}
	var names []string
	for _, a := range doc.Archives {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}

// TestArchiveDateFiltersMatchBorg runs each filter over one repository with both tools.
func TestArchiveDateFiltersMatchBorg(t *testing.T) {
	f := newFilterFixture(t)

	// Every case has to select *some* archives and leave *some* out, or it would pass
	// with a filter that did nothing - or with one that dropped everything.
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"newer than 30 days", []string{"--newer", "30d"}, []string{"age-001", "age-010"}},
		{"older than 30 days", []string{"--older", "30d"}, []string{"age-040", "age-100"}},
		{"newer than 5 days", []string{"--newer", "5d"}, []string{"age-001"}},
		{"older than 90 days", []string{"--older", "90d"}, []string{"age-100"}},
		// Measured from the oldest archive (100 days old), so 70 days covers it and the
		// 40-day-old one, and stops short of the 10-day-old one.
		{"within 70 days of the oldest", []string{"--oldest", "70d"}, []string{"age-040", "age-100"}},
		// Measured from the newest (1 day old): 30 days back reaches the 10-day-old one.
		{"within 30 days of the newest", []string{"--newest", "30d"}, []string{"age-001", "age-010"}},
		// A month is calendar arithmetic in borg, not 30 days, and 2m reaches past the
		// 40-day-old archive either way - the point of the row is that the unit parses
		// and means roughly what it says.
		{"newer than 2 months", []string{"--newer", "2m"}, []string{"age-001", "age-010", "age-040"}},
		// Hours, to cover the unit that is not days.
		{"newer than 36 hours", []string{"--newer", "36H"}, []string{"age-001"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			borg := f.borgNames(t, c.args...)
			borge := f.borgeNames(t, c.args...)

			if len(borg) == 0 || len(borg) == len(f.ages) {
				t.Fatalf("borg selected %d of %d archives; this row proves nothing about "+
					"filtering: %v", len(borg), len(f.ages), borg)
			}
			if strings.Join(borg, ",") != strings.Join(c.want, ",") {
				t.Errorf("borg selected %v, this test expected %v - if borg changed, the "+
					"expectation is what has to move", borg, c.want)
			}
			if strings.Join(borge, ",") != strings.Join(borg, ",") {
				t.Errorf("borge selected %v, borg selected %v", borge, borg)
			}
		})
	}
}

// TestArchiveDateFiltersCombine: --newer and --oldest are in different mutually exclusive
// groups, so they can be given together - and the order they are applied in is visible in
// the answer. --oldest measures from the oldest archive *that survived --newer*, not from
// the oldest in the repository.
func TestArchiveDateFiltersCombine(t *testing.T) {
	f := newFilterFixture(t)

	args := []string{"--newer", "50d", "--oldest", "20d"}
	borg := f.borgNames(t, args...)
	borge := f.borgeNames(t, args...)

	// --newer 50d leaves age-040, age-010, age-001. The oldest of those is 40 days old,
	// and 20 days after it is 20 days ago, which still excludes age-010 and age-001.
	want := []string{"age-040"}
	if strings.Join(borg, ",") != strings.Join(want, ",") {
		t.Errorf("borg selected %v, expected %v", borg, want)
	}
	if strings.Join(borge, ",") != strings.Join(borg, ",") {
		t.Errorf("borge selected %v, borg selected %v", borge, borg)
	}
	// If --oldest were measured from the repository's oldest archive instead, it would
	// have selected nothing here, so the row above distinguishes the two orders.
	if len(borg) == 0 {
		t.Error("borg selected nothing; the case no longer distinguishes the two orders")
	}
}

// TestArchiveDateFilterRefusesBothEndsOfARange: borg makes each pair mutually exclusive in
// its parser. Accepting both would be read as an empty range or as one silently winning.
func TestArchiveDateFilterRefusesBothEndsOfARange(t *testing.T) {
	f := newFilterFixture(t)

	for _, pair := range [][]string{
		{"--older", "1d", "--newer", "1d"},
		{"--oldest", "1d", "--newest", "1d"},
	} {
		_, stderr, code := f.r.borge(t, append([]string{"repo-list"}, pair...)...)
		if code != ExitError {
			t.Errorf("%v exited %d, want %d", pair, code, ExitError)
		}
		if !strings.Contains(stderr, "give one") {
			t.Errorf("%v: unhelpful message %q", pair, stderr)
		}
	}

	// And a span borg would reject is rejected here too. The empty string is in the list
	// on purpose: it is what "--newer $SPAN" expands to when SPAN is unset, borg exits 2
	// for it, and reading it as "no filter given" would list every archive and report
	// success. borge did exactly that until this test caught it.
	for _, bad := range []string{"7", "d", "7x", "-7d", "7 d", ""} {
		_, _, code := f.r.borge(t, "repo-list", "--newer", bad)
		if code != ExitError {
			t.Errorf("--newer %q exited %d, want %d", bad, code, ExitError)
		}
	}
}

// TestEveryArchiveFilterCommandOffersTheDateOptions: the options live on one shared
// struct, so every command that takes archive filters should have gained all four. This
// is asserted rather than trusted, because the failure mode - one command quietly missing
// one option - is exactly what the option gate found across borge in the first place.
func TestEveryArchiveFilterCommandOffersTheDateOptions(t *testing.T) {
	// The commands that register listSelectors. Naming them here means that adding a
	// command without the filters, or dropping them from one, fails.
	filtered := []string{
		"repo-list", "delete", "undelete", "prune", "check", "recreate", "find", "analyze",
		// info and tag took a single archive until 2026-08-18; borg has always given
		// them the whole filter group.
		"info", "tag",
	}
	want := []string{"--older", "--newer", "--oldest", "--newest"}

	e := &Env{Stdout: nopWriter{}, Stderr: nopWriter{}}
	for _, name := range filtered {
		t.Run(name, func(t *testing.T) {
			var run func(*Env, []string) int
			for _, c := range commands() {
				if c.name == name {
					run = c.run
				}
			}
			if run == nil {
				t.Fatalf("no such command: %s", name)
			}
			have := flagsOf(e, run, nil)
			// A command with no flags at all would satisfy nothing below by accident,
			// but it would make the failure message confusing.
			if len(have) < 4 {
				t.Fatalf("%s registered only %d options; the probe is broken", name, len(have))
			}
			for _, w := range want {
				if !contains(have, w) {
					t.Errorf("borge %s has no %s (it has %v)", name, w, have)
				}
			}
		})
	}
}

// TestInfoDescribesEveryMatchingArchive: borg's info describes a *set*. borge described
// exactly one archive and refused to run without a selector, which is a different question
// answered by the same command name.
func TestInfoDescribesEveryMatchingArchive(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a1", "a2", "b3"} {
		r.mustRun("create", "-r", r.path, name, src)
	}

	count := func(out string) int {
		return strings.Count(out, "Archive name: ")
	}
	borgeInfo := func(args ...string) string {
		t.Helper()
		stdout, stderr, code := r.borge(t, append([]string{"info"}, args...)...)
		if code != ExitOK {
			t.Fatalf("borge info %v exited %d\n%s", args, code, stderr)
		}
		return stdout
	}

	// No selector at all: the set is the repository.
	if got, want := count(borgeInfo()), count(r.mustRun("info", "-r", r.path)); got != want {
		t.Errorf("borge info described %d archives, borg %d", got, want)
	}
	if got := count(borgeInfo()); got != 3 {
		t.Fatalf("described %d archives; the fixture is not what this test needs", got)
	}
	// And the filters narrow it, which is what proves they reach info at all.
	if got := count(borgeInfo("-a", "sh:a*")); got != 2 {
		t.Errorf("-a sh:a* described %d archives, want 2", got)
	}
	if got := count(borgeInfo("--last", "1")); got != 1 {
		t.Errorf("--last 1 described %d archives, want 1", got)
	}
	if got := count(borgeInfo("--newer", "1d")); got != 3 {
		t.Errorf("--newer 1d described %d archives, want 3", got)
	}
	// The positional name borge accepts and borg does not still works.
	if got := count(borgeInfo("b3")); got != 1 {
		t.Errorf("a positional name described %d archives, want 1", got)
	}
}

// TestTagAppliesToTheWholeSelection checks tag against borg over the three shapes that
// matter: a filter, no selector at all, and one name.
func TestTagAppliesToTheWholeSelection(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a1", "a2", "b3"} {
		r.mustRun("create", "-r", r.path, name, src)
	}

	// tags reads the repository as borg sees it, so the assertion is about what was
	// stored rather than about borge's own report of it.
	tags := func() map[string]string {
		t.Helper()
		// borg reports tags as one comma-separated string, already sorted - not as a
		// list. Reading it as a list is a parse error, which is how this test found out.
		var doc struct {
			Archives []struct {
				Name string `json:"name"`
				Tags string `json:"tags"`
			} `json:"archives"`
		}
		out := r.mustRun("repo-list", "-r", r.path, "--json")
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("repo-list --json does not parse: %v\n%s", err, out)
		}
		got := map[string]string{}
		for _, a := range doc.Archives {
			got[a.Name] = a.Tags
		}
		return got
	}
	mustTag := func(args ...string) {
		t.Helper()
		if _, stderr, code := r.borge(t, append([]string{"tag"}, args...)...); code != ExitOK {
			t.Fatalf("borge tag %v exited %d\n%s", args, code, stderr)
		}
	}

	mustTag("-add", "T", "-a", "sh:a*")
	if got := tags(); got["a1"] != "T" || got["a2"] != "T" || got["b3"] != "" {
		t.Fatalf("after -a sh:a*: %v", got)
	}
	// No selector means every archive, which is borg's behaviour and worth pinning
	// because it is the dangerous one.
	mustTag("-add", "ALL")
	if got := tags(); got["a1"] != "ALL,T" || got["b3"] != "ALL" {
		t.Fatalf("after no selector: %v", got)
	}
	mustTag("-add", "ONE", "b3")
	if got := tags(); got["b3"] != "ALL,ONE" || got["a1"] != "ALL,T" {
		t.Fatalf("after one name: %v", got)
	}
}

// TestTagWithNoMatchSaysSo: a selection that matches nothing must not look like a change
// that happened. borg exits 0 here; borge refuses, as it already does for delete. See
// PORTING_PLAN.md §2.3 and DIVERGENCES.md #28.
func TestTagWithNoMatchSaysSo(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "only", src)

	_, stderr, code := r.borge(t, "tag", "-add", "X", "-a", "sh:nothing-matches-this*")
	if code != ExitError {
		t.Errorf("a selection matching nothing exited %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "no archive matched") {
		t.Errorf("nothing said about the empty selection: %q", stderr)
	}
	if strings.Contains(r.mustRun("repo-list", "-r", r.path), "X") {
		t.Error("a tag was applied despite nothing matching")
	}
}
