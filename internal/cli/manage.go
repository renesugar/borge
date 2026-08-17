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
	sel.register(fs)
	dryRun := fs.Bool("dry-run", false, "say what would be deleted, delete nothing")
	force := fs.Bool("force", false, "delete without requiring a selector to match exactly one archive")
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

	infos, err := o.manifest.Archives.List(sel.options())
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

	for _, info := range infos {
		if *dryRun {
			fmt.Fprintf(e.Stdout, "would delete %s %s\n", hex.EncodeToString(info.ID)[:8], info.Name)
			continue
		}
		if err := o.manifest.Archives.Delete(info.ID); err != nil {
			return e.fail(err)
		}
		if common.verbose {
			fmt.Fprintf(e.Stdout, "deleted %s %s\n", hex.EncodeToString(info.ID)[:8], info.Name)
		}
	}
	if !*dryRun {
		if err := o.manifest.Write(); err != nil {
			return e.fail(err)
		}
	}
	return ExitOK
}

// cmdUndelete restores soft-deleted archives.
func cmdUndelete(e *Env, args []string) int {
	fs := newFlagSet(e, "undelete")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs)
	dryRun := fs.Bool("dry-run", false, "say what would be restored, restore nothing")
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

	opts := sel.options()
	opts.Deleted = true
	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		e.errorf("no soft-deleted archive matches")
		return ExitError
	}

	for _, info := range infos {
		if *dryRun {
			fmt.Fprintf(e.Stdout, "would undelete %s %s\n", hex.EncodeToString(info.ID)[:8], info.Name)
			continue
		}
		if err := o.manifest.Archives.Undelete(info.ID); err != nil {
			return e.fail(err)
		}
		if common.verbose {
			fmt.Fprintf(e.Stdout, "undeleted %s %s\n", hex.EncodeToString(info.ID)[:8], info.Name)
		}
	}
	if !*dryRun {
		if err := o.manifest.Write(); err != nil {
			return e.fail(err)
		}
	}
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
	oldName, newName := fs.Arg(0), fs.Arg(1)

	return e.rewriteArchive(common, oldName, func(meta *item.ArchiveItem) error {
		meta.Name = newName
		return nil
	})
}

// cmdTag changes an archive's tags.
func cmdTag(e *Env, args []string) int {
	fs := newFlagSet(e, "tag")
	var common commonFlags
	common.register(fs)
	var add, remove multiFlag
	fs.Var(&add, "add", "add a tag (repeatable)")
	fs.Var(&remove, "remove", "remove a tag (repeatable)")
	set := fs.String("set", "", "replace all tags with this comma-separated list")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("tag needs an archive")
		return ExitError
	}
	if len(add) == 0 && len(remove) == 0 && *set == "" {
		e.errorf("tag needs --add, --remove or --set")
		return ExitError
	}

	return e.rewriteArchive(common, fs.Arg(0), func(meta *item.ArchiveItem) error {
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
