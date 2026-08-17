// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of FilesCacheMixin in borg's src/borg/cache.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package cache is the files cache: the record of which files were already backed up and
// with which chunks, so an unchanged file is not read again.
//
// # What it is for, and what it risks
//
// Without it, every backup reads every byte. With it, a backup of a mostly-unchanged tree
// reads almost nothing. That is the difference between an hourly backup being practical
// and not.
//
// The risk is the other side of the same coin: if the cache says a file is unchanged and
// it is not, the backup silently stores the *old* contents. Everything awkward in this
// package exists because of that - the cache modes, the newest-timestamp exclusion, the
// entry ages. None of it is an optimization; it is what keeps the optimization honest.
package cache

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/msgpackx"
)

// Mode says which properties of a file are compared to decide it is unchanged.
//
// The letters are borg's --files-cache values:
//
//	d  disabled: never consult the cache
//	r  rechunk: update the cache but never trust it
//	s  size
//	i  inode number
//	c  ctime
//	m  mtime
//
// # Why ctime and mtime are exclusive
//
// mtime changes when a file's *contents* change. ctime changes for that and also when its
// metadata does - and, crucially, it cannot be set by a program, while mtime can. A
// program that rewrites a file and restores its old mtime is invisible to an mtime-only
// cache; ctime catches it. So ctime is the safe default, and mtime exists for the cases
// where ctime is useless: a restored tree, or a filesystem that does not keep it
// meaningfully.
//
// Using both would be redundant, since ctime changes whenever mtime does; borg asserts
// against it and so does borge.
type Mode struct {
	Disabled bool
	Rechunk  bool
	Size     bool
	Inode    bool
	CTime    bool
	MTime    bool
}

// DefaultMode is borg's FILES_CACHE_MODE_UI_DEFAULT_POSIX: ctime, size and inode.
func DefaultMode() Mode {
	return Mode{CTime: true, Size: true, Inode: true}
}

// DisabledMode never consults or updates the cache.
func DisabledMode() Mode { return Mode{Disabled: true} }

// ParseMode reads a --files-cache specification such as "ctime,size,inode".
func ParseMode(spec string) (Mode, error) {
	var m Mode
	for _, part := range strings.Split(spec, ",") {
		switch strings.TrimSpace(part) {
		case "disabled", "d":
			m.Disabled = true
		case "rechunk", "r":
			m.Rechunk = true
		case "size", "s":
			m.Size = true
		case "inode", "i":
			m.Inode = true
		case "ctime", "c":
			m.CTime = true
		case "mtime", "m":
			m.MTime = true
		default:
			return Mode{}, fmt.Errorf("cache: %q is not a files-cache property "+
				"(want some of: ctime, mtime, size, inode, rechunk, disabled)", part)
		}
	}
	if m.CTime && m.MTime {
		return Mode{}, fmt.Errorf("cache: ctime and mtime cannot both be used; " +
			"ctime already changes whenever mtime does")
	}
	if !m.Disabled && !m.CTime && !m.MTime {
		return Mode{}, fmt.Errorf("cache: a files cache needs either ctime or mtime; " +
			"comparing only size and inode would treat an edited file of the same size as unchanged")
	}
	return m, nil
}

// String renders a mode the way it is written on the command line.
func (m Mode) String() string {
	var parts []string
	for _, p := range []struct {
		on   bool
		name string
	}{
		{m.Disabled, "disabled"}, {m.Rechunk, "rechunk"}, {m.CTime, "ctime"},
		{m.MTime, "mtime"}, {m.Size, "size"}, {m.Inode, "inode"},
	} {
		if p.on {
			parts = append(parts, p.name)
		}
	}
	if len(parts) == 0 {
		return "disabled"
	}
	return strings.Join(parts, ",")
}

// Entry is what the cache remembers about one file.
type Entry struct {
	// Age is how many backups ago this entry was last confirmed. Zero means this run.
	Age int
	// Inode, Size, CTime and MTime are the properties a Mode may compare. Times are
	// nanoseconds since the epoch.
	Inode int64
	Size  int64
	CTime int64
	MTime int64
	// Chunks is the file's content, which is what the cache exists to avoid recomputing.
	Chunks []item.ChunkListEntry
}

// timeDiffers2 is borg's TIME_DIFFERS2_NS: three seconds, allowing for clocks that
// disagree and filesystems whose timestamps are coarse.
const timeDiffers2 = 3_000_000_000

// defaultTTL is how many backups an entry survives without being seen again.
//
// Keeping entries for a few runs is what makes an alternating backup pattern work - back
// up A, then B, then A again - without the second A run re-reading everything.
const defaultTTL = 2

// FilesCache is the per-archive record of already-backed-up files.
//
// It is keyed by a hash of the path rather than the path itself: a tree of a million
// files would otherwise hold a million path strings in memory, and the hash is fixed-width.
type FilesCache struct {
	mode Mode

	entries map[string]*Entry

	// newestCMTime is the newest ctime or mtime seen this run, and newestPaths the
	// entries carrying it. Those entries are dropped when saving - see Save.
	newestCMTime int64
	newestPaths  map[string]bool

	// startBackup is when this run began, in nanoseconds. An entry whose file was touched
	// after that is not trustworthy either.
	startBackup int64

	// ttl is how many runs an unseen entry survives.
	ttl int

	// hits and misses count what the cache achieved, for reporting.
	hits, misses int
}

// New returns an empty files cache.
func New(mode Mode, startBackupNS int64) *FilesCache {
	ttl := defaultTTL
	if v, ok := lookupEnv("FILES_CACHE_TTL"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ttl = n
		}
	}
	return &FilesCache{
		mode:        mode,
		entries:     map[string]*Entry{},
		newestPaths: map[string]bool{},
		startBackup: startBackupNS,
		ttl:         ttl,
	}
}

// Mode reports the comparison mode.
func (c *FilesCache) Mode() Mode { return c.mode }

// Len is how many entries the cache holds.
func (c *FilesCache) Len() int { return len(c.entries) }

// Stats reports how many lookups were served from the cache and how many were not.
func (c *FilesCache) Stats() (hits, misses int) { return c.hits, c.misses }

// PathKey is the cache key for a path.
//
// The hash is unkeyed: the files cache is local, it never enters the repository, and a
// keyed hash here would only mean the cache could not be used without unlocking the key.
func PathKey(path string) string {
	sum := sha256Sum([]byte(path))
	return string(sum[:16])
}

// Lookup reports what the cache knows about a file.
//
// known is true when there is an entry for the path at all; chunks is non-nil only when
// the file is also unchanged. The two are separate because a *known but changed* file is
// still worth reporting - it is the difference between "new file" and "modified file" in
// a listing.
func (c *FilesCache) Lookup(key string, st FileInfo) (known bool, chunks []item.ChunkListEntry) {
	if c.mode.Disabled || c.mode.Rechunk {
		// Rechunk deliberately reads everything: it is how a user re-chunks an existing
		// repository with new parameters. The cache is still *updated*, so the run after
		// it is fast again.
		c.misses++
		return false, nil
	}
	entry, ok := c.entries[key]
	if !ok {
		c.misses++
		return false, nil
	}

	changed := false
	switch {
	case c.mode.Size && entry.Size != st.Size:
		changed = true
	case c.mode.Inode && entry.Inode != st.Inode:
		changed = true
	case c.mode.CTime && entry.CTime != st.CTime:
		changed = true
	case c.mode.MTime && entry.MTime != st.MTime:
		changed = true
	}
	if changed {
		c.misses++
		return true, nil
	}

	// Refresh the values that were *not* compared. A user who switched off the inode
	// comparison because a copy changed every inode wants to be able to switch it back on
	// later without re-reading the whole tree; that only works if the cache learns the
	// new values while it is ignoring them.
	entry.Inode = st.Inode
	entry.CTime = st.CTime
	entry.MTime = st.MTime
	entry.Age = 0
	c.recordNewest(key, st.CTime, st.MTime)

	c.hits++
	return true, entry.Chunks
}

// Memorize records a file that was just read and chunked.
func (c *FilesCache) Memorize(key string, st FileInfo, chunks []item.ChunkListEntry) {
	if c.mode.Disabled {
		return
	}
	c.entries[key] = &Entry{
		Age:    0,
		Inode:  st.Inode,
		Size:   st.Size,
		CTime:  st.CTime,
		MTime:  st.MTime,
		Chunks: append([]item.ChunkListEntry(nil), chunks...),
	}
	c.recordNewest(key, st.CTime, st.MTime)
}

// Load adds an entry read from elsewhere - a previous archive, or the cache file.
func (c *FilesCache) Load(key string, e *Entry) {
	if c.mode.Disabled {
		return
	}
	c.entries[key] = e
	c.recordNewest(key, e.CTime, e.MTime)
}

// recordNewest tracks the newest ctime/mtime seen and which entries carry it.
func (c *FilesCache) recordNewest(key string, ctime, mtime int64) {
	for _, t := range []int64{ctime, mtime} {
		switch {
		case t > c.newestCMTime:
			c.newestCMTime = t
			c.newestPaths = map[string]bool{key: true}
		case t == c.newestCMTime:
			c.newestPaths[key] = true
		}
	}
}

// FileInfo is the subset of stat() the cache compares.
type FileInfo struct {
	Size  int64
	Inode int64
	CTime int64
	MTime int64
}

// ---------------------------------------------------------------- persistence

// cacheFileMagic identifies a borge files cache and its version.
//
// It is borge's own format, not borg's: docs/DIVERGENCES.md §4 records that the caches are
// not shared, so there is nothing to interoperate with and no reason to carry borg's
// index-compression scheme, which only makes sense while its ChunkIndex is in memory.
const cacheFileMagic = "BORGE_FILES_CACHE_1"

// Save writes the cache, dropping the entries that must not be trusted next time.
//
// # The two exclusions, and why they are not optional
//
// An entry is only kept if its file's newest timestamp is comfortably *older* than both:
//
//   - the newest timestamp seen this run, and
//   - the moment this backup started, less three seconds.
//
// Consider a file modified twice within one timestamp tick, once before borge read it and
// once after. Its recorded ctime and mtime are identical either way, so next run the cache
// would call it unchanged and the second modification would never be backed up - silently,
// and permanently. Filesystems with one-second granularity make this easy to hit; a
// snapshot taken mid-run makes it easier.
//
// Dropping those entries costs one re-read of a handful of files. Keeping them costs data.
func (c *FilesCache) Save(path string) error {
	if c.mode.Disabled {
		return nil
	}
	const maxTime = int64(^uint64(0) >> 1)

	discardAfter := maxTime
	if c.newestCMTime != 0 {
		discardAfter = c.newestCMTime
	}
	if c.startBackup != 0 && c.startBackup-timeDiffers2 < discardAfter {
		discardAfter = c.startBackup - timeDiffers2
	}

	var kept, raceDiscarded, ageDiscarded int
	out := msgpackx.NewMap()
	list := make([]any, 0, len(c.entries))

	for key, e := range c.entries {
		keep := false
		if e.Age == 0 {
			newest := e.CTime
			if e.MTime > newest {
				newest = e.MTime
			}
			if newest < discardAfter {
				keep = true
			} else {
				raceDiscarded++
			}
		} else if e.Age < c.ttl {
			keep = true
		} else {
			ageDiscarded++
		}
		if !keep {
			continue
		}
		list = append(list, encodeEntry(key, e))
		kept++
	}
	out.Set("magic", cacheFileMagic)
	out.Set("entries", list)

	packed, err := msgpackx.Marshal(out)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, packed); err != nil {
		return err
	}
	_ = raceDiscarded
	_ = ageDiscarded
	return nil
}

// SaveReport is what Save discarded, for a verbose run.
type SaveReport struct{ Kept, RaceDiscarded, AgeDiscarded int }

func encodeEntry(key string, e *Entry) *msgpackx.Map {
	m := msgpackx.NewMap()
	m.Set("k", []byte(key))
	m.Set("a", int64(e.Age+1)) // saved entries are one backup older when read back
	m.Set("i", e.Inode)
	m.Set("s", e.Size)
	m.Set("c", e.CTime)
	m.Set("m", e.MTime)
	chunks := make([]any, 0, len(e.Chunks))
	for _, c := range e.Chunks {
		pair := []any{c.ID, c.Size}
		chunks = append(chunks, pair)
	}
	m.Set("h", chunks)
	return m
}

// Read loads a cache file. A missing or unreadable file is not an error: the cache is an
// optimization, and starting without one only costs time.
func Read(path string, mode Mode, startBackupNS int64) (*FilesCache, error) {
	c := New(mode, startBackupNS)
	if mode.Disabled {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, nil
	}
	v, err := msgpackx.Unmarshal(data)
	if err != nil {
		return c, nil
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return c, nil
	}
	if magic, _ := m.Get("magic"); magic != cacheFileMagic {
		// A cache written by another version: ignore it rather than guessing at its
		// shape. Guessing wrong here means trusting stale chunk lists.
		return c, nil
	}
	raw, _ := m.Get("entries")
	list, ok := raw.([]any)
	if !ok {
		return c, nil
	}
	for _, e := range list {
		em, ok := e.(*msgpackx.Map)
		if !ok {
			continue
		}
		key, entry, ok := decodeEntry(em)
		if !ok {
			continue
		}
		c.entries[key] = entry
	}
	return c, nil
}

func decodeEntry(m *msgpackx.Map) (string, *Entry, bool) {
	getInt := func(name string) (int64, bool) {
		v, ok := m.Get(name)
		if !ok {
			return 0, false
		}
		n, ok := v.(int64)
		return n, ok
	}
	kv, ok := m.Get("k")
	if !ok {
		return "", nil, false
	}
	key, ok := kv.([]byte)
	if !ok {
		return "", nil, false
	}
	age, ok1 := getInt("a")
	inode, ok2 := getInt("i")
	size, ok3 := getInt("s")
	ctime, ok4 := getInt("c")
	mtime, ok5 := getInt("m")
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return "", nil, false
	}
	e := &Entry{Age: int(age), Inode: inode, Size: size, CTime: ctime, MTime: mtime}

	hv, _ := m.Get("h")
	chunks, ok := hv.([]any)
	if !ok {
		return "", nil, false
	}
	for _, c := range chunks {
		pair, ok := c.([]any)
		if !ok || len(pair) != 2 {
			return "", nil, false
		}
		id, ok1 := pair[0].([]byte)
		sz, ok2 := pair[1].(int64)
		if !ok1 || !ok2 {
			return "", nil, false
		}
		e.Chunks = append(e.Chunks, item.ChunkListEntry{ID: id, Size: sz})
	}
	return string(key), e, true
}

// ---------------------------------------------------------------- locations

// Dir is where borge keeps a repository's cache.
//
// Not borg's directory: docs/DIVERGENCES.md §4. A borge bug must not be able to damage a
// working borg installation, and nothing is lost by keeping them apart - the files cache
// is rebuilt from the repository when it is absent.
func Dir(repoID []byte) (string, error) {
	if v, ok := lookupEnv("CACHE_DIR"); ok && v != "" {
		return filepath.Join(v, hex.EncodeToString(repoID)), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache: cannot determine the cache directory: %w", err)
	}
	return filepath.Join(base, "borge", hex.EncodeToString(repoID)), nil
}

// FileName is the cache file for one archive name.
//
// It is per archive *name*, because that is what makes a series of backups - "daily",
// taken every night - reuse the previous run's cache. Two different archive names in one
// repository are different working sets and would only evict each other.
func FileName(archiveName string) string {
	sum := sha256Sum([]byte(archiveName))
	return "files." + hex.EncodeToString(sum[:8])
}

// Path is the full path of an archive's cache file.
func Path(repoID []byte, archiveName string) (string, error) {
	dir, err := Dir(repoID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName(archiveName)), nil
}

// lookupEnv reads BORGE_<name>, falling back to BORG_<name>.
func lookupEnv(name string) (string, bool) {
	if v, ok := os.LookupEnv("BORGE_" + name); ok {
		return v, true
	}
	return os.LookupEnv("BORG_" + name)
}
