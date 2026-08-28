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

// policyOf builds a policy from alternating rule/value pairs, in the order given.
func policyOf(now time.Time, pairs ...any) PrunePolicy {
	p := PrunePolicy{Keep: map[RuleKind]KeepValue{}, Now: now}
	for i := 0; i+1 < len(pairs); i += 2 {
		p.Keep[pairs[i].(RuleKind)] = pairs[i+1].(KeepValue)
	}
	return p
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

	decisions := Prune(archives, policyOf(base, RuleDaily, KeepCount(3)))
	kept := keptNames(decisions)
	// Three, and not four: the daily rule is also the last rule, so it would keep the
	// oldest archive as well - but only if it still had room, and a count of three against
	// five days' worth of groups is spent by the third.
	if len(kept) != 3 {
		t.Fatalf("kept %d archives (%v), want 3 - one per day", len(kept), kept)
	}
	// And each archive kept by the rule itself is the *newest* of its day: hour 2, not 0.
	for _, d := range decisions {
		if d.Keep && !d.Oldest && d.Info.Time.Hour() != 14 {
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

	decisions := Prune(archives, policyOf(base, RuleDaily, KeepCount(7), RuleMonthly, KeepCount(6)))
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

// TestPruneKeepInterval: "--keep 48h" keeps everything newer than the interval, which is
// what borg 1's --keep-within meant. Every archive is its own group under RuleKeep, so the
// interval is the only thing limiting it.
func TestPruneKeepInterval(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 20; i++ {
		archives = append(archives, at(i+1, base.Add(-time.Duration(i)*6*time.Hour)))
	}

	decisions := Prune(archives, policyOf(base, RuleKeep, KeepInterval(48*time.Hour)))
	for _, d := range decisions {
		age := base.Sub(d.Info.Time)
		if age <= 48*time.Hour && !d.Keep {
			t.Errorf("%s is %v old and was pruned despite --keep 48h", d.Info.Name, age)
		}
	}
	kept := keptNames(decisions)
	if len(kept) < 9 {
		t.Errorf("kept %d archives; the last 48 hours alone hold 9", len(kept))
	}
}

// TestPruneNeverTouchesProtected: the protected tag is what a retention policy must not be
// able to override.
//
//borge:checks match-archives/protected-tag
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

	decisions := Prune(archives, policyOf(base, RuleDaily, KeepCount(2)))

	// borg removes protected archives before anything else happens, so they appear in no
	// decision at all: not kept with a reason, not pruned, not counted. borge used to
	// report them as "Keeping archive (rule: protected by @PROT)", which is friendlier and
	// is not what a frontend reading borg's output sees.
	for _, d := range decisions {
		for _, tag := range d.Info.Tags {
			if tag == ProtectedTag {
				t.Errorf("the protected archive appears in the decisions as %+v; borg does "+
					"not consider it at all", d)
			}
		}
	}
	if len(decisions) != 9 {
		t.Errorf("%d decisions for ten archives one of which is protected, want 9", len(decisions))
	}
	// And it did not consume the daily rule's quota.
	var daily int
	for _, d := range decisions {
		if d.Rule == RuleDaily && !d.Oldest {
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
	decisions := Prune(archives, policyOf(jan1, RuleWeekly, KeepCount(1)))
	// One by the rule; the other is the oldest, which the last rule keeps if it can - and
	// with a quota of one already spent it cannot, so exactly one survives.
	if len(keptNames(decisions)) != 1 {
		t.Errorf("--keep-weekly=1 kept %v across a year boundary, want one", keptNames(decisions))
	}
}

// TestPruneKeepCount is borg's --keep N, the rule that does mean "the last N archives" -
// borg 1 spelled it --keep-last.
func TestPruneKeepCount(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		archives = append(archives, at(i+1, base.Add(-time.Duration(i)*time.Minute)))
	}
	decisions := Prune(archives, policyOf(base, RuleKeep, KeepCount(3)))
	kept := keptNames(decisions)
	if len(kept) != 3 {
		t.Fatalf("--keep 3 kept %d archives (%v)", len(kept), kept)
	}
	// The three newest, and in newest-first order. The oldest is not added: the quota is
	// spent.
	if kept[0] != "a001" || kept[1] != "a002" || kept[2] != "a003" {
		t.Errorf("kept %v, want the three newest", kept)
	}
}

// TestPruneEmptyPolicyIsRefusedByTheCaller documents the guard, which lives in the
// command: Prune itself applied to an empty policy legitimately keeps nothing.
func TestPruneEmptyPolicyKeepsNothing(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	archives := []Info{at(1, base), at(2, base.AddDate(0, 0, -1))}

	policy := PrunePolicy{Keep: map[RuleKind]KeepValue{}}
	if !policy.Empty() {
		t.Fatal("a policy with no rules does not report itself empty")
	}
	decisions := Prune(archives, policy)
	if len(keptNames(decisions)) != 0 {
		t.Error("an empty policy kept something; the command's refusal is what protects the user")
	}
}

// TestPruneKeepOldest: the last rule given keeps the oldest archive, if it has room.
//
// It is not an option in borg 2 and was one in borge (--keep-oldest), which meant borge
// DELETED the start of the history where borg keeps it. Three properties, because the rule
// is easy to implement in a way that passes one of them and fails the others.
func TestPruneKeepOldest(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		archives = append(archives, at(i+1, base.AddDate(0, 0, -i)))
	}

	// Ten daily archives fall into one yearly group, so "--keep-yearly 4" keeps one and
	// has room to spare: the oldest is kept too, and marked.
	decisions := Prune(archives, policyOf(base, RuleYearly, KeepCount(4)))
	last := decisions[len(decisions)-1]
	if !last.Keep || !last.Oldest {
		t.Errorf("the oldest archive was not kept by the last rule: %+v", last)
	}
	if last.Rule != RuleYearly {
		t.Errorf("the oldest archive is attributed to %q, want the last rule", last.Rule)
	}

	// With the quota exactly spent it is not kept: can_retain is false, and borg lets the
	// start of the history go rather than exceed the count it was given. That is why the
	// count has to be checked against the number of *groups*, not against the archives.
	decisions = Prune(archives, policyOf(base, RuleYearly, KeepCount(1)))
	last = decisions[len(decisions)-1]
	if last.Keep {
		t.Errorf("the oldest archive was kept although the rule's quota was spent: %+v", last)
	}

	// Only the LAST rule does it. With daily and yearly both given, the oldest belongs to
	// yearly - and never to daily.
	decisions = Prune(archives, policyOf(base, RuleDaily, KeepCount(2), RuleYearly, KeepCount(4)))
	oldestSeen := false
	for _, d := range decisions {
		if !d.Oldest {
			continue
		}
		oldestSeen = true
		if d.Rule != RuleYearly {
			t.Errorf("the oldest archive was kept by %q; only the last rule does that", d.Rule)
		}
	}
	if !oldestSeen {
		t.Error("no archive was kept as the oldest, though the last rule had room")
	}
}

// TestPruneZeroRuleIsStillTheLastRule: a rule given as zero keeps nothing AND takes the
// keep-oldest job with it, because borg picks the last rule that was *given*.
func TestPruneZeroRuleIsStillTheLastRule(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		archives = append(archives, at(i+1, base.AddDate(0, 0, -i)))
	}
	decisions := Prune(archives, policyOf(base, RuleDaily, KeepCount(4), RuleYearly, KeepCount(0)))
	for _, d := range decisions {
		if d.Oldest {
			t.Errorf("%s was kept as the oldest; the last rule given was yearly=0, which "+
				"keeps nothing at all", d.Info.Name)
		}
	}
	if n := len(keptNames(decisions)); n != 4 {
		t.Errorf("kept %d archives, want the four daily ones and no oldest", n)
	}
}

// TestPruneFrom: --from holds back everything at or after it, and those archives do not
// occupy a retention period.
func TestPruneFrom(t *testing.T) {
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	var archives []Info
	for i := 0; i < 10; i++ {
		archives = append(archives, at(i+1, base.AddDate(0, 0, -i)))
	}
	from := base.AddDate(0, 0, -2) // the newest three are held back

	policy := policyOf(base, RuleDaily, KeepCount(2))
	policy.From = from
	decisions := Prune(archives, policy)

	var held, daily int
	for _, d := range decisions {
		switch {
		case d.Rule == RuleFrom:
			held++
			if !d.Keep {
				t.Errorf("%s is newer than --from and was pruned", d.Info.Name)
			}
		case d.Rule == RuleDaily:
			daily++
		}
	}
	if held != 3 {
		t.Errorf("%d archives held back by --from, want 3", held)
	}
	// The daily rule still keeps two of its own, from the archives --from left it.
	if daily != 2 {
		t.Errorf("%d archives kept by the daily rule, want 2 - the held-back ones must not "+
			"occupy its periods", daily)
	}
}

func TestDescribePolicy(t *testing.T) {
	p := PrunePolicy{Keep: map[RuleKind]KeepValue{
		RuleKeep:    KeepInterval(48 * time.Hour),
		RuleDaily:   KeepCount(7),
		RuleMonthly: KeepCount(-1),
	}}
	got := DescribePolicy(p)
	for _, want := range []string{"keep=48h", "daily=7", "monthly=all"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribePolicy is %q, missing %q", got, want)
		}
	}
	if DescribePolicy(PrunePolicy{}) != "(nothing)" {
		t.Errorf("an empty policy describes as %q", DescribePolicy(PrunePolicy{}))
	}
}
