// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the filesystem walk and item construction in borg's
// src/borg/archive.py (FilesystemObjectProcessors) and src/borg/archiver/create_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

//go:build linux

package archive

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/renesugar/borge/internal/cache"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/patterns"
)

// CreateOptions control what a backup includes and how.
type CreateOptions struct {
	// Paths are the roots to back up.
	Paths []string

	// Matcher decides which paths are included. Nil includes everything.
	Matcher *patterns.Matcher

	// OneFileSystem stops the walk at a mount point.
	OneFileSystem bool
	// NumericIDs stores only numeric uid/gid, omitting the names.
	NumericIDs bool
	// NoXAttrs, NoACLs and NoFlags leave those attributes out of the archive.
	NoXAttrs bool
	NoACLs   bool
	NoFlags  bool
	// ReadSpecial reads the contents of fifos and devices instead of recording them as
	// special files. Dangerous by nature - reading a fifo can block forever - so it is
	// off unless asked for.
	ReadSpecial bool

	// ExcludeCaches skips any directory holding a CACHEDIR.TAG with the standard
	// signature (https://bford.info/cachedir/).
	ExcludeCaches bool
	// ExcludeIfPresent names files or directories whose presence excludes the directory
	// holding them.
	ExcludeIfPresent []string
	// KeepExcludeTags archives the excluded directory itself and the tag files that
	// excluded it, so a restore can be excluded again the same way. Without it, nothing
	// from a tagged directory is stored - not even its entry.
	KeepExcludeTags bool

	// DryRun walks and decides but stores nothing: no file is read, no chunk is written,
	// no item is added. It is how a user checks an exclude pattern before trusting it,
	// so what it reports through OnItem has to be exactly what a real run would store.
	DryRun bool

	// Files is the files cache, or nil to read every file. It is consulted before a file
	// is opened and updated after it is chunked.
	Files *cache.FilesCache

	// OnItem is called for each item as it is archived.
	OnItem func(status byte, path string)
	// OnError is called for a per-path failure. Returning an error aborts; returning nil
	// continues. Nil means "abort on the first error".
	OnError func(path string, err error) error
}

// CreateStats is what a backup did, beyond the chunk accounting in Stats.
type CreateStats struct {
	Stats
	Errors  int
	Skipped int
	// Unchanged counts files the files cache spared from being read.
	Unchanged int
}

// Create walks the given paths and writes every matching object into the archive.
//
// # Path form
//
// A path is stored as it was typed, cleaned: "/home/alice" becomes "home/alice",
// "./home/alice" and "home/alice/" both become "home/alice", and "." stays ".". Stored
// paths are always relative and always use "/", which is what makes an archive restorable
// somewhere other than where it came from and why extraction refuses a stored path
// containing "..".
//
// borge used to resolve every path to an absolute one first, so "borge create A home/me"
// run in /srv/work stored "srv/work/home/me/..." where borg stores "home/me/...". Same
// command, same tree, a different archive - and the difference only shows at restore
// time. See docs/DIVERGENCES.md #21.
//
// # The slashdot hack
//
// A "/./" in the middle of a path says where the stored path should start, the way rsync's
// does: "/a/b/./c/d" reads from /a/b/c/d and stores it as "c/d". The whole path is used to
// reach the filesystem; only what follows the dot is archived. It is the way to back up
// /srv/www/site and have it restore as "site" rather than "srv/www/site". See
// docs/DIVERGENCES.md #24.
func (b *Builder) Create(opts CreateOptions) (*CreateStats, error) {
	if len(opts.Paths) == 0 {
		return nil, errors.New("archive: no paths to back up")
	}
	w := &walker{
		builder:   b,
		opts:      opts,
		stats:     &CreateStats{},
		hardlinks: map[hardlinkKey][]item.ChunkListEntry{},
		users:     map[uint32]string{},
		groups:    map[uint32]string{},
	}
	for _, root := range opts.Paths {
		// An empty path would clean to "." and quietly archive the working directory,
		// which is never what an empty argument meant. borg rejects it outright, before
		// anything is written, and so does this.
		if root == "" {
			return w.stats, errors.New("archive: an empty string is not a path")
		}
		// Taken from the path as typed: cleaning removes the "." element, and with it
		// the instruction.
		w.strip = stripPrefix(root)
		if err := w.walk(filepath.Clean(root), 0); err != nil {
			return w.stats, err
		}
	}
	if !opts.DryRun {
		// A dry run has its own counts, taken from stat rather than from chunking;
		// the builder's are empty because nothing was stored, and copying them over
		// would report a backup of nothing.
		w.stats.Stats = b.stats
	}
	return w.stats, nil
}

// hardlinkKey identifies an inode.
type hardlinkKey struct{ dev, ino uint64 }

type walker struct {
	builder *Builder
	opts    CreateOptions
	stats   *CreateStats

	// hardlinks remembers the chunk list of each inode already archived, so the second
	// and later links store an item with the same hlid and no content of their own.
	hardlinks map[hardlinkKey][]item.ChunkListEntry
	// rootDev is the device of the first root, for --one-file-system.
	rootDev uint64
	haveDev bool

	// strip is the slashdot prefix of the root being walked, with a trailing "/", or ""
	// when this root carried no "/./". It is per-root: two paths on one command line may
	// disagree about where their stored paths start.
	strip string

	// users and groups cache id-to-name lookups, which otherwise cost a syscall or an
	// NSS round trip per file.
	users  map[uint32]string
	groups map[uint32]string
}

func (w *walker) fail(path string, err error) error {
	w.stats.Errors++
	if w.opts.OnError == nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return w.opts.OnError(path, err)
}

// archivedPath turns the path being walked into the form stored in the archive.
//
// This is borg's remove_dotdot_prefixes (helpers/fs.py): every leading slash goes, then
// every leading "../", and a path left as "" or ".." becomes ".". The walk itself keeps
// each path cleaned - filepath.Join cleans, as borg's normpath(join(...)) does - so there
// is nothing else left to normalise here.
//
// Dropping the "../" rather than refusing it is borg's choice and worth stating: an
// archive of "../sibling" stores "sibling", so what comes back out is a tree the user can
// place anywhere, not one that climbs out of wherever it is extracted.
func archivedPath(p string) string {
	s := strings.TrimLeft(filepath.ToSlash(p), "/")
	for strings.HasPrefix(s, "../") {
		s = strings.TrimPrefix(s, "../")
	}
	if s == "" || s == ".." {
		return "."
	}
	return s
}

// cacheTagName and cacheTagSignature are the CACHEDIR.TAG protocol
// (https://bford.info/cachedir/). borg reads exactly as many bytes as the signature is
// long and compares them, so a file that merely starts with the signature counts - which
// is what the specification asks for, since the rest of the line is free text.
const (
	cacheTagName      = "CACHEDIR.TAG"
	cacheTagSignature = "Signature: 8a477f597d28d172789f06886806bc55"
)

// tagsExcluding returns the names of the tag files that exclude this directory, in borg's
// order: CACHEDIR.TAG first, then each --exclude-if-present name that is there.
//
// An empty result means the directory is not tagged. The names are returned rather than a
// bool because --keep-exclude-tags has to archive exactly the files that did the
// excluding.
func (w *walker) tagsExcluding(dir string) ([]string, error) {
	if !w.opts.ExcludeCaches && len(w.opts.ExcludeIfPresent) == 0 {
		return nil, nil
	}
	var tags []string
	if w.opts.ExcludeCaches && isCacheDir(dir) {
		tags = append(tags, cacheTagName)
	}
	for _, name := range w.opts.ExcludeIfPresent {
		// Lstat, not Stat: a dangling symlink named as the tag is still present, and borg
		// uses os.stat which follows - but a directory is as good as a file here, and
		// either way the question is whether the name exists.
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			tags = append(tags, name)
		}
	}
	return tags, nil
}

// isCacheDir reports whether the directory holds a CACHEDIR.TAG with the signature.
//
// Any error - missing, unreadable, a directory of that name - means "not a cache
// directory", as borg's except-and-return-False does. A backup that refused to run because
// something was unreadable here would be trading a whole backup for one exclusion.
func isCacheDir(dir string) bool {
	f, err := os.Open(filepath.Join(dir, cacheTagName))
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(cacheTagSignature))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return string(buf) == cacheTagSignature
}

// stripPrefix reads the slashdot hack out of a path as the user typed it, returning the
// part to remove from the front of every stored path, with a trailing "/", or "" for none.
//
// This is borg's get_strip_prefix (helpers/fs.py), including its two edges: only the
// *first* "/./" counts, so "/a/./b/./c" stores "b/c"; and a "/./" at position zero does
// not count, so "/./x" is an ordinary path. A trailing "/." is not the hack either - the
// string has no "/./" in it - which is why "/a/b/." stores the whole path while "/a/b/./"
// stores ".".
func stripPrefix(given string) string {
	pos := strings.Index(given, "/./")
	if pos <= 0 {
		return ""
	}
	return filepath.Clean(given[:pos]) + "/"
}

// storedPath turns a walked path into the one to archive, applying the slashdot prefix.
// The second result is false when this level is above the dot and gets no item at all.
func (w *walker) storedPath(p string) (string, bool) {
	if w.strip == "" {
		return archivedPath(p), true
	}
	switch {
	case p+"/" == w.strip:
		// This is the directory the dot points at: it is the archive's root.
		return ".", true
	case strings.HasPrefix(w.strip, p+"/"):
		// Still above the dot, so there is nothing to store for this level - borg
		// yields no item here. A walk that starts at the cleaned root never reaches
		// this, since that root is always at or below the dot; it is here because it is
		// borg's third case and leaving it out would be a silent difference if a future
		// caller ever did walk from higher up.
		return "", false
	default:
		return archivedPath(strings.TrimPrefix(p, w.strip)), true
	}
}

func (w *walker) walk(abs string, depth int) error {
	var st unix.Stat_t
	if err := unix.Lstat(abs, &st); err != nil {
		return w.fail(abs, err)
	}

	if w.opts.OneFileSystem {
		if !w.haveDev {
			w.rootDev, w.haveDev = st.Dev, true
		} else if st.Dev != w.rootDev && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			w.stats.Skipped++
			return nil
		}
	}

	// Patterns are matched against the path as walked, not as stored. Without the
	// slashdot hack those are the same string; with it they are not, and borg matches the
	// walked one - an --exclude is written against the filesystem the user is looking at.
	included := true
	if w.opts.Matcher != nil {
		included = w.opts.Matcher.Match(archivedPath(abs))
	}
	stored, storable := w.storedPath(abs)

	// Tag-based exclusion is decided before the directory is stored, because a tagged
	// directory is not stored at all unless --keep-exclude-tags asks for it. Checking
	// after would archive the entry and then decline to recurse, which is a different
	// archive.
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if isDir {
		tags, err := w.tagsExcluding(abs)
		if err != nil {
			return err
		}
		if len(tags) > 0 {
			w.stats.Skipped++
			w.report('-', stored)
			if !included || !storable || !w.opts.KeepExcludeTags {
				// Nothing from here, and no recursion either way: borg returns at this
				// point whether or not it kept the tags.
				return nil
			}
			// The directory itself, then the tag files that excluded it - so a restore
			// can be excluded again the same way.
			if err := w.archive(abs, stored, &st); err != nil {
				return err
			}
			for _, tag := range tags {
				if err := w.walk(filepath.Join(abs, tag), depth+1); err != nil {
					return err
				}
			}
			return nil
		}
	}

	switch {
	case !included || !storable:
		// borg prints these too, and for a dry run they are the whole point: a listing
		// that only showed what would be kept could not confirm that an --exclude did
		// anything.
		w.stats.Skipped++
		w.report('-', stored)
	case w.opts.DryRun:
		// Nothing is read and nothing is stored, but the counting is real so that
		// --stats means something.
		if st.Mode&unix.S_IFMT == unix.S_IFREG {
			w.stats.Stats.NFiles++
			w.stats.Stats.OriginalSize += st.Size
		}
		w.report('+', stored)
	default:
		if err := w.archive(abs, stored, &st); err != nil {
			return err
		}
	}

	// Descend into a directory unless the matcher said not to. An excluded directory is
	// still descended into by default, so an include pattern *inside* it can be found;
	// only the no-recurse exclude form stops the walk.
	if !isDir {
		return nil
	}
	if !included && w.opts.Matcher != nil && !w.opts.Matcher.RecurseDir() {
		return nil
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return w.fail(abs, err)
	}
	// Sorted, so an archive of the same tree is reproducible and two runs can be
	// compared. Readdir order is not defined and differs between filesystems.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		if err := w.walk(filepath.Join(abs, name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

// archive builds and stores the item for one filesystem object.
func (w *walker) archive(abs, stored string, st *unix.Stat_t) error {
	it := &item.Item{Path: stored}
	w.fillMetadata(it, abs, st)

	status := byte('A')
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		status = 'd'

	case unix.S_IFLNK:
		target, err := os.Readlink(abs)
		if err != nil {
			return w.fail(abs, err)
		}
		it.Target = &target
		status = 's'
		// A symlink can be hard-linked too, and then it needs an hlid like any other
		// multiply-linked inode.
		w.markHardlink(it, st)

	case unix.S_IFREG:
		chunks, st2, err := w.fileChunks(abs, stored, st)
		if err != nil {
			return w.fail(abs, err)
		}
		it.Chunks = chunks
		it.ChunksSet = true
		// The hlid is what makes a restore recreate the hard link rather than two
		// independent files with the same contents. Sharing the chunk list saves space;
		// the hlid is what preserves the *relationship*, and the two are separate.
		w.markHardlink(it, st)
		status = st2

	case unix.S_IFIFO, unix.S_IFCHR, unix.S_IFBLK, unix.S_IFSOCK:
		if st.Mode&unix.S_IFMT == unix.S_IFSOCK {
			// A socket has no content and cannot be recreated meaningfully. borg skips
			// them, and so does borge - archiving one would restore something that is not
			// the same object.
			w.stats.Skipped++
			return nil
		}
		if w.opts.ReadSpecial {
			chunks, _, err := w.fileChunks(abs, stored, st)
			if err != nil {
				return w.fail(abs, err)
			}
			it.Chunks = chunks
			it.ChunksSet = true
			// Reading a special file turns it into a regular one on restore, which is what
			// --read-special is for: the point is to capture what flows through it.
			mode := int64(st.Mode&0o7777) | item.SIFREG
			it.Mode = &mode
		} else {
			rdev := int64(st.Rdev)
			it.RDev = &rdev
			w.markHardlink(it, st)
		}
		status = 'i'

	default:
		w.stats.Skipped++
		return nil
	}

	if err := w.builder.AddItem(it); err != nil {
		return w.fail(abs, err)
	}
	w.report(status, stored)
	return nil
}

// report tells the caller what happened to one path. borg's status characters: "A" added,
// "d" directory, "s" symlink, "i" special file, "h" a further hard link, "U" unchanged,
// "-" excluded, and "+" for anything a dry run would have stored.
func (w *walker) report(status byte, stored string) {
	if w.opts.OnItem != nil {
		w.opts.OnItem(status, stored)
	}
}

// fileChunks reads and chunks a regular file, reusing the chunk list of an inode already
// archived.
//
// The second link to an inode stores no content at all: it shares the first one's chunk
// list and its hlid. That is not only a size saving - it is what makes a restore
// recreate the hard link rather than two independent files.
func (w *walker) fileChunks(abs, stored string, st *unix.Stat_t) ([]item.ChunkListEntry, byte, error) {
	hlKey := hardlinkKey{dev: st.Dev, ino: st.Ino}
	if st.Nlink > 1 {
		if chunks, ok := w.hardlinks[hlKey]; ok {
			return chunks, 'h', nil
		}
	}

	// The files cache: if it says this file is unchanged, its chunk list is reused and
	// the file is never opened. That is the whole point of an incremental backup, and it
	// is also the one place borge can silently store the wrong contents - see the cache
	// package for what keeps it honest.
	status := byte('A')
	var cacheKey string
	if w.opts.Files != nil {
		cacheKey = cache.PathKey(stored)
		known, chunks := w.opts.Files.Lookup(cacheKey, cache.FileInfo{
			Size:  st.Size,
			Inode: int64(st.Ino),
			CTime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
			MTime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		})
		if known && chunks != nil {
			w.stats.Unchanged++
			// The chunks still count towards the archive's size: the archive contains the
			// file whether or not this run had to read it.
			for _, c := range chunks {
				w.builder.stats.Chunks++
				w.builder.stats.OriginalSize += c.Size
			}
			if st.Nlink > 1 {
				w.hardlinks[hlKey] = chunks
			}
			return chunks, 'U', nil
		}
		if known {
			status = 'M'
		}
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, status, err
	}
	defer f.Close()

	chunks, err := w.builder.ChunkFile(f)
	if err != nil {
		return nil, status, err
	}
	if st.Nlink > 1 {
		w.hardlinks[hlKey] = chunks
	}
	if w.opts.Files != nil {
		w.opts.Files.Memorize(cacheKey, cache.FileInfo{
			Size:  st.Size,
			Inode: int64(st.Ino),
			CTime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
			MTime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		}, chunks)
	}
	return chunks, status, nil
}

// markHardlink sets an item's hlid when its inode has more than one link.
func (w *walker) markHardlink(it *item.Item, st *unix.Stat_t) {
	if st.Nlink <= 1 {
		return
	}
	it.HLID = hardlinkID(st.Ino, st.Dev)
}

// hardlinkID is borg's hardlink_id_from_inode: sha256("<ino>/<dev>").
//
// A hash rather than the raw pair, because the identifier is stored in the archive and
// inode numbers say something about the source filesystem that the archive has no reason
// to carry.
func hardlinkID(ino, dev uint64) []byte {
	sum := sha256.Sum256([]byte(strconv.FormatUint(ino, 10) + "/" + strconv.FormatUint(dev, 10)))
	return sum[:]
}

// fillMetadata records everything about an object except its content.
func (w *walker) fillMetadata(it *item.Item, abs string, st *unix.Stat_t) {
	mode := int64(st.Mode)
	it.Mode = &mode

	uid, gid := int64(st.Uid), int64(st.Gid)
	it.UID, it.GID = &uid, &gid
	if !w.opts.NumericIDs {
		if name := w.userName(st.Uid); name != "" {
			it.User = &name
		}
		if name := w.groupName(st.Gid); name != "" {
			it.Group = &name
		}
	}

	mtime := st.Mtim.Sec*1e9 + st.Mtim.Nsec
	atime := st.Atim.Sec*1e9 + st.Atim.Nsec
	ctime := st.Ctim.Sec*1e9 + st.Ctim.Nsec
	it.MTime, it.ATime, it.CTime = &mtime, &atime, &ctime

	size := int64(st.Size)
	if st.Mode&unix.S_IFMT == unix.S_IFREG {
		it.Size = &size
	}
	inode := st.Ino
	it.Inode = &inode

	if !w.opts.NoXAttrs {
		if attrs, err := GetXAttrs(abs); err == nil && len(attrs) > 0 {
			it.XAttrs = attrs
			it.XAttrsSet = true
		}
	}
	if !w.opts.NoACLs {
		w.fillACLs(it, abs, st)
	}
}

// fillACLs records the POSIX ACLs of an object.
//
// Only an object that actually carries an extended ACL gets the fields: a file whose
// "ACL" is just its mode bits has nothing to record, and storing one would make every
// file in the archive larger for no gain.
func (w *walker) fillACLs(it *item.Item, abs string, st *unix.Stat_t) {
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return // symlinks carry no ACL
	}
	if text, err := GetACLText(abs, xattrACLAccess, w.opts.NumericIDs); err == nil && text != "" {
		it.ACLAccess = []byte(text)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if text, err := GetACLText(abs, xattrACLDefault, w.opts.NumericIDs); err == nil && text != "" {
			it.ACLDefault = []byte(text)
		}
	}
}

func (w *walker) userName(uid uint32) string {
	if name, ok := w.users[uid]; ok {
		return name
	}
	name := ""
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
		name = u.Username
	}
	w.users[uid] = name
	return name
}

func (w *walker) groupName(gid uint32) string {
	if name, ok := w.groups[gid]; ok {
		return name
	}
	name := ""
	if g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10)); err == nil {
		name = g.Name
	}
	w.groups[gid] = name
	return name
}
