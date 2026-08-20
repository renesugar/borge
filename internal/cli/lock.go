// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the break-lock and with-lock commands in borg's
// src/borg/archiver/lock_cmds.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/renesugar/borge/internal/repository"
)

// cmdBreakLock removes the repository's locks.
//
// # Why this is not automatic
//
// A lock is removed on its own once it goes stale, after a safety period of about half an
// hour. That period exists because the alternative - assuming a lock whose holder has not
// checked in recently is dead - would break a legitimate long operation on a slow link.
//
// break-lock skips the wait. That is exactly the right thing after a crash, and exactly
// the wrong thing while another borge is still running: the two would then write to the
// repository at once. The command cannot tell the difference, so it says what it removed
// and who held it, and leaves the judgement where it belongs.
func cmdBreakLock(e *Env, args []string) int {
	fs := newFlagSet(e, "break-lock")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}

	// Opened without taking a lock: taking one is the thing that is currently failing.
	repo, err := repository.Open(path, repository.Options{NoLock: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	held, err := repo.ListLocks()
	if err != nil {
		return e.fail(err)
	}
	if len(held) == 0 {
		fmt.Fprintln(e.Stderr, "no locks are held on this repository")
		return ExitOK
	}

	// borg breaks locks unconditionally, and so does this: refusing on a heuristic would
	// block the one case the command exists for. What it does add is saying *what* is
	// being broken. A lock that has gone stale was going to be removed anyway and
	// breaking it is free; a lock refreshed a minute ago means somebody is very likely
	// still running, and that is worth an exit code the caller can notice.
	status := ExitOK
	for _, h := range held {
		age := time.Since(h.Time).Round(time.Second)
		if h.Stale {
			fmt.Fprintf(e.Stderr, "breaking stale %s lock locks/%s: %s pid %d, last refreshed %s ago\n",
				lockKind(h.Exclusive), h.Key, h.Host, h.PID, age)
			continue
		}
		e.warnf("locks/%s is a live %s lock: %s pid %d refreshed it %s ago, so that client "+
			"is probably still running", h.Key, lockKind(h.Exclusive), h.Host, h.PID, age)
		status = ExitWarning
	}

	if err := repo.BreakLock(); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stderr, "%d lock(s) broken\n", len(held))
	return status
}

func lockKind(exclusive bool) string {
	if exclusive {
		return "exclusive"
	}
	return "shared"
}

// cmdWithLock runs a command with the repository lock held.
//
// # What it is for
//
// Copying a repository with rsync, or snapshotting the filesystem under it, is only safe
// if nothing is writing to it meanwhile. Holding borge's own lock is how that is arranged
// without inventing a second mechanism the rest of borge would not respect.
//
// The lock has to be refreshed while the command runs, or a long copy would have its own
// lock declared stale underneath it - so a goroutine touches it periodically until the
// command exits.
//
// # The exit code is the command's
//
// borge is a wrapper here, and a wrapper that swallows the exit code of what it wrapped
// makes "borge with-lock ... && something-else" silently wrong.
func cmdWithLock(e *Env, args []string) int {
	fs := newPassthroughFlagSet(e, "with-lock")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 1 {
		e.errorf("with-lock needs a command to run")
		return ExitError
	}
	name, rest := fs.Arg(0), fs.Args()[1:]

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}

	// Exclusive: the point of the command is that nothing else touches the repository.
	repo, err := repository.Open(path, repository.Options{Exclusive: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	// Refresh while the command runs. Without this a copy taking longer than the stale
	// period would have its own lock removed by the next borge to come along.
	//
	// The wait on the way out is not optional: closing the channel only stops the *next*
	// iteration, so without it a refresh already in progress would still be touching the
	// lock while the deferred Close released it.
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := repo.RefreshLock(); err != nil {
					// Reported, not fatal: the command is already running and killing it
					// here would be a worse outcome than a lock that may go stale.
					e.warnf("could not refresh the repository lock: %v", err)
				}
			}
		}
	}()
	defer func() {
		close(done)
		<-stopped
	}()

	cmd := exec.Command(name, rest...)
	cmd.Stdin = e.Stdin
	cmd.Stdout = e.Stdout
	cmd.Stderr = e.Stderr
	cmd.Env = append(os.Environ(), "BORG_REPO="+path, "BORGE_REPO="+path)

	err = cmd.Run()
	if err == nil {
		return ExitOK
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	e.errorf("could not run %s: %v", name, err)
	return ExitError
}
