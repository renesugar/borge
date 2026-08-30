// SPDX-License-Identifier: Apache-2.0
//
// The rendering below reproduces Python's stat.filemode, which is what borg's output
// format uses, so that borge's listings can be compared against borg's line for line.

package item

// File type bits, as stored in an item's mode. These are the POSIX values, not Go's
// os.FileMode: the mode in an archive is the raw st_mode from the filesystem, and Go's
// abstraction over it does not round-trip (it cannot express setuid-with-execute, and it
// spells a symlink differently).
const (
	SIFMT   = 0o170000
	SIFSOCK = 0o140000
	SIFLNK  = 0o120000
	SIFREG  = 0o100000
	SIFBLK  = 0o060000
	SIFDIR  = 0o040000
	SIFCHR  = 0o020000
	SIFIFO  = 0o010000

	SISUID = 0o4000
	SISGID = 0o2000
	SISVTX = 0o1000

	SIRUSR = 0o400
	SIWUSR = 0o200
	SIXUSR = 0o100
	SIRGRP = 0o040
	SIWGRP = 0o020
	SIXGRP = 0o010
	SIROTH = 0o004
	SIWOTH = 0o002
	SIXOTH = 0o001
)

// modeEntry is one candidate character for one position of the rendered mode.
type modeEntry struct {
	bits int64
	char byte
}

// modeTable is Python's _filemode_table. Order matters within each row: the first entry
// whose bits are *all* set wins, which is how "s" (setuid and executable) is
// distinguished from "S" (setuid without execute).
var modeTable = [10][]modeEntry{
	{{SIFLNK, 'l'}, {SIFSOCK, 's'}, {SIFREG, '-'}, {SIFBLK, 'b'}, {SIFDIR, 'd'}, {SIFCHR, 'c'}, {SIFIFO, 'p'}},
	{{SIRUSR, 'r'}},
	{{SIWUSR, 'w'}},
	{{SIXUSR | SISUID, 's'}, {SISUID, 'S'}, {SIXUSR, 'x'}},
	{{SIRGRP, 'r'}},
	{{SIWGRP, 'w'}},
	{{SIXGRP | SISGID, 's'}, {SISGID, 'S'}, {SIXGRP, 'x'}},
	{{SIROTH, 'r'}},
	{{SIWOTH, 'w'}},
	{{SIXOTH | SISVTX, 't'}, {SISVTX, 'T'}, {SIXOTH, 'x'}},
}

// FormatMode renders a mode the way `ls -l` does: "drwxr-xr-x", "-rw-r--r--",
// "lrwxrwxrwx".
//
// An unrecognised file type renders as "?", not "-". That is what CPython's stat.filemode
// produces - the C implementation in the _stat module, which is the one that actually runs
// and which differs from the pure-Python fallback in exactly this character. It only shows
// up for a mode with no file type bits, which no real item has.
//
// **Reviewed under ROADMAP R0 T9 on 2026-08-30 and kept, which is a decision rather than
// the reproduction it used to be.** R0.1 listed this among the borg quirks to correct once
// compatibility was lifted. Examined with the constraint gone, there is nothing to correct:
// "?" says the file type is unknown, where "-" would assert it is a regular file, and the
// second is wrong in a way the first is not. borge's own documentation never claimed
// otherwise, so the mismatch R0.1 recorded was between borg's output and CPython's
// documented fallback - inherited by copying borg, and harmless.
func FormatMode(mode int64) string {
	out := make([]byte, 0, 10)
	for i, row := range modeTable {
		c := byte('-')
		if i == 0 {
			c = '?'
		}
		for _, e := range row {
			if mode&e.bits == e.bits {
				c = e.char
				break
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// TypeChar is the single character naming an item's kind, the first character of the
// rendered mode.
func TypeChar(mode int64) string { return FormatMode(mode)[:1] }

// IsDir, IsSymlink, IsRegular and IsFIFO classify an item's mode.
func IsDir(mode int64) bool     { return mode&SIFMT == SIFDIR }
func IsSymlink(mode int64) bool { return mode&SIFMT == SIFLNK }
func IsRegular(mode int64) bool { return mode&SIFMT == SIFREG }
func IsFIFO(mode int64) bool    { return mode&SIFMT == SIFIFO }

// IsDevice reports whether the mode is a block or character device.
func IsDevice(mode int64) bool {
	t := mode & SIFMT
	return t == SIFBLK || t == SIFCHR
}

// Mode returns an item's mode, or zero if it carries none.
func (i *Item) ModeOr(def int64) int64 {
	if i.Mode == nil {
		return def
	}
	return *i.Mode
}

// IsDir and friends, as methods, for the common case of asking about an item.
func (i *Item) IsDir() bool     { return IsDir(i.ModeOr(0)) }
func (i *Item) IsSymlink() bool { return IsSymlink(i.ModeOr(0)) }
func (i *Item) IsRegular() bool { return IsRegular(i.ModeOr(0)) }
func (i *Item) IsDevice() bool  { return IsDevice(i.ModeOr(0)) }
func (i *Item) IsFIFO() bool    { return IsFIFO(i.ModeOr(0)) }

// ContentSize is the item's size in bytes: the sum of its chunk sizes.
//
// It is computed rather than read from the stored `size` field, because that field is
// borg 1.x's and is not written by borg 2 - the chunk list is the authority.
func (i *Item) ContentSize() int64 {
	var total int64
	for _, c := range i.Chunks {
		total += c.Size
	}
	return total
}
