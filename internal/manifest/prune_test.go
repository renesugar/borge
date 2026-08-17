// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// at builds an archive whose timestamp is the given local time.
func at(id int, t time.Time, tags ...string) Info {
	return Info{
		ID:     []byte{byte(id >> 8), byte(id)},
		Name:   fmt.Sprintf("a%03d", id),
		Time:   t,
		Tags:   tags,
		Exists: true,
	}
}

// keptNames returns the names of the kept archives, newest first.
func keptNames(decisions []PruneDecision) []string {
	var out []string
	for _, d := range decisions {
		if d.Keep {
			out = append(out, d.Info.Name)
		}
	}
	return out
}

func prunedNames(decisions []PruneDecision) []string {
	var out []string
	for _, d := range decisions {
		if !d.Keep {
			out = append(out, d.Info.Name)
		}
	}
	return out
}

// TestPruneDailyKeepsOnePerDay is the property the rules exist for, and the one most often
// misread: --keep-daily=3 keeps one archive from each of the last three *days*, not the
// last three archives.
func TestPruneDailyKeepsOnePerDay(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	id := 0
	// Three backups a day for five days.
	for day := 0; day < 5; day++ {
		for hour := 0; hour < 3; hour++ {
			id++
			archives = append(archives, at(id, base.AddDate(0, 0, -day).Add(time.Duration(hour)*time.Hour)))
		}
	}

	decisions := Prune(archives, PrunePolicy{Counts: map[RuleKind]int{RuleDaily: 3}, Now: base})
	kept := keptNames(decisions)
	if len(kept) != 3 {
		t.Fatalf("kept %d archives (%v), want 3 - one per day", len(kept), kept)
	}
	// And each kept archive is the *newest* of its day: hour 2, not hour 0.
	for _, d := range decisions {
		if d.Keep && d.Info.Time.Hour() != 14 {
			t.Errorf("%s kept from hour %d; the newest of each day is the one to keep",
				d.Info.Name, d.Info.Time.Hour())
		}
	}
	if len(prunedNames(decisions)) != 12 {
		t.Errorf("pruned %d, want 12", len(prunedNames(decisions)))
	}
}

// TestPruneRulesDoNotShareQuota: a coarser rule must not have its quota consumed by an
// archive a finer rule already kept, or "--keep-daily=7 --keep-monthly=6" would give six
// months that happen to include this week rather than seven days *and* six months.
func TestPruneRulesDoNotShareQuota(t *testing.T) {
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	var archives []Info
	id := 0
	// One backup a day for a year.
	for day := 0; day < 365; day++ {
		id++
		archives = append(archives, at(id, base.AddDate(0, 0, -day)))
	}

	decisions := Prune(archives, PrunePolicy{
		Counts: map[RuleKind]int{RuleDaily: 7, RuleMonthly: 6},
		Now:    base,
	})
	kept := keptNames(decisions)

	// Seven daily, plus monthly archives for the months the daily rule did not already
	// cover. The current month is covered by a daily archive, so five more months.
	if len(kept) < 11 || len(kept) > 13 {
		t.Errorf("kept %d archives (%v); expected about 7 daily plus 5-6 monthly", len(kept), kept)
	}

	var daily, monthly int
	for _, d := range decisions {
		switch {
		case strings.HasPrefix(d.Reason, "daily"):
			daily++
		case strings.HasPrefix(d.Reason, "monthly"):
			monthly++
		}
	}
	if daily != 7 {
		t.Errorf("%d archives kept by the daily rule, want 7", daily)
	}
	if monthly < 4 {
		t.Errorf("%d archives kept by the monthly rule, want about 5", monthly)
	}
}

// TestPruneWithin keeps everything newer than the duration, whatever the counts say.
func TestPruneWithin(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 20; i++ {
		archives = append(archives, at(i+1, base.Add(-time.Duration(i)*6*time.Hour)))
	}

	decisions := Prune(archives, PrunePolicy{
		Within: 48 * time.Hour,
		Counts: map[RuleKind]int{RuleDaily: 1},
		Now:    base,
	})
	for _, d := range decisions {
		age := base.Sub(d.Info.Time)
		if age <= 48*time.Hour && !d.Keep {
			t.Errorf("%s is %v old and was pruned despite --keep-within 48h", d.Info.Name, age)
		}
	}
	kept := keptNames(decisions)
	if len(kept) < 9 {
		t.Errorf("kept %d archives; the last 48 hours alone hold 9", len(kept))
	}
}

// TestPruneNeverTouchesProtected: the protected tag is what a retention policy must not be
// able to override.
func TestPruneNeverTouchesProtected(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		tags := []string{}
		if i == 7 {
			tags = append(tags, ProtectedTag)
		}
		archives = append(archives, at(i+1, base.AddDate(0, 0, -i), tags...))
	}

	decisions := Prune(archives, PrunePolicy{Counts: map[RuleKind]int{RuleDaily: 2}, Now: base})
	var protectedKept bool
	for _, d := range decisions {
		for _, tag := range d.Info.Tags {
			if tag == ProtectedTag {
				protectedKept = d.Keep
				if !strings.Contains(d.Reason, ProtectedTag) {
					t.Errorf("the protected archive was kept for the wrong reason: %q", d.Reason)
				}
			}
		}
	}
	if !protectedKept {
		t.Error("a protected archive was pruned")
	}
	// And it did not consume the daily rule's quota.
	var daily int
	for _, d := range decisions {
		if strings.HasPrefix(d.Reason, "daily") {
			daily++
		}
	}
	if daily != 2 {
		t.Errorf("%d archives kept by the daily rule, want 2; a protected archive must not "+
			"consume a rule's quota", daily)
	}
}

// TestPruneWeeklyUsesISOWeeks: the turn of the year is where a calendar-year grouping
// splits one week into two and keeps an extra archive.
func TestPruneWeeklyUsesISOWeeks(t *testing.T) {
	// 2026-12-31 is a Thursday; it and 2027-01-01 are in the same ISO week.
	dec31 := time.Date(2026, 12, 31, 12, 0, 0, 0, time.Local)
	jan1 := time.Date(2027, 1, 1, 12, 0, 0, 0, time.Local)

	if periodOf(RuleWeekly, dec31, 0) != periodOf(RuleWeekly, jan1, 1) {
		t.Errorf("2026-12-31 and 2027-01-01 are in different weekly groups (%q vs %q); "+
			"they are in the same ISO week",
			periodOf(RuleWeekly, dec31, 0), periodOf(RuleWeekly, jan1, 1))
	}

	archives := []Info{at(1, jan1), at(2, dec31)}
	decisions := Prune(archives, PrunePolicy{Counts: map[RuleKind]int{RuleWeekly: 1}, Now: jan1})
	if len(keptNames(decisions)) != 1 {
		t.Errorf("--keep-weekly=1 kept %v across a year boundary, want one", keptNames(decisions))
	}
}

// TestPruneKeepLast is the count-based rule, which is the one that does mean "the last N
// archives".
func TestPruneKeepLast(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		archives = append(archives, at(i+1, base.Add(-time.Duration(i)*time.Minute)))
	}
	decisions := Prune(archives, PrunePolicy{Counts: map[RuleKind]int{RuleLast: 3}, Now: base})
	kept := keptNames(decisions)
	if len(kept) != 3 {
		t.Fatalf("--keep-last=3 kept %d archives (%v)", len(kept), kept)
	}
	// The three newest, and in newest-first order.
	if kept[0] != "a001" || kept[1] != "a002" || kept[2] != "a003" {
		t.Errorf("kept %v, want the three newest", kept)
	}
}

// TestPruneEmptyPolicyIsRefusedByTheCaller documents the guard, which lives in the
// command: Prune itself applied to an empty policy legitimately keeps nothing.
func TestPruneEmptyPolicyKeepsNothing(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	archives := []Info{at(1, base), at(2, base.AddDate(0, 0, -1))}

	policy := PrunePolicy{Counts: map[RuleKind]int{}}
	if !policy.Empty() {
		t.Fatal("a policy with no rules does not report itself empty")
	}
	decisions := Prune(archives, policy)
	if len(keptNames(decisions)) != 0 {
		t.Error("an empty policy kept something; the command's refusal is what protects the user")
	}
}

func TestPruneKeepOldest(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		archives = append(archives, at(i+1, base.AddDate(0, 0, -i)))
	}
	decisions := Prune(archives, PrunePolicy{
		Counts:     map[RuleKind]int{RuleDaily: 2},
		KeepOldest: true,
		Now:        base,
	})
	last := decisions[len(decisions)-1]
	if !last.Keep || last.Reason != "oldest" {
		t.Errorf("the oldest archive was not kept: %+v", last)
	}
}

func TestDescribePolicy(t *testing.T) {
	p := PrunePolicy{
		Within: 48 * time.Hour,
		Counts: map[RuleKind]int{RuleDaily: 7, RuleMonthly: -1},
	}
	got := DescribePolicy(p)
	for _, want := range []string{"within 48h", "daily=7", "monthly=all"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribePolicy is %q, missing %q", got, want)
		}
	}
	if DescribePolicy(PrunePolicy{}) != "(nothing)" {
		t.Errorf("an empty policy describes as %q", DescribePolicy(PrunePolicy{}))
	}
}
