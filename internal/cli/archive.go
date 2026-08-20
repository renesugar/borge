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
	"fmt"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/cache"
	"github.com/renesugar/borge/internal/formatter"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/patterns"
)

// patternFlags are the include/exclude options shared by create, list, extract and the
// rest.
//
// # One list, in the order the user wrote them
//
// The first matching pattern decides, so "--exclude X --pattern +X" and the reverse mean
// different things - and the four options interleave: an --exclude written before a
// --pattern has to be tried before it. borge kept a slice per option and processed all the
// --patterns, then all the --excludes, so the order the user wrote was thrown away and
// "--exclude 'sh:**/drop' --pattern '+sh:**/drop'" archived the file borg leaves out. See
// docs/DIVERGENCES.md #26.
//
// Go's flag calls Set in command-line order across every option, so one shared slice with
// a tag per entry is all it takes to keep it. (Argument permutation preserves the relative
// order of the options it moves, so this survives an option written after the paths; see
// args.go.)
type patternFlags struct {
	specs []patternSpec
}

// multiFlag collects a repeated option in order. Still used by the options where order is
// all a caller needs - "borge tag --add a --add b" - as against the pattern options, where
// the order has to be kept *across* four different flags and patternSpec does that.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// patternSpecKind says which option a spec came from.
type patternSpecKind int

const (
	specExclude patternSpecKind = iota
	specExcludeFrom
	specPattern
	specPatternsFrom
)

type patternSpec struct {
	kind  patternSpecKind
	value string
}

// patternSpecFlag appends to the shared list, tagging each entry with its option.
type patternSpecFlag struct {
	specs *[]patternSpec
	kind  patternSpecKind
}

func (f patternSpecFlag) String() string { return "" }

func (f patternSpecFlag) Set(v string) error {
	*f.specs = append(*f.specs, patternSpec{kind: f.kind, value: v})
	return nil
}

func (p *patternFlags) register(fs *flagSet) {
	excl := patternSpecFlag{&p.specs, specExclude}
	fs.Var(excl, "e", "exclude paths matching this pattern (repeatable)")
	fs.Var(excl, "exclude", "exclude paths matching this pattern (repeatable)")
	fs.Var(patternSpecFlag{&p.specs, specExcludeFrom}, "exclude-from",
		"read exclude patterns from a file (repeatable)")
	fs.Var(patternSpecFlag{&p.specs, specPattern}, "pattern",
		"an include/exclude pattern with a leading +, - or ! (repeatable)")
	fs.Var(patternSpecFlag{&p.specs, specPatternsFrom}, "patterns-from",
		"read include/exclude patterns from a file (repeatable)")
}

// any reports whether any pattern option was given. A command that would otherwise match
// everything uses it to tell "no filter asked for" from "a filter that matches all".
func (p *patternFlags) any() bool { return len(p.specs) > 0 }

// matcher builds the pattern matcher from the flags and the positional paths, walking the
// specs in the order they were written.
func (p *patternFlags) matcher(paths []string) (*patterns.Matcher, error) {
	m := patterns.NewMatcher(true)

	for _, spec := range p.specs {
		switch spec.kind {
		case specPattern:
			e, err := patterns.ParseInclExclCommand(spec.value, patterns.StyleShellPath)
			if err != nil {
				return nil, err
			}
			// An "R" root carries no pattern, and a "P" style on the command line
			// changes nothing - borg ignores it there too, and only honours it inside a
			// patterns file, where the following lines are what it applies to.
			if e.Pattern != nil {
				m.Add(e.Pattern, e.Cmd)
			}

		case specPatternsFrom:
			entries, _, err := loadPatternFile(spec.value)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				m.Add(e.Pattern, e.Cmd)
			}

		case specExclude:
			// --exclude defaults to fnmatch and does not recurse, which is borg's shape.
			pat, err := patterns.ParsePattern(spec.value, patterns.StyleFnmatch, false)
			if err != nil {
				return nil, err
			}
			m.Add(pat, patterns.CmdExcludeNoRecurse)

		case specExcludeFrom:
			f, err := os.Open(spec.value)
			if err != nil {
				return nil, err
			}
			pats, err := patterns.LoadExcludeFile(f)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", spec.value, err)
			}
			for _, pat := range pats {
				m.Add(pat, patterns.CmdExcludeNoRecurse)
			}
		}
	}

	if len(paths) > 0 {
		if err := m.AddIncludePaths(paths); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// roots are the "R PATH" recursion roots: extra paths to back up, named in a patterns file
// or in a --pattern option rather than on the command line.
//
// borge parsed them and threw them away, so a patterns file whose only root was an R line
// made borge refuse a command borg runs. See docs/DIVERGENCES.md #25.
//
// Only "create" uses these, as in borg: for every other command a pattern file describes
// what to select from an archive, and a root has nothing to select.
func (p *patternFlags) roots() ([]string, error) {
	var out []string
	for _, spec := range p.specs {
		switch spec.kind {
		case specPattern:
			e, err := patterns.ParseInclExclCommand(spec.value, patterns.StyleShellPath)
			if err != nil {
				return nil, err
			}
			if e.Cmd == patterns.CmdRootPath {
				out = append(out, e.Value)
			}
		case specPatternsFrom:
			_, roots, err := loadPatternFile(spec.value)
			if err != nil {
				return nil, err
			}
			out = append(out, roots...)
		}
	}
	return out, nil
}

func loadPatternFile(path string) ([]patterns.FileEntry, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	entries, roots, err := patterns.LoadPatternFile(f, patterns.StyleShellPath)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, roots, nil
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
	format := fs.String("format", "", "output format, e.g. '{mode} {path}{NL}'")
	sortBy := fs.String("sort-by", "", "sort by comma-separated fields, '>' for descending, e.g. '>size,path'")
	depth := fs.Int("depth", 0, "list only paths at or above this directory depth")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 1 {
		e.errorf("list needs an archive name")
		return ExitError
	}
	// Validated here, with the format, and for the same reason: both are cheap to check
	// and expensive to discover late. An explicitly empty spec is an error, as it is in
	// borg - see flagSet.wasSet.
	sorting := fs.wasSet("sort-by")
	if sorting {
		if _, err := validateSortSpec(*sortBy, itemSortKeys); err != nil {
			return e.fail(err)
		}
	}
	limitDepth := fs.wasSet("depth")
	// Validated before the archive is opened, as borg does: a bad key found on the ten
	// thousandth item has already printed nine thousand lines.
	listFormat, _ := e.lookupBorg("LIST_FORMAT")
	template := itemFormat(*format, *short, listFormat,
		"{mode} {user:6} {group:6} {size:8} {mtime} {path}{extra}{NL}")
	if err := checkItemFormat(template); err != nil {
		return e.fail(err)
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

	// borg's item_filter: the patterns, and then the depth. borg counts the separators in
	// the stored path, so "src/a.txt" is depth 1 and "--depth 0" lists nothing of it - the
	// depth is of the path as archived, not relative to whatever was passed on the command
	// line.
	keep := func(it *item.Item) bool {
		if !matcher.Match(it.Path) {
			return false
		}
		return !limitDepth || strings.Count(it.Path, "/") <= *depth
	}

	enc := json.NewEncoder(e.Stdout)

	// Without --sort-by the items are written as they stream, which is what lets a listing
	// of a huge archive start printing at once. Sorting has to read them all first, and
	// borg's help says so in as many words.
	emit := func(it *item.Item) error {
		if *jsonLines {
			data, err := itemJSONData(it, template, a.Info.Name, hex.EncodeToString(a.ID))
			if err != nil {
				return err
			}
			return enc.Encode(data)
		}
		line, err := formatter.Format(template, itemValues(it, a.Info.Name, hex.EncodeToString(a.ID)))
		if err != nil {
			return err
		}
		_, werr := fmt.Fprint(e.Stdout, line)
		return werr
	}

	if !sorting {
		err = a.Items(func(it *item.Item) error {
			if !keep(it) {
				return nil
			}
			return emit(it)
		})
		if err != nil {
			return e.fail(err)
		}
		return ExitOK
	}

	var items []*item.Item
	err = a.Items(func(it *item.Item) error {
		if keep(it) {
			items = append(items, it)
		}
		return nil
	})
	if err != nil {
		return e.fail(err)
	}
	sortBySpec(items, *sortBy, itemSortKey)
	for _, it := range items {
		if err := emit(it); err != nil {
			return e.fail(err)
		}
	}
	return ExitOK
}

// cmdInfo prints the metadata of every archive the filters select.
//
// borg's info takes the whole archive-filter group and no positional at all: it describes
// a *set*, and with no filter that set is the repository. borge described exactly one
// archive and refused to run without a selector, so eight of borg's options were missing
// here and "borge info" answered a different question from "borg info".
//
// The positional archive name is kept as a convenience borge already had; borg has no such
// positional, so it is an addition rather than a difference in a shared spelling.
func cmdInfo(e *Env, args []string) int {
	fs := newFlagSet(e, "info")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	common.registerJSON(fs, "")
	sel.register(fs, selectorExtras{deleted: true, reverse: true})
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if sel.match == "" && fs.NArg() > 0 {
		sel.match = fs.Arg(0)
	}
	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
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

	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}

	archives := make([]*archive.Archive, 0, len(infos))
	for _, info := range infos {
		a, err := archive.Open(o.manifest, info.ID)
		if err != nil {
			return e.fail(err)
		}
		archives = append(archives, a)
	}

	if common.json {
		list := make([]map[string]any, 0, len(archives))
		for _, a := range archives {
			list = append(list, infoArchiveJSON(a))
		}
		cacheDir, err := cache.Dir(o.repo.ID())
		if err != nil {
			return e.fail(err)
		}
		repoBlock, encBlock := o.envelope(path)
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(map[string]any{
			"archives":   list,
			"cache":      map[string]any{"path": cacheDir},
			"encryption": encBlock,
			"repository": repoBlock,
		}); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}

	for _, a := range archives {
		printArchiveInfo(e, a)
	}
	return ExitOK
}

// printArchiveInfo writes one archive's block of the text report.
func printArchiveInfo(e *Env, a *archive.Archive) {
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
}

// cmdExtract restores an archive.
func cmdExtract(e *Env, args []string) int {
	fs := newFlagSet(e, "extract")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	// Named -C to match tar and rsync, and it is worth knowing that borg spells its own
	// -C differently: on create, import-tar, recreate, repo-compress and transfer, borg's
	// -C is --compression. borg's extract has no -C at all, so nothing is shadowed, but a
	// borg habit typing "-C zstd,3" here would name a destination directory rather than a
	// compression. The help says so rather than leaving it to be discovered.
	dest := fs.String("C", "",
		"extract into this directory (borge only; borg's -C elsewhere means --compression)")
	dryRun := fs.Bool("dry-run", false, "read and verify, but write nothing")
	fs.BoolVar(dryRun, "n", false, "read and verify, but write nothing")
	toStdout := fs.Bool("stdout", false, "write every extracted file's contents to stdout")
	keepGoing := fs.Bool("continue", false,
		"skip items an interrupted extraction already wrote correctly")
	stats := fs.Bool("stats", false, "print the repository's I/O statistics afterwards")
	fs.BoolVar(stats, "s", false, "print the repository's I/O statistics afterwards")
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
		Continue:        *keepGoing,
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
	if *toStdout {
		// The contents go to stdout and nothing goes to the filesystem, so the destination
		// is not even looked at. borg treats this as a kind of dry run for everything
		// except the bytes.
		opts.Stdout = e.Stdout
	}
	if *list {
		// On stderr, where borg puts it: "borg extract --list" writes its listing to
		// stderr and leaves stdout for --stdout's file contents. borge wrote to stdout,
		// so "borge extract --stdout --list f" interleaved the names into the data.
		opts.OnProgress = func(it *item.Item) { fmt.Fprintln(e.Stderr, it.Path) }
	}

	extracted, err := a.Extract(opts)
	if err != nil {
		return e.fail(err)
	}
	if *stats {
		// borg's --stats on extract is not about the archive: it prints what the
		// *repository* did - calls, time and volume per store method. It goes to stderr
		// with every other report, and after the extraction rather than during it.
		fmt.Fprint(e.Stderr, formatStoreStats(o.repo.Store().Stats(), e.sizeUnits()))
	}
	for _, p := range matcher.UnmatchedIncludePatterns() {
		e.warnf("include pattern %q never matched anything", p.String())
		status = ExitWarning
	}
	if common.verbose {
		fmt.Fprintf(e.Stderr,
			"extracted %d items (%d files, %d dirs, %d symlinks, %d hard links, %d other), %d bytes\n",
			extracted.Items, extracted.Files, extracted.Dirs, extracted.Symlinks,
			extracted.Hardlinks, extracted.Others, extracted.Bytes)
	}
	if extracted.SkippedACL > 0 {
		e.warnf("%d ACL(s) could not be restored", extracted.SkippedACL)
		status = ExitWarning
	}
	return status
}
