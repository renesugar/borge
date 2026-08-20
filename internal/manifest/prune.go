// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the pruning rules in borg's
// src/borg/archiver/prune_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package manifest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pruning: deciding which archives to keep.
//
// # How the rules work
//
// Each rule groups archives into periods - by hour, by day, by ISO week - and keeps the
// **newest** archive in each of the most recent N periods. A rule is not "keep the last 7
// archives"; it is "keep one archive from each of the last 7 days". Seven backups taken in
// one afternoon satisfy --keep-daily=7 with a single archive, not seven, which is the
// whole point: the rules describe the shape of the history you want to be able to restore
// from, not a count of files.
//
// Rules are applied in order from finest to coarsest, and an archive already kept by a
// finer rule does not consume a coarser rule's quota. So --keep-daily=7 --keep-monthly=6
// keeps seven daily archives *and* six monthly ones, rather than six months that happen to
// include this week.
//
// # Why local time
//
// The period keys are computed in the reader's timezone, as borg's are. "One backup a day"
// means one per local day, and a user in UTC+13 whose backups run at 20:00 would otherwise
// find them all landing in the next UTC day and being pruned as duplicates.

// ProtectedTag marks an archive that no pruning may ever remove.
//
// It exists because a retention policy is a blunt instrument and some archives are not
// interchangeable with their neighbours - the one from before a migration, the one a
// regulator asked for.
//
// borg removes them from the list before anything else happens, so they are not merely
// kept: they are not considered, do not appear in the listing, and are not counted in
// "Applying rules to the matching N archives". borge said "Keeping archive (rule: protected
// by @PROT)" instead, which is friendlier and is not what a frontend parsing prune's output
// gets from borg.
const ProtectedTag = "@PROT"

// RuleKind names a pruning rule. The value is the name borg prints in the listing and puts
// in the JSON as "keep_rule" - which for the two quarterly rules is not the option's name.
type RuleKind string

const (
	// RuleKeep is borg's --keep: every archive is its own group, so a count keeps the
	// newest N archives and an interval keeps everything within it. borg 1's --keep-last
	// and --keep-within were these two cases as separate options.
	RuleKeep              RuleKind = "keep"
	RuleSecondly          RuleKind = "secondly"
	RuleMinutely          RuleKind = "minutely"
	RuleHourly            RuleKind = "hourly"
	RuleDaily             RuleKind = "daily"
	RuleWeekly            RuleKind = "weekly"
	RuleMonthly           RuleKind = "monthly"
	RuleQuarterly13Weekly RuleKind = "quarterly_13weekly"
	RuleQuarterly3Monthly RuleKind = "quarterly_3monthly"
	RuleYearly            RuleKind = "yearly"

	// RuleFrom is borg's PRUNE_FROM: not a rule the user sets, but the reason attached to
	// an archive that --from held back from consideration. Its key really is "skip".
	RuleFrom RuleKind = "skip"
)

// RuleOrder is finest to coarsest, and it is borg's PRUNING_RULES order.
//
// The order is load-bearing twice over. A rule only keeps archives that a finer rule has
// not already kept, so applying "yearly" first would let one archive satisfy both the
// yearly and the daily rule and leave the history full of holes. And the *last* rule the
// user gave is the one that also keeps the oldest archive.
var RuleOrder = []RuleKind{
	RuleKeep, RuleSecondly, RuleMinutely, RuleHourly, RuleDaily, RuleWeekly,
	RuleMonthly, RuleQuarterly13Weekly, RuleQuarterly3Monthly, RuleYearly,
}

// KeepValue is what a --keep-* option was set to: a count, or a time interval.
//
// borg 2 accepts either for every rule ("number or time interval of archives to keep"),
// which is why this is a type and not an int. A count of -1 is "all", and an interval is
// measured back from --from when it is given and from now when it is not.
type KeepValue struct {
	count      int
	interval   time.Duration
	isInterval bool
}

// KeepCount returns a count-based setting. -1 means "all".
func KeepCount(n int) KeepValue { return KeepValue{count: n} }

// KeepInterval returns an interval-based setting.
func KeepInterval(d time.Duration) KeepValue {
	return KeepValue{interval: d, isInterval: true}
}

func (v KeepValue) IsInterval() bool        { return v.isInterval }
func (v KeepValue) Interval() time.Duration { return v.interval }
func (v KeepValue) Count() int              { return v.count }

// IsAll reports the -1 sentinel: no limit on how many this rule keeps.
func (v KeepValue) IsAll() bool { return !v.isInterval && v.count == -1 }

// IsZero reports a setting that keeps nothing, which borg checks for: a policy whose rules
// are all zero would delete every archive.
func (v KeepValue) IsZero() bool {
	if v.isInterval {
		return v.interval == 0
	}
	return v.count == 0
}

// String renders the value the way Python does, because the one place it is printed is
// borg's "the combination of X and Y is invalid" message, which is compared against borg's:
// an interval appears as str(timedelta) - "7 days, 0:00:00" - and a count as the bare
// number, so "all" appears there as -1.
func (v KeepValue) String() string {
	if v.isInterval {
		return pythonTimedelta(v.interval)
	}
	return strconv.Itoa(v.count)
}

// pythonTimedelta is str(datetime.timedelta): an optional "N day(s), " and then H:MM:SS,
// with the hours unpadded.
func pythonTimedelta(d time.Duration) string {
	days := int64(d / (24 * time.Hour))
	rest := d - time.Duration(days)*24*time.Hour
	h := int64(rest / time.Hour)
	m := int64(rest%time.Hour) / int64(time.Minute)
	sec := int64(rest%time.Minute) / int64(time.Second)
	hms := fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	switch {
	case days == 0:
		return hms
	case days == 1:
		return "1 day, " + hms
	default:
		return fmt.Sprintf("%d days, %s", days, hms)
	}
}

// periodOf returns the grouping key for a rule.
//
// The layouts are borg's strftime patterns, computed in local time. "One backup a day"
// means one per *local* day, and a user in UTC+13 whose backups run at 20:00 would
// otherwise find them all landing in the next UTC day and being pruned as duplicates.
//
// seq makes RuleKeep's key unique per archive, which is what turns that rule into "keep
// the newest N" (a count) or "keep everything within the interval".
func periodOf(kind RuleKind, t time.Time, seq int) string {
	local := t.Local()
	switch kind {
	case RuleKeep:
		return fmt.Sprintf("%09d", seq)
	case RuleSecondly:
		return local.Format("2006-01-02 15:04:05")
	case RuleMinutely:
		return local.Format("2006-01-02 15:04")
	case RuleHourly:
		return local.Format("2006-01-02 15")
	case RuleDaily:
		return local.Format("2006-01-02")
	case RuleWeekly:
		// ISO week, matching borg's %G-%V: the week-based year, not the calendar year.
		// They differ at the turn of the year, and using the calendar year there would
		// split one week across two groups.
		y, w := local.ISOWeek()
		return fmt.Sprintf("%04d-%02d", y, w)
	case RuleMonthly:
		return local.Format("2006-01")
	case RuleQuarterly13Weekly:
		// borg: f"{year}-{min(max((week - 1) // 13, 0), 3):02}". Thirteen weeks is a
		// quarter only approximately - a year has 52 or 53 ISO weeks - so week 53 is
		// clamped into the fourth quarter rather than opening a fifth.
		y, w := local.ISOWeek()
		q := (w - 1) / 13
		if q < 0 {
			q = 0
		}
		if q > 3 {
			q = 3
		}
		return fmt.Sprintf("%d-%02d", y, q)
	case RuleQuarterly3Monthly:
		return fmt.Sprintf("%d-%02d", local.Year(), (int(local.Month())-1)/3)
	case RuleYearly:
		return local.Format("2006")
	default:
		return ""
	}
}

// PrunePolicy is a set of retention rules.
type PrunePolicy struct {
	// Keep holds every rule the user gave, including one given as zero: borg distinguishes
	// "not given" from "given 0", and the difference decides which rule is last and
	// therefore which keeps the oldest archive.
	Keep map[RuleKind]KeepValue
	// From holds back everything at or after it: those archives are kept unconditionally
	// and are not considered by any rule, so they cannot occupy a retention period. Zero
	// means the option was not given.
	From time.Time
	// Now is the reference point for interval rules. Zero uses the current time. It is
	// overridden by From when that is set, as borg's base_timestamp is.
	Now time.Time
}

// Empty reports whether no rule was given at all.
func (p PrunePolicy) Empty() bool { return len(p.Keep) == 0 }

// AllZero reports whether every rule that was given keeps nothing.
func (p PrunePolicy) AllZero() bool {
	for _, v := range p.Keep {
		if !v.IsZero() {
			return false
		}
	}
	return true
}

// Active lists the rules the user gave, finest first. A rule set to zero is still active:
// it runs, keeps nothing, and can still be the last rule - which is how "--keep-daily 3
// --keep-yearly 0" ends up keeping no oldest archive at all.
func (p PrunePolicy) Active() []RuleKind {
	var out []RuleKind
	for _, kind := range RuleOrder {
		if _, ok := p.Keep[kind]; ok {
			out = append(out, kind)
		}
	}
	return out
}

// keptBy is which rule saved an archive, its index within that rule, and whether it was the
// "keep the oldest" case.
type keptBy struct {
	rule   RuleKind
	index  int
	oldest bool
}

// PruneDecision is what happens to one archive and why.
type PruneDecision struct {
	Info Info
	Keep bool
	// Reason names the rule that saved it, or why it is going.
	Reason string
	// Rule, Index and Oldest are the same decision in parts, for a caller that has to
	// render it borg's way: "daily #1", or "yearly[oldest] #4". Index is zero-based here
	// and one-based in borg's message.
	Rule   RuleKind
	Index  int
	Oldest bool
}

// Prune decides which archives to keep.
//
// The result is newest first and holds one entry per archive *considered* - which excludes
// the protected ones, as borg's does.
func Prune(archives []Info, policy PrunePolicy) []PruneDecision {
	// Newest first: every rule walks from the present backwards.
	sorted := make([]Info, 0, len(archives))
	for _, info := range archives {
		if !hasProtectedTag(info) {
			sorted = append(sorted, info)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.After(sorted[j].Time) })

	base := policy.Now
	if base.IsZero() {
		base = time.Now()
	}

	keep := map[string]keptBy{}
	idOf := func(info Info) string { return string(info.ID) }

	candidates := sorted
	if !policy.From.IsZero() {
		// --from is a prefilter, not a rule: archives at or after it are kept whatever the
		// rules say, and are removed from consideration so they cannot occupy a retention
		// period that an older archive would otherwise have filled.
		base = policy.From
		n := 0
		for _, info := range sorted {
			if info.Time.Before(policy.From) {
				break
			}
			keep[idOf(info)] = keptBy{rule: RuleFrom, index: len(keep)}
			n++
		}
		candidates = sorted[n:]
	}

	active := policy.Active()
	for i, kind := range active {
		value := policy.Keep[kind]
		// Only the last rule given keeps the oldest archive. A coarser rule doing it too
		// would keep an archive that the rule below it had already decided to let go.
		applyRule(kind, value, candidates, base, i == len(active)-1, keep, idOf)
	}

	out := make([]PruneDecision, 0, len(sorted))
	for _, info := range sorted {
		by, ok := keep[idOf(info)]
		if !ok {
			out = append(out, PruneDecision{Info: info, Keep: false, Reason: "no rule keeps it"})
			continue
		}
		out = append(out, PruneDecision{
			Info: info, Keep: true, Reason: describeKept(by),
			Rule: by.rule, Index: by.index, Oldest: by.oldest,
		})
	}
	return out
}

// applyRule is borg's prune(): one rule against the candidates, merging into keep.
func applyRule(kind RuleKind, value KeepValue, candidates []Info, base time.Time,
	keepOldest bool, keep map[string]keptBy, idOf func(Info) string) {

	if len(candidates) == 0 || value.IsZero() {
		return
	}

	var earliest time.Time
	if value.IsInterval() {
		earliest = base.Add(-value.Interval())
	}
	mine := 0 // how many this rule has kept, which is what a count limits
	canRetain := func(info Info) bool {
		if value.IsInterval() {
			return !info.Time.Before(earliest)
		}
		return value.IsAll() || mine < value.Count()
	}

	prevPeriod := ""
	havePeriod := false
	for seq, info := range candidates {
		if !canRetain(info) {
			break
		}
		period := periodOf(kind, info.Time, seq)
		if havePeriod && period == prevPeriod {
			// Not the newest archive of this period; this rule does not keep it.
			continue
		}
		prevPeriod, havePeriod = period, true
		if _, already := keep[idOf(info)]; already {
			// Kept by a finer rule already. It does **not** consume this rule's quota: a
			// rule keeps N archives *of its own*, so "--keep-daily 7 --keep-monthly 6"
			// gives seven days and six further months rather than six months that happen
			// to include this week. Counting it here instead is a plausible reading and a
			// wrong one - it makes a coarser rule stop short and silently discard older
			// history.
			continue
		}
		keep[idOf(info)] = keptBy{rule: kind, index: mine}
		mine++
	}

	if !keepOldest {
		return
	}
	// The oldest archive, if this rule still has room for it. borg does this for the last
	// rule given and only then: without it, a policy whose coarsest rule is satisfied by
	// recent archives silently discards the start of the history.
	oldest := candidates[len(candidates)-1]
	if _, already := keep[idOf(oldest)]; already {
		return
	}
	if !canRetain(oldest) {
		return
	}
	keep[idOf(oldest)] = keptBy{rule: kind, index: mine, oldest: true}
}

func hasProtectedTag(info Info) bool {
	for _, tag := range info.Tags {
		if tag == ProtectedTag {
			return true
		}
	}
	return false
}

// describeKept is borge's own wording for a decision, used where borg prints nothing - the
// JSON schema and the listing both go through RuleLabel instead.
func describeKept(by keptBy) string {
	if by.rule == RuleFrom {
		return "held back by --from"
	}
	oldest := ""
	if by.oldest {
		oldest = "[oldest]"
	}
	return fmt.Sprintf("%s%s[%d]", by.rule, oldest, by.index)
}

// DescribePolicy renders a policy the way borge's own summary does.
func DescribePolicy(p PrunePolicy) string {
	var parts []string
	if !p.From.IsZero() {
		parts = append(parts, "from "+p.From.Format(time.RFC3339))
	}
	for _, kind := range RuleOrder {
		v, ok := p.Keep[kind]
		if !ok {
			continue
		}
		// borge's own summary, so its own wording: "all" rather than the -1 that borg's
		// error messages print.
		switch {
		case v.IsAll():
			parts = append(parts, string(kind)+"=all")
		case v.IsInterval():
			parts = append(parts, string(kind)+"="+v.Interval().String())
		default:
			parts = append(parts, fmt.Sprintf("%s=%d", kind, v.Count()))
		}
	}
	if len(parts) == 0 {
		return "(nothing)"
	}
	return strings.Join(parts, ", ")
}
