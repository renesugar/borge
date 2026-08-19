// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the command dispatch and common options in borg's
// src/borg/archiver/, principally __init__.py and _common.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package cli is borge's command line: argument parsing, repository opening, and the
// commands themselves.
//
// # Exit codes
//
// borg's convention, and borge keeps it so scripts written for one work with the other:
// 0 success, 1 a warning (the operation completed but something was skipped), 2 an error.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/placeholders"
	"github.com/renesugar/borge/internal/repository"
	"github.com/renesugar/borge/internal/version"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitWarning = 1
	ExitError   = 2
)

// Env is where a command reads and writes, so the whole CLI is testable without touching
// the real process.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	// Getenv resolves environment variables. Nil means os.Getenv.
	Getenv func(string) (string, bool)

	// captureFlags, when set, receives every FlagSet a command builds. It is how
	// "borge completion" enumerates a command's options without keeping a second copy
	// of them; see internal/cli/completion.go.
	captureFlags func(*flag.FlagSet)

	// placeholders are the {now}/{hostname} substitutions, resolved on first use.
	placeholders     placeholders.Values
	placeholdersOnce sync.Once

	// prompted is a passphrase typed at the terminal, kept for the rest of this command
	// so a second unlock does not ask again. Never written anywhere.
	prompted *string
}

func (e *Env) lookup(name string) (string, bool) {
	if e.Getenv != nil {
		return e.Getenv(name)
	}
	return os.LookupEnv(name)
}

// lookupBorg reads BORGE_<name>, falling back to BORG_<name> (docs/PORTING_PLAN.md §0.5).
func (e *Env) lookupBorg(name string) (string, bool) {
	if v, ok := e.lookup("BORGE_" + name); ok {
		return v, true
	}
	return e.lookup("BORG_" + name)
}

func (e *Env) errorf(format string, args ...any) {
	fmt.Fprintf(e.Stderr, "borge: "+format+"\n", args...)
}

func (e *Env) warnf(format string, args ...any) {
	fmt.Fprintf(e.Stderr, "borge: warning: "+format+"\n", args...)
}

// command is one subcommand.
type command struct {
	name    string
	summary string
	// run parses the command's own flags and does the work.
	run func(e *Env, args []string) int
}

// commands is the dispatch table, in the order the help lists them.
//
// It is a list rather than a map so the help has a sensible order: alphabetical would put
// "delete" before "extract", which is not how anybody thinks about a backup tool.
//
// It is a function rather than a package variable because "benchmark crud" measures the
// real commands by running them through Run, so the table reaches a command that reaches
// the table. That is fine at run time and is a cycle Go refuses to order at initialisation.
func commands() []command {
	return []command{
		{"repo-create", "create a new repository", cmdRepoCreate},
		{"repo-delete", "delete a repository and everything in it", cmdRepoDelete},
		{"repo-list", "list the archives in a repository", cmdRepoList},
		{"repo-info", "show information about a repository", cmdRepoInfo},
		{"list", "list the contents of an archive", cmdList},
		{"info", "show information about an archive", cmdInfo},
		{"create", "create an archive from paths", cmdCreate},
		{"extract", "extract an archive", cmdExtract},
		{"diff", "report what changed between two archives", cmdDiff},
		{"export-tar", "write an archive as a tar stream", cmdExportTar},
		{"import-tar", "create an archive from a tar stream", cmdImportTar},
		{"rename", "rename an archive", cmdRename},
		{"tag", "add or remove an archive's tags", cmdTag},
		{"delete", "soft-delete archives", cmdDelete},
		{"undelete", "restore soft-deleted archives", cmdUndelete},
		{"check", "verify a repository and its archives", cmdCheck},
		{"recreate", "rewrite archives: re-chunk, recompress or drop files", cmdRecreate},
		{"prune", "apply a retention policy to the archives", cmdPrune},
		{"compact", "reclaim the space of unreferenced chunks", cmdCompact},
		{"repo-compress", "recompress everything already stored", cmdRepoCompress},
		{"find", "search for paths across archives", cmdFind},
		{"break-lock", "remove the repository's locks", cmdBreakLock},
		{"with-lock", "run a command with the repository lock held", cmdWithLock},
		{"analyze", "report where the repository's space goes", cmdAnalyze},
		{"repo-space", "manage the repository's emergency reserved space", cmdRepoSpace},
		{"key", "manage the repository's keys", cmdKey},
		{"version", "print the client and server versions", cmdVersion},
		{"debug", "low-level repository inspection (dangerous)", cmdDebug},
		{"benchmark", "measure this build's speed", cmdBenchmark},
		{"completion", "print a shell completion script", cmdCompletion},
		{"help", "explain patterns, selectors, placeholders, compression and the environment", cmdHelp},
	}
}

// Run dispatches a command line. It never panics on bad input; every failure becomes an
// exit code and a message on stderr.
func Run(e *Env, args []string) int {
	if len(args) == 0 {
		printUsage(e.Stdout)
		return ExitOK
	}
	name := args[0]
	for _, c := range commands() {
		if c.name == name {
			return c.run(e, args[1:])
		}
	}
	e.errorf("unknown command %q", name)
	printUsage(e.Stderr)
	return ExitError
}

// Commands lists the implemented subcommands, for the top-level help.
func Commands() []string {
	var out []string
	for _, c := range commands() {
		out = append(out, fmt.Sprintf("  %-12s %s", c.name, c.summary))
	}
	return out
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: borge <command> [options]\n\ncommands:\n%s\n",
		strings.Join(Commands(), "\n"))
}

// ---------------------------------------------------------------- common options

// commonFlags are the options every repository command takes.
type commonFlags struct {
	repo    string
	verbose bool
	json    bool
}

func (c *commonFlags) register(fs *flagSet) {
	fs.StringVar(&c.repo, "r", "", "repository path (or BORGE_REPO / BORG_REPO)")
	fs.StringVar(&c.repo, "repo", "", "repository path (or BORGE_REPO / BORG_REPO)")
	fs.BoolVar(&c.verbose, "v", false, "more output")
	fs.BoolVar(&c.verbose, "verbose", false, "more output")
}

// registerJSON adds --json, and is separate from register because borg does not put
// --json on every repository command.
//
// borge registered it in register() until 2026-08-18, so nineteen commands accepted
// --json and six acted on it: "borge check --json" ran a check and printed text, having
// silently accepted an option that promised otherwise. borg has --json on eight commands
// (create, import-tar, prune, info, repo-info, repo-list, version, analyze) and rejects
// it everywhere else, which is the more useful answer: a frontend learns straight away
// that the command has no JSON form.
//
// Not to be confused with --json-lines, which borg has on list, find, diff and
// "benchmark crud". "borg list --json" is accepted, but only because argparse expands
// unambiguous prefixes; borg's own help does not offer it. See docs/DIVERGENCES.md #35.
//
// The help text is per command because borg's is: on create and import-tar it says
// "implies --stats", which is behaviour a caller needs to know about and not a rewording.
func (c *commonFlags) registerJSON(fs *flagSet, help string) {
	if help == "" {
		help = "print JSON instead of text"
	}
	fs.BoolVar(&c.json, "json", false, help)
}

// newFlagSet builds a flag set that reports usage the way borg does and does not exit the
// process on a parse error.
func newFlagSet(e *Env, name string) *flagSet {
	inner := flag.NewFlagSet("borge "+name, flag.ContinueOnError)
	inner.SetOutput(e.Stderr)
	if e.captureFlags != nil {
		e.captureFlags(inner)
	}
	return &flagSet{FlagSet: inner}
}

// newPassthroughFlagSet is newFlagSet for a command whose trailing arguments are another
// program's; see the note in args.go.
func newPassthroughFlagSet(e *Env, name string) *flagSet {
	fs := newFlagSet(e, name)
	fs.passthrough = true
	return fs
}

// resolveRepo works out which repository to act on. The answer is always absolute.
//
// borg resolves a relative "-r sub/repo", or a relative BORG_REPO, against the working
// directory, and reports the absolute form as the repository's Location. borge refused a
// relative path outright until 2026-08-18; see docs/DIVERGENCES.md #22.
//
// The resolution happens here rather than in the store because the store's rule - a
// backend is rooted at an absolute path - is worth keeping. A backend rooted at something
// that depends on the process working directory is one nothing else can reason about, and
// borg resolves at argument parsing too.
//
// No "~" expansion, because borg does none: "-r ~/backups" means a directory literally
// named "~" in both tools, and expanding it here would be borge inventing behaviour.
func (e *Env) resolveRepo(given string) (string, error) {
	path := given
	if path == "" {
		if v, ok := e.lookupBorg("REPO"); ok && v != "" {
			path = v
		}
	}
	if path == "" {
		return "", errors.New("no repository given; pass -r or set BORGE_REPO")
	}
	// A repository path may carry placeholders, as borg's may: "-r /backups/{hostname}"
	// is how one BORGE_REPO setting serves a fleet. Expanded before the path is made
	// absolute, so "-r {hostname}/repo" resolves the way a reader expects.
	expanded, err := e.expand(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

// placeholderValues is the substitution set for this process, taken once.
//
// Once, because every placeholder in one command has to agree with the others: a name
// built from {now} and {unixtime} must not straddle a second boundary, and two archives
// created by one command must not be filed under two different days.
func (e *Env) placeholderValues() placeholders.Values {
	e.placeholdersOnce.Do(func() {
		e.placeholders = placeholders.Default(version.Version)
	})
	return e.placeholders
}

// expand substitutes placeholders in a user-supplied string.
//
// An unknown placeholder is an error rather than a literal; see the package comment. The
// affected arguments are the ones borg substitutes: archive names, comments, archive
// selectors and the repository path.
func (e *Env) expand(text string) (string, error) {
	if !strings.ContainsAny(text, "{}") {
		return text, nil // the overwhelmingly common case, and it cannot fail
	}
	return e.placeholderValues().Expand(text)
}

// expandAll is expand over a slice, for the repeatable options.
func (e *Env) expandAll(texts []string) ([]string, error) {
	out := make([]string, len(texts))
	for i, t := range texts {
		var err error
		if out[i], err = e.expand(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// passphrase is the passphrase to try first: one already typed at a prompt during this
// command, otherwise the environment's.
//
// It does not prompt. Prompting happens on failure instead - see passphrase.go for why -
// so this stays a plain lookup that every caller can make without deciding anything.
//
// borg also reads a file descriptor and runs a command to obtain a passphrase; borge does
// not do either yet.
func (e *Env) passphrase() string {
	if e.prompted != nil {
		return *e.prompted
	}
	if v, ok := e.lookupBorg("PASSPHRASE"); ok {
		return v
	}
	return ""
}

// opened is a repository, its key and its manifest, which is what every command needs.
type opened struct {
	repo     *repository.Repository
	key      key.Key
	manifest *manifest.Manifest
}

func (o *opened) Close() error {
	if o.repo == nil {
		return nil
	}
	return o.repo.Close()
}

// openRepo opens a repository, unlocks it and loads the manifest.
func (e *Env) openRepo(path string, exclusive bool, ops ...manifest.Operation) (*opened, error) {
	repo, err := repository.Open(path, repository.Options{Exclusive: exclusive})
	if err != nil {
		return nil, err
	}
	k, _, err := e.unlockWithPrompt(repo)
	if err != nil {
		repo.Close()
		return nil, err
	}
	m, err := manifest.Load(repo, k, ops...)
	if err != nil {
		repo.Close()
		return nil, err
	}
	return &opened{repo: repo, key: k, manifest: m}, nil
}

// fail prints an error and returns the error exit code, so a command body can end with
// "return e.fail(err)".
func (e *Env) fail(err error) int {
	e.errorf("%v", err)
	return ExitError
}
