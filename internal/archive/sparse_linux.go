// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the sparse-file handling in borg's src/borg/chunkers/reader.pyx
// (sparsemap and FileReader's hole ranges).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// --sparse: reading a sparse file without reading its holes.
//
// # What it does not change
//
// Not one stored byte. An all-zero region is recorded by size with no data whether or not
// this is used, because both tools detect an all-zero block "regardless of sparse mode"
// (borg's reader.pyx says so in as many words). What changes is the *reading*: a 100 GB
// file with 1 GB of data in it costs 1 GB of reads instead of 100 GB.
//
// So an archive made with --sparse is identical to one made without it, and the option can
// be added, removed or ignored without any effect a later command could detect. That is
// what makes it safe to implement as a reader wrapper rather than as a chunker mode.

// sparseReader reads a file's data ranges and synthesises its holes.
//
// SEEK_DATA and SEEK_HOLE are how the filesystem is asked where the data is. A filesystem
// that does not implement them reports the whole file as data, which is the correct answer
// for a file with no holes and a safe one for anything else - so a failure here costs
// nothing but the optimisation.
type sparseReader struct {
	f   *os.File
	pos int64
	end int64

	// dataEnd is the end of the data range the position is in, or <= pos when the
	// position is in a hole.
	dataEnd int64
	// holeEnd is the end of the hole the position is in.
	holeEnd int64

	zeros []byte
}

// newSparseReader returns a reader over f, or nil if the file has no holes worth skipping.
func newSparseReader(f *os.File, size int64) (io.Reader, error) {
	// A file whose first hole is its end has no holes at all: reading it through this
	// would add seeks and save nothing.
	off, err := unix.Seek(int(f.Fd()), 0, unix.SEEK_HOLE)
	if err != nil || off >= size {
		// Rewind whatever the probe moved, and read it the ordinary way.
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return nil, serr
		}
		return f, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return &sparseReader{f: f, end: size, zeros: make([]byte, 64*1024)}, nil
}

func (r *sparseReader) Read(p []byte) (int, error) {
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if r.pos >= r.dataEnd && r.pos >= r.holeEnd {
		if err := r.locate(); err != nil {
			return 0, err
		}
	}

	if r.pos < r.dataEnd {
		// Inside data: an ordinary read, bounded by the end of the range.
		want := int64(len(p))
		if avail := r.dataEnd - r.pos; avail < want {
			want = avail
		}
		n, err := r.f.ReadAt(p[:want], r.pos)
		r.pos += int64(n)
		if err == io.EOF && r.pos < r.end {
			// The file shrank under us; the caller's change detection is what reports it.
			return n, nil
		}
		return n, err
	}

	// Inside a hole: hand back zeros without touching the disk.
	want := int64(len(p))
	if avail := r.holeEnd - r.pos; avail < want {
		want = avail
	}
	if int64(len(r.zeros)) < want {
		want = int64(len(r.zeros))
	}
	copy(p[:want], r.zeros[:want])
	r.pos += want
	return int(want), nil
}

// locate finds the range the current position is in.
func (r *sparseReader) locate() error {
	fd := int(r.f.Fd())
	// Where does data start at or after pos? If that is pos itself, we are in data and the
	// next hole ends it.
	dataStart, err := unix.Seek(fd, r.pos, unix.SEEK_DATA)
	if err != nil {
		// ENXIO means "no data at or after here": the rest of the file is a hole.
		r.holeEnd = r.end
		r.dataEnd = r.pos
		return nil
	}
	if dataStart > r.pos {
		r.holeEnd = dataStart
		r.dataEnd = r.pos
		return nil
	}
	holeStart, err := unix.Seek(fd, r.pos, unix.SEEK_HOLE)
	if err != nil {
		holeStart = r.end
	}
	r.dataEnd = holeStart
	r.holeEnd = r.pos
	return nil
}
