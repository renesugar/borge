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
	"strings"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
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
var commands = []command{
	{"repo-create", "create a new repository", cmdRepoCreate},
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
	{"version", "print the client and server versions", cmdVersion},
	{"debug", "low-level repository inspection (dangerous)", cmdDebug},
}

// Run dispatches a command line. It never panics on bad input; every failure becomes an
// exit code and a message on stderr.
func Run(e *Env, args []string) int {
	if len(args) == 0 {
		printUsage(e.Stdout)
		return ExitOK
	}
	name := args[0]
	for _, c := range commands {
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
	for _, c := range commands {
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

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.repo, "r", "", "repository path (or BORGE_REPO / BORG_REPO)")
	fs.StringVar(&c.repo, "repo", "", "repository path (or BORGE_REPO / BORG_REPO)")
	fs.BoolVar(&c.verbose, "v", false, "more output")
	fs.BoolVar(&c.verbose, "verbose", false, "more output")
	fs.BoolVar(&c.json, "json", false, "print JSON instead of text")
}

// newFlagSet builds a flag set that reports usage the way borg does and does not exit the
// process on a parse error.
func newFlagSet(e *Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("borge "+name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	return fs
}

// resolveRepo works out which repository to act on.
func (e *Env) resolveRepo(given string) (string, error) {
	if given != "" {
		return given, nil
	}
	if v, ok := e.lookupBorg("REPO"); ok && v != "" {
		return v, nil
	}
	return "", errors.New("no repository given; pass -r or set BORGE_REPO")
}

// passphrase resolves the repository passphrase.
//
// Only the environment is consulted. borg also prompts, reads a file descriptor and runs
// a command; those arrive with the write path, because a read-only command that hangs
// waiting for a passphrase in a script is worse than one that says where to put it.
func (e *Env) passphrase() string {
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
	k, _, err := repo.Unlock(e.passphrase())
	if err != nil {
		repo.Close()
		if errors.Is(err, key.ErrPassphraseWrong) {
			return nil, fmt.Errorf("%w (set BORGE_PASSPHRASE or BORG_PASSPHRASE)", err)
		}
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
