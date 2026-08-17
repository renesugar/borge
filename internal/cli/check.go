// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the verification half of borg's src/borg/archiver/check_cmd.py
// and ArchiveChecker in src/borg/archive.py. The repair half is stage 8.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repoobj"
)

// cmdCheck verifies a repository.
//
// # What the two levels check, and why both are needed
//
// The *repository* check reads every object and confirms it is intact. The *archive*
// check walks every archive and confirms that everything it references is present. Either
// alone leaves a hole: intact objects that no archive can reach are wasted space, and an
// archive referencing a chunk that is not there is a restore that will fail - and neither
// is visible from the other side.
//
// --verify-data additionally re-hashes every chunk's plaintext and compares it against
// its id. Without it the check trusts the envelope's authentication, which detects
// corruption but not a chunk stored under the wrong id in the first place. With it, the
// invariant "id == hash(plaintext)" is re-established for the whole repository, which is
// what makes deduplication trustworthy.
//
// Repair is not implemented: borge reports what is wrong and changes nothing. A check
// that writes is a much more dangerous thing than one that reads, and it belongs with the
// rest of stage 8.
func cmdCheck(e *Env, args []string) int {
	fs := newFlagSet(e, "check")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs)
	verifyData := fs.Bool("verify-data", false, "re-hash every chunk and compare against its id")
	repositoryOnly := fs.Bool("repository-only", false, "check the repository, not the archives")
	archivesOnly := fs.Bool("archives-only", false, "check the archives, not the repository")
	repair := fs.Bool("repair", false, "not implemented; borge check never writes")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *repair {
		e.errorf("borge check --repair is not implemented (docs/PORTING_PLAN.md stage 8); " +
			"use borg check --repair, or run borge check to see what is wrong")
		return ExitError
	}
	if *repositoryOnly && *archivesOnly {
		e.errorf("--repository-only and --archives-only are mutually exclusive")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false, manifest.OpCheck)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	c := &checker{env: e, opened: o, verifyData: *verifyData, verbose: common.verbose}

	if !*archivesOnly {
		if err := c.checkRepository(); err != nil {
			return e.fail(err)
		}
	}
	if !*repositoryOnly {
		if err := c.checkArchives(sel.options()); err != nil {
			return e.fail(err)
		}
	}

	if c.errors == 0 {
		fmt.Fprintf(e.Stdout, "Archive and repository consistency check complete, no problems found.\n")
		if common.verbose {
			fmt.Fprintf(e.Stdout, "checked %d object(s) and %d archive(s), %d chunk reference(s)\n",
				c.objects, c.archives, c.references)
		}
		return ExitOK
	}
	fmt.Fprintf(e.Stdout, "%d error(s) found.\n", c.errors)
	return ExitError
}

type checker struct {
	env        *Env
	opened     *opened
	verifyData bool
	verbose    bool

	objects    int
	archives   int
	references int
	errors     int
}

func (c *checker) problem(format string, args ...any) {
	c.errors++
	fmt.Fprintf(c.env.Stderr, "borge: "+format+"\n", args...)
}

// checkRepository reads every object the chunk index names.
func (c *checker) checkRepository() error {
	chunks, err := c.opened.repo.Chunks()
	if err != nil {
		return err
	}

	// A sorted walk, so two runs report the same problems in the same order - which is
	// what makes "did this get better or worse" answerable.
	var ids [][]byte
	chunks.Iterate(func(id []byte, _ hashindex.Entry) bool {
		ids = append(ids, append([]byte(nil), id...))
		return true
	})
	sort.Slice(ids, func(i, j int) bool { return string(ids[i]) < string(ids[j]) })

	ro := c.opened.manifest.RepoObj()
	if c.verifyData {
		// Verifying data re-establishes id == hash(plaintext), so ask the object layer to
		// assert it rather than trusting the envelope alone.
		if err := ro.SetAssertIDPlace(repoobj.PlaceVerifyData); err != nil {
			return err
		}
	}

	for _, id := range ids {
		obj, err := c.opened.repo.Get(id)
		if err != nil {
			c.problem("chunk %s: %v", hex.EncodeToString(id), err)
			continue
		}
		c.objects++
		if !c.verifyData {
			// Without --verify-data, only the object's framing is checked: reading and
			// decrypting every chunk is the expensive part, and this is the cheap pass.
			if _, _, err := repoobj.ParseHeader(obj); err != nil {
				c.problem("chunk %s: %v", hex.EncodeToString(id), err)
			}
			continue
		}
		if _, _, err := ro.Parse(id, obj, repoobj.TypeDontCare, repoobj.ParseOptions{}); err != nil {
			c.problem("chunk %s: %v", hex.EncodeToString(id), err)
		}
	}
	return nil
}

// checkArchives walks every archive and confirms everything it references exists.
func (c *checker) checkArchives(opts manifest.ListOptions) error {
	infos, err := c.opened.manifest.Archives.List(opts)
	if err != nil {
		return err
	}
	chunks, err := c.opened.repo.Chunks()
	if err != nil {
		return err
	}

	for _, info := range infos {
		c.archives++
		if !info.Exists {
			c.problem("archive %s: %s", hex.EncodeToString(info.ID)[:8], info.Problem)
			continue
		}
		a, err := archive.Open(c.opened.manifest, info.ID)
		if err != nil {
			c.problem("archive %s (%s): %v", hex.EncodeToString(info.ID)[:8], info.Name, err)
			continue
		}

		// The item stream itself: reading it decrypts and authenticates every metadata
		// chunk, so a failure here is reported against the archive rather than as an
		// anonymous chunk error.
		missing := 0
		err = a.Items(func(it *item.Item) error {
			for _, ch := range it.Chunks {
				c.references++
				if _, ok := chunks.Get(ch.ID); !ok {
					if missing < 10 {
						c.problem("archive %s: %s references missing chunk %s",
							info.Name, it.Path, hex.EncodeToString(ch.ID))
					}
					missing++
				}
			}
			return nil
		})
		if err != nil {
			c.problem("archive %s (%s): %v", hex.EncodeToString(info.ID)[:8], info.Name, err)
			continue
		}
		if missing > 10 {
			// Naming every one of a million missing chunks helps nobody; the count does.
			c.problem("archive %s: %d further missing chunk(s) not listed", info.Name, missing-10)
		}
		if c.verbose {
			fmt.Fprintf(c.env.Stdout, "archive %s: ok\n", info.Name)
		}
	}
	return nil
}
