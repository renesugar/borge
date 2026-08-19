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
const ProtectedTag = "@PROT"

// RuleKind names a pruning rule.
type RuleKind string

const (
	RuleLast     RuleKind = "last"
	RuleSecondly RuleKind = "secondly"
	RuleMinutely RuleKind = "minutely"
	RuleHourly   RuleKind = "hourly"
	RuleDaily    RuleKind = "daily"
	RuleWeekly   RuleKind = "weekly"
	RuleMonthly  RuleKind = "monthly"
	RuleYearly   RuleKind = "yearly"
	// RuleWithin keeps everything newer than a duration, regardless of the others.
	RuleWithin RuleKind = "within"
)

// periodOf returns the grouping key for a rule.
//
// The layouts are borg's strftime patterns. "last" gives every archive its own group, so
// the rule degenerates into "keep the newest N archives".
func periodOf(kind RuleKind, t time.Time, seq int) string {
	local := t.Local()
	switch kind {
	case RuleLast:
		// A unique key per archive, zero-padded so lexical order matches numeric order.
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
	case RuleYearly:
		return local.Format("2006")
	default:
		return ""
	}
}

// PrunePolicy is a set of retention rules.
type PrunePolicy struct {
	// Counts maps a rule to how many periods to keep. A negative value means "unlimited".
	// A rule absent from the map, or set to zero, is not applied.
	Counts map[RuleKind]int
	// Within keeps every archive newer than this, on top of whatever the counts keep.
	Within time.Duration
	// KeepOldest keeps the oldest archive even when the rules would not, which is a
	// safeguard against a policy that silently discards the start of the history.
	KeepOldest bool
	// Now is the reference time for Within. Zero uses the current time.
	Now time.Time
}

// Empty reports whether the policy would keep nothing at all.
//
// It is checked before anything is deleted: an empty policy applied to a repository
// deletes every archive in it, and that is almost never what somebody meant to type.
func (p PrunePolicy) Empty() bool {
	if p.Within > 0 {
		return false
	}
	for _, n := range p.Counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// PruneDecision is what happens to one archive and why.
// keptBy is which rule saved an archive, and whether --keep-oldest also did.
type keptBy struct {
	rule   RuleKind
	index  int
	oldest bool
}

type PruneDecision struct {
	Info Info
	Keep bool
	// Reason names the rule that saved it, or why it is going.
	Reason string
	// Rule, Index and Oldest are the same decision in parts, for a caller that has to
	// render it borg's way: "daily #1", or "daily[oldest] #1". Index is zero-based here
	// and one-based in borg's message.
	Rule   RuleKind
	Index  int
	Oldest bool
}

// ruleOrder is finest to coarsest. The order is load-bearing: a rule only keeps archives
// that a finer rule has not already kept, so applying "yearly" first would let one archive
// satisfy both the yearly and the daily rule and leave the history full of holes.
var ruleOrder = []RuleKind{
	RuleLast, RuleSecondly, RuleMinutely, RuleHourly, RuleDaily, RuleWeekly, RuleMonthly, RuleYearly,
}

// Prune decides which archives to keep.
//
// The result is in the order given, so a caller can print it as a listing. Archives
// carrying ProtectedTag are always kept and never counted against a rule's quota.
func Prune(archives []Info, policy PrunePolicy) []PruneDecision {
	// Newest first: every rule walks from the present backwards.
	sorted := append([]Info(nil), archives...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.After(sorted[j].Time) })

	now := policy.Now
	if now.IsZero() {
		now = time.Now()
	}

	keep := map[string]string{} // archive id -> the reason it is kept
	// keepRule is the same decision in parts, for a caller that renders it borg's way.
	keepRule := map[string]keptBy{}
	idOf := func(info Info) string { return string(info.ID) }

	// Protected archives first, so they never consume a rule's quota.
	for _, info := range sorted {
		for _, tag := range info.Tags {
			if tag == ProtectedTag {
				keep[idOf(info)] = "protected by " + ProtectedTag
			}
		}
	}

	if policy.Within > 0 {
		cutoff := now.Add(-policy.Within)
		for _, info := range sorted {
			if !info.Time.Before(cutoff) {
				if _, ok := keep[idOf(info)]; !ok {
					keep[idOf(info)] = "within " + policy.Within.String()
				}
			}
		}
	}

	for _, kind := range ruleOrder {
		n, ok := policy.Counts[kind]
		if !ok || n == 0 {
			continue
		}
		kept := 0
		prevPeriod := ""
		for seq, info := range sorted {
			if n >= 0 && kept >= n {
				break
			}
			period := periodOf(kind, info.Time, seq)
			if period == prevPeriod {
				// Not the newest archive of this period; a finer rule may have kept it,
				// but this one does not.
				continue
			}
			prevPeriod = period
			if _, already := keep[idOf(info)]; already {
				// Already kept by a finer rule, or protected. It does **not** consume this
				// rule's quota: a rule keeps N archives *of its own*, so
				// "--keep-daily=7 --keep-monthly=6" gives seven days and six further
				// months rather than six months that happen to include this week.
				//
				// Counting it here instead is a plausible reading and a wrong one - it
				// makes a coarser rule stop short and silently discard older history.
				// borg's prune() grows its own per-rule dict and checks that, which is
				// what this reproduces.
				continue
			}
			keep[idOf(info)] = fmt.Sprintf("%s[%d]", kind, kept)
			keepRule[idOf(info)] = keptBy{rule: kind, index: kept}
			kept++
		}
	}

	if policy.KeepOldest && len(sorted) > 0 {
		oldest := sorted[len(sorted)-1]
		id := idOf(oldest)
		if _, ok := keep[id]; !ok {
			keep[id] = "oldest"
			keepRule[id] = keptBy{oldest: true}
		} else {
			by := keepRule[id]
			by.oldest = true
			keepRule[id] = by
		}
	}

	out := make([]PruneDecision, 0, len(sorted))
	for _, info := range sorted {
		id := idOf(info)
		if reason, ok := keep[id]; ok {
			by := keepRule[id]
			out = append(out, PruneDecision{
				Info: info, Keep: true, Reason: reason,
				Rule: by.rule, Index: by.index, Oldest: by.oldest,
			})
		} else {
			out = append(out, PruneDecision{Info: info, Keep: false, Reason: "no rule keeps it"})
		}
	}
	return out
}

// ParseRuleCounts reads a policy from the command line's rule flags.
func ParseRuleCounts(values map[RuleKind]int) PrunePolicy {
	counts := map[RuleKind]int{}
	for k, v := range values {
		if v != 0 {
			counts[k] = v
		}
	}
	return PrunePolicy{Counts: counts}
}

// DescribePolicy renders a policy for the output of a dry run, so a user can check that
// what borge understood is what they meant.
func DescribePolicy(p PrunePolicy) string {
	var parts []string
	if p.Within > 0 {
		parts = append(parts, "within "+p.Within.String())
	}
	for _, kind := range ruleOrder {
		if n, ok := p.Counts[kind]; ok && n != 0 {
			if n < 0 {
				parts = append(parts, string(kind)+"=all")
			} else {
				parts = append(parts, fmt.Sprintf("%s=%d", kind, n))
			}
		}
	}
	if len(parts) == 0 {
		return "(nothing)"
	}
	return strings.Join(parts, ", ")
}
