// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of acl_set and the ACL text conversions in borg's
// src/borg/platform/linux.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

//go:build linux

package archive

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os/user"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/renesugar/borge/internal/item"
)

// POSIX ACLs, without libacl.
//
// borg calls into libacl through Cython. borge writes the kernel's binary ACL
// representation to the `system.posix_acl_access` and `system.posix_acl_default` extended
// attributes directly, which is exactly what libacl does underneath - the library is a
// text parser and a struct packer, not a privileged interface. Avoiding it keeps borge
// free of cgo, which docs/PORTING_PLAN.md §0.4 asks for.
//
// # The stored text
//
// borg stores an ACL as text, one entry per line, produced by acl_to_any_text with
// numeric ids and then rewritten to carry both a name and a number:
//
//	user::rwx            the owner
//	user:alice:r-x:1000  a named user, with its uid as a fourth field
//	group::r-x           the owning group
//	group:staff:r--:50   a named group, with its gid
//	mask::rwx
//	other::r--
//
// The fourth field is what makes a restore onto another machine work: the name is tried
// first, and the number is the fallback when it does not resolve there.

// ACL entry tags, from <linux/posix_acl.h>.
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20

	aclXattrVersion = 0x0002
	aclUndefinedID  = 0xFFFFFFFF

	xattrACLAccess  = "system.posix_acl_access"
	xattrACLDefault = "system.posix_acl_default"
)

// aclEntry is one parsed ACL entry.
type aclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

// ErrACLUnsupported means an ACL text form borge cannot represent.
var ErrACLUnsupported = errors.New("archive: unsupported ACL entry")

// restoreACLs applies an item's access and default ACLs.
func (x *extractor) restoreACLs(path string, it *item.Item) error {
	// Linux cannot set ACLs on a symlink; borg returns early for the same reason.
	if it.IsSymlink() {
		return nil
	}
	if len(it.ACLNFS4) > 0 {
		// NFSv4 ACLs are a FreeBSD thing that borg stores in a different format entirely.
		// Counting them rather than failing keeps a Linux restore of a FreeBSD archive
		// working, with the omission reported instead of hidden.
		x.stats.SkippedACL++
	}
	if err := x.applyACL(path, it.ACLAccess, xattrACLAccess); err != nil {
		return err
	}
	return x.applyACL(path, it.ACLDefault, xattrACLDefault)
}

func (x *extractor) applyACL(path string, text []byte, attr string) error {
	if len(text) == 0 {
		return nil
	}
	entries, err := parseACLText(string(text), x.opts.NumericIDs)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	packed := packACL(entries)
	if err := unix.Lsetxattr(path, attr, packed, 0); err != nil {
		switch {
		case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP),
			errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES),
			errors.Is(err, unix.EINVAL):
			// The filesystem does not carry ACLs, or will not take this one. borg treats
			// ENOTSUP the same way; the file's contents matter more than its ACL.
			x.stats.SkippedACL++
			return nil
		default:
			return fmt.Errorf("archive: setting %s on %s: %w", attr, path, err)
		}
	}
	return nil
}

// parseACLText turns borg's stored ACL text into entries.
//
// numericIDs uses the stored number and ignores the name. Otherwise the name is looked up
// on this machine first, which is what makes "alice" mean the local alice rather than
// whoever holds uid 1000 here.
func parseACLText(text string, numericIDs bool) ([]aclEntry, error) {
	var out []aclEntry
	for _, line := range strings.Split(text, "\n") {
		// Strip a trailing comment: libacl appends "#effective:..." when the mask
		// restricts an entry, and it is not part of the ACL.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			return nil, fmt.Errorf("%w: %q", ErrACLUnsupported, line)
		}
		kind, name, perms := fields[0], fields[1], fields[2]
		stored := ""
		if len(fields) >= 4 {
			stored = fields[3]
		}

		perm, err := parseACLPerms(perms)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", err, line)
		}

		e := aclEntry{perm: perm, id: aclUndefinedID}
		switch {
		case kind == "user" || kind == "u":
			if name == "" {
				e.tag = aclUserObj
			} else {
				e.tag = aclUser
				id, err := resolveUser(name, stored, numericIDs)
				if err != nil {
					return nil, err
				}
				e.id = id
			}
		case kind == "group" || kind == "g":
			if name == "" {
				e.tag = aclGroupObj
			} else {
				e.tag = aclGroup
				id, err := resolveGroup(name, stored, numericIDs)
				if err != nil {
					return nil, err
				}
				e.id = id
			}
		case kind == "mask" || kind == "m":
			e.tag = aclMask
		case kind == "other" || kind == "o":
			e.tag = aclOther
		case kind == "default":
			// A "default:" prefix belongs to a default ACL, which borg stores in its own
			// field, so seeing it here means the text was assembled differently than
			// expected. Refuse rather than silently applying it to the access ACL.
			return nil, fmt.Errorf("%w: a default: prefix in an access ACL (%q)", ErrACLUnsupported, line)
		default:
			return nil, fmt.Errorf("%w: %q", ErrACLUnsupported, line)
		}
		out = append(out, e)
	}
	return out, nil
}

func parseACLPerms(s string) (uint16, error) {
	if len(s) != 3 {
		return 0, ErrACLUnsupported
	}
	var perm uint16
	for i, want := range []byte{'r', 'w', 'x'} {
		switch s[i] {
		case want:
			perm |= uint16(4 >> i)
		case '-':
		default:
			// "X" and other libacl spellings do not occur in a stored ACL, which comes
			// from acl_to_any_text rather than from a user.
			return 0, ErrACLUnsupported
		}
	}
	return perm, nil
}

// resolveUser turns an ACL's name/number pair into a uid.
func resolveUser(name, stored string, numericIDs bool) (uint32, error) {
	if !numericIDs {
		if u, err := user.Lookup(name); err == nil {
			if n, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
				return uint32(n), nil
			}
		}
	}
	return fallbackID(name, stored)
}

func resolveGroup(name, stored string, numericIDs bool) (uint32, error) {
	if !numericIDs {
		if g, err := user.LookupGroup(name); err == nil {
			if n, err := strconv.ParseUint(g.Gid, 10, 32); err == nil {
				return uint32(n), nil
			}
		}
	}
	return fallbackID(name, stored)
}

// fallbackID uses the stored number, or the name itself when it is one.
func fallbackID(name, stored string) (uint32, error) {
	for _, candidate := range []string{stored, name} {
		if candidate == "" {
			continue
		}
		if n, err := strconv.ParseUint(candidate, 10, 32); err == nil {
			return uint32(n), nil
		}
	}
	return 0, fmt.Errorf("%w: cannot resolve %q to an id", ErrACLUnsupported, name)
}

// packACL renders entries as the kernel's binary ACL attribute.
//
// The kernel requires entries in tag order, so they are sorted rather than written in the
// order they were read: libacl sorts on the way in, and an unsorted attribute is rejected
// with EINVAL.
func packACL(entries []aclEntry) []byte {
	sorted := append([]aclEntry(nil), entries...)
	// Insertion sort: an ACL has a handful of entries, and this keeps equal tags in their
	// original relative order, which matters for several named users.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].tag < sorted[j-1].tag; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	buf := make([]byte, 4+8*len(sorted))
	binary.LittleEndian.PutUint32(buf[0:4], aclXattrVersion)
	for i, e := range sorted {
		off := 4 + 8*i
		binary.LittleEndian.PutUint16(buf[off:off+2], e.tag)
		binary.LittleEndian.PutUint16(buf[off+2:off+4], e.perm)
		binary.LittleEndian.PutUint32(buf[off+4:off+8], e.id)
	}
	return buf
}

// GetACLText reads a path's ACL back as text in borg's stored form, for the extraction
// comparator. It returns "" when the path carries no ACL.
func GetACLText(path, attr string, numericIDs bool) (string, error) {
	size, err := unix.Lgetxattr(path, attr, nil)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
			return "", nil
		}
		return "", err
	}
	buf := make([]byte, size)
	if size > 0 {
		if _, err := unix.Lgetxattr(path, attr, buf); err != nil {
			return "", err
		}
	}
	entries, err := unpackACL(buf)
	if err != nil {
		return "", err
	}

	var lines []string
	for _, e := range entries {
		perms := formatACLPerms(e.perm)
		switch e.tag {
		case aclUserObj:
			lines = append(lines, "user::"+perms)
		case aclUser:
			lines = append(lines, fmt.Sprintf("user:%d:%s", e.id, perms))
		case aclGroupObj:
			lines = append(lines, "group::"+perms)
		case aclGroup:
			lines = append(lines, fmt.Sprintf("group:%d:%s", e.id, perms))
		case aclMask:
			lines = append(lines, "mask::"+perms)
		case aclOther:
			lines = append(lines, "other::"+perms)
		default:
			return "", fmt.Errorf("%w: tag 0x%02x", ErrACLUnsupported, e.tag)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func unpackACL(buf []byte) ([]aclEntry, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	if len(buf) < 4 || (len(buf)-4)%8 != 0 {
		return nil, fmt.Errorf("%w: attribute is %d bytes", ErrACLUnsupported, len(buf))
	}
	if v := binary.LittleEndian.Uint32(buf[:4]); v != aclXattrVersion {
		return nil, fmt.Errorf("%w: ACL version %d", ErrACLUnsupported, v)
	}
	var out []aclEntry
	for off := 4; off < len(buf); off += 8 {
		out = append(out, aclEntry{
			tag:  binary.LittleEndian.Uint16(buf[off : off+2]),
			perm: binary.LittleEndian.Uint16(buf[off+2 : off+4]),
			id:   binary.LittleEndian.Uint32(buf[off+4 : off+8]),
		})
	}
	return out, nil
}

func formatACLPerms(perm uint16) string {
	out := []byte("---")
	if perm&4 != 0 {
		out[0] = 'r'
	}
	if perm&2 != 0 {
		out[1] = 'w'
	}
	if perm&1 != 0 {
		out[2] = 'x'
	}
	return string(out)
}
