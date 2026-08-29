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
	"math"
	"time"
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

	// PathsOnly archives exactly the paths given and descends into nothing.
	//
	// It is what borg's --paths-from-stdin and friends do, and the documented promise is
	// "all control is external: it will back up all files given - no more, no less". So a
	// directory in the list contributes its own entry and none of its contents, the
	// include/exclude patterns are not consulted at all, and neither are the
	// CACHEDIR.TAG rules: the caller has already decided.
	PathsOnly bool

	// DryRun walks and decides but stores nothing: no file is read, no chunk is written,
	// no item is added. It is how a user checks an exclude pattern before trusting it,
	// so what it reports through OnItem has to be exactly what a real run would store.
	DryRun bool

	// StoreATime records each item's access time. Off by default, as in borg, and the
	// default is the interesting half: atime changes whenever a file is *read*, so
	// storing it makes an item stream differ between two backups of a tree that did not
	// change, which costs space and makes "diff" report files nobody touched.
	StoreATime bool
	// NoCTime leaves out the inode change time, which borg stores by default.
	NoCTime bool
	// NoBirthTime leaves out the creation time. Accepted for borg compatibility and
	// currently a no-op: Linux exposes birthtime only through statx, which neither tool
	// reads here, so neither stores it. Recorded rather than silently ignored.
	NoBirthTime bool

	// Files is the files cache, or nil to read every file. It is consulted before a file
	// is opened and updated after it is chunked.
	Files *cache.FilesCache

	// FilesChanged selects how a file changing while it is read is detected: by ctime
	// (borg's default), by mtime, or not at all. See readFile.
	FilesChanged FilesChangedMode

	// ReadSpecialTimeout bounds a read from a fifo or character device under
	// --read-special: if nothing arrives for this long, the file is reported as an error
	// and the backup carries on. Zero or less means wait forever, which is what a fifo
	// with no writer does otherwise - borg opens such a fifo with O_NONBLOCK precisely so
	// that the wait becomes this timeout rather than a hang.
	ReadSpecialTimeout time.Duration

	// Sparse asks the reader to skip a sparse file's holes instead of reading them.
	//
	// It changes no stored bytes: an all-zero region is recorded by size with no data
	// either way, because both tools detect an all-zero block "regardless of sparse mode"
	// (borg's reader.pyx). What it changes is the reading - a hole is seeked over rather
	// than read - so a 100 GB file with 1 GB of data in it costs 1 GB of reads.
	//
	// borg's help says "supported only by fixed chunker" and borge's implementation has
	// the same limit, for the same reason: a content-defined chunker's boundaries depend
	// on the bytes it has seen, so skipping a hole would have to synthesise the zeros to
	// keep the rolling hash honest - at which point nothing was saved.
	Sparse bool

	// ExcludeDataless skips a file carrying the SF_DATALESS flag: a macOS placeholder for
	// content that lives in cloud storage and is not on this machine. Reading one makes
	// macOS download it, so the check happens before the file is opened.
	//
	// Nothing on Linux sets that flag, so this is inert here - but it is the flag word
	// borge already reads, and an option that silently did nothing on the platform it was
	// written for would be worse than one that does nothing on this one.
	ExcludeDataless bool

	// OnItem is called for each item as it is archived.
	OnItem func(status byte, path string)
	// OnError is called for a per-path failure. Returning an error aborts; returning nil
	// continues. Nil means "abort on the first error".
	OnError func(path string, err error) error
	// OnWarning reports something worth saying that is not a failure - a file being
	// re-read because it changed, so far.
	OnWarning func(path, message string)
}

// FilesChangedMode is --files-changed.
type FilesChangedMode string

const (
	// FilesChangedCTime is borg's default everywhere but Windows. ctime moves for a
	// metadata change as well as a data change, which makes it the cautious choice: it
	// reports some files that were not written to, and misses none that were.
	FilesChangedCTime FilesChangedMode = "ctime"
	// FilesChangedMTime watches the modification time alone.
	FilesChangedMTime FilesChangedMode = "mtime"
	// FilesChangedDisabled does not look. borg offers it, and the cost of it is that a
	// file written during the backup is stored torn and reported as though it were fine.
	FilesChangedDisabled FilesChangedMode = "disabled"
)

// ParseFilesChanged validates a --files-changed value.
func ParseFilesChanged(s string) (FilesChangedMode, error) {
	switch FilesChangedMode(s) {
	case FilesChangedCTime, FilesChangedMTime, FilesChangedDisabled:
		return FilesChangedMode(s), nil
	}
	return "", fmt.Errorf("archive: --files-changed must be ctime, mtime or disabled, got %q", s)
}

// CreateStats is what a backup did, beyond the chunk accounting in Stats.
type CreateStats struct {
	Stats
	Errors  int
	Skipped int
	// Unchanged counts files the files cache spared from being read.
	Unchanged int
	// FileStatus counts items by the status character a listing would show them with:
	// "A" added, "M" modified, "U" unchanged, "d" directory, "s" symlink, "-" excluded,
	// and so on. It is borg's files_stats, reported by "create --json".
	//
	// Counted here rather than by the caller because the walker is the only place that
	// knows the status of an item it decided not to report: with no --list nothing is
	// printed, and a count taken from the printed lines would be zero.
	FileStatus map[string]int64
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
		stats:     &CreateStats{FileStatus: map[string]int64{}},
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
	// borg reports a failed item in the listing as "E", and counts it in files_stats -
	// which is where "Error files: N" in its summary comes from. borge reported the
	// warning and nothing else, so "create --list" showed a file that was neither stored
	// nor mentioned, and --filter E selected nothing.
	w.report('E', path)
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
	if w.opts.Matcher != nil && !w.opts.PathsOnly {
		included = w.opts.Matcher.Match(archivedPath(abs))
	}
	stored, storable := w.storedPath(abs)

	// Tag-based exclusion is decided before the directory is stored, because a tagged
	// directory is not stored at all unless --keep-exclude-tags asks for it. Checking
	// after would archive the entry and then decline to recurse, which is a different
	// archive.
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if isDir && !w.opts.PathsOnly {
		tags, err := w.tagsExcluding(abs)
		if err != nil {
			return err
		}
		if len(tags) > 0 {
			w.stats.Skipped++
			w.report('-', abs)
			if !included || !storable || !w.opts.KeepExcludeTags {
				// Nothing from here, and no recursion either way: borg returns at this
				// point whether or not it kept the tags.
				return nil
			}
			// The directory itself, then the tag files that excluded it - so a restore
			// can be excluded again the same way.
			excluded, err := w.archive(abs, stored, &st)
			if err != nil {
				return err
			}
			if excluded {
				// An attribute excluded it as well as the tag. Keeping the tag files of a
				// directory that is not itself in the archive would store the marker for
				// something absent.
				return nil
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
		w.report('-', abs)
	case w.opts.DryRun:
		// Nothing is read and nothing is stored, but the counting is real so that
		// --stats means something.
		if st.Mode&unix.S_IFMT == unix.S_IFREG {
			w.stats.Stats.NFiles++
			w.stats.Stats.OriginalSize += st.Size
		}
		w.report('+', abs)
	default:
		excluded, err := w.archive(abs, stored, &st)
		if err != nil {
			return err
		}
		if excluded {
			// borg stops here too, setting recurse = False: the nodump flag on a
			// directory means the subtree, not the directory entry alone. Descending
			// would archive the children of something that is not in the archive.
			return nil
		}
	}

	// Descend into a directory unless the matcher said not to. An excluded directory is
	// still descended into by default, so an include pattern *inside* it can be found;
	// only the no-recurse exclude form stops the walk.
	if !isDir || w.opts.PathsOnly {
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
func (w *walker) archive(abs, stored string, st *unix.Stat_t) (bool, error) {
	it := &item.Item{Path: stored}
	w.fillMetadata(it, abs, st)

	// Checked here, after the extended attributes are known and before any content is
	// read, which is where borg checks it and for borg's stated reason: a file the flags
	// exclude should not be chunked first. The caller uses the result to decide whether to
	// descend, because an excluded directory takes its whole subtree with it.
	if excludedByAttr(it.XAttrs, it.BSDFlags) {
		w.stats.Skipped++
		w.report('-', abs)
		return true, nil
	}

	// --exclude-dataless, checked here for borg's stated reason: "this needs to be done
	// BEFORE opening the file, as opening would otherwise materialize the file contents".
	// borg reports these with "x", not the "-" an exclude pattern gets.
	if w.opts.ExcludeDataless && it.BSDFlags != nil && *it.BSDFlags&sfDataless != 0 {
		w.stats.Skipped++
		w.report('x', abs)
		return true, nil
	}

	status := byte('A')
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		status = 'd'

	case unix.S_IFLNK:
		target, err := os.Readlink(abs)
		if err != nil {
			return false, w.fail(abs, err)
		}
		it.Target = &target
		status = 's'
		// A symlink can be hard-linked too, and then it needs an hlid like any other
		// multiply-linked inode.
		w.markHardlink(it, st)

	case unix.S_IFREG:
		chunks, st2, err := w.fileChunks(abs, stored, st)
		if err != nil {
			return false, w.fail(abs, err)
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
			return false, nil
		}
		if w.opts.ReadSpecial {
			chunks, st2, err := w.fileChunks(abs, stored, st)
			if err != nil {
				return false, w.fail(abs, err)
			}
			it.Chunks = chunks
			it.ChunksSet = true
			// Reading a special file turns it into a regular one on restore, which is what
			// --read-special is for: the point is to capture what flows through it. And it
			// is reported as the regular file it has become - "A", "M" or "C" - which is
			// borg's behaviour: only the *unread* special file gets a type letter.
			mode := int64(st.Mode&0o7777) | item.SIFREG
			it.Mode = &mode
			status = st2
		} else {
			rdev := int64(st.Rdev)
			it.RDev = &rdev
			w.markHardlink(it, st)
			// One letter per kind, as borg has them: "f" fifo, "c" character device, "b"
			// block device. borge reported "i" for all three, which is borg's letter for
			// something else entirely - content read from stdin or a pipe (archive.py:
			// 'status = "i"  # stdin (or other pipe)'). So "create --list" named the wrong
			// kind, and "--filter f" selected nothing.
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFIFO:
				status = 'f'
			case unix.S_IFCHR:
				status = 'c'
			default:
				status = 'b'
			}
		}

	default:
		w.stats.Skipped++
		return false, nil
	}

	if err := w.builder.AddItem(it); err != nil {
		return false, w.fail(abs, err)
	}
	w.report(status, abs)
	return false, nil
}

// Attribute names borg treats as "do not back this up". Both are conventions from
// elsewhere rather than borg's own: the first is how macOS marks a path excluded from Time
// Machine, the second is the XDG proposal for telling backup tools to skip a directory.
const (
	xattrAppleExclude = "com.apple.metadata:com_apple_backup_excludeItem"
	xattrXDGBackup    = "user.xdg.robots.backup"
)

// excludedByAttr is borg's maybe_exclude_by_attr: an item the filesystem itself asks not to
// be backed up.
//
// Three markers, and the tests for them are not uniform, so each is spelled out:
//
//   - the Apple attribute excludes if it is *present at all*, whatever its value, because
//     the value is a plist nobody reads;
//   - the XDG attribute excludes only when it is exactly "false" - the attribute exists to
//     say "yes, back this up", and any other value including "true" leaves the item in;
//   - the nodump flag excludes, which is what the flag has meant since dump(8).
//
// borge did not implement any of this until 2026-08-19, so a file its owner had marked "do
// not back up" was backed up. It could not have been implemented earlier: the rule reads
// exactly the two item fields borge did not record until the same day. See
// docs/DIVERGENCES.md #39 and #8.
func excludedByAttr(xattrs map[string][]byte, bsdFlags *int64) bool {
	if _, ok := xattrs[xattrAppleExclude]; ok {
		return true
	}
	if v, ok := xattrs[xattrXDGBackup]; ok && string(v) == "false" {
		return true
	}
	return bsdFlags != nil && *bsdFlags&bsdNoDump != 0
}

// report tells the caller what happened to one path. borg's status characters: "A" added,
// "d" directory, "s" symlink, "i" special file, "h" a further hard link, "U" unchanged,
// "-" excluded, and "+" for anything a dry run would have stored.
//
// The path is the one *walked*, not the one stored, which is borg's choice and matters for
// exactly one case: an absolute source. "borg create A /srv/data --list" reports
// "/srv/data/f" where the archive holds "srv/data/f", and borge reported the stored form
// until 2026-08-19. The two coincide for a relative source, which is why it went unnoticed.
// A listing answers "what is being read", so the source path is the useful one - and under
// --log-json it becomes file_status.path, which a frontend uses to show progress against
// the filesystem the user is looking at.
func (w *walker) report(status byte, stored string) {
	w.stats.FileStatus[string(status)]++
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

	chunks, changed, err := w.readFile(abs, st)
	if err != nil {
		return nil, status, err
	}
	if changed {
		// borg's "C": the file changed while it was being read, so what was stored is a
		// mix of before and after. It is stored anyway - a torn copy of a busy file is
		// usually better than no copy - but it is reported, and it is NOT memorized in
		// the files cache, because a cache entry would let the next run skip re-reading
		// the one file known to be wrong.
		status = 'C'
	}
	if st.Nlink > 1 {
		w.hardlinks[hlKey] = chunks
	}
	if w.opts.Files != nil && !changed {
		w.opts.Files.Memorize(cacheKey, cache.FileInfo{
			Size:  st.Size,
			Inode: int64(st.Ino),
			CTime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
			MTime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		}, chunks)
	}
	return chunks, status, nil
}

// maxFileReadRetries is borg's MAX_RETRIES, and the count includes the first attempt.
const maxFileReadRetries = 10

// readFile reads and chunks a file, retrying while it keeps changing underneath.
//
// # Why a backup tool has to look
//
// A file being written while it is read is stored as a mix of before and after: not the old
// contents and not the new ones, but something that never existed. borg stats the file
// again after reading it, and if the timestamp moved it throws the work away and starts
// over - up to ten times, sleeping a little longer each time - on the theory that whatever
// was writing will stop. If it never stops, the last attempt is stored and marked "C".
//
// borge did none of this until 2026-08-20. It read the file, stored it, and reported "A" or
// "M", so a torn file looked exactly like a good one. See DIVERGENCES.md #52.
func (w *walker) readFile(abs string, st *unix.Stat_t) ([]item.ChunkListEntry, bool, error) {
	before := *st
	for retry := 0; ; retry++ {
		last := retry == maxFileReadRetries-1

		chunks, changed, err := w.readOnce(abs, &before)
		if err != nil {
			return nil, false, err
		}
		if !changed {
			return chunks, false, nil
		}
		if last {
			return chunks, true, nil
		}

		// borg's schedule: 1ms, then 10^(retry/2) times that - retry 6 is a second. Long
		// enough for a log rotation or a save to finish, short enough that ten of them do
		// not stall a backup.
		sleep := time.Duration(float64(time.Millisecond) * math.Pow(10, float64(retry)/2))
		if w.opts.OnWarning != nil {
			w.opts.OnWarning(abs, fmt.Sprintf(
				"file changed while we read it, slept %.3fs, next: retry %d of %d",
				sleep.Seconds(), retry+1, maxFileReadRetries-1))
		}
		time.Sleep(sleep)

		// A fresh stat before trying again, as borg takes: the file may have been
		// replaced, and the next comparison must be against what is there now.
		var fresh unix.Stat_t
		if err := unix.Lstat(abs, &fresh); err != nil {
			return nil, false, err
		}
		before = fresh
	}
}

// readOnce reads the file and reports whether it changed while being read.
func (w *walker) readOnce(abs string, before *unix.Stat_t) ([]item.ChunkListEntry, bool, error) {
	f, reader, err := w.open(abs, before)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	startReading := time.Now().UnixNano()
	// The size decides whether the worker pool is worth using; see pipelineMinFileSize.
	chunks, err := w.builder.ChunkFileSized(reader, before.Size)
	if err != nil {
		return nil, false, err
	}
	endReading := time.Now().UnixNano()

	var after unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &after); err != nil {
		return nil, false, err
	}
	return chunks, w.changedWhileReading(before, &after, startReading, endReading), nil
}

// open opens a file for reading, applying --read-special-timeout where it applies.
//
// # Why a fifo needs O_NONBLOCK to have a timeout at all
//
// Opening a fifo for reading blocks until a writer connects - in the kernel, before any
// read happens - so a deadline on the reads alone would never be reached. borg opens with
// O_NONBLOCK for exactly that: the open returns at once, and the waiting becomes a read
// that the deadline can bound. borge does the same, and then sets a read deadline through
// os.File's SetReadDeadline, which works on a pipe or character device because Go's runtime
// polls them.
func (w *walker) open(abs string, st *unix.Stat_t) (*os.File, io.Reader, error) {
	special := st.Mode&unix.S_IFMT == unix.S_IFIFO || st.Mode&unix.S_IFMT == unix.S_IFCHR
	if w.opts.ReadSpecialTimeout <= 0 || !special {
		f, err := os.Open(abs)
		if err != nil {
			return nil, nil, err
		}
		if w.opts.Sparse && st.Mode&unix.S_IFMT == unix.S_IFREG {
			r, err := newSparseReader(f, st.Size)
			if err != nil {
				f.Close()
				return nil, nil, err
			}
			return f, r, nil
		}
		return f, f, nil
	}
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: abs, Err: err}
	}
	f := os.NewFile(uintptr(fd), abs)
	return f, &specialFileReader{fd: fd, timeout: w.opts.ReadSpecialTimeout}, nil
}

// noWriterPollInterval is borg's NO_WRITER_POLL_INTERVAL: how often to look again at a fifo
// nobody has opened for writing.
const noWriterPollInterval = 100 * time.Millisecond

// specialFileReader reads a fifo or character device under a timeout.
//
// # Why an empty read is not the end
//
// A fifo opened O_RDONLY|O_NONBLOCK with no writer returns zero bytes from read(2) - which
// everywhere else means end of file. Here it means "nobody has connected yet", and treating
// it as EOF is what made borge store an *empty file* for a fifo where borg reports an error.
// So zero bytes counts as EOF only once some data has actually arrived; before that, it is
// something to wait through.
//
// borg spells out the consequence and it is worth keeping: a writer that connects and closes
// without writing anything produces a timeout, not empty content.
//
// # The timeout is a gap, not a budget
//
// It bounds how long a read waits for data to *arrive*, and it restarts whenever any
// arrives. A slow writer trickling bytes forever never times out; a writer that stops for
// longer than the timeout does.
type specialFileReader struct {
	fd      int
	timeout time.Duration
	gotData bool
}

func (r *specialFileReader) Read(p []byte) (int, error) {
	deadline := time.Now().Add(r.timeout)
	for {
		n, err := unix.Read(r.fd, p)
		switch {
		case n > 0:
			r.gotData = true
			return n, nil
		case err == unix.EAGAIN || err == unix.EWOULDBLOCK:
			// A writer is connected but has nothing for us right now.
		case err != nil:
			return 0, err
		case r.gotData:
			// A real end of file: every writer closed after sending something.
			return 0, io.EOF
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			return 0, &os.PathError{Op: "read", Path: "", Err: unix.ETIMEDOUT}
		}
		// Waiting for a writer that is not there yet cannot be waited *on*, so it is
		// polled; waiting for data from a connected writer can be, and poll returns as
		// soon as it arrives or the writer closes.
		wait := noWriterPollInterval
		if err != nil { // EAGAIN: there is a writer, so the fd is pollable
			if until := time.Until(deadline); until < wait {
				wait = until
			}
			fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
			_, _ = unix.Poll(fds, int(wait.Milliseconds()))
			continue
		}
		if until := time.Until(deadline); until < wait {
			wait = until
		}
		time.Sleep(wait)
	}
}

// timeDiffers is borg's TIME_DIFFERS1_NS: 20ms of slack around the read window.
const timeDiffers = 20 * time.Millisecond

// changedWhileReading compares the timestamps taken before and after the read.
//
// The straightforward half is "the timestamp moved". The other half is borg's answer to
// issue #3536, and it is the reason a naive implementation misses real corruption: if the
// file was changed just before the first stat, and changed again during the read, the
// second timestamp can equal the first because the filesystem's clock granularity hid it.
// So a timestamp that merely *falls inside the read window* is treated as suspicious too,
// widened by 20ms at each end.
func (w *walker) changedWhileReading(before, after *unix.Stat_t, startReading, endReading int64) bool {
	// Special files are never checked, and borg says why: "fifos change naturally, because
	// they are fed from the other side. no problem" - and block or character devices do not
	// change ctime when read at all. Checking them anyway is not merely noisy: it makes
	// borge re-read the fifo, and the second read finds the writer gone. Measured, having
	// written the check without this and watched a fifo's contents disappear.
	switch before.Mode & unix.S_IFMT {
	case unix.S_IFIFO, unix.S_IFCHR, unix.S_IFBLK, unix.S_IFSOCK:
		return false
	}

	var oldNS, newNS int64
	switch w.opts.FilesChanged {
	case FilesChangedDisabled:
		return false
	case FilesChangedMTime:
		oldNS = before.Mtim.Sec*1e9 + before.Mtim.Nsec
		newNS = after.Mtim.Sec*1e9 + after.Mtim.Nsec
	default: // FilesChangedCTime, borg's default on everything but Windows
		oldNS = before.Ctim.Sec*1e9 + before.Ctim.Nsec
		newNS = after.Ctim.Sec*1e9 + after.Ctim.Nsec
	}
	if oldNS != newNS {
		return true
	}
	return startReading-int64(timeDiffers) < newNS && newNS < endReading+int64(timeDiffers)
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

	// borg stores mtime always, ctime unless --noctime, and atime only with --atime.
	// borge stored all three, which made every archive carry a timestamp borg leaves out
	// - larger, different from borg's for the same tree, and noisy, because atime moves
	// when a file is merely read. The stage 7 comparator comes nowhere near it: it checks
	// mtime and deliberately excludes atime and ctime from the contract.
	mtime := st.Mtim.Sec*1e9 + st.Mtim.Nsec
	it.MTime = &mtime
	if w.opts.StoreATime {
		atime := st.Atim.Sec*1e9 + st.Atim.Nsec
		it.ATime = &atime
	}
	if !w.opts.NoCTime {
		ctime := st.Ctim.Sec*1e9 + st.Ctim.Nsec
		it.CTime = &ctime
	}

	size := int64(st.Size)
	if st.Mode&unix.S_IFMT == unix.S_IFREG {
		it.Size = &size
	}
	inode := st.Ino
	it.Inode = &inode

	// Both of these record the *key* whenever the attribute was examined, even when there
	// was nothing to record, because in borg the key's presence is the statement that it
	// looked: "borg create --noxattrs" leaves the key out and an ordinary create writes an
	// empty dict. "Checked, found none" and "not recorded" are different answers, and an
	// archive that cannot tell them apart cannot be distinguished from one taken with the
	// option. borge wrote neither key until 2026-08-19; see docs/DIVERGENCES.md #8.
	if !w.opts.NoXAttrs {
		attrs, err := GetXAttrs(abs)
		if err != nil || attrs == nil {
			// A filesystem without xattr support answers the same as one with none, which
			// is also what borg records: the read failing is not the backup failing.
			attrs = map[string][]byte{}
		}
		it.XAttrs, it.XAttrsSet = attrs, true
	}
	if !w.opts.NoFlags {
		flags := GetFlags(abs, st.Mode)
		it.BSDFlags = &flags
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
