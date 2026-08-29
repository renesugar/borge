// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of Archive.extract_item, Archive.restore_attrs,
// Archive._check_safe_parent and Archive.extract_helper in borg's src/borg/archive.py,
// together with the extraction loop of src/borg/archiver/extract_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/repoobj"
)

// Extraction errors. They are separate types because a caller usually continues past a
// per-item failure but must never continue past a path that escapes the target.
var (
	// ErrPathTraversal means an archived path contains "..".
	ErrPathTraversal = errors.New("archive: archived path escapes the extraction directory")
	// ErrSymlinkParent means a parent component of an archived path is an existing
	// symlink or other non-directory.
	ErrSymlinkParent = errors.New("archive: a parent of the archived path is not a real directory")
	// ErrSizeMismatch means an item's recorded size disagrees with its chunks.
	ErrSizeMismatch = errors.New("archive: size inconsistency")
)

// ExtractOptions control an extraction.
type ExtractOptions struct {
	// Dest is the directory to extract into. Archived paths are relative to it.
	Dest string

	// NumericIDs restores uid and gid as numbers, ignoring the recorded user and group
	// names. Without it, names win when they resolve on this machine, which is what makes
	// a restore onto a different machine put files in the right hands.
	NumericIDs bool

	// Sparse writes an all-zero chunk as a hole rather than as zeros.
	Sparse bool

	// NoXAttrs, NoACLs and NoFlags skip those attributes.
	NoXAttrs bool
	NoACLs   bool
	NoFlags  bool

	// NoAttrs skips *all* attribute restoration: mode, ownership, times. The content is
	// still written.
	NoAttrs bool

	// DryRun reads and verifies everything but writes nothing.
	DryRun bool

	// Stdout, when set, sends every extracted file's contents there instead of writing
	// anything to the filesystem. borg treats it as a kind of dry run - "if dry_run or
	// stdout" - and so does this: no directories are made, no attributes are applied, and
	// a hard link is not linked. What comes out is the concatenation of the matched
	// files' contents, in archive order, which is what makes "borge extract --stdout
	// ARCHIVE some/file | less" work and what makes it meaningless for a whole tree.
	Stdout io.Writer

	// Continue skips an item whose extracted copy is already there and already right,
	// so an interrupted extraction can be resumed without reading everything again.
	Continue bool

	// Filter selects which items to extract. Nil extracts everything.
	Filter func(*item.Item) bool

	// StripComponents removes this many leading path components.
	StripComponents int

	// OnError is called for a per-item failure. Returning an error stops the extraction;
	// returning nil continues with the next item. Nil means "stop on the first error".
	OnError func(path string, err error) error

	// OnProgress, if set, is called with each item as it is extracted.
	OnProgress func(*item.Item)
}

// ExtractStats counts what an extraction did.
type ExtractStats struct {
	Items      int
	Files      int
	Dirs       int
	Symlinks   int
	Hardlinks  int
	Others     int
	Bytes      int64
	Errors     int
	Unmatched  int
	SkippedACL int
	// Skipped counts items --continue found already extracted and correct.
	Skipped int
}

// Extract writes the archive's items into opts.Dest.
//
// # Ordering
//
// Directories are created without their attributes and pushed on a stack; the attributes
// are applied as the stack unwinds, once nothing more will be written inside. Setting a
// directory's mtime before filling it would be pointless - every file created inside
// updates it again - and setting a read-only mode too early would stop the extraction
// from writing into it at all.
func (a *Archive) Extract(opts ExtractOptions) (*ExtractStats, error) {
	if opts.Dest == "" {
		return nil, errors.New("archive: no extraction directory given")
	}
	dest, err := filepath.Abs(opts.Dest)
	if err != nil {
		return nil, err
	}
	// Neither a dry run nor --stdout touches the destination, so neither creates it: a
	// "borge extract --stdout" that made an empty directory as a side effect would be a
	// surprise, and borg makes none.
	if !opts.DryRun && opts.Stdout == nil {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
	}

	x := &extractor{
		archive:   a,
		opts:      opts,
		dest:      dest,
		safeDirs:  map[string]bool{dest: true},
		hardlinks: map[string]string{},
		uids:      map[string]int{},
		gids:      map[string]int{},
		stats:     &ExtractStats{},
	}

	err = a.Items(func(it *item.Item) error {
		return x.item(it)
	})
	if err != nil {
		return x.stats, err
	}
	if err := x.finishDirs(""); err != nil {
		return x.stats, err
	}
	return x.stats, nil
}

// extractor is the per-run state: the safe-directory cache, the hard link map, and the
// stack of directories waiting for their attributes.
type extractor struct {
	archive *Archive
	opts    ExtractOptions
	dest    string

	// safeDirs remembers directories already verified to be real directories, so each is
	// lstat'ed once per extraction rather than once per item inside it.
	safeDirs map[string]bool
	// hardlinks maps an item's hlid to the path already extracted for it.
	hardlinks map[string]string
	// pendingDirs is the stack of directories whose attributes are still to be applied.
	pendingDirs []*item.Item
	// uids and gids cache name lookups, including the ones that fail.
	//
	// resolveOwner asked the system for a name on every item. os/user goes through cgo -
	// getpwnam_r and getgrnam_r - and over the 118,866-file corpus of §12.1b that was
	// 12.5 seconds, 21% of the extract, in a port whose design premise is that it needs
	// no C. An archive holds a handful of distinct names however many files it has.
	//
	// Failures are cached too: a name that does not exist on this machine is the common
	// case when restoring somebody else's backup, and looking it up 118,866 times to be
	// told so each time is the same bug wearing a different hat.
	//
	// Unguarded maps, like safeDirs and hardlinks above: an extractor belongs to one
	// goroutine. Parallelising extract (§12.5 step 3) has to deal with all three
	// together, and guarding only this one would suggest the others were already safe.
	uids map[string]int
	gids map[string]int

	stats *ExtractStats
}

func (x *extractor) fail(path string, err error) error {
	x.stats.Errors++
	if x.opts.OnError == nil {
		return err
	}
	return x.opts.OnError(path, err)
}

// stripped applies StripComponents, returning "" when nothing is left.
func (x *extractor) stripped(path string) string {
	if x.opts.StripComponents <= 0 {
		return path
	}
	parts := strings.Split(path, "/")
	if len(parts) <= x.opts.StripComponents {
		return ""
	}
	return strings.Join(parts[x.opts.StripComponents:], "/")
}

func (x *extractor) item(it *item.Item) error {
	if x.opts.Filter != nil && !x.opts.Filter(it) {
		return nil
	}
	archived := x.stripped(it.Path)
	if archived == "" {
		x.stats.Unmatched++
		return nil
	}

	// Apply the attributes of every pending directory this item is no longer inside.
	if err := x.finishDirs(archived); err != nil {
		return err
	}

	x.stats.Items++
	if x.opts.OnProgress != nil {
		x.opts.OnProgress(it)
	}

	if x.opts.DryRun || x.opts.Stdout != nil {
		return x.stream(it)
	}

	if err := x.checkSafeParent(archived); err != nil {
		// Never continue past this: it is not a damaged item, it is an archive trying to
		// write outside the directory it was given.
		return err
	}
	path := filepath.Join(x.dest, filepath.FromSlash(archived))
	delete(x.safeDirs, path) // about to be (re)created; a stale entry must not be trusted

	mode := it.ModeOr(0o100644)

	// Remove whatever is already there, except an existing directory where a directory is
	// wanted - replacing that would destroy a btrfs subvolume (borg #4233).
	if st, err := os.Lstat(path); err == nil {
		if x.opts.Continue && sameAsExtracted(it, st) {
			// Already extracted, by a run that was interrupted later. Skipping it is the
			// whole point of --continue: the chunks are not fetched, which is where the
			// time goes.
			x.stats.Skipped++
			return nil
		}
		switch {
		case !st.IsDir():
			_ = os.Remove(path)
		case item.IsDir(mode):
			// keep it
		default:
			_ = os.Remove(path)
		}
	}

	switch {
	case item.IsDir(mode):
		if err := x.makeParent(path); err != nil {
			return x.fail(it.Path, err)
		}
		if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
			return x.fail(it.Path, err)
		}
		// Attributes are applied later, when nothing more will be written inside.
		x.pendingDirs = append(x.pendingDirs, cloneWithPath(it, archived))
		x.stats.Dirs++
		return nil

	case item.IsRegular(mode):
		if err := x.makeParent(path); err != nil {
			return x.fail(it.Path, err)
		}
		linked, err := x.tryHardlink(it, path)
		if err != nil {
			return x.fail(it.Path, err)
		}
		if linked {
			x.stats.Hardlinks++
			return nil
		}
		if err := x.writeFile(it, path); err != nil {
			return x.fail(it.Path, err)
		}
		x.rememberHardlink(it, path)
		x.stats.Files++
		return nil

	case item.IsSymlink(mode):
		if err := x.makeParent(path); err != nil {
			return x.fail(it.Path, err)
		}
		linked, err := x.tryHardlink(it, path)
		if err != nil {
			return x.fail(it.Path, err)
		}
		if linked {
			x.stats.Hardlinks++
			return nil
		}
		target := ""
		if it.Target != nil {
			target = *it.Target
		}
		if err := os.Symlink(target, path); err != nil {
			return x.fail(it.Path, err)
		}
		if err := x.restoreAttrs(path, it, true); err != nil {
			return x.fail(it.Path, err)
		}
		// A symlink's own timestamps are restored too. Not every filesystem allows it;
		// restoreTimes tolerates that rather than failing the restore.
		if err := x.restoreTimes(path, it); err != nil {
			return x.fail(it.Path, err)
		}
		if err := x.restoreFlags(path, it); err != nil {
			return x.fail(it.Path, err)
		}
		x.rememberHardlink(it, path)
		x.stats.Symlinks++
		return nil

	default:
		if err := x.makeParent(path); err != nil {
			return x.fail(it.Path, err)
		}
		linked, err := x.tryHardlink(it, path)
		if err != nil {
			return x.fail(it.Path, err)
		}
		if linked {
			x.stats.Hardlinks++
			return nil
		}
		if err := makeSpecial(path, mode, it); err != nil {
			return x.fail(it.Path, err)
		}
		if err := x.restoreAttrs(path, it, false); err != nil {
			return x.fail(it.Path, err)
		}
		if err := x.restoreTimes(path, it); err != nil {
			return x.fail(it.Path, err)
		}
		if err := x.restoreFlags(path, it); err != nil {
			return x.fail(it.Path, err)
		}
		x.rememberHardlink(it, path)
		x.stats.Others++
		return nil
	}
}

// cloneWithPath copies an item with a rewritten path, so the deferred directory pass
// works on the stripped path without mutating the caller's item.
func cloneWithPath(it *item.Item, path string) *item.Item {
	clone := *it
	clone.Path = path
	return &clone
}

// dryRun reads an item's content without writing anything, which is what makes a dry run
// worth doing: it proves the chunks are there and authenticate.
// stream is the path shared by --dry-run and --stdout: read every chunk, count it, check
// the recorded size, and - for --stdout - write it out.
//
// borg shares them the same way, and the shared shape is the point: a dry run that did not
// read the chunks would not verify anything, and --stdout that did not check the size
// would hand a truncated file to whatever is reading the pipe.
func (x *extractor) stream(it *item.Item) error {
	if !it.ChunksSet {
		return nil
	}
	var total int64
	err := x.fetchChunks(it, func(data []byte) error {
		total += int64(len(data))
		if x.opts.Stdout == nil {
			return nil
		}
		_, werr := x.opts.Stdout.Write(data)
		return werr
	})
	if err != nil {
		return x.fail(it.Path, err)
	}
	x.stats.Bytes += total
	return x.checkSize(it, total)
}

// sameAsExtracted is borg's same_item: is what is on disk already this archived item?
//
// borg's own comment says what the check is for and how far it can be trusted: "this is
// good enough for the intended use case: continuing an extraction of same archive that
// initially started in an empty directory", with a noted risk that a file interrupted
// between its mtime and its flags keeps the wrong flags.
//
// Only regular files and directories are compared. borg says why: the others "are less
// frequent and have no content extraction we could 'optimize away'" - skipping a symlink
// saves nothing, so it is re-created rather than examined.
func sameAsExtracted(it *item.Item, st os.FileInfo) bool {
	mode := it.ModeOr(0)
	isFile := st.Mode().IsRegular()
	isDir := st.IsDir()
	if !isFile && !isDir {
		return false
	}
	// The whole mode, not just the type: extracting a file over one with different
	// permissions is still work to do. The stat is read through syscall.Stat_t rather than
	// os.FileMode, because Go's FileMode is a portable abstraction and what has to be
	// compared here is the raw st_mode the item recorded.
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if uint32(mode) != sys.Mode {
		return false
	}
	if isFile && (it.Size == nil || *it.Size != st.Size()) {
		// The size check is what catches a file whose extraction was interrupted part way.
		return false
	}
	if it.MTime == nil || *it.MTime != st.ModTime().UnixNano() {
		// borg notes that mtime is set late - after xattrs and ACLs, before flags - which
		// is what makes it a reasonable marker for "this one finished".
		return false
	}
	return true
}

// checkSize compares the item's recorded size against what its chunks actually held.
func (x *extractor) checkSize(it *item.Item, got int64) error {
	if it.Size == nil {
		return nil
	}
	if *it.Size != got {
		return x.fail(it.Path, fmt.Errorf("%w: metadata says %d bytes, the chunks hold %d",
			ErrSizeMismatch, *it.Size, got))
	}
	return nil
}

// checkSafeParent refuses a path whose parent chain is not safe.
//
// borg create never produces a path containing ".." or one below a symlinked directory,
// so such a path can only come from a damaged or hostile archive - where it would let the
// extraction write through a symlink to somewhere outside the destination.
func (x *extractor) checkSafeParent(archived string) error {
	parts := strings.Split(archived, "/")
	var components []string
	for _, c := range parts[:len(parts)-1] {
		if c == "" || c == "." {
			continue
		}
		components = append(components, c)
	}
	for _, c := range components {
		if c == ".." {
			// Rejected before the walk below, so the walk cannot climb above dest and so
			// that stopping early at a not-yet-existing component cannot skip a later "..".
			return fmt.Errorf("%w: %s", ErrPathTraversal, archived)
		}
	}

	current := x.dest
	for _, c := range components {
		current = filepath.Join(current, c)
		if x.safeDirs[current] {
			continue
		}
		st, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// Not there yet; it will be created as a real directory below a chain
				// already verified.
				break
			}
			return err
		}
		if !st.IsDir() {
			// Lstat does not follow symlinks, so a symlinked parent shows up here as a
			// non-directory.
			return fmt.Errorf("%w: %s", ErrSymlinkParent, archived)
		}
		x.safeDirs[current] = true
	}
	return nil
}

func (x *extractor) makeParent(path string) error {
	parent := filepath.Dir(path)
	if x.safeDirs[parent] {
		return nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	x.safeDirs[parent] = true
	return nil
}

// hlidKey is the map key for a hard link group.
func hlidKey(it *item.Item) string {
	if len(it.HLID) == 0 {
		return ""
	}
	return hex.EncodeToString(it.HLID)
}

// tryHardlink links path to an earlier member of the same hard link group, if there is
// one.
//
// The link is made with the symlink-following disabled, so a hard link to an archived
// *symlink* recreates the symlink rather than linking the file it points at. That is both
// the faithful restore and the fix for CVE-2026-62268: otherwise an archive could pair a
// symlink to /etc/shadow with a contentless hard link sharing its hlid and pull an
// arbitrary external file into the extracted tree.
func (x *extractor) tryHardlink(it *item.Item, path string) (bool, error) {
	key := hlidKey(it)
	if key == "" {
		return false, nil
	}
	target, ok := x.hardlinks[key]
	if !ok {
		return false, nil
	}
	if err := linkNoFollow(target, path); err != nil {
		return false, err
	}
	return true, nil
}

func (x *extractor) rememberHardlink(it *item.Item, path string) {
	if key := hlidKey(it); key != "" {
		x.hardlinks[key] = path
	}
}

// writeFile creates a regular file from the item's chunks.
func (x *extractor) writeFile(it *item.Item, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	var written int64
	var trailingHole bool
	err = x.fetchChunks(it, func(data []byte) error {
		if x.opts.Sparse && isAllZero(data) {
			if _, err := f.Seek(int64(len(data)), io.SeekCurrent); err != nil {
				return err
			}
			trailingHole = true
		} else {
			if _, err := f.Write(data); err != nil {
				return err
			}
			trailingHole = false
		}
		written += int64(len(data))
		return nil
	})
	if err != nil {
		return err
	}

	// A file ending in a hole is only the right length once it is truncated to it: seeking
	// past the end does not extend the file.
	if trailingHole || written == 0 {
		if err := f.Truncate(written); err != nil {
			return err
		}
	}
	x.stats.Bytes += written

	if err := x.restoreAttrsFd(f, path, it); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Times are set after the file is closed: writing updates mtime, so setting it while
	// the file is still open and unflushed would be undone.
	if err := x.restoreTimes(path, it); err != nil {
		return err
	}
	if err := x.restoreFlags(path, it); err != nil {
		return err
	}
	return x.checkSize(it, written)
}

// fetchChunks reads an item's content chunks in order.
func (x *extractor) fetchChunks(it *item.Item, fn func([]byte) error) error {
	for _, c := range it.Chunks {
		obj, err := x.archive.repo.Get(c.ID)
		if err != nil {
			return fmt.Errorf("archive: chunk %s: %w", hex.EncodeToString(c.ID), err)
		}
		_, data, err := x.archive.ro.Parse(c.ID, obj, repoobj.TypeFileStream, repoobj.ParseOptions{})
		if err != nil {
			return err
		}
		if int64(len(data)) != c.Size {
			return fmt.Errorf("archive: chunk %s is %d bytes, the item says %d",
				hex.EncodeToString(c.ID), len(data), c.Size)
		}
		if err := fn(data); err != nil {
			return err
		}
	}
	return nil
}

func isAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return len(b) > 0
}

// finishDirs applies the attributes of every pending directory that the given path is no
// longer inside. An empty path flushes them all.
func (x *extractor) finishDirs(next string) error {
	for len(x.pendingDirs) > 0 {
		top := x.pendingDirs[len(x.pendingDirs)-1]
		if next != "" && strings.HasPrefix(next, top.Path+"/") {
			break
		}
		x.pendingDirs = x.pendingDirs[:len(x.pendingDirs)-1]
		if x.opts.DryRun {
			continue
		}
		path := filepath.Join(x.dest, filepath.FromSlash(top.Path))
		if err := x.restoreAttrs(path, top, false); err != nil {
			if err := x.fail(top.Path, err); err != nil {
				return err
			}
		}
		if err := x.restoreTimes(path, top); err != nil {
			if err := x.fail(top.Path, err); err != nil {
				return err
			}
		}
		// Last for a directory too: an immutable directory cannot have entries added, so
		// a flag set before its children were written would fail the rest of the restore.
		// Directories are deferred to here for the same family of reason - their times
		// would be changed again by every file written into them.
		if err := x.restoreFlags(path, top); err != nil {
			if err := x.fail(top.Path, err); err != nil {
				return err
			}
		}
	}
	return nil
}

// SortedPendingDirs is for tests: the directories still awaiting their attributes.
func (x *extractor) SortedPendingDirs() []string {
	var out []string
	for _, d := range x.pendingDirs {
		out = append(out, d.Path)
	}
	sort.Strings(out)
	return out
}
