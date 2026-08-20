// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of TarfileObjectProcessors in borg's src/borg/archive.py and
// the import-tar command body in src/borg/archiver/tar_cmds.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"archive/tar"
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
)

// Importing a tar stream as an archive.
//
// # Why this is not just "extract, then create"
//
// It could be - and for a tar on disk the result would be the same. What it would also be
// is a second full copy on the filesystem, and a requirement that the filesystem can
// represent everything in the tar. Importing directly means a tar arriving on a pipe
// never lands anywhere, and it means device nodes and ACLs survive on a machine where
// creating them would need root.
//
// # What tar cannot tell us, and what is done about it
//
// A tar header carries less than a borg item does. Nothing in it records birthtime, BSD
// flags, or which entries share an inode as opposed to merely being linked. The standard
// pax records add sub-second times, extended attributes and ACLs, and that is where the
// PAX format stops.
//
// The BORG format goes further: it carries the entire item as a msgpacked pax record, so
// an import restores exactly what an export wrote. When that record is present it is used
// *instead of* the tar header, not merged with it - borg asserts on the version and then
// trusts the record, and a merge would mean two sources of truth for the same field.
//
// # Hard links become shared content, not shared inodes
//
// tar's model is that the first entry for an inode is a regular file and the rest are
// link entries naming it. borg turns each later entry into an ordinary item that reuses
// the first one's chunk list. Restoring gives two files with identical content rather
// than two names for one inode - the link relationship itself is not preserved.
//
// That is borg's behaviour and borge matches it, so an export-tar/import-tar round trip
// through PAX quietly unshares hard links. The BORG format does keep the hlid, because it
// keeps everything.

// ImportTarOptions control an import.
type ImportTarOptions struct {
	// Name is the archive to create.
	Name string
	// Comment, Tags and Timestamp go into the archive metadata as they would for create.
	Comment   string
	Tags      []string
	Timestamp time.Time

	// ChunkerParams is how file content is split.
	ChunkerParams chunker.Params
	// ChunkSeed is the repository key's chunk seed.
	ChunkSeed uint32
	// Compressor compresses the content.
	Compressor compress.Compressor

	// IgnoreZeros keeps reading past the end-of-archive marker, which is what allows
	// several tars concatenated into one stream to be imported as one archive.
	IgnoreZeros bool

	// OnItem is called for each entry with borg's status character: A added file,
	// d directory, s symlink, h hard link, b/c device, f fifo, E unsupported.
	OnItem func(status byte, path string)
	// OnWarning reports an entry that could not be imported.
	OnWarning func(path, reason string)

	// CommandLine and CWD go into the archive metadata, as they do for create. borg's
	// import-tar records both; borge recorded neither until 2026-08-18, so an archive it
	// imported had no answer to "what made this?" - and "info" and the JSON API both
	// report that field.
	CommandLine string
	CWD         string
}

// ImportTarStats counts what an import wrote.
type ImportTarStats struct {
	Items     int
	Files     int
	Hardlinks int
	Skipped   int
	Bytes     int64

	// FileStatus counts entries by their status character, borg's files_stats.
	FileStatus map[string]int64
	// Meta is the archive metadata as saved, so a caller can report the times and the
	// command line without reopening the archive it just wrote.
	Meta *item.ArchiveItem
	// OriginalSize is the builder's count, sampled after Save, which is borg's rule for
	// the figure "--stats" and "--json" report. Not Bytes: that is the content read from
	// the tar, where this also carries the item pointers and the archive object. See
	// Builder.AddChunk and docs/DIVERGENCES.md #36.
	OriginalSize int64
	// Timings carries the builder's hashing and chunking times, which "import-tar --json"
	// reports exactly as create does.
	Timings Stats
}

// ImportTar reads a tar stream and writes it as a new archive.
func ImportTar(m *manifest.Manifest, r io.Reader, opts ImportTarOptions) (*ImportTarStats, []byte, error) {
	stats := &ImportTarStats{FileStatus: map[string]int64{}}
	if opts.Name == "" {
		return stats, nil, fmt.Errorf("archive: an archive needs a name")
	}

	b, err := NewBuilder(m, BuilderOptions{
		ChunkerParams: opts.ChunkerParams,
		ChunkSeed:     opts.ChunkSeed,
		Compressor:    opts.Compressor,
	})
	if err != nil {
		return stats, nil, err
	}

	caller := opts.OnItem
	if caller == nil {
		caller = func(byte, string) {}
	}
	// Counted here rather than in the caller's callback: with no --list there is no
	// callback doing anything, and a count taken from printed lines would be zero.
	report := func(status byte, path string) {
		stats.FileStatus[string(status)]++
		caller(status, path)
	}
	warn := opts.OnWarning
	if warn == nil {
		warn = func(string, string) {}
	}

	imp := &tarImporter{builder: b, stats: stats, report: report, warn: warn,
		contentOf: map[string][]item.ChunkListEntry{}}

	// bufio, so --ignore-zeros can look ahead without consuming: see readAll.
	src := bufio.NewReaderSize(r, 1<<16)
	if err := imp.readAll(src, opts.IgnoreZeros); err != nil {
		return stats, nil, err
	}

	ts := opts.Timestamp
	if ts.IsZero() {
		ts = b.Start()
	}
	meta, id, err := b.Save(SaveOptions{
		Name:        opts.Name,
		Comment:     opts.Comment,
		Tags:        opts.Tags,
		Timestamp:   ts,
		CommandLine: opts.CommandLine,
		CWD:         opts.CWD,
	})
	if err != nil {
		return stats, nil, err
	}
	stats.Meta = meta
	stats.OriginalSize = b.Stats().OriginalSize
	stats.Timings = b.Stats()
	return stats, id, nil
}

type tarImporter struct {
	builder *Builder
	stats   *ImportTarStats
	report  func(byte, string)
	warn    func(path, reason string)

	// contentOf remembers every regular file's chunk list by path, because a later link
	// entry can name any earlier one - so all of them have to be kept, not just those
	// that turn out to be linked.
	contentOf map[string][]item.ChunkListEntry
}

// readAll reads entries until the stream ends.
//
// With ignoreZeros, the end-of-archive marker is not the end: several tars concatenated
// (and each padded out to its blocking factor) are read as one. Go's tar reader stops at
// the marker, so a fresh reader is started on the same buffered source, which still holds
// whatever the previous one did not consume. Each retry eats one 1024-byte marker or pad,
// and the peek is what distinguishes "more padding" from "actually finished".
func (imp *tarImporter) readAll(src *bufio.Reader, ignoreZeros bool) error {
	for {
		tr := tar.NewReader(src)
		if err := imp.readOne(tr); err != nil {
			return err
		}
		if !ignoreZeros {
			return nil
		}
		if _, err := src.Peek(1); err != nil {
			return nil // nothing left, padding or otherwise
		}
	}
}

// readOne reads entries until this tar reader reaches its end-of-archive marker.
func (imp *tarImporter) readOne(tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: reading tar: %w", err)
		}
		if err := imp.entry(tr, hdr); err != nil {
			return err
		}
	}
}

// entry converts one tar header, and its content if it has any, into an item.
func (imp *tarImporter) entry(tr *tar.Reader, hdr *tar.Header) error {
	mode, status, ok := modeForTypeflag(hdr.Typeflag)
	if !ok {
		imp.warn(hdr.Name, fmt.Sprintf("unsupported tar entry type %q", hdr.Typeflag))
		imp.report('E', hdr.Name)
		imp.stats.Skipped++
		return nil
	}

	it, err := itemFromHeader(hdr, mode)
	if err != nil {
		// A path tar can express but borg cannot store - an absolute path, or one with a
		// ".." in it - is refused rather than silently rewritten into something else.
		imp.warn(hdr.Name, err.Error())
		imp.report('E', hdr.Name)
		imp.stats.Skipped++
		return nil
	}

	switch hdr.Typeflag {
	case tar.TypeReg:
		chunks, err := imp.builder.ChunkFile(tr)
		if err != nil {
			return err
		}
		it.Chunks = chunks
		it.ChunksSet = true
		it.Size = item.OptInt(it.ContentSize())
		imp.contentOf[it.Path] = chunks
		imp.stats.Files++
		imp.stats.Bytes += *it.Size

	case tar.TypeLink:
		// The entry names an earlier path. If that path was in this stream its chunks are
		// reused; if it was not, the link is stored as an empty file rather than failing,
		// which is what borg does with a tar that starts mid-stream.
		target, err := item.MakePathSafe(hdr.Linkname)
		if err != nil {
			imp.warn(hdr.Name, fmt.Sprintf("hard link target %q: %v", hdr.Linkname, err))
			imp.report('E', hdr.Name)
			imp.stats.Skipped++
			return nil
		}
		chunks, found := imp.contentOf[target]
		if !found {
			imp.warn(hdr.Name, fmt.Sprintf("hard link to %q, which is not in this stream; "+
				"stored as an empty file", hdr.Linkname))
		}
		it.Chunks = chunks
		it.ChunksSet = true
		it.Size = item.OptInt(it.ContentSize())
		imp.stats.Hardlinks++
		imp.stats.Files++

	case tar.TypeSymlink:
		it.Target = item.OptString(hdr.Linkname)

	case tar.TypeChar, tar.TypeBlock:
		it.RDev = item.OptInt(int64(unixMakedev(uint32(hdr.Devmajor), uint32(hdr.Devminor))))
	}

	if err := imp.builder.AddItem(it); err != nil {
		return err
	}
	imp.stats.Items++
	imp.report(status, it.Path)
	return nil
}

// modeForTypeflag maps a tar entry type to a file type and borg's status character.
func modeForTypeflag(flag byte) (mode int64, status byte, ok bool) {
	switch flag {
	case tar.TypeReg:
		return item.SIFREG, 'A', true
	case tar.TypeLink:
		// A hard link entry becomes a regular file: see the note at the top of this file.
		return item.SIFREG, 'h', true
	case tar.TypeDir:
		return item.SIFDIR, 'd', true
	case tar.TypeSymlink:
		return item.SIFLNK, 's', true
	case tar.TypeFifo:
		return item.SIFIFO, 'f', true
	case tar.TypeBlock:
		return item.SIFBLK, 'b', true
	case tar.TypeChar:
		return item.SIFCHR, 'c', true
	default:
		return 0, 0, false
	}
}

// itemFromHeader builds an item from a tar header.
//
// A BORG.item.meta record replaces the header outright rather than being merged into it,
// so there is exactly one source of truth for every field. Only the path is re-checked,
// because a path arriving from outside is a security question and not a fidelity one.
func itemFromHeader(hdr *tar.Header, mode int64) (*item.Item, error) {
	if meta, present := hdr.PAXRecords[paxBorgMeta]; present {
		return itemFromBorgRecord(hdr, meta)
	}

	path, err := item.MakePathSafe(hdr.Name)
	if err != nil {
		return nil, err
	}
	it := &item.Item{
		Path: path,
		// tar's Mode holds permission bits only; the file type comes from the typeflag.
		Mode: item.OptInt((hdr.Mode & 0o7777) | mode),
		UID:  item.OptInt(int64(hdr.Uid)),
		GID:  item.OptInt(int64(hdr.Gid)),
	}
	if hdr.Uname != "" {
		it.User = item.OptString(hdr.Uname)
	}
	if hdr.Gname != "" {
		it.Group = item.OptString(hdr.Gname)
	}
	if !hdr.ModTime.IsZero() {
		it.MTime = item.OptInt(hdr.ModTime.UnixNano())
	}
	if !hdr.AccessTime.IsZero() {
		it.ATime = item.OptInt(hdr.AccessTime.UnixNano())
	}
	if !hdr.ChangeTime.IsZero() {
		it.CTime = item.OptInt(hdr.ChangeTime.UnixNano())
	}

	// The pax records Go parses into fields are read from the fields above; the rest are
	// still in the map, which is where the xattrs and ACLs are.
	for k, v := range hdr.PAXRecords {
		switch {
		case strings.HasPrefix(k, paxXAttrPrefix):
			if it.XAttrs == nil {
				it.XAttrs = map[string][]byte{}
				it.XAttrsSet = true
			}
			it.XAttrs[strings.TrimPrefix(k, paxXAttrPrefix)] = []byte(v)
		case k == paxACLAccess:
			it.ACLAccess = []byte(v)
		case k == paxACLDefault:
			it.ACLDefault = []byte(v)
		}
	}
	return it, nil
}

// itemFromBorgRecord decodes a BORG.item.meta pax record.
func itemFromBorgRecord(hdr *tar.Header, meta string) (*item.Item, error) {
	if v := hdr.PAXRecords[paxBorgVersion]; v != borgItemVersion {
		// borg asserts here. A version it does not know means the record's layout is not
		// what it expects, and guessing would produce a corrupt item rather than an error.
		return nil, fmt.Errorf("%s is %q, and only %q is understood",
			paxBorgVersion, v, borgItemVersion)
	}
	raw, err := base64.StdEncoding.DecodeString(meta)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid base64: %w", paxBorgMeta, err)
	}
	it, err := item.UnmarshalItem(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", paxBorgMeta, err)
	}
	// The chunk list in the record names objects in whatever repository the tar came
	// from, which say nothing about this one. It is dropped here and refilled from the
	// stream's own content, so an import can never produce an item pointing at chunks
	// that were never written.
	it.Chunks = nil
	it.ChunksSet = false
	it.ChunksHealthy = nil
	it.ChunksHealthySet = false
	return it, nil
}

// unixMakedev is the inverse of unixMajor/unixMinor.
func unixMakedev(major, minor uint32) uint64 {
	return (uint64(major&0xfff) << 8) | (uint64(major&^uint32(0xfff)) << 32) |
		uint64(minor&0xff) | (uint64(minor&^uint32(0xff)) << 12)
}
