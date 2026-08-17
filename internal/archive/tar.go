// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of item_to_tarinfo, item_to_paxheaders and the export-tar
// command body in borg's src/borg/archiver/tar_cmds.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"archive/tar"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/repoobj"
)

// pax header names for the attributes tar has no field for. These are star/GNU
// conventions that GNU tar and bsdtar both understand, so an exported archive is not a
// borge-only artefact.
const (
	paxXAttrPrefix = "SCHILY.xattr."
	paxACLAccess   = "SCHILY.acl.access"
	paxACLDefault  = "SCHILY.acl.default"
)

// TarFormat selects how much metadata the output carries.
type TarFormat string

const (
	// TarPAX is the POSIX format: it carries sub-second times, long names, extended
	// attributes and ACLs. It is the default because it is the only one that does not
	// silently lose things.
	TarPAX TarFormat = "PAX"
	// TarGNU is the older GNU format: long names work, but xattrs, ACLs and sub-second
	// times do not survive.
	TarGNU TarFormat = "GNU"
)

// TarOptions control an export.
type TarOptions struct {
	Format TarFormat
	// Filter selects which items are exported. Nil exports everything.
	Filter func(*item.Item) bool
	// StripComponents removes this many leading path components.
	StripComponents int
	// OnWarning reports an item that could not be represented.
	OnWarning func(path, reason string)
}

// TarStats counts what an export wrote.
type TarStats struct {
	Items     int
	Files     int
	Hardlinks int
	Skipped   int
	Bytes     int64
}

// ExportTar writes the archive as a tar stream.
//
// # What tar cannot carry
//
// A tar archive is a weaker container than a borg one. Sockets have no representation at
// all, and in the GNU format extended attributes, ACLs and sub-second timestamps have
// nowhere to go. Anything that cannot be represented is *reported*, not dropped quietly -
// an export that silently loses a file's ACLs is worse than one that says so, because the
// user believes they have a faithful copy.
func (a *Archive) ExportTar(w io.Writer, opts TarOptions) (*TarStats, error) {
	format := opts.Format
	if format == "" {
		format = TarPAX
	}
	warn := opts.OnWarning
	if warn == nil {
		warn = func(string, string) {}
	}

	tw := tar.NewWriter(w)
	stats := &TarStats{}
	// hardlinks maps an hlid to the path already written for it, so the second and later
	// links become tar hard link entries rather than second copies of the content.
	hardlinks := map[string]string{}

	err := a.Items(func(it *item.Item) error {
		if opts.Filter != nil && !opts.Filter(it) {
			return nil
		}
		name := stripComponents(it.Path, opts.StripComponents)
		if name == "" {
			return nil
		}

		hdr, needsContent, err := tarHeader(it, name, format, hardlinks, warn)
		if err != nil {
			return err
		}
		if hdr == nil {
			stats.Skipped++
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		stats.Items++

		if !needsContent {
			if hdr.Typeflag == tar.TypeLink {
				stats.Hardlinks++
			}
			return nil
		}
		stats.Files++

		var written int64
		if err := a.fetchInto(it, func(data []byte) error {
			n, err := tw.Write(data)
			written += int64(n)
			return err
		}); err != nil {
			return err
		}
		stats.Bytes += written
		if written != hdr.Size {
			// tar records the size in the header, before the data. A mismatch means the
			// stream is malformed from here on, so it is an error rather than a warning.
			return fmt.Errorf("archive: %s: header says %d bytes, wrote %d", it.Path, hdr.Size, written)
		}
		return nil
	})
	if err != nil {
		tw.Close()
		return stats, err
	}
	return stats, tw.Close()
}

// fetchInto reads an item's content chunks in order.
func (a *Archive) fetchInto(it *item.Item, fn func([]byte) error) error {
	for _, c := range it.Chunks {
		obj, err := a.repo.Get(c.ID)
		if err != nil {
			return fmt.Errorf("archive: chunk %s: %w", hex.EncodeToString(c.ID), err)
		}
		_, data, err := a.ro.Parse(c.ID, obj, repoobj.TypeFileStream, repoobj.ParseOptions{})
		if err != nil {
			return err
		}
		if err := fn(data); err != nil {
			return err
		}
	}
	return nil
}

// tarHeader builds the tar header for one item. A nil header means "cannot be
// represented"; the reason has already been reported.
func tarHeader(it *item.Item, name string, format TarFormat, hardlinks map[string]string,
	warn func(path, reason string),
) (*tar.Header, bool, error) {
	mode := it.ModeOr(0o100644)

	hdr := &tar.Header{
		Name: name,
		Mode: mode & 0o7777,
		Uid:  int(derefInt(it.UID)),
		Gid:  int(derefInt(it.GID)),
	}
	if hdr.Uid < 0 {
		hdr.Uid = 0
	}
	if hdr.Gid < 0 {
		hdr.Gid = 0
	}
	hdr.Uname = derefStr(it.User)
	hdr.Gname = derefStr(it.Group)
	if it.MTime != nil {
		hdr.ModTime = time.Unix(0, *it.MTime)
	}

	needsContent := false
	switch {
	case item.IsRegular(mode):
		hdr.Typeflag = tar.TypeReg
		if key := hlidKey(it); key != "" {
			if target, ok := hardlinks[key]; ok {
				// A later link to an inode already written: tar records it as a link to
				// the first, which is what makes a restore recreate the hard link rather
				// than two independent copies.
				hdr.Typeflag = tar.TypeLink
				hdr.Linkname = target
				break
			}
			hardlinks[key] = name
		}
		hdr.Size = it.ContentSize()
		needsContent = true

	case item.IsDir(mode):
		hdr.Typeflag = tar.TypeDir
		// tar's convention: a directory entry's name ends in a slash.
		if len(hdr.Name) > 0 && hdr.Name[len(hdr.Name)-1] != '/' {
			hdr.Name += "/"
		}

	case item.IsSymlink(mode):
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = derefStr(it.Target)

	case item.IsFIFO(mode):
		hdr.Typeflag = tar.TypeFifo

	case item.IsDevice(mode):
		if mode&item.SIFMT == item.SIFBLK {
			hdr.Typeflag = tar.TypeBlock
		} else {
			hdr.Typeflag = tar.TypeChar
		}
		rdev := derefInt(it.RDev)
		hdr.Devmajor = int64(unixMajor(uint64(rdev)))
		hdr.Devminor = int64(unixMinor(uint64(rdev)))

	default:
		warn(it.Path, fmt.Sprintf("file type 0o%o has no tar representation", mode&item.SIFMT))
		return nil, false, nil
	}

	if format == TarGNU {
		// GNU format: no pax records, so the attributes below have nowhere to go. Say so
		// once per item that actually has some, rather than leaving the user to discover
		// it from a restore.
		hdr.Format = tar.FormatGNU
		if it.XAttrsSet && len(it.XAttrs) > 0 {
			warn(it.Path, "extended attributes are not carried by the GNU tar format")
		}
		if len(it.ACLAccess) > 0 || len(it.ACLDefault) > 0 {
			warn(it.Path, "ACLs are not carried by the GNU tar format")
		}
		return hdr, needsContent, nil
	}

	hdr.Format = tar.FormatPAX
	hdr.PAXRecords = map[string]string{}
	// Sub-second times, which tar's own fields cannot hold.
	if it.ATime != nil {
		hdr.AccessTime = time.Unix(0, *it.ATime)
	}
	if it.CTime != nil {
		hdr.ChangeTime = time.Unix(0, *it.CTime)
	}
	if it.XAttrsSet {
		names := make([]string, 0, len(it.XAttrs))
		for k := range it.XAttrs {
			names = append(names, k)
		}
		sort.Strings(names) // a deterministic export is one that can be compared
		for _, k := range names {
			hdr.PAXRecords[paxXAttrPrefix+k] = string(it.XAttrs[k])
		}
	}
	if len(it.ACLAccess) > 0 {
		hdr.PAXRecords[paxACLAccess] = string(it.ACLAccess)
	}
	if len(it.ACLDefault) > 0 {
		hdr.PAXRecords[paxACLDefault] = string(it.ACLDefault)
	}
	if len(hdr.PAXRecords) == 0 {
		hdr.PAXRecords = nil
	}
	return hdr, needsContent, nil
}

// unixMajor and unixMinor split a device number the way Linux does.
func unixMajor(dev uint64) uint32 {
	return uint32((dev>>8)&0xfff) | uint32((dev>>32)&^uint64(0xfff))
}

func unixMinor(dev uint64) uint32 {
	return uint32(dev&0xff) | uint32((dev>>12)&^uint64(0xff))
}

func stripComponents(path string, n int) string {
	if n <= 0 {
		return path
	}
	parts := strings.Split(path, "/")
	if len(parts) <= n {
		return ""
	}
	return strings.Join(parts[n:], "/")
}
