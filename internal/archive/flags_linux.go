// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of get_flags and set_flags in borg's src/borg/platform/linux.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

//go:build linux

package archive

import (
	"errors"

	"golang.org/x/sys/unix"
)

// File flags are the inode flags "chattr" sets: immutable, append-only, nodump. borg
// stores them under "bsdflags", and stores BSD values rather than the Linux ones the
// kernel reports, so an archive means the same thing on either kind of system.
//
// Only three of them travel. borg maps exactly these and masks everything else out, which
// matters on the restore side: an inode carries flags userspace cannot control, and
// writing back a whole word read from one filesystem onto another would clear them.
const (
	// The BSD values, from Python's stat module, which is what borg's mapping is keyed on.
	bsdNoDump    int64 = 0x00000001 // stat.UF_NODUMP
	bsdImmutable int64 = 0x00000002 // stat.UF_IMMUTABLE
	bsdAppend    int64 = 0x00000004 // stat.UF_APPEND

	// The Linux values, from linux/fs.h. golang.org/x/sys/unix has the two ioctls but not
	// these, so they are written out here with their source.
	linuxImmutable int64 = 0x00000010 // FS_IMMUTABLE_FL
	linuxAppend    int64 = 0x00000020 // FS_APPEND_FL
	linuxNoDump    int64 = 0x00000040 // FS_NODUMP_FL

	// linuxFlagMask is every bit the mapping covers, and the only bits a restore writes.
	linuxFlagMask = linuxImmutable | linuxAppend | linuxNoDump
)

var bsdToLinuxFlag = [...]struct{ bsd, linux int64 }{
	{bsdNoDump, linuxNoDump},
	{bsdImmutable, linuxImmutable},
	{bsdAppend, linuxAppend},
}

// GetFlags reads a path's file flags as borg's bsdflags value.
//
// It answers 0 rather than an error for everything that can go wrong - an unsupported
// filesystem, a path that cannot be opened, an ioctl the kernel refuses - because that is
// what borg does, and because the alternative is failing a backup over an attribute the
// destination may not even have. 0 means "no flags set", and the *caller* decides whether
// to record it: a stored bsdflags of 0 and no stored bsdflags at all are different
// statements, the second being "not examined". See docs/DIVERGENCES.md #8.
func GetFlags(path string, mode uint32) int64 {
	switch mode & unix.S_IFMT {
	case unix.S_IFBLK, unix.S_IFCHR, unix.S_IFLNK:
		// borg skips these deliberately: opening a device file can block for a long time
		// on a device that is not present, and O_NOFOLLOW makes opening a symlink fail
		// anyway.
		return 0
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0
	}
	defer unix.Close(fd)

	linuxFlags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return 0
	}
	var bsd int64
	for _, m := range bsdToLinuxFlag {
		if int64(linuxFlags)&m.linux != 0 {
			bsd |= m.bsd
		}
	}
	return bsd
}

// setFlags applies borg's bsdflags value to a path.
//
// It must run after every other attribute: the immutable flag makes all further changes
// to the inode impossible, so setting it before the times or the mode would lock the file
// against the rest of its own restore. borg says so in a comment and orders it that way;
// so does the caller here.
//
// Only the three mapped bits are written. The current flags are read first and the rest
// are preserved, because an inode carries bits userspace does not control and writing a
// whole word back would clear them - borg learned that as its issue #9039.
func setFlags(path string, bsdFlags int64, mode uint32) error {
	switch mode & unix.S_IFMT {
	case unix.S_IFBLK, unix.S_IFCHR, unix.S_IFLNK:
		return nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(fd)

	current, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		// Give up silently, as borg does, and for its reason: without the current value
		// there is no way to change the three bits without guessing at the others.
		return nil
	}
	var want int64
	for _, m := range bsdToLinuxFlag {
		if bsdFlags&m.bsd != 0 {
			want |= m.linux
		}
	}
	updated := (int64(current) &^ linuxFlagMask) | (want & linuxFlagMask)
	if updated == int64(current) {
		// Nothing to do, and worth skipping rather than writing the same value back: the
		// common case is a file with no flags on a filesystem that has none to set, and
		// an ioctl that changes nothing can still fail.
		return nil
	}
	if err := unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(updated)); err != nil {
		// EOPNOTSUPP is a filesystem without flag support. Linux 6.17 returns ENOTTY
		// where it means EOPNOTSUPP, which borg works around by accepting both.
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) {
			return nil
		}
		return err
	}
	return nil
}

// sfDataless is macOS's SF_DATALESS, the flag on a placeholder whose content lives in
// cloud storage rather than on this machine. borg falls back to the literal value for the
// same reason borge uses one: it is only in Python's stat module from 3.13, and it is not
// in any Linux header at all.
//
// It appears in this file, which is the Linux flag translation, because that is where the
// bsdflags word borge stores is defined - and an archive made on macOS by borg carries the
// flag into a word borge reads on Linux. Nothing on Linux sets it, so --exclude-dataless
// excludes nothing here; the constant is what makes the option mean the same thing when
// borge is built for macOS.
const sfDataless = 0x40000000
