// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/repo_list_cmd.py and
// repo_info_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/manifest"
)

// timeLayout is how borge prints a timestamp: borg's format_time, in local time.
//
// A backup's times are stored in UTC and shown in the reader's zone, because "when did
// this run" is a question about the reader's day, not about UTC.
const timeLayout = "Mon, 2006-01-02 15:04:05 -0700"

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(timeLayout)
}

// archiveJSON is one row of the machine-readable listing. The field names are borg's, so
// a script written against borg's --json keeps working.
type archiveJSON struct {
	Archive  string `json:"archive"`
	Name     string `json:"name"`
	ID       string `json:"id"`
	Time     string `json:"time"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Comment  string `json:"comment"`
	Tags     string `json:"tags"`
}

func toArchiveJSON(info manifest.Info) archiveJSON {
	return archiveJSON{
		Archive:  info.Name,
		Name:     info.Name,
		ID:       hex.EncodeToString(info.ID),
		Time:     info.Time.Local().Format("2006-01-02T15:04:05.000000-07:00"),
		Hostname: info.Host,
		Username: info.User,
		Comment:  info.Comment,
		Tags:     strings.Join(info.Tags, ","),
	}
}

// listSelectors registers the archive-selection options shared by the commands that take
// them.
type listSelectors struct {
	match   string
	first   int
	last    int
	sortBy  string
	reverse bool
	deleted bool

	// The four relative time filters.
	older  timespanFlag
	newer  timespanFlag
	oldest timespanFlag
	newest timespanFlag
}

// timespanFlag is a relative time marker option: "7d", "12m".
//
// It parses when the option is set, so a bad span is refused before the repository is
// opened - which is where borg refuses it, in the argparse type validator. A typo then
// costs nothing and reports itself, instead of surfacing after a slow open or hiding
// behind an unrelated error about the repository.
//
// It also records that the option was *given*, which is not the same as a non-empty
// value: an explicitly empty span is what "--newer $SPAN" expands to when SPAN is unset,
// borg exits 2 for it, and reading it as "no filter" would list every archive and report
// success. That is the failure that looks most like a correct answer, and a test caught
// it here.
type timespanFlag struct {
	span manifest.Timespan
	set  bool
}

func (t *timespanFlag) String() string {
	if !t.set {
		return ""
	}
	return t.span.String()
}

func (t *timespanFlag) Set(v string) error {
	span, err := manifest.ParseTimespan(v)
	if err != nil {
		return err
	}
	t.span, t.set = span, true
	return nil
}

func (s *listSelectors) register(fs *flagSet) {
	fs.StringVar(&s.match, "a", "", "select archives (name, sh:, re:, aid:, tags:, user:, host:)")
	fs.StringVar(&s.match, "match-archives", "", "select archives")
	fs.IntVar(&s.first, "first", 0, "keep only the first N archives")
	fs.IntVar(&s.last, "last", 0, "keep only the last N archives")
	fs.StringVar(&s.sortBy, "sort-by", "", "comma-separated sort keys (timestamp, name, id, host, user, tags)")
	fs.BoolVar(&s.reverse, "reverse", false, "reverse the order")
	fs.BoolVar(&s.deleted, "deleted", false, "list soft-deleted archives instead")
	fs.Var(&s.older, "older", "only archives older than now minus this span, e.g. 7d or 12m")
	fs.Var(&s.newer, "newer", "only archives newer than now minus this span, e.g. 7d or 12m")
	fs.Var(&s.oldest, "oldest", "only archives within this span of the oldest one, e.g. 7d")
	fs.Var(&s.newest, "newest", "only archives within this span of the newest one, e.g. 7d")
}

// options builds the listing options, substituting placeholders in the selector as borg
// does for --match-archives: "borge delete -a 'sh:{hostname}-*'" is the point of it.
//
// It takes the Env and returns an error for that one reason. The alternative - expanding
// at each call site - is eight places to remember, and the ninth would be the bug.
func (s *listSelectors) options(e *Env) (manifest.ListOptions, error) {
	match, err := e.expand(s.match)
	if err != nil {
		return manifest.ListOptions{}, err
	}
	s.match = match
	return s.rawOptions()
}

func (s *listSelectors) rawOptions() (manifest.ListOptions, error) {
	opts := manifest.ListOptions{
		First:   s.first,
		Last:    s.last,
		Reverse: s.reverse,
		Deleted: s.deleted,
	}
	if s.match != "" {
		opts.Match = []string{s.match}
	}
	if s.sortBy != "" {
		opts.SortBy = strings.Split(s.sortBy, ",")
	}

	// borg makes each pair mutually exclusive in the parser. Giving both would otherwise
	// be read as an empty range or as one silently winning, and neither is anything the
	// user meant.
	if s.older.set && s.newer.set {
		return opts, errors.New("--older and --newer are two ends of the same range; give one")
	}
	if s.oldest.set && s.newest.set {
		return opts, errors.New("--oldest and --newest are two ends of the same range; give one")
	}

	// "now" is taken once for the whole command, so that two filters in one invocation
	// cannot straddle a second boundary and disagree about which archives exist.
	now := time.Now().UTC()
	if s.older.set {
		opts.Older = s.older.span.Offset(now, true)
	}
	if s.newer.set {
		opts.Newer = s.newer.span.Offset(now, true)
	}
	if s.oldest.set {
		opts.Oldest = s.oldest.span
	}
	if s.newest.set {
		opts.Newest = s.newest.span
	}
	return opts, nil
}

// cmdRepoList lists a repository's archives.
func cmdRepoList(e *Env, args []string) int {
	fs := newFlagSet(e, "repo-list")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs)
	// borg's --short prints the archive *ids*, not the names: an id is what uniquely
	// selects an archive, and names are not unique. Printing names here would look
	// friendlier and would be wrong.
	short := fs.Bool("short", false, "print only the archive ids")
	if err := fs.Parse(args); err != nil {
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

	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
	}
	infos, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}

	if common.json {
		out := struct {
			Archives []archiveJSON `json:"archives"`
		}{Archives: []archiveJSON{}}
		for _, info := range infos {
			out.Archives = append(out.Archives, toArchiveJSON(info))
		}
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(out); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}

	status := ExitOK
	for _, info := range infos {
		full := hex.EncodeToString(info.ID)
		if *short {
			fmt.Fprintln(e.Stdout, full)
			continue
		}
		// The column layout is borg's default BORG_REPO_LIST_FORMAT:
		// "{id:.8}  {time}  {archive:<15}  {tags:<10}  {username:<10}  {hostname:<10}  {comment:.40}".
		// Eight hex characters of the id is enough to name an archive and short enough to
		// read; the comment is truncated for the same reason.
		id := full
		if len(id) > 8 {
			id = id[:8]
		}
		comment := info.Comment
		if len(comment) > 40 {
			comment = comment[:40]
		}
		fmt.Fprintf(e.Stdout, "%s  %s  %-15s  %-10s  %-10s  %-10s  %s\n",
			id, formatTime(info.Time), info.Name, strings.Join(info.Tags, ","),
			info.User, info.Host, comment)
		if !info.Exists {
			// A pointer with no readable archive behind it is damage, and a listing that
			// showed it as an ordinary row would hide that.
			e.warnf("%s: %s", id, info.Problem)
			status = ExitWarning
		}
	}
	return status
}

// cmdRepoInfo prints what is known about a repository without unlocking every archive.
func cmdRepoInfo(e *Env, args []string) int {
	fs := newFlagSet(e, "repo-info")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
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

	count, err := o.manifest.Archives.Count()
	if err != nil {
		return e.fail(err)
	}
	deleted, err := o.manifest.Archives.IDs(true)
	if err != nil {
		return e.fail(err)
	}

	if common.json {
		out := map[string]any{
			"repository": map[string]any{
				"id":            o.repo.IDString(),
				"location":      path,
				"version":       o.repo.Version(),
				"archive_count": count,
			},
			"encryption": map[string]any{"mode": o.key.Name()},
			"manifest": map[string]any{
				"id":        hex.EncodeToString(o.manifest.ID),
				"timestamp": o.manifest.Timestamp,
				"version":   o.manifest.Version(),
			},
		}
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(out); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}

	fmt.Fprintf(e.Stdout, "Repository ID: %s\n", o.repo.IDString())
	fmt.Fprintf(e.Stdout, "Location: %s\n", path)
	fmt.Fprintf(e.Stdout, "Repository version: %d\n", o.repo.Version())
	fmt.Fprintf(e.Stdout, "Encryption: %s\n", o.key.Name())
	fmt.Fprintf(e.Stdout, "Manifest version: %d\n", o.manifest.Version())
	if ts, err := o.manifest.LastTimestamp(); err == nil {
		fmt.Fprintf(e.Stdout, "Last modified: %s\n", formatTime(ts))
	}
	fmt.Fprintf(e.Stdout, "Archives: %d\n", count)
	if len(deleted) > 0 {
		fmt.Fprintf(e.Stdout, "Archives (soft-deleted): %d\n", len(deleted))
	}
	return ExitOK
}
