// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/prune_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/renesugar/borge/internal/manifest"
)

// cmdPrune applies a retention policy.
//
// Pruning is a soft delete: the archives it removes can be undeleted until a compaction
// runs. That is deliberate - a retention policy applied to the wrong repository, or with
// a rule the user misread, is recoverable right up until somebody reclaims the space.
func cmdPrune(e *Env, args []string) int {
	fs := newFlagSet(e, "prune")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs)

	counts := map[manifest.RuleKind]*int{}
	for _, kind := range []manifest.RuleKind{
		manifest.RuleLast, manifest.RuleSecondly, manifest.RuleMinutely, manifest.RuleHourly,
		manifest.RuleDaily, manifest.RuleWeekly, manifest.RuleMonthly, manifest.RuleYearly,
	} {
		counts[kind] = fs.Int("keep-"+string(kind), 0,
			fmt.Sprintf("keep one archive from each of the last N %s periods (-1 for all)", kind))
	}
	within := fs.String("keep-within", "", "keep every archive newer than this, e.g. 48h or 7d")
	keepOldest := fs.Bool("keep-oldest", false, "always keep the oldest archive")
	dryRun := fs.Bool("dry-run", false, "say what would be pruned, prune nothing")
	list := fs.Bool("list", false, "print every archive and the rule that kept it")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	policy := manifest.PrunePolicy{Counts: map[manifest.RuleKind]int{}, KeepOldest: *keepOldest}
	for kind, n := range counts {
		if *n != 0 {
			policy.Counts[kind] = *n
		}
	}
	if *within != "" {
		d, err := parseKeepWithin(*within)
		if err != nil {
			return e.fail(err)
		}
		policy.Within = d
	}

	// An empty policy would delete every archive in the repository. That is almost never
	// what somebody meant to type, and it is not recoverable once a compaction runs, so it
	// is refused rather than confirmed.
	if policy.Empty() {
		e.errorf("prune needs at least one --keep-* rule; " +
			"with none, every archive would be pruned")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, true, manifest.OpDelete)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	infos, err := o.manifest.Archives.List(sel.options())
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		fmt.Fprintln(e.Stdout, "no archives to prune")
		return ExitOK
	}

	decisions := manifest.Prune(infos, policy)

	var pruned, kept int
	for _, d := range decisions {
		short := hex.EncodeToString(d.Info.ID)[:8]
		if d.Keep {
			kept++
			if *list || *dryRun || common.verbose {
				fmt.Fprintf(e.Stdout, "keep   %s %-20s %s  (%s)\n",
					short, d.Info.Name, formatTime(d.Info.Time), d.Reason)
			}
			continue
		}
		pruned++
		if *list || *dryRun || common.verbose {
			fmt.Fprintf(e.Stdout, "prune  %s %-20s %s\n", short, d.Info.Name, formatTime(d.Info.Time))
		}
		if *dryRun {
			continue
		}
		if err := o.manifest.Archives.Delete(d.Info.ID); err != nil {
			return e.fail(err)
		}
	}

	if !*dryRun && pruned > 0 {
		if err := o.manifest.Write(); err != nil {
			return e.fail(err)
		}
	}

	verb := "pruned"
	if *dryRun {
		verb = "would prune"
	}
	fmt.Fprintf(e.Stdout, "%s %d archive(s), kept %d, policy: %s\n",
		verb, pruned, kept, manifest.DescribePolicy(policy))
	if !*dryRun && pruned > 0 {
		fmt.Fprintln(e.Stdout, "the pruned archives are soft-deleted; run 'borge compact' to reclaim the space")
	}
	return ExitOK
}

// parseKeepWithin reads a --keep-within duration.
//
// Go's ParseDuration knows hours but not days, weeks, months or years, which are the units
// a retention policy is actually written in. The extra suffixes are borg's.
func parseKeepWithin(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	unit := s[len(s)-1]
	var mult time.Duration
	switch unit {
	case 'd':
		mult = 24 * time.Hour
	case 'w':
		mult = 7 * 24 * time.Hour
	case 'm':
		// A calendar month is not a fixed length; borg uses 31 days here so that
		// "--keep-within 1m" never expires an archive early.
		mult = 31 * 24 * time.Hour
	case 'y':
		mult = 365 * 24 * time.Hour
	default:
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("--keep-within %q: use a Go duration (48h) or a count with "+
				"d, w, m or y (7d, 2w, 6m, 1y)", s)
		}
		return d, nil
	}
	var n int
	if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("--keep-within %q: %q is not a count", s, s[:len(s)-1])
	}
	return time.Duration(n) * mult, nil
}
