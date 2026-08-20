// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/delete_cmd.py, rename_cmd.py,
// tag_cmd.py and undelete_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/formatter"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repoobj"
)

// cmdDelete soft-deletes archives.
//
// Soft: the directory entry is renamed, the objects stay. The space is reclaimed by a
// later compaction, which is what makes an accidental delete recoverable and what stops a
// delete from having to walk every archive to find out what is still referenced.
func cmdDelete(e *Env, args []string) int {
	fs := newFlagSet(e, "delete")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs, selectorExtras{})
	dryRun := fs.Bool("dry-run", false,
		"say what would be deleted, delete nothing (with --list, say which)")
	list := fs.Bool("list", false, "print each archive as it is deleted")
	force := fs.Bool("force", false,
		"delete without requiring a selector to match exactly one archive (borge only on this command)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if sel.match == "" && fs.NArg() > 0 {
		sel.match = fs.Arg(0)
	}
	if sel.match == "" {
		e.errorf("delete needs an archive; pass a name or -a SELECTOR")
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
		e.errorf("no archive matches %q", sel.match)
		return ExitError
	}
	if len(infos) > 1 && !*force {
		// Deleting more than was meant is not recoverable in the way a mistyped name is,
		// so a selector that matches several archives has to say so explicitly.
		var names []string
		for _, info := range infos {
			names = append(names, info.Name)
		}
		e.errorf("%q matches %d archives (%s); pass --force to delete them all",
			sel.match, len(infos), strings.Join(names, ", "))
		return ExitError
	}

	for i, info := range infos {
		if !*dryRun {
			if err := o.manifest.Archives.Delete(info.ID); err != nil {
				return e.fail(err)
			}
		}
		if err := e.listArchive(*list, *dryRun, "delete", "Deleted archive", info, i, len(infos)); err != nil {
			return e.fail(err)
		}
	}
	if *dryRun {
		e.reportDryRun("delete", *list, len(infos))
		return ExitOK
	}
	if err := o.manifest.Write(); err != nil {
		return e.fail(err)
	}
	// borg says this on every real delete, and it is the sentence that stops a user
	// wondering why the disk did not shrink: the delete is soft until a compaction runs.
	fmt.Fprintln(e.Stderr, `Done. Run "borge compact" to free space.`)
	return ExitOK
}

// reportDryRun says what a dry run would have done.
//
// borg prints nothing at all here, and silence is an answer nobody can act on: the whole
// point of a dry run is to decide something from what it says. So borge says it, and says
// which option to pass to see the detail. See docs/DIVERGENCES.md #31 and
// PORTING_PLAN.md §2.3.
//
// Dry runs only. The real paths are byte-identical to borg's and are left that way -
// there is a format there that scripts parse, and there is none here.
func (e *Env) reportDryRun(verb string, listed bool, n int) {
	hint := " (pass --list to see which)"
	if listed {
		hint = ""
	}
	fmt.Fprintf(e.Stderr, "would %s %d archive(s); nothing was changed%s\n", verb, n, hint)
}

// listArchive prints borg's per-archive line for delete and undelete.
//
// borg prints these *only* under --list: its -v (which is --info) produces exactly what a
// plain run does. borge used to print its own line under -v and another under --dry-run,
// which meant three different shapes for one event; there is one now, and it is borg's.
//
// The template is borg's fixed archive format - neither command takes --format - and the
// counter is borg's too: it tells a user watching a long delete how far along it is.
func (e *Env) listArchive(list, dryRun bool, verb, doneLabel string, info manifest.Info, i, total int) error {
	if !list {
		return nil
	}
	label := doneLabel + ": "
	if dryRun {
		label = "Would " + verb + ": "
	}
	line, err := formatter.Format("{archive:<36} {time} [{id}]", archiveValues(info))
	if err != nil {
		return err
	}
	// stderr, as borg's is: these are progress, not the command's data.
	fmt.Fprintf(e.Stderr, "%s%s (%d/%d)\n", label, line, i+1, total)
	return nil
}

// cmdUndelete restores soft-deleted archives.
func cmdUndelete(e *Env, args []string) int {
	fs := newFlagSet(e, "undelete")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs, selectorExtras{})
	dryRun := fs.Bool("dry-run", false,
		"say what would be restored, restore nothing (with --list, say which)")
	list := fs.Bool("list", false, "print each archive as it is restored")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if sel.match == "" && fs.NArg() > 0 {
		sel.match = fs.Arg(0)
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
	opts.Deleted = true
	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		e.errorf("no soft-deleted archive matches")
		return ExitError
	}

	for i, info := range infos {
		if !*dryRun {
			if err := o.manifest.Archives.Undelete(info.ID); err != nil {
				return e.fail(err)
			}
		}
		if err := e.listArchive(*list, *dryRun, "undelete", "Undeleted archive", info, i, len(infos)); err != nil {
			return e.fail(err)
		}
	}
	if *dryRun {
		e.reportDryRun("undelete", *list, len(infos))
		return ExitOK
	}
	if err := o.manifest.Write(); err != nil {
		return e.fail(err)
	}
	fmt.Fprintln(e.Stderr, "Done.")
	return ExitOK
}

// cmdRename gives an archive a new name.
//
// The archive object carries the name, so renaming rewrites it - and the object is
// content-addressed, so the rewrite produces a *new* archive id and a new directory entry.
// The old one is soft-deleted rather than removed, for the same reason delete is soft.
func cmdRename(e *Env, args []string) int {
	fs := newFlagSet(e, "rename")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 2 {
		e.errorf("rename needs the old archive and the new name")
		return ExitError
	}
	oldName := fs.Arg(0)
	newName, err := e.expand(fs.Arg(1))
	if err != nil {
		return e.fail(err)
	}

	return e.rewriteArchive(common, oldName, func(meta *item.ArchiveItem) error {
		meta.Name = newName
		return nil
	})
}

// cmdTag changes the tags of every archive the filters select.
//
// borg's tag takes the whole archive-filter group and an *optional* archive name, and acts
// on the whole selection - with no selector at all, on every archive in the repository.
// borge required exactly one name, so eight of borg's options were missing here.
//
// # What is deliberately not copied
//
// borg spells the tag options as "--add [TAG ...]", variadic, and argparse's greedy
// nargs="*" then swallows the positional: "borg tag --add Z a2" does not tag archive a2,
// it adds the tags "Z" and "a2" to *every* archive in the repository. Measured, not
// inferred. borge's --add takes one value and is repeatable, so the same command line is
// unambiguous, and reproducing borg's spelling would import a footgun that silently
// rewrites every archive. See docs/DIVERGENCES.md #27.
func cmdTag(e *Env, args []string) int {
	fs := newFlagSet(e, "tag")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs, selectorExtras{})
	var add, remove multiFlag
	fs.Var(&add, "add", "add a tag (repeatable)")
	fs.Var(&remove, "remove", "remove a tag (repeatable)")
	set := fs.String("set", "", "replace all tags with this comma-separated list")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() > 1 {
		e.errorf("tag takes at most one archive name; use -a to select several")
		return ExitError
	}
	if sel.match == "" && fs.NArg() == 1 {
		sel.match = fs.Arg(0)
	}
	if len(add) == 0 && len(remove) == 0 && *set == "" {
		e.errorf("tag needs --add, --remove or --set")
		return ExitError
	}
	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
	}

	// Validated before anything is rewritten, and validated for --add, --remove and --set
	// alike: borge accepted any string at all, so "borge tag --add 'my tag'" wrote a tag
	// borg refuses to create - and one that a comma-separated "{tags}" listing cannot be
	// read back from. Found while giving create the same option.
	for _, tag := range append(append([]string{}, add...), remove...) {
		if err := validateTag(tag); err != nil {
			return e.fail(err)
		}
	}
	if *set != "" {
		for _, tag := range strings.Split(*set, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				if err := validateTag(tag); err != nil {
					return e.fail(err)
				}
			}
		}
	}

	return e.rewriteArchives(common, opts, func(meta *item.ArchiveItem) error {
		tags := map[string]bool{}
		if *set == "" {
			for _, t := range meta.Tags {
				tags[t] = true
			}
		} else {
			for _, t := range strings.Split(*set, ",") {
				if t = strings.TrimSpace(t); t != "" {
					tags[t] = true
				}
			}
		}
		for _, t := range add {
			tags[t] = true
		}
		for _, t := range remove {
			delete(tags, t)
		}
		meta.Tags = meta.Tags[:0]
		for t := range tags {
			meta.Tags = append(meta.Tags, t)
		}
		sort.Strings(meta.Tags)
		meta.TagsSet = true
		return nil
	})
}

// rewriteArchive reads an archive's metadata object, lets the caller change it, writes the
// new object and swaps the directory entry.
//
// The item stream is untouched: only the archive object is rewritten, so the cost does not
// depend on how many files the archive holds. What it does cost is a new object, because
// the store is content-addressed and there is no such thing as changing one in place.
func (e *Env) rewriteArchive(common commonFlags, selector string, change func(*item.ArchiveItem) error) int {
	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, true, manifest.OpWrite)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, selector)
	if err != nil {
		return e.fail(err)
	}
	return e.rewriteOne(o, common, a, change)
}

// rewriteArchives is rewriteArchive over a selection: every archive the filters match,
// each rewritten in turn under one repository lock.
//
// An empty selection is an error, not a quiet success. borg exits 0 having done nothing,
// but borge already refuses that for delete, and a write command that changed nothing
// while reporting success is the failure §2.3 of the porting plan is about: a typo in
// "-a 'sh:dayly-*'" would leave the user believing their archives were tagged. See
// docs/DIVERGENCES.md #28.
func (e *Env) rewriteArchives(common commonFlags, opts manifest.ListOptions, change func(*item.ArchiveItem) error) int {
	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, true, manifest.OpWrite)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		e.errorf("no archive matched; nothing was changed")
		return ExitError
	}
	for _, info := range infos {
		a, err := archive.Open(o.manifest, info.ID)
		if err != nil {
			return e.fail(err)
		}
		if code := e.rewriteOne(o, common, a, change); code != ExitOK {
			return code
		}
	}
	return ExitOK
}

// rewriteOne reads an archive's metadata object, lets the caller change it, writes the new
// object and swaps the directory entry.
func (e *Env) rewriteOne(o *opened, common commonFlags, a *archive.Archive, change func(*item.ArchiveItem) error) int {
	meta := a.Meta
	if err := change(meta); err != nil {
		return e.fail(err)
	}

	data, err := meta.Marshal()
	if err != nil {
		return e.fail(err)
	}
	newID := o.key.IDHash(data)
	if string(newID) == string(a.ID) {
		// Nothing actually changed. Rewriting anyway would leave an identical object and
		// a pointless manifest write.
		if common.verbose {
			fmt.Fprintln(e.Stdout, "no change")
		}
		return ExitOK
	}

	obj, err := o.manifest.RepoObj().Format(newID, &repoobj.Meta{Type: repoobj.TypeArchiveMeta}, data)
	if err != nil {
		return e.fail(err)
	}
	if _, err := o.repo.Put(newID, obj); err != nil {
		return e.fail(err)
	}
	// Flush before the pointer: the directory entry must never name an object that is
	// still sitting in the pack writer's buffer.
	if err := o.repo.Flush(); err != nil {
		return e.fail(err)
	}
	if err := o.manifest.Archives.Create(newID); err != nil {
		return e.fail(err)
	}
	if err := o.manifest.Archives.Delete(a.ID); err != nil {
		return e.fail(err)
	}
	if err := o.manifest.Write(); err != nil {
		return e.fail(err)
	}

	if common.verbose {
		fmt.Fprintf(e.Stdout, "%s -> %s\n", hex.EncodeToString(a.ID)[:8], hex.EncodeToString(newID)[:8])
	}
	return ExitOK
}
