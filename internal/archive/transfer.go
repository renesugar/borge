// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/transfer_cmd.py, minus its borg 1.x
// upgrader (a §0.6 non-goal).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"encoding/hex"
	"fmt"
	"io"

	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repoobj"
)

// Copying archives from one repository into another.
//
// # What transfer is for, and what nothing else does
//
// It moves archives between two repositories without re-reading the source data. Nothing
// else borge has does that: recreate and repo-compress rewrite chunks *in place*, so
// neither can move a repository, and copying the directory brings the old format and the
// old key along with it. The motivating case is the ordinary one - moving a repository to a
// new drive, and re-keying it on the way.
//
// # Why the destination must be RELATED
//
// The chunk ids and the chunk boundaries have to mean the same thing in both repositories,
// or every chunk is stored afresh: the transfer would appear to work and would deduplicate
// nothing. So the caller checks the id-hash family and the chunker secret before a single
// object is written, and "repo-create --other-repo" is what produces a destination that
// passes. See internal/cli/related.go.
//
// # Three ways a chunk crosses
//
//   - **recompress never** (the default): the compressed payload is kept byte for byte and
//     only re-encrypted. The object is still parsed with the id verified, so a corrupt
//     source chunk is caught rather than carried over.
//   - **recompress always**: decompress and compress again under the destination's setting.
//   - **--chunker-params**: the content is re-chunked, which computes new ids from the
//     plaintext. That is why this path re-certifies ids at the "rechunk" place - a chunk
//     whose content did not match its id would otherwise become a valid chunk under a new
//     id and the violation would be unnoticeable afterwards.

// RecompressMode is --recompress.
type RecompressMode string

const (
	// RecompressNever keeps each chunk's compressed payload as it is.
	RecompressNever RecompressMode = "never"
	// RecompressAlways decompresses and compresses again.
	RecompressAlways RecompressMode = "always"
)

// TransferOptions control one transfer.
type TransferOptions struct {
	// Recompress selects how content chunks cross. The zero value is invalid; the caller
	// passes borg's default, "never".
	Recompress RecompressMode
	// Compressor is used for metadata always, and for content when Recompress is always.
	Compressor compress.Compressor
	// ChunkerParams re-chunks the content when set.
	ChunkerParams *chunker.Params
	// ChunkSeed is the destination key's chunk seed, for the re-chunking case.
	ChunkSeed uint32
	// DryRun reads and reports and writes nothing.
	DryRun bool
	// OnProgress, if set, is called once per archive with the line borg prints.
	OnProgress func(string)
}

// TransferResult is what one archive's transfer moved.
type TransferResult struct {
	// TransferSize is the plaintext size of the chunks this transfer had to copy;
	// PresentSize is that of the chunks the destination already had.
	TransferSize int64
	PresentSize  int64
	Skipped      bool
	NFiles       int64
}

// Transfer copies one archive from src into the repository dst belongs to.
func Transfer(dst *manifest.Manifest, src *manifest.Manifest, id []byte, opts TransferOptions) (
	*TransferResult, []byte, error) {

	res := &TransferResult{}
	other, err := Open(src, id)
	if err != nil {
		return res, nil, err
	}

	// Transferring re-anchors content in another repository, so this is the boundary at
	// which "chunk id == hash(content)" is re-certified. borg sets the same place, and
	// BORGE_ASSERT_ID already documents "transfer" as one of the four - a piece of
	// documentation that becomes true here.
	if err := src.RepoObj().SetAssertIDPlace(repoobj.PlaceTransfer); err != nil {
		return res, nil, err
	}

	rechunk := opts.ChunkerParams != nil
	params := chunker.DefaultParams()
	if rechunk {
		params = *opts.ChunkerParams
		if err := dst.RepoObj().SetAssertIDPlace(repoobj.PlaceRechunk); err != nil {
			return res, nil, err
		}
	} else if other.Meta.ChunkerParamsSet {
		if p, ok := paramsFromList(other.Meta.ChunkerParams); ok {
			params = p
		}
	}

	if opts.DryRun {
		// Nothing is built and nothing is written; the walk still reads every chunk list
		// so that the reported sizes are the real ones.
		if err := transferItems(nil, other, dst, opts, res, rechunk); err != nil {
			return res, nil, err
		}
		return res, nil, nil
	}

	b, err := NewBuilder(dst, BuilderOptions{
		ChunkerParams: params,
		ChunkSeed:     opts.ChunkSeed,
		Compressor:    opts.Compressor,
		// borg's transfer builds a normal archive, and its size accounting is create's:
		// the item stream is not counted. See Builder.AddChunk and DIVERGENCES #36.
	})
	if err != nil {
		return res, nil, err
	}
	if err := transferItems(b, other, dst, opts, res, rechunk); err != nil {
		return res, nil, err
	}

	// The destination archive keeps the source's name, time, comment, command line and
	// working directory: a transferred archive is the same archive, in another place.
	save := SaveOptions{
		Name:      other.Info.Name,
		Timestamp: other.Info.Time,
		Tags:      other.Info.Tags,
	}
	if other.Meta.Comment != nil {
		save.Comment = *other.Meta.Comment
	}
	if other.Meta.CommandLine != nil {
		save.CommandLine = *other.Meta.CommandLine
	}
	if other.Meta.CWD != nil {
		save.CWD = *other.Meta.CWD
	}
	_, newID, err := b.Save(save)
	if err != nil {
		return res, nil, err
	}
	return res, newID, nil
}

// transferItems walks the source archive and copies every item into the builder.
//
// b is nil for a dry run, in which case nothing is written but everything is still read.
func transferItems(b *Builder, other *Archive, dst *manifest.Manifest, opts TransferOptions,
	res *TransferResult, rechunk bool) error {

	return other.Items(func(it *item.Item) error {
		if !it.ChunksSet || len(it.Chunks) == 0 {
			if b != nil {
				return b.AddItem(it)
			}
			return nil
		}
		chunks, moved, present, err := transferChunks(b, other, dst, it.Chunks, opts, rechunk)
		if err != nil {
			return err
		}
		res.TransferSize += moved
		res.PresentSize += present
		if b == nil {
			return nil
		}
		it.Chunks = chunks
		res.NFiles++
		return b.AddItem(it)
	})
}

// transferChunks copies one item's content, returning the destination's chunk list.
func transferChunks(b *Builder, other *Archive, dst *manifest.Manifest,
	srcChunks []item.ChunkListEntry, opts TransferOptions, rechunk bool) (
	[]item.ChunkListEntry, int64, int64, error) {

	if rechunk {
		return rechunkContent(b, other, srcChunks, opts)
	}

	var out []item.ChunkListEntry
	var moved, present int64
	for _, c := range srcChunks {
		if b == nil {
			// A dry run reports what it would move. Whether the destination already has
			// the chunk is the one thing worth checking without writing.
			if dstHasChunk(dst, c.ID) {
				present += c.Size
			} else {
				moved += c.Size
			}
			continue
		}
		if entry, ok := b.ReuseChunk(c.ID, c.Size); ok {
			out = append(out, entry)
			present += c.Size
			continue
		}
		entry, err := copyChunk(b, other, c, opts)
		if err != nil {
			return nil, 0, 0, err
		}
		out = append(out, entry)
		moved += c.Size
	}
	return out, moved, present, nil
}

// copyChunk moves one chunk across, in whichever of the two ways was asked for.
func copyChunk(b *Builder, other *Archive, c item.ChunkListEntry, opts TransferOptions) (
	item.ChunkListEntry, error) {

	obj, err := other.repo.Get(c.ID)
	if err != nil {
		// borg's behaviour, and worth keeping: a chunk missing from the source does not
		// stop the transfer. The chunk list entry is written with the correct id and size
		// and nothing is stored, so the gap is *recorded* rather than papered over with
		// zeros - and if the chunk turns up again the archive is complete.
		return item.ChunkListEntry{ID: c.ID, Size: c.Size}, nil
	}

	if opts.Recompress == RecompressAlways {
		_, data, err := other.ro.Parse(c.ID, obj, repoobj.TypeFileStream, repoobj.ParseOptions{})
		if err != nil {
			return item.ChunkListEntry{}, err
		}
		return b.AddChunk(data, repoobj.TypeFileStream)
	}

	// "never": keep the payload, re-encrypt it. Parsed with WantCompressed rather than
	// SkipDecompress, so the id is still verified against the plaintext - the saving is in
	// not compressing again, not in trusting the source.
	meta, compressed, err := other.ro.Parse(c.ID, obj, repoobj.TypeFileStream,
		repoobj.ParseOptions{WantCompressed: true})
	if err != nil {
		return item.ChunkListEntry{}, err
	}
	return b.AddCompressedChunk(c.ID, meta, compressed, c.Size)
}

// rechunkContent re-chunks an item's content through the destination's chunker.
//
// The content is streamed rather than assembled: a transfer is run on whole repositories,
// and buffering a file to re-cut it would make the memory cost the size of the largest file
// in the archive. borg streams here for the same reason.
func rechunkContent(b *Builder, other *Archive, srcChunks []item.ChunkListEntry,
	opts TransferOptions) ([]item.ChunkListEntry, int64, int64, error) {

	// A chunk the source no longer has cannot be represented in the new list - there is no
	// new chunk it belongs to - so the item is re-chunked from what can be read.
	r := &chunkStreamReader{archive: other, chunks: srcChunks, skipMissing: true}

	if b == nil {
		// A dry run still reads, because the sizes it reports are the real ones, but it
		// has nothing to chunk into.
		n, err := io.Copy(io.Discard, r)
		return nil, n, 0, err
	}

	// Through the Builder's cache: a transfer re-chunks every item in the archive, so
	// this is the same per-item construction cost ChunkFile had.
	ch, err := b.contentChunker(*opts.ChunkerParams, opts.ChunkSeed, r)
	if err != nil {
		return nil, 0, 0, err
	}
	var out []item.ChunkListEntry
	var moved, present int64
	for {
		c, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, 0, err
		}
		data := chunkData(c)
		before := b.Stats().NewChunks
		entry, err := b.AddChunk(data, repoobj.TypeFileStream)
		if err != nil {
			return nil, 0, 0, err
		}
		if b.Stats().NewChunks > before {
			moved += entry.Size
		} else {
			present += entry.Size
		}
		out = append(out, entry)
	}
	return out, moved, present, nil
}

// dstHasChunk reports whether the destination repository already holds a chunk. Only a dry
// run needs this; the real path asks the builder, which holds the index it is updating.
func dstHasChunk(dst *manifest.Manifest, id []byte) bool {
	chunks, err := dst.Repository().Chunks()
	if err != nil {
		return false
	}
	_, ok := chunks.Get(id)
	return ok
}

// TransferReport is borg's per-archive line, kept here so both the real and the dry-run
// paths render it the same way.
func TransferReport(name string, id []byte, ts string, res *TransferResult, dryRun bool,
	sizeFmt func(int64) string) string {

	idHex := hex.EncodeToString(id)
	if !dryRun {
		return fmt.Sprintf("%s %s %s: finished. transfer_size: %s present_size: %s",
			name, ts, idHex, sizeFmt(res.TransferSize), sizeFmt(res.PresentSize))
	}
	if res.TransferSize == 0 {
		return fmt.Sprintf("%s %s %s: completed", name, ts, idHex)
	}
	return fmt.Sprintf("%s %s %s: incomplete, transfer_size: %s present_size: %s",
		name, ts, idHex, sizeFmt(res.TransferSize), sizeFmt(res.PresentSize))
}
