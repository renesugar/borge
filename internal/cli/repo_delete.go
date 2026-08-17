// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the repo-delete command in borg's
// src/borg/archiver/repo_delete_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/renesugar/borge/internal/cache"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// cmdRepoDelete deletes a whole repository.
//
// # Why it asks
//
// Every other destructive command in borge is recoverable for a while: delete and prune
// are soft, and a compaction has to run before anything is really gone. This one is not.
// It removes the archives, the chunks and the keys together, and if the key was a repokey
// then the only copy of it goes with them.
//
// So without --force it prints what is about to be destroyed and requires the word YES on
// standard input. BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING=YES answers for a script, which is
// borg's variable and borg's spelling.
//
// # What it refuses to guess
//
// A path that is not a repository is an error, not an empty success. "borge repo-delete
// -r /home/me" must not be a way to lose a home directory to a typo, so the directory is
// opened as a repository first and only its known contents are removed.
func cmdRepoDelete(e *Env, args []string) int {
	fs := newFlagSet(e, "repo-delete")
	var common commonFlags
	common.register(fs)
	dryRun := fs.Bool("dry-run", false, "say what would be deleted, delete nothing")
	fs.BoolVar(dryRun, "n", false, "say what would be deleted, delete nothing")
	list := fs.Bool("list", false, "list the archives that would be destroyed")
	force := fs.Bool("force", false, "do not ask for confirmation")
	cacheOnly := fs.Bool("cache-only", false, "delete only this client's cache, not the repository")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}

	if *cacheOnly {
		return deleteCacheOnly(e, path, *dryRun)
	}

	// Opened before anything is printed, so a path that is not a repository fails here
	// rather than after the user has read a confirmation prompt about it.
	repo, err := repository.Open(path, repository.Options{Exclusive: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	summary, err := describeRepoForDeletion(e, repo, *list)
	if err != nil {
		return e.fail(err)
	}
	// Located now, while the repository can still be asked for its id.
	cacheDir, err := repoCacheDir(repo)
	if err != nil {
		e.warnf("cannot locate this repository's cache: %v", err)
	}

	if !*force {
		fmt.Fprint(e.Stdout, summary)
		ok, err := e.confirmDestruction()
		if err != nil {
			return e.fail(err)
		}
		if !ok {
			fmt.Fprintln(e.Stdout, "aborting; nothing was deleted")
			return ExitError
		}
	} else if *dryRun || common.verbose {
		fmt.Fprint(e.Stdout, summary)
	}

	if *dryRun {
		fmt.Fprintln(e.Stdout, "dry run: the repository was not deleted")
		return ExitOK
	}

	// Closed before the files go, so the lock is released and the chunk index is not
	// written back into a directory that is being removed.
	if err := repo.Close(); err != nil {
		return e.fail(err)
	}
	leftover, err := destroyRepository(path)
	if err != nil {
		return e.fail(err)
	}
	if err := deleteRepoCache(cacheDir); err != nil {
		e.warnf("the repository is gone but its cache could not be removed: %v", err)
	}
	fmt.Fprintf(e.Stdout, "repository %s deleted\n", path)
	if len(leftover) > 0 {
		// The repository is gone, which is what was asked, so this is not a failure. It
		// is worth saying out loud: somebody put files in the repository directory, and
		// they are still there.
		e.warnf("%s was not removed because it still holds %s",
			path, strings.Join(leftover, ", "))
		return ExitWarning
	}
	return ExitOK
}

// describeRepoForDeletion builds the text shown before the prompt.
func describeRepoForDeletion(e *Env, repo *repository.Repository, list bool) (string, error) {
	var b strings.Builder
	fmt.Fprintln(&b, "You have asked to DELETE this repository completely, including "+
		"every archive in it:")
	fmt.Fprintln(&b, strings.Repeat("-", 72))
	fmt.Fprintf(&b, "Repository ID: %s\n", repo.IDString())
	fmt.Fprintf(&b, "Location:      %s\n", repo.Path())

	// The manifest may be unreadable - a damaged repository is one of the reasons to
	// delete one - so its absence is reported rather than treated as a failure.
	k, _, err := repo.Unlock(e.passphrase())
	if err != nil {
		fmt.Fprintf(&b, "Archives:      cannot be counted (%v)\n", err)
		fmt.Fprintln(&b, strings.Repeat("-", 72))
		return b.String(), nil
	}
	m, err := manifest.Load(repo, k)
	if err != nil {
		fmt.Fprintf(&b, "Archives:      cannot be counted (%v)\n", err)
		fmt.Fprintln(&b, strings.Repeat("-", 72))
		return b.String(), nil
	}
	infos, err := m.Archives.List(manifest.ListOptions{})
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Archives:      %d\n", len(infos))
	if list {
		for _, info := range infos {
			fmt.Fprintf(&b, "  %-36s %s\n", info.Name, info.TimeString)
		}
	} else if len(infos) > 0 {
		fmt.Fprintln(&b, "               (use --list to see them)")
	}
	fmt.Fprintln(&b, strings.Repeat("-", 72))
	return b.String(), nil
}

// confirmDestruction requires the word YES.
//
// The environment variable short-circuits it, as borg's does, because an unattended
// caller has no terminal to answer from. Anything other than YES is a no: a prompt whose
// default is destruction is not a prompt.
func (e *Env) confirmDestruction() (bool, error) {
	if v, ok := e.lookupBorg("DELETE_I_KNOW_WHAT_I_AM_DOING"); ok {
		return strings.TrimSpace(v) == "YES", nil
	}
	if e.Stdin == nil {
		return false, errors.New("no terminal to confirm on; pass --force, or set " +
			"BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING=YES")
	}
	fmt.Fprint(e.Stdout, "Type 'YES' if you understand this and want to continue: ")
	line, err := bufio.NewReader(e.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	return strings.TrimSpace(line) == "YES", nil
}

// repoNamespaces are the directories a borg repository owns. Nothing else in the
// repository directory belongs to borge, and nothing else is removed.
var repoNamespaces = []string{"archives", "cache", "config", "index", "keys", "locks", "packs"}

// destroyRepository removes a repository's own directories, and the directory itself only
// if nothing else was in it.
//
// It returns what was left behind. A repository created inside a directory holding other
// files must not take those files with it - "borge repo-delete -r ~/backups" where the
// user also keeps notes in that directory should cost them the backups they asked to
// delete and nothing else. borg's store.destroy() removes the directory outright, which
// is the divergence recorded as DIVERGENCES #18.
func destroyRepository(path string) ([]string, error) {
	for _, ns := range repoNamespaces {
		if err := os.RemoveAll(filepath.Join(path, ns)); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names, nil
	}
	return nil, os.Remove(path)
}

// deleteRepoCache removes this client's cache for a repository.
//
// The cache is keyed by repository id, so it has to be located before the repository is
// destroyed - afterwards there is nothing left to read the id from.
func deleteRepoCache(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// repoCacheDir is where this client keeps its files cache for a repository.
func repoCacheDir(repo *repository.Repository) (string, error) {
	return cache.Dir(repo.ID())
}

func deleteCacheOnly(e *Env, path string, dryRun bool) int {
	repo, err := repository.Open(path, repository.Options{NoLock: true})
	if err != nil {
		return e.fail(err)
	}
	dir, err := repoCacheDir(repo)
	repo.Close()
	if err != nil {
		return e.fail(err)
	}
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintln(e.Stdout, "there is no cache for this repository")
		return ExitOK
	}
	if dryRun {
		fmt.Fprintf(e.Stdout, "dry run: would delete the cache at %s\n", dir)
		return ExitOK
	}
	if err := os.RemoveAll(dir); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stdout, "deleted the cache at %s\n", dir)
	return ExitOK
}
