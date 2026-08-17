// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the repo-space command in borg's
// src/borg/archiver/repo_space_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

	"github.com/renesugar/borge/internal/repository"
	"github.com/renesugar/borge/internal/store"
)

// reserveBlockSize is how much one space-reserve object holds.
//
// borg's value, and it has to stay borg's value: the objects are named
// "space-reserve.<n>" with no record of their size, so the two tools have to agree on how
// many objects a given reservation needs or "borge repo-space --reserve 1G" after "borg
// repo-space --reserve 1G" would leave a mixture.
const reserveBlockSize = 64 << 20 // 64 MiB

// reservePrefix names the objects. They live in config/ because that namespace is not
// swept by compaction and not touched by anything that walks chunks.
const reservePrefix = "space-reserve."

// cmdRepoSpace manages the repository's emergency reserve.
//
// # The problem it solves
//
// borg cannot work on a full disk, and the operations that free space are exactly the ones
// it cannot run there: taking a lock is a write, and prune, delete and compact all need
// one. A repository that fills its filesystem is therefore wedged - the tool that would
// fix it cannot start.
//
// The reserve is a deliberately wasteful pile of incompressible bytes held back for that
// moment. Freeing it does not fix anything by itself; it buys back enough room for borge
// to run, so that prune and compact can then free the real space.
//
// # Why random bytes
//
// The objects are filled from the system random source rather than with zeroes, because a
// filesystem with compression or deduplication would store a megabyte of zeroes in almost
// no space at all - and a reserve that occupies no disk reserves nothing.
func cmdRepoSpace(e *Env, args []string) int {
	fs := newFlagSet(e, "repo-space")
	var common commonFlags
	common.register(fs)
	reserve := fs.String("reserve", "", "reserve about this much space, e.g. 1G")
	free := fs.Bool("free", false, "free all reserved space")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *reserve != "" && *free {
		e.errorf("--reserve and --free are opposites; give one or the other")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	// No lock: the whole point is to work in the disk-full state where locking fails.
	// borg does the same, and documents it.
	repo, err := repository.Open(path, repository.Options{NoLock: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()
	s := repo.Store()

	switch {
	case *reserve != "":
		want, err := parseFileSize(*reserve)
		if err != nil {
			return e.fail(err)
		}
		return reserveSpace(e, s, want)
	case *free:
		return freeSpace(e, s)
	default:
		return reportSpace(e, s)
	}
}

// reserveObjects lists the reserve objects and their total size.
func reserveObjects(s *store.Store) (names []string, total int64, err error) {
	err = s.List("config", false, func(info store.ItemInfo) bool {
		if strings.HasPrefix(info.Name, reservePrefix) {
			names = append(names, info.Name)
			total += info.Size
		}
		return true
	})
	sort.Strings(names)
	return names, total, err
}

func reserveSpace(e *Env, s *store.Store, want int64) int {
	// Rounded up: a reservation smaller than asked for would be a reserve that runs out
	// before it was meant to, which is the one failure mode that matters here.
	count := int((want + reserveBlockSize - 1) / reserveBlockSize)
	if count == 0 && want > 0 {
		count = 1
	}

	existing, had, err := reserveObjects(s)
	if err != nil {
		return e.fail(err)
	}
	// Replacing rather than adding: "--reserve 1G" means "let there be 1G", not "one more
	// gigabyte each time I run it".
	for _, name := range existing {
		if err := s.Delete("config/"+name, false); err != nil {
			return e.fail(err)
		}
	}

	buf := make([]byte, reserveBlockSize)
	var written int64
	for i := 0; i < count; i++ {
		if _, err := rand.Read(buf); err != nil {
			return e.fail(err)
		}
		if err := s.Store(fmt.Sprintf("config/%s%d", reservePrefix, i), buf); err != nil {
			// A failure part-way through leaves fewer objects than asked for. Saying how
			// many landed matters: the user has to know whether to retry or to free first.
			e.errorf("wrote %s of the reserve before failing: %v", e.fmtBytes(written), err)
			return ExitError
		}
		written += int64(len(buf))
	}

	if had > 0 {
		fmt.Fprintf(e.Stdout, "replaced %s of previously reserved space\n", e.fmtBytes(had))
	}
	fmt.Fprintf(e.Stdout, "there is %s of reserved space in this repository now\n", e.fmtBytes(written))
	return ExitOK
}

func freeSpace(e *Env, s *store.Store) int {
	names, total, err := reserveObjects(s)
	if err != nil {
		return e.fail(err)
	}
	if len(names) == 0 {
		fmt.Fprintln(e.Stdout, "there was no reserved space to free")
		return ExitOK
	}
	for _, name := range names {
		if err := s.Delete("config/"+name, false); err != nil {
			return e.fail(err)
		}
	}
	fmt.Fprintf(e.Stdout, "freed %s in the repository\n", e.fmtBytes(total))
	// Freeing the reserve does not free anything a backup would notice. Saying what to do
	// next is the difference between a working escape hatch and an obscure one.
	fmt.Fprintln(e.Stdout, "now run 'borge prune' or 'borge delete', then 'borge compact', "+
		"to free real space - and reserve again afterwards, for next time")
	return ExitOK
}

func reportSpace(e *Env, s *store.Store) int {
	_, total, err := reserveObjects(s)
	if err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stdout, "there is %s of reserved space in this repository\n", e.fmtBytes(total))
	if total > 0 {
		fmt.Fprintln(e.Stdout, "to change the amount, use --free first, then --reserve with the new size")
	}
	return ExitOK
}
