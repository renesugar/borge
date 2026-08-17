// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/list_cmd.py, info_cmd.py and
// extract_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/patterns"
)

// patternFlags are the include/exclude options shared by list and extract.
//
// They are collected in the order the user wrote them, because order decides the outcome:
// the first matching pattern wins, so "--exclude X --pattern +X" and the reverse mean
// different things.
type patternFlags struct {
	excludes     multiFlag
	excludeFrom  multiFlag
	patternsFrom multiFlag
	pattern      multiFlag
}

// multiFlag collects a repeated option in order.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func (p *patternFlags) register(fs *flag.FlagSet) {
	fs.Var(&p.excludes, "e", "exclude paths matching this pattern (repeatable)")
	fs.Var(&p.excludes, "exclude", "exclude paths matching this pattern (repeatable)")
	fs.Var(&p.excludeFrom, "exclude-from", "read exclude patterns from a file (repeatable)")
	fs.Var(&p.pattern, "pattern", "an include/exclude pattern with a leading +, - or ! (repeatable)")
	fs.Var(&p.patternsFrom, "patterns-from", "read include/exclude patterns from a file (repeatable)")
}

// any reports whether any pattern option was given. A command that would otherwise match
// everything uses it to tell "no filter asked for" from "a filter that matches all".
func (p *patternFlags) any() bool {
	return len(p.excludes) > 0 || len(p.excludeFrom) > 0 ||
		len(p.pattern) > 0 || len(p.patternsFrom) > 0
}

// matcher builds the pattern matcher from the flags and the positional paths.
func (p *patternFlags) matcher(paths []string) (*patterns.Matcher, error) {
	m := patterns.NewMatcher(true)

	for _, spec := range p.pattern {
		e, err := patterns.ParseInclExclCommand(spec, patterns.StyleShellPath)
		if err != nil {
			return nil, err
		}
		if e.Pattern != nil {
			m.Add(e.Pattern, e.Cmd)
		}
	}
	for _, path := range p.patternsFrom {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		entries, _, err := patterns.LoadPatternFile(f, patterns.StyleShellPath)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, e := range entries {
			m.Add(e.Pattern, e.Cmd)
		}
	}
	for _, spec := range p.excludes {
		// --exclude defaults to fnmatch and does not recurse, which is borg's shape.
		pat, err := patterns.ParsePattern(spec, patterns.StyleFnmatch, false)
		if err != nil {
			return nil, err
		}
		m.Add(pat, patterns.CmdExcludeNoRecurse)
	}
	for _, path := range p.excludeFrom {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		pats, err := patterns.LoadExcludeFile(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, pat := range pats {
			m.Add(pat, patterns.CmdExcludeNoRecurse)
		}
	}
	if len(paths) > 0 {
		if err := m.AddIncludePaths(paths); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// openArchive resolves an archive selector - a name, or "aid:<hex prefix>".
func openArchive(m *manifest.Manifest, selector string) (*archive.Archive, error) {
	if strings.HasPrefix(selector, "aid:") {
		infos, err := m.Archives.List(manifest.ListOptions{Match: []string{selector}})
		if err != nil {
			return nil, err
		}
		if len(infos) != 1 {
			return nil, fmt.Errorf("%q matches %d archives", selector, len(infos))
		}
		return archive.Open(m, infos[0].ID)
	}
	return archive.OpenByName(m, selector)
}

// itemJSON is one line of --json-lines, with borg's field names.
type itemJSON struct {
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	User   string `json:"user"`
	Group  string `json:"group"`
	UID    int64  `json:"uid"`
	GID    int64  `json:"gid"`
	Path   string `json:"path"`
	Target string `json:"target"`
	Size   int64  `json:"size"`
	MTime  string `json:"mtime"`
	HLID   string `json:"hlid"`
}

func toItemJSON(it *item.Item) itemJSON {
	mode := it.ModeOr(0)
	out := itemJSON{
		Type: item.TypeChar(mode),
		Mode: item.FormatMode(mode),
		Path: it.Path,
		Size: itemSize(it),
		HLID: hex.EncodeToString(it.HLID),
	}
	if it.User != nil {
		out.User = *it.User
	}
	if it.Group != nil {
		out.Group = *it.Group
	}
	if it.UID != nil {
		out.UID = *it.UID
	}
	if it.GID != nil {
		out.GID = *it.GID
	}
	if it.Target != nil {
		out.Target = *it.Target
	}
	if it.MTime != nil {
		out.MTime = time.Unix(0, *it.MTime).Local().Format("2006-01-02T15:04:05.000000-07:00")
	}
	return out
}

// itemSize is what a listing reports: a file's content size, a symlink's target length,
// zero for everything else. It is computed from the chunk list rather than read from a
// stored field, because borg 2 does not write one.
func itemSize(it *item.Item) int64 {
	switch {
	case it.IsSymlink():
		if it.Target != nil {
			return int64(len(*it.Target))
		}
		return 0
	case it.IsRegular():
		return it.ContentSize()
	default:
		return 0
	}
}

// cmdList lists an archive's contents.
func cmdList(e *Env, args []string) int {
	fs := newFlagSet(e, "list")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	jsonLines := fs.Bool("json-lines", false, "print one JSON object per item")
	short := fs.Bool("short", false, "print only paths")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 1 {
		e.errorf("list needs an archive name")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false, manifest.OpRead)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	matcher, err := pf.matcher(fs.Args()[1:])
	if err != nil {
		return e.fail(err)
	}

	enc := json.NewEncoder(e.Stdout)
	err = a.Items(func(it *item.Item) error {
		if !matcher.Match(it.Path) {
			return nil
		}
		switch {
		case *jsonLines:
			return enc.Encode(toItemJSON(it))
		case *short:
			_, err := fmt.Fprintln(e.Stdout, it.Path)
			return err
		default:
			mode := it.ModeOr(0)
			owner, group := "", ""
			if it.User != nil {
				owner = *it.User
			}
			if it.Group != nil {
				group = *it.Group
			}
			mtime := ""
			if it.MTime != nil {
				mtime = formatTime(time.Unix(0, *it.MTime))
			}
			line := fmt.Sprintf("%s %-6s %-6s %8d %s %s",
				item.FormatMode(mode), owner, group, itemSize(it), mtime, it.Path)
			if it.IsSymlink() && it.Target != nil {
				line += " -> " + *it.Target
			}
			_, err := fmt.Fprintln(e.Stdout, line)
			return err
		}
	})
	if err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// cmdInfo prints an archive's metadata.
func cmdInfo(e *Env, args []string) int {
	fs := newFlagSet(e, "info")
	var common commonFlags
	common.register(fs)
	selector := fs.String("a", "", "the archive to describe")
	fs.StringVar(selector, "match-archives", "", "the archive to describe")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	name := *selector
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		e.errorf("info needs an archive; pass -a NAME")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false, manifest.OpRead)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, name)
	if err != nil {
		return e.fail(err)
	}

	if common.json {
		out := map[string]any{
			"archives": []map[string]any{{
				"name":     a.Info.Name,
				"id":       hex.EncodeToString(a.ID),
				"hostname": a.Info.Host,
				"username": a.Info.User,
				"comment":  a.Info.Comment,
				"start":    a.Info.Start.Local().Format("2006-01-02T15:04:05.000000-07:00"),
				"end":      a.Info.End.Local().Format("2006-01-02T15:04:05.000000-07:00"),
				"time":     a.Info.Time.Local().Format("2006-01-02T15:04:05.000000-07:00"),
				"nfiles":   a.Info.NFiles,
				"tags":     a.Info.Tags,
			}},
		}
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(out); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}

	fmt.Fprintf(e.Stdout, "Archive name: %s\n", a.Info.Name)
	fmt.Fprintf(e.Stdout, "Archive fingerprint: %s\n", hex.EncodeToString(a.ID))
	fmt.Fprintf(e.Stdout, "Comment: %s\n", a.Info.Comment)
	fmt.Fprintf(e.Stdout, "Hostname: %s\n", a.Info.Host)
	fmt.Fprintf(e.Stdout, "Username: %s\n", a.Info.User)
	fmt.Fprintf(e.Stdout, "Tags: %s\n", strings.Join(a.Info.Tags, ","))
	fmt.Fprintf(e.Stdout, "Time (nominal): %s\n", formatTime(a.Info.Time))
	fmt.Fprintf(e.Stdout, "Time (start): %s\n", formatTime(a.Info.Start))
	fmt.Fprintf(e.Stdout, "Time (end): %s\n", formatTime(a.Info.End))
	if !a.Info.Start.IsZero() && !a.Info.End.IsZero() {
		fmt.Fprintf(e.Stdout, "Duration: %.3f seconds\n", a.Info.End.Sub(a.Info.Start).Seconds())
	}
	if a.Meta.CommandLine != nil {
		fmt.Fprintf(e.Stdout, "Command line: %s\n", *a.Meta.CommandLine)
	}
	if a.Meta.CWD != nil {
		fmt.Fprintf(e.Stdout, "Working Directory: %s\n", *a.Meta.CWD)
	}
	fmt.Fprintf(e.Stdout, "Number of files: %d\n", a.Info.NFiles)
	fmt.Fprintf(e.Stdout, "Original size: %d\n", a.Info.Size)
	if a.Meta.ChunkerParamsSet {
		fmt.Fprintf(e.Stdout, "Chunker params: %v\n", a.Meta.ChunkerParams)
	}
	return ExitOK
}

// cmdExtract restores an archive.
func cmdExtract(e *Env, args []string) int {
	fs := newFlagSet(e, "extract")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	dest := fs.String("C", "", "extract into this directory (default: the working directory)")
	dryRun := fs.Bool("dry-run", false, "read and verify, but write nothing")
	sparse := fs.Bool("sparse", false, "write all-zero chunks as holes")
	numericIDs := fs.Bool("numeric-ids", false, "restore numeric uid/gid, ignoring names")
	noXAttrs := fs.Bool("noxattrs", false, "do not restore extended attributes")
	noACLs := fs.Bool("noacls", false, "do not restore ACLs")
	noFlags := fs.Bool("noflags", false, "do not restore file flags")
	strip := fs.Int("strip-components", 0, "remove this many leading path components")
	list := fs.Bool("list", false, "print each item as it is extracted")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 1 {
		e.errorf("extract needs an archive name")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false, manifest.OpRead)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	matcher, err := pf.matcher(fs.Args()[1:])
	if err != nil {
		return e.fail(err)
	}

	target := *dest
	if target == "" {
		target, err = os.Getwd()
		if err != nil {
			return e.fail(err)
		}
	}

	status := ExitOK
	opts := archive.ExtractOptions{
		Dest:            target,
		NumericIDs:      *numericIDs,
		Sparse:          *sparse,
		NoXAttrs:        *noXAttrs,
		NoACLs:          *noACLs,
		NoFlags:         *noFlags,
		DryRun:          *dryRun,
		StripComponents: *strip,
		Filter:          func(it *item.Item) bool { return matcher.Match(it.Path) },
		OnError: func(itemPath string, err error) error {
			// One unreadable item does not stop a restore: the rest of the archive is
			// still worth having, and the exit code says something went wrong.
			e.warnf("%s: %v", itemPath, err)
			status = ExitWarning
			return nil
		},
	}
	if *list {
		opts.OnProgress = func(it *item.Item) { fmt.Fprintln(e.Stdout, it.Path) }
	}

	stats, err := a.Extract(opts)
	if err != nil {
		return e.fail(err)
	}
	for _, p := range matcher.UnmatchedIncludePatterns() {
		e.warnf("include pattern %q never matched anything", p.String())
		status = ExitWarning
	}
	if common.verbose {
		fmt.Fprintf(e.Stdout,
			"extracted %d items (%d files, %d dirs, %d symlinks, %d hard links, %d other), %d bytes\n",
			stats.Items, stats.Files, stats.Dirs, stats.Symlinks, stats.Hardlinks, stats.Others, stats.Bytes)
	}
	if stats.SkippedACL > 0 {
		e.warnf("%d ACL(s) could not be restored", stats.SkippedACL)
		status = ExitWarning
	}
	return status
}
