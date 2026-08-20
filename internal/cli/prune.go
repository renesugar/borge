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
	// No --first, --last or --sort-by here: borg has none of the three on prune, and each
	// changes what prune deletes rather than what it shows. See selectorExtras.omitRange.
	sel.register(fs, selectorExtras{omitRange: true})

	// Every --keep-* option borg has, in borg's order, each taking a count or an interval.
	//
	// borge used to offer borg 1's shape instead: --keep-last, --keep-within and
	// --keep-oldest, none of which borg 2 has. All three are now spellings of borg's
	// --keep - "--keep N" is --keep-last, "--keep 7d" is --keep-within - and keeping the
	// oldest archive is no longer a flag at all, because borg does it automatically for
	// the last rule given. See DIVERGENCES.md #50.
	keeps := map[manifest.RuleKind]*keepFlag{}
	for _, spec := range keepSpellings {
		flag := &keepFlag{}
		keeps[spec.rule] = flag
		help := fmt.Sprintf("number or time interval of %s to keep", spec.noun)
		fs.Var(flag, spec.long, help)
		if spec.short != "" {
			fs.Var(flag, spec.short, help)
		}
	}
	var from timestampFlag
	fs.Var(&from, "from", "only consider archives older than this for pruning")

	dryRun := fs.Bool("dry-run", false, "say what would be pruned, prune nothing")
	fs.BoolVar(dryRun, "n", false, "say what would be pruned, prune nothing")
	list := fs.Bool("list", false, "print every archive and the rule that kept it")
	listKept := fs.Bool("list-kept", false, "print only the archives that are kept")
	listPruned := fs.Bool("list-pruned", false, "print only the archives that are pruned")
	short := fs.Bool("short", false, "print only the archive names")
	format := fs.String("format", "", "output format for each archive, e.g. '{archive} {time}'")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	policy := manifest.PrunePolicy{Keep: map[manifest.RuleKind]manifest.KeepValue{}, From: from.value()}
	for _, spec := range keepSpellings {
		if f := keeps[spec.rule]; f.set {
			policy.Keep[spec.rule] = f.value
		}
	}

	// borg's two checks, in borg's words. They are separate because they are different
	// mistakes: giving no rule at all, and giving rules that all keep nothing. Either one
	// applied to a repository deletes every archive in it.
	if policy.Empty() {
		e.errorf("At least one of the %s settings must be specified.", keepOptionNames())
		return ExitError
	}
	if policy.AllZero() {
		e.errorf("None of the %s settings have a positive value. At least one must be non-zero.",
			keepOptionNames())
		return ExitError
	}
	// The two quarterly rules are a mutually exclusive group in borg's parser, and the
	// reason is in their names: they are two answers to "what is a quarter", 13 ISO weeks
	// or 3 calendar months. Giving both would keep two overlapping sets of quarters.
	_, has13 := policy.Keep[manifest.RuleQuarterly13Weekly]
	_, has3m := policy.Keep[manifest.RuleQuarterly3Monthly]
	if has13 && has3m {
		e.errorf("argument --keep-3monthly: not allowed with argument --keep-13weekly")
		return ExitError
	}
	if err := validateKeepCombination(policy); err != nil {
		return e.fail(err)
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

	// borg's three lines, at its level: they are logger.info, so "-v" shows them and a
	// plain run shows nothing. The counts are of the archives actually considered, which
	// excludes the protected ones - manifest.Prune drops those, as borg does.
	if common.verbose && !common.json {
		keptCount := 0
		for _, d := range decisions {
			if d.Keep {
				keptCount++
			}
		}
		count, err := o.manifest.Archives.Count()
		if err != nil {
			return e.fail(err)
		}
		fmt.Fprintf(e.Stderr, "Repository contains %d archives.\n", count)
		fmt.Fprintf(e.Stderr, "Applying rules to the matching %d archives...\n", len(decisions))
		fmt.Fprintf(e.Stderr, "Keeping %d archives, pruning %d archives.\n",
			keptCount, len(decisions)-keptCount)
	}

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

// validateKeepCombination is borg's lo/hi mismatch check.
//
// Two settings of the same *kind* where the finer rule reaches at least as far back as the
// coarser one make the coarser one useless: every archive it could keep has already been
// kept. borg refuses that rather than silently doing nothing with an option somebody typed
// on purpose, and it checks the two kinds separately because they are not comparable -
// "--keep-daily 30" and "--keep-monthly 7d" limit different things.
//
// "all" (-1) counts as infinite: it is bigger than every interval, and a finer rule set to
// it makes every coarser rule useless.
func validateKeepCombination(policy manifest.PrunePolicy) error {
	active := policy.Active()

	// The names are the RULE names, not the option names: borg builds its message from a
	// dict keyed by rule.key, so "--keep-13weekly" appears there as "quarterly_13weekly".
	mismatch := func(lo manifest.RuleKind, loV manifest.KeepValue, hi manifest.RuleKind, hiV manifest.KeepValue) error {
		return fmt.Errorf("The combination of \"%s='%s'\" and \"%s='%s'\" is invalid. It is "+
			"effectively useless since every archive matched by %s would have already been "+
			"matched by %s.", lo, loV, hi, hiV, hi, lo)
	}

	// "all" is in BOTH groups, which is not tidiness but borg's behaviour: its filters are
	// "isinstance(val, timedelta) or val == -1" and "isinstance(val, int)", and -1 is an
	// int. So "--keep-daily all --keep-monthly 5" is rejected through the count group,
	// while "--keep-daily 30 --keep-monthly all" is accepted - infinity is bigger than
	// any count, so the coarser rule still has work to do. Putting -1 in one group only
	// let the first of those through.
	var intervals []manifest.RuleKind
	var counts []manifest.RuleKind
	for _, kind := range active {
		v := policy.Keep[kind]
		if v.IsInterval() || v.IsAll() {
			intervals = append(intervals, kind)
		}
		if !v.IsInterval() {
			counts = append(counts, kind)
		}
	}

	for i := 0; i < len(intervals); i++ {
		for j := i + 1; j < len(intervals); j++ {
			lo, hi := intervals[i], intervals[j]
			loV, hiV := policy.Keep[lo], policy.Keep[hi]
			if hiV.IsAll() {
				// Infinity is always bigger, so the coarser rule still has work to do.
				continue
			}
			if loV.IsAll() || loV.Interval() >= hiV.Interval() {
				return mismatch(lo, loV, hi, hiV)
			}
		}
	}
	for i := 0; i < len(counts); i++ {
		for j := i + 1; j < len(counts); j++ {
			lo, hi := counts[i], counts[j]
			loV, hiV := policy.Keep[lo], policy.Keep[hi]
			if loV.IsAll() {
				return mismatch(lo, loV, hi, hiV)
			}
		}
	}
	return nil
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
