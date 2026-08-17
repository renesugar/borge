// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ArchiveRecreater in borg's src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repoobj"
)

// Recreating an archive: rewriting it with different chunking, different compression, or
// fewer files.
//
// # What it is for
//
// Three things a backup accumulates and cannot otherwise shed:
//
//   - Files that should never have been archived. Excluding them from *future* backups
//     does nothing about the ones already stored.
//   - Compression settings that turned out wrong. Recompressing needs the plaintext, which
//     means reading and rewriting.
//   - Chunker parameters that turned out wrong. Re-chunking changes every boundary, so it
//     is the most expensive of the three and the one that breaks deduplication against
//     every archive not also re-chunked.
//
// # Why re-chunking re-verifies ids
//
// Re-chunking computes fresh ids from the plaintext it reads. A chunk whose content did
// not match its id would silently become valid data under a new id, and the violation
// could never be noticed again. So the read happens at the "rechunk" assert-id place,
// which re-certifies the invariant.

// RecreateOptions control a recreate.
type RecreateOptions struct {
	// Target is the name of the archive to write. Empty reuses the source's name, which
	// with DeleteOriginal set is a replace-in-place.
	Target string
	// Comment replaces the archive's comment when non-nil.
	Comment *string

	// ChunkerParams re-chunks the content. The zero value keeps the existing chunks,
	// which is much cheaper: nothing is read at all.
	ChunkerParams *chunker.Params
	// ChunkSeed is the repository key's chunk seed, needed by the borg 1.x buzhash chunker.
	ChunkSeed uint32
	// Compressor recompresses the content. Recompression implies reading every chunk, so
	// it is only done when asked for.
	Compressor compress.Compressor

	// Filter selects which items survive. Nil keeps everything.
	Filter func(*item.Item) bool

	// DeleteOriginal soft-deletes the source archive when the new one is written.
	DeleteOriginal bool
	// DryRun reports what would happen and writes nothing.
	DryRun bool

	// OnItem is called for each item, with a status character: "+" kept, "-" excluded.
	OnItem func(status byte, path string)
}

// RecreateStats is what a recreate did.
type RecreateStats struct {
	ItemsKept     int
	ItemsExcluded int
	FilesRewrit   int
	ChunksRead    int
	ChunksWritten int
	BytesRead     int64
}

// NeedsWork reports whether the options would change anything at all.
//
// A recreate that changes nothing still costs a full rewrite of the archive's metadata and
// a new archive object, so it is worth not doing.
func (o RecreateOptions) NeedsWork() bool {
	return o.Filter != nil || o.ChunkerParams != nil || o.Compressor != nil || o.Comment != nil
}

// Recreate writes a new archive from an existing one.
func Recreate(m *manifest.Manifest, id []byte, opts RecreateOptions) (*RecreateStats, []byte, error) {
	stats := &RecreateStats{}

	a, err := Open(m, id)
	if err != nil {
		return stats, nil, err
	}

	rechunk := opts.ChunkerParams != nil
	recompress := opts.Compressor != nil
	if rechunk {
		// See the note at the top of this file: fresh ids from re-read plaintext have to
		// be certified against the old ones.
		if err := m.RepoObj().SetAssertIDPlace(repoobj.PlaceRechunk); err != nil {
			return stats, nil, err
		}
	}

	params := chunker.DefaultParams()
	if rechunk {
		params = *opts.ChunkerParams
	} else if a.Meta.ChunkerParamsSet {
		if p, ok := paramsFromList(a.Meta.ChunkerParams); ok {
			params = p
		}
	}

	b, err := NewBuilder(m, BuilderOptions{
		ChunkerParams: params,
		ChunkSeed:     opts.ChunkSeed,
		Compressor:    opts.Compressor,
	})
	if err != nil {
		return stats, nil, err
	}

	report := opts.OnItem
	if report == nil {
		report = func(byte, string) {}
	}

	err = a.Items(func(it *item.Item) error {
		if opts.Filter != nil && !opts.Filter(it) {
			stats.ItemsExcluded++
			report('-', it.Path)
			return nil
		}
		stats.ItemsKept++
		report('+', it.Path)
		if opts.DryRun {
			return nil
		}

		if it.ChunksSet && len(it.Chunks) > 0 && (rechunk || recompress) {
			chunks, err := rewriteContent(a, b, it, rechunk, stats)
			if err != nil {
				return err
			}
			it.Chunks = chunks
			stats.FilesRewrit++
		} else if it.ChunksSet {
			// Nothing to change: the existing chunks are reused as they are, which is what
			// makes an exclude-only recreate cheap - not one byte of content is read.
			for range it.Chunks {
				stats.ChunksRead++
			}
		}
		return b.AddItem(it)
	})
	if err != nil {
		return stats, nil, err
	}
	if opts.DryRun {
		return stats, nil, nil
	}

	name := opts.Target
	if name == "" {
		name = a.Info.Name
	}
	comment := a.Info.Comment
	if opts.Comment != nil {
		comment = *opts.Comment
	}

	saveOpts := SaveOptions{
		Name:     name,
		Comment:  comment,
		Tags:     a.Info.Tags,
		Hostname: a.Info.Host,
		Username: a.Info.User,
	}
	// The archive keeps its original time: a recreate is a rewriting of an existing
	// backup, not a new one, and redating it would misplace it in the history.
	if a.Info.Time.IsZero() {
		saveOpts.Timestamp = b.Start()
	} else {
		saveOpts.Timestamp = a.Info.Time
	}
	if a.Meta.CommandLine != nil {
		saveOpts.CommandLine = *a.Meta.CommandLine
	}
	if a.Meta.CWD != nil {
		saveOpts.CWD = *a.Meta.CWD
	}

	meta, newID, err := b.Save(saveOpts)
	if err != nil {
		return stats, nil, err
	}
	_ = meta

	if opts.DeleteOriginal && !bytes.Equal(newID, id) {
		if err := m.Archives.Delete(id); err != nil {
			return stats, newID, err
		}
	}
	return stats, newID, nil
}

// rewriteContent reads a file's chunks and writes them again, re-chunked and/or
// recompressed.
func rewriteContent(a *Archive, b *Builder, it *item.Item, rechunk bool, stats *RecreateStats) ([]item.ChunkListEntry, error) {
	if rechunk {
		// Re-chunking needs the file as one stream: the new boundaries have nothing to do
		// with the old ones. It is read chunk by chunk rather than loaded whole, so a
		// file larger than memory still works.
		reader := &chunkStreamReader{archive: a, chunks: it.Chunks, stats: stats}
		chunks, err := b.ChunkFile(reader)
		if err != nil {
			return nil, err
		}
		stats.ChunksWritten += len(chunks)
		return chunks, nil
	}

	// Recompression only: the boundaries stay, so each chunk is read and written back
	// under the same id - which means it deduplicates against itself and only the stored
	// bytes change.
	out := make([]item.ChunkListEntry, 0, len(it.Chunks))
	for _, c := range it.Chunks {
		data, err := a.readChunk(c.ID)
		if err != nil {
			return nil, err
		}
		stats.ChunksRead++
		stats.BytesRead += int64(len(data))
		entry, err := b.AddChunk(data, repoobj.TypeFileStream)
		if err != nil {
			return nil, err
		}
		stats.ChunksWritten++
		out = append(out, entry)
	}
	return out, nil
}

// readChunk reads and decrypts one content chunk.
func (a *Archive) readChunk(id []byte) ([]byte, error) {
	obj, err := a.repo.Get(id)
	if err != nil {
		return nil, fmt.Errorf("archive: chunk %s: %w", hex.EncodeToString(id), err)
	}
	_, data, err := a.ro.Parse(id, obj, repoobj.TypeFileStream, repoobj.ParseOptions{})
	return data, err
}

// chunkStreamReader presents a file's chunks as one byte stream, so the chunker can
// re-split it without the whole file being in memory.
type chunkStreamReader struct {
	archive *Archive
	chunks  []item.ChunkListEntry
	stats   *RecreateStats

	next int
	buf  []byte
}

func (r *chunkStreamReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.next >= len(r.chunks) {
			return 0, io.EOF
		}
		data, err := r.archive.readChunk(r.chunks[r.next].ID)
		if err != nil {
			return 0, err
		}
		r.next++
		r.stats.ChunksRead++
		r.stats.BytesRead += int64(len(data))
		r.buf = data
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// paramsFromList reads chunker parameters back out of an archive's metadata.
//
// It is the inverse of chunkerParamsList. A recreate that is not re-chunking must keep the
// archive's own parameters, or the new archive would claim to be chunked one way and
// actually be chunked another - and the next recreate would re-chunk it needlessly.
func paramsFromList(list []any) (chunker.Params, bool) {
	if len(list) == 0 {
		return chunker.Params{}, false
	}
	name, ok := list[0].(string)
	if !ok {
		if b, isBytes := list[0].([]byte); isBytes {
			name = string(b)
		} else {
			return chunker.Params{}, false
		}
	}
	nums := make([]int, 0, len(list)-1)
	for _, v := range list[1:] {
		n, ok := v.(int64)
		if !ok {
			return chunker.Params{}, false
		}
		nums = append(nums, int(n))
	}

	switch name {
	case chunker.AlgoFastCDC:
		if len(nums) != 4 {
			return chunker.Params{}, false
		}
		return chunker.Params{Algorithm: name, ChunkMinExp: nums[0], ChunkMaxExp: nums[1],
			HashMaskBits: nums[2], NCLevel: nums[3]}, true
	case chunker.AlgoBuzhash64:
		if len(nums) != 5 {
			return chunker.Params{}, false
		}
		return chunker.Params{Algorithm: name, ChunkMinExp: nums[0], ChunkMaxExp: nums[1],
			HashMaskBits: nums[2], WindowSize: nums[3], NCLevel: nums[4]}, true
	case chunker.AlgoBuzhash:
		if len(nums) != 4 {
			return chunker.Params{}, false
		}
		return chunker.Params{Algorithm: name, ChunkMinExp: nums[0], ChunkMaxExp: nums[1],
			HashMaskBits: nums[2], WindowSize: nums[3]}, true
	case chunker.AlgoFixed:
		if len(nums) < 1 {
			return chunker.Params{}, false
		}
		p := chunker.Params{Algorithm: name, BlockSize: nums[0]}
		if len(nums) > 1 {
			p.HeaderSize = nums[1]
		}
		return p, true
	default:
		return chunker.Params{}, false
	}
}
