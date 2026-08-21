// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ArchiveRecreater.matcher_add_tagged_dirs in borg's
// src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/patterns"
	"github.com/renesugar/borge/internal/repoobj"
)

// Tag-based exclusion for recreate.
//
// # The same rule, read from a different place
//
// create decides whether a directory is a cache by opening CACHEDIR.TAG on the filesystem.
// recreate has no filesystem to look at - the tree it is filtering exists only as an item
// stream - so it reads the tag file's *stored content* instead. Everything else is the same
// rule, and it has to be: an archive recreated with --exclude-caches should hold what a
// fresh create with --exclude-caches would have held.
//
// # Why it is a separate pass
//
// The decision about a directory is made by a file *inside* it, which the stream may not
// have reached when the directory item goes past. borg solves that by walking the whole
// stream once to collect the tagged directories, turning them into patterns, and only then
// filtering - and so does this.

// AddTaggedDirs scans an archive for tag files and adds the resulting patterns to matcher.
//
// The patterns are borg's, and the order they go in matters: the tag files are added as
// includes *first*, so that with --keep-exclude-tags a tag file wins over the exclusion of
// the directory holding it. Matching stops at the first pattern that hits, in both tools.
func AddTaggedDirs(a *Archive, matcher *patterns.Matcher, excludeCaches bool,
	excludeIfPresent []string, keepExcludeTags bool) error {

	if !excludeCaches && len(excludeIfPresent) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, name := range excludeIfPresent {
		present[name] = true
	}

	var tagFiles, taggedDirs []patterns.Pattern
	err := a.Items(func(it *item.Item) error {
		dir, base := path.Split(it.Path)
		dir = strings.TrimSuffix(dir, "/")

		switch {
		case present[base]:
			// Any file of that name, whatever it holds - borg does not look inside.
		case excludeCaches && base == cacheTagName && it.IsRegular():
			ok, err := hasCacheTagSignature(a, it)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		default:
			return nil
		}

		if keepExcludeTags {
			// The tag file itself is kept, so that a restore of this archive can be
			// excluded again the same way. Its directory is dropped without recursing.
			p, err := patterns.NewPattern("pp", it.Path, false)
			if err != nil {
				return err
			}
			tagFiles = append(tagFiles, p)
			d, err := patterns.NewPattern("fm", dir+"/", false)
			if err != nil {
				return err
			}
			taggedDirs = append(taggedDirs, d)
			return nil
		}
		d, err := patterns.NewPattern("pp", dir, false)
		if err != nil {
			return err
		}
		taggedDirs = append(taggedDirs, d)
		return nil
	})
	if err != nil {
		return err
	}

	for _, p := range tagFiles {
		matcher.Add(p, patterns.CmdInclude)
	}
	for _, p := range taggedDirs {
		matcher.Add(p, patterns.CmdExcludeNoRecurse)
	}
	return nil
}

// hasCacheTagSignature reads the beginning of a stored CACHEDIR.TAG.
//
// Only the first chunk is fetched: the signature is the first 43 bytes of the file, and a
// cache directory's tag file is not something to read in full.
func hasCacheTagSignature(a *Archive, it *item.Item) (bool, error) {
	if !it.ChunksSet || len(it.Chunks) == 0 {
		return false, nil
	}
	c := it.Chunks[0]
	obj, err := a.repo.Get(c.ID)
	if err != nil {
		// A tag file whose chunk is missing is not evidence of a cache directory, and a
		// damaged archive is not a reason to refuse to recreate the rest of it.
		return false, nil
	}
	_, data, err := a.ro.Parse(c.ID, obj, repoobj.TypeFileStream, repoobj.ParseOptions{})
	if err != nil {
		return false, fmt.Errorf("archive: reading %s (%s): %w",
			it.Path, hex.EncodeToString(c.ID), err)
	}
	return strings.HasPrefix(string(data), cacheTagSignature), nil
}
