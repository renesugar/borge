// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/prune_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/renesugar/borge/internal/formatter"
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
	common.registerJSON(fs, "print the decisions as JSON; unlike the text form it lists every archive without --list")
	// borg's prune has no archive-filter group, so --first, --last and --sort-by are
	// borge's here and say so. They are also the ones that most deserve care: each
	// changes which archives prune considers, and --sort-by changes the order the keep
	// rules walk, so all three can change what is deleted.
	sel.register(fs, selectorExtras{rangeIsBorgeOnly: true})

	counts := map[manifest.RuleKind]*int{}
	for _, kind := range []manifest.RuleKind{
		manifest.RuleLast, manifest.RuleSecondly, manifest.RuleMinutely, manifest.RuleHourly,
		manifest.RuleDaily, manifest.RuleWeekly, manifest.RuleMonthly, manifest.RuleYearly,
	} {
		help := fmt.Sprintf("keep one archive from each of the last N %s periods (-1 for all)", kind)
		if kind == manifest.RuleLast {
			// borg has every other keep-* rule and not this one: "keep the newest N
			// archives, whenever they were made".
			help = "keep the newest N archives regardless of when they were made (borge only)"
		}
		counts[kind] = fs.Int("keep-"+string(kind), 0, help)
	}
	within := fs.String("keep-within", "", "keep every archive newer than this, e.g. 48h or 7d (borge only)")
	keepOldest := fs.Bool("keep-oldest", false, "always keep the oldest archive (borge only)")
	dryRun := fs.Bool("dry-run", false, "say what would be pruned, prune nothing")
	list := fs.Bool("list", false, "print every archive and the rule that kept it")
	listKept := fs.Bool("list-kept", false, "print only the archives that are kept")
	listPruned := fs.Bool("list-pruned", false, "print only the archives that are pruned")
	short := fs.Bool("short", false, "print only the archive names")
	format := fs.String("format", "", "output format for each archive, e.g. '{archive} {time}'")
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

	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
	}
	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		// In JSON mode this still has to be a document: a frontend that gets nothing on
		// stdout cannot tell "no archives" from "the command died", and an empty
		// "archives" list says exactly which it was. borg emits the envelope here too.
		if common.json {
			repoBlock, encBlock := o.envelope(path)
			enc := json.NewEncoder(e.Stdout)
			enc.SetIndent("", "    ")
			if err := enc.Encode(map[string]any{
				"archives":   []map[string]any{},
				"repository": repoBlock,
				"encryption": encBlock,
			}); err != nil {
				return e.fail(err)
			}
			return ExitOK
		}
		fmt.Fprintln(e.Stderr, "no archives to prune")
		return ExitOK
	}

	// borg's default, and the same precedence as everywhere else: --format, then --short,
	// then the environment, then the built-in. Note it carries no {NL}: the command adds
	// the newline, because the line is a label plus this.
	template := *format
	if template == "" {
		if *short {
			template = "{archive}"
		} else if v, ok := e.lookupBorg("PRUNE_FORMAT"); ok && v != "" {
			template = v
		} else {
			template = "{archive:<36} {time} [{id}]"
		}
	}
	if _, err := formatter.Keys(template); err != nil {
		return e.fail(err)
	}

	decisions := manifest.Prune(infos, policy)

	total := 0
	for _, d := range decisions {
		if !d.Keep {
			total++
		}
	}

	var pruned, kept int
	rows := []map[string]any{}
	for _, d := range decisions {
		var line string
		var data map[string]any
		if common.json {
			data, err = archiveJSONData(d.Info, template)
		} else {
			line, err = formatter.Format(template, archiveValues(d.Info))
		}
		if err != nil {
			return e.fail(err)
		}
		var label string
		if d.Keep {
			kept++
			label = fmt.Sprintf("Keeping archive (rule: %s):", keepRuleLabel(d))
			if common.json {
				data["kept"] = true
				data["keep_rule"] = string(d.Rule)
				data["kept_oldest"] = d.Oldest
				data["kept_archive_number"] = d.Index + 1
			}
		} else {
			pruned++
			if *dryRun {
				label = "Would prune:"
			} else {
				label = fmt.Sprintf("Pruning archive (%d/%d):", pruned, total)
			}
			if common.json {
				data["kept"] = false
				data["deleted_archive_number"] = pruned
			}
		}
		// borg's own layout: the label padded to 44 columns, a space, then the archive.
		// On stderr, because it is progress rather than the command's data.
		//
		// The JSON form has a different default, and it is borg's: without --list-kept or
		// --list-pruned every archive is included, where the text form shows none. That
		// is not an inconsistency to iron out - a document with an empty "archives" list
		// is a document a frontend can read and act on, whereas an empty stream is the
		// silence PORTING_PLAN.md section 2.3 is about.
		switch {
		case common.json:
			if *list || !(*listPruned || *listKept) ||
				(*listPruned && !d.Keep) || (*listKept && d.Keep) {
				rows = append(rows, data)
			}
		case *list || (*listKept && d.Keep) || (*listPruned && !d.Keep):
			fmt.Fprintf(e.Stderr, "%-44s %s\n", label, line)
		}
		if d.Keep || *dryRun {
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

	if common.json {
		repoBlock, encBlock := o.envelope(path)
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(map[string]any{
			"archives":   rows,
			"repository": repoBlock,
			"encryption": encBlock,
		}); err != nil {
			return e.fail(err)
		}
		// The compact hint still goes to stderr, where it cannot corrupt the document.
		if !*dryRun && pruned > 0 {
			fmt.Fprintln(e.Stderr, `Done. Run "borge compact" to free space.`)
		}
		return ExitOK
	}

	// borg prints no summary at all, and without a --list option prints nothing whatever.
	// borge says what happened, for the reason in PORTING_PLAN.md §2.3: a prune that
	// reports nothing cannot be told from one that matched nothing, and this is the
	// command that removes history. See DIVERGENCES.md #34.
	verb := "pruned"
	if *dryRun {
		verb = "would prune"
	}
	fmt.Fprintf(e.Stderr, "%s %d archive(s), kept %d, policy: %s\n",
		verb, pruned, kept, manifest.DescribePolicy(policy))
	if !*dryRun && pruned > 0 {
		fmt.Fprintln(e.Stderr, `Done. Run "borge compact" to free space.`)
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

// keepRuleLabel is borg's description of why an archive survived: the rule's name, a
// "[oldest]" mark when --keep-oldest also applied, and the rule's one-based index.
//
// borge's own reason strings ("daily[0]", "within 48h", "protected by @PROT") say more in
// some cases, but the listing is compared against borg's, so the shape is borg's.
func keepRuleLabel(d manifest.PruneDecision) string {
	if d.Rule == "" {
		// Kept by something that is not one of the counted rules: --keep-within, the
		// protected tag, or --keep-oldest on its own. borg has no equivalent label for
		// these, so borge's reason is used rather than inventing a rule name.
		return d.Reason
	}
	oldest := ""
	if d.Oldest {
		oldest = "[oldest]"
	}
	return fmt.Sprintf("%s%s #%d", d.Rule, oldest, d.Index+1)
}
