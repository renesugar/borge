// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of Archive.restore_attrs in borg's src/borg/archive.py and the
// Linux parts of src/borg/platform/.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

//go:build linux

package archive

import (
	"errors"
	"os"
	"os/user"
	"sort"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/renesugar/borge/internal/item"
)

// Attribute restoration, in borg's order, and the order is load-bearing:
//
//  1. ownership, then mode. chown can clear the setuid and setgid bits, so the mode has
//     to be set after it, not before.
//  2. ACLs, then extended attributes. chown also clears Linux capabilities, which live
//     in the security.capability xattr - so xattrs come after ownership too.
//  3. times, late, because writing content and setting other attributes updates them.
//  4. bsdflags last of all: the immutable flag makes everything above impossible.

// restoreAttrs applies ownership, mode, ACLs and xattrs to a path.
//
// symlink selects the no-follow variants, so a symlink's own attributes are set rather
// than its target's.
func (x *extractor) restoreAttrs(path string, it *item.Item, symlink bool) error {
	if x.opts.NoAttrs {
		return nil
	}
	uid, gid := x.resolveOwner(it)
	if uid != -1 || gid != -1 {
		// A failure here is ignored, as in borg: an unprivileged restore cannot change
		// ownership, and refusing to restore the data over that would be worse than
		// restoring it owned by the user doing the restore.
		_ = unix.Lchown(path, uid, gid)
	}

	if it.Mode != nil && !symlink {
		// Linux has no lchmod: a symlink's mode is not settable and is not meaningful.
		if err := os.Chmod(path, os.FileMode(*it.Mode&0o7777)); err != nil {
			return err
		}
	}

	if !x.opts.NoACLs {
		if err := x.restoreACLs(path, it); err != nil {
			return err
		}
	}
	if !x.opts.NoXAttrs && it.XAttrsSet {
		if err := setXAttrs(path, it.XAttrs); err != nil {
			return err
		}
	}
	return nil
}

// restoreAttrsFd is restoreAttrs for a file that is still open, which lets the mode and
// ownership be set on the descriptor rather than the path - no window in which another
// process could swap the path for something else.
func (x *extractor) restoreAttrsFd(f *os.File, path string, it *item.Item) error {
	if x.opts.NoAttrs {
		return nil
	}
	fd := int(f.Fd())
	uid, gid := x.resolveOwner(it)
	if uid != -1 || gid != -1 {
		_ = unix.Fchown(fd, uid, gid)
	}
	if it.Mode != nil {
		if err := unix.Fchmod(fd, uint32(*it.Mode&0o7777)); err != nil {
			return err
		}
	}
	if !x.opts.NoACLs {
		if err := x.restoreACLs(path, it); err != nil {
			return err
		}
	}
	if !x.opts.NoXAttrs && it.XAttrsSet {
		if err := setXAttrs(path, it.XAttrs); err != nil {
			return err
		}
	}
	return nil
}

// restoreFlags applies the item's file flags, and is called after restoreTimes at every
// site rather than from inside restoreAttrs.
//
// The ordering is the whole point: the immutable flag makes every further change to the
// inode impossible, so a restore that set it before the timestamps would lock the file
// against the rest of its own restore. borg puts it last of all attribute restoration for
// that reason and says so in a comment; this keeps it visible at the call sites instead of
// buried where a later change might reorder it.
//
// A failure is swallowed, as borg swallows it: setting the immutable flag needs
// CAP_LINUX_IMMUTABLE, so an unprivileged restore cannot do it, and failing the restore of
// the data over an attribute the user may not be able to set would be the worse answer.
// The consequence is worth knowing - a non-root restore of an immutable file gives back
// the right contents without the flag - and it is borg's behaviour exactly.
func (x *extractor) restoreFlags(path string, it *item.Item) error {
	if x.opts.NoAttrs || x.opts.NoFlags || it.BSDFlags == nil || it.Mode == nil {
		return nil
	}
	_ = setFlags(path, *it.BSDFlags, uint32(*it.Mode))
	return nil
}

// restoreTimes sets atime, mtime and, where the filesystem supports it, birthtime.
//
// Times are nanoseconds since the epoch in the archive, and are restored with nanosecond
// precision. A filesystem that stores less keeps what it can; that is a property of the
// destination, not a loss in the archive.
func (x *extractor) restoreTimes(path string, it *item.Item) error {
	if x.opts.NoAttrs || it.MTime == nil {
		return nil
	}
	mtime := *it.MTime
	atime := mtime
	if it.ATime != nil {
		atime = *it.ATime
	}
	times := []unix.Timespec{
		unix.NsecToTimespec(atime),
		unix.NsecToTimespec(mtime),
	}
	// AT_SYMLINK_NOFOLLOW so a symlink's own timestamps are set. Some filesystems refuse
	// that; borg tolerates the failure on POSIX and so does borge, because the alternative
	// is failing a restore over a symlink's atime.
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			return nil
		}
		return err
	}
	return nil
}

// resolveOwner decides the uid and gid to restore.
//
// Names win over numbers unless NumericIDs is set, because a restore onto a different
// machine should put files in the hands of the same *user*, not of whoever happens to
// hold that number there. A name that does not resolve falls back to the stored number,
// and -1 means "leave it alone".
func (x *extractor) resolveOwner(it *item.Item) (uid, gid int) {
	uid, gid = -1, -1
	if it.UID != nil && *it.UID >= 0 {
		uid = int(*it.UID)
	}
	if it.GID != nil && *it.GID >= 0 {
		gid = int(*it.GID)
	}
	if x.opts.NumericIDs {
		return uid, gid
	}
	if it.User != nil && *it.User != "" {
		if n := x.lookupUID(*it.User); n >= 0 {
			uid = n
		}
	}
	if it.Group != nil && *it.Group != "" {
		if n := x.lookupGID(*it.Group); n >= 0 {
			gid = n
		}
	}
	return uid, gid
}

// lookupUID resolves a user name to a uid, remembering the answer.
//
// -1 means the name does not resolve here, and that is cached as firmly as a hit: see the
// note on extractor.uids for why.
func (x *extractor) lookupUID(name string) int {
	if id, ok := x.uids[name]; ok {
		return id
	}
	id := -1
	if u, err := user.Lookup(name); err == nil {
		if n, err := strconv.Atoi(u.Uid); err == nil {
			id = n
		}
	}
	x.uids[name] = id
	return id
}

// lookupGID resolves a group name to a gid, remembering the answer.
func (x *extractor) lookupGID(name string) int {
	if id, ok := x.gids[name]; ok {
		return id
	}
	id := -1
	if g, err := user.LookupGroup(name); err == nil {
		if n, err := strconv.Atoi(g.Gid); err == nil {
			id = n
		}
	}
	x.gids[name] = id
	return id
}

// setXAttrs writes an item's extended attributes.
//
// Names are restored in sorted order so a restore is reproducible; the kernel does not
// preserve order anyway. A namespace the caller may not write (security.* and trusted.*
// without privilege) is skipped rather than failing the file.
func setXAttrs(path string, attrs map[string][]byte) error {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		err := unix.Lsetxattr(path, name, attrs[name], 0)
		if err == nil {
			continue
		}
		switch {
		case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES),
			errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP):
			// Not permitted or not supported here. borg warns and carries on; failing the
			// restore of the file's *contents* over an attribute would be the wrong trade.
			continue
		default:
			return err
		}
	}
	return nil
}

// GetXAttrs reads a path's extended attributes without following symlinks. It is here
// rather than in a test helper because the extraction comparator needs it too.
func GetXAttrs(path string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil, err
	}

	out := map[string][]byte{}
	for _, name := range splitNulTerminated(buf[:size]) {
		vsize, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			continue
		}
		value := make([]byte, vsize)
		if vsize > 0 {
			if _, err := unix.Lgetxattr(path, name, value); err != nil {
				continue
			}
		}
		out[name] = value
	}
	return out, nil
}

func splitNulTerminated(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// makeSpecial creates a FIFO or a device node.
func makeSpecial(path string, mode int64, it *item.Item) error {
	switch {
	case item.IsFIFO(mode):
		return unix.Mkfifo(path, uint32(mode&0o7777))
	case item.IsDevice(mode):
		rdev := 0
		if it.RDev != nil {
			rdev = int(*it.RDev)
		}
		// Creating a device node needs privilege. Without it the whole restore would fail
		// on one node, so the failure is reported to the caller, which decides.
		return unix.Mknod(path, uint32(mode&0o7777)|deviceTypeBits(mode), rdev)
	default:
		return errUnknownItemType(mode)
	}
}

func deviceTypeBits(mode int64) uint32 {
	if mode&item.SIFMT == item.SIFBLK {
		return unix.S_IFBLK
	}
	return unix.S_IFCHR
}

func errUnknownItemType(mode int64) error {
	return errors.New("archive: unknown item type in mode 0o" + strconv.FormatInt(mode, 8))
}

// linkNoFollow creates a hard link without following a symlink at the source.
//
// The distinction matters for security, not tidiness: see tryHardlink.
func linkNoFollow(oldpath, newpath string) error {
	err := unix.Linkat(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		// A filesystem without linkat's no-follow behaviour. Falling back to link() would
		// silently reintroduce the vulnerability, so report it instead.
		return err
	}
	return err
}
