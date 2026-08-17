// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ChunkBuffer, CacheChunkBuffer, archive_put_items and
// Archive.save in borg's src/borg/archive.py, together with Cache.add_chunk from
// src/borg/cache.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/msgpackx"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
)

// itemStreamBufferSize is how much item metadata is buffered before it is chunked.
// borg's ChunkBuffer.BUFFER_SIZE.
const itemStreamBufferSize = 8 * 1024 * 1024

// chunkerKeyDomain is the domain the content chunker's table key is derived under. It is
// format: two tools that derive it differently chunk differently and deduplicate against
// nothing.
var chunkerKeyDomain = map[string][]byte{
	chunker.AlgoFastCDC:   []byte("fastcdc"),
	chunker.AlgoBuzhash64: []byte("buzhash64"),
}

// Stats counts what a backup did.
type Stats struct {
	// OriginalSize is the plaintext size of everything considered, deduplicated or not.
	OriginalSize int64
	// DedupedSize is the plaintext size of the chunks actually stored.
	DedupedSize int64
	NFiles      int64
	// Chunks and NewChunks count chunk references and first-time stores.
	Chunks    int64
	NewChunks int64
}

// BuilderOptions configure a new archive.
type BuilderOptions struct {
	// ChunkerParams is how file content is split. The default is borg's fastcdc.
	ChunkerParams chunker.Params
	// ChunkSeed is the 32-bit seed from the repository key material, used only by the
	// borg 1.x buzhash chunker.
	ChunkSeed uint32
	// Compressor compresses chunk payloads. Nil means lz4, borg's default.
	Compressor compress.Compressor
}

// Builder writes one archive.
//
// # What it owns
//
// Two streams go into the repository: the file content chunks, and the item metadata
// stream. Both are deduplicated against the repository's chunk index, which is why the
// builder holds it rather than each caller doing its own lookups - a chunk stored twice
// is not a correctness problem but it is the whole point of the program.
type Builder struct {
	manifest *manifest.Manifest
	repo     *repository.Repository
	ro       *repoobj.RepoObj
	chunks   *hashindex.ChunkIndex

	chunkerParams chunker.Params
	chunkerKey    []byte
	chunkSeed     uint32

	items *itemStream
	stats Stats
	start time.Time
}

// NewBuilder starts an archive.
func NewBuilder(m *manifest.Manifest, opts BuilderOptions) (*Builder, error) {
	repo := m.Repository()
	ro := m.RepoObj()

	compressor := opts.Compressor
	if compressor == nil {
		var err error
		compressor, err = compress.FromSpec("lz4")
		if err != nil {
			return nil, err
		}
	}
	ro.SetCompressor(compressor)

	params := opts.ChunkerParams
	if params.Algorithm == "" {
		params = chunker.DefaultParams()
	}
	chunkerKey, err := deriveChunkerKey(m, params.Algorithm)
	if err != nil {
		return nil, err
	}

	chunks, err := repo.Chunks()
	if err != nil {
		return nil, err
	}

	b := &Builder{
		manifest:      m,
		repo:          repo,
		ro:            ro,
		chunks:        chunks,
		chunkerParams: params,
		chunkerKey:    chunkerKey,
		chunkSeed:     opts.ChunkSeed,
		start:         time.Now().UTC(),
	}

	// The item stream is chunked with its own, finer parameters: it is far smaller than
	// file content, and a coarse chunker would make every archive rewrite most of it.
	itemsKey, err := deriveChunkerKey(m, chunker.ItemsParams().Algorithm)
	if err != nil {
		return nil, err
	}
	b.items = &itemStream{
		builder: b,
		params:  chunker.ItemsParams(),
		key:     itemsKey,
	}
	return b, nil
}

// deriveChunkerKey produces the chunker's secret table key.
func deriveChunkerKey(m *manifest.Manifest, algo string) ([]byte, error) {
	domain, ok := chunkerKeyDomain[algo]
	if !ok {
		// buzhash (32-bit) and fixed take no table key; the seed carries what they need.
		return nil, nil
	}
	return m.Key().DeriveIDKey(domain, 32)
}

// Stats returns the running totals.
func (b *Builder) Stats() Stats { return b.stats }

// Start is when the archive began, which is what its "start" field records.
func (b *Builder) Start() time.Time { return b.start }

// ChunkerParams is how this archive is being chunked, for the metadata.
func (b *Builder) ChunkerParams() chunker.Params { return b.chunkerParams }

// AddChunk stores one plaintext chunk, deduplicating against the repository.
//
// The returned entry is what goes in an item's chunk list. A chunk already in the
// repository is not stored again - that is the deduplication - but it is still counted,
// because an item referencing it still needs its size.
func (b *Builder) AddChunk(data []byte, roType string) (item.ChunkListEntry, error) {
	id := b.manifest.Key().IDHash(data)
	size := int64(len(data))

	b.stats.Chunks++
	b.stats.OriginalSize += size

	if _, seen := b.chunks.Get(id); seen {
		return item.ChunkListEntry{ID: id, Size: size}, nil
	}

	obj, err := b.ro.Format(id, &repoobj.Meta{Type: roType}, data)
	if err != nil {
		return item.ChunkListEntry{}, err
	}
	results, err := b.repo.Put(id, obj)
	if err != nil {
		return item.ChunkListEntry{}, err
	}
	if size > int64(^uint32(0)) {
		return item.ChunkListEntry{}, fmt.Errorf("archive: chunk of %d bytes is too large", size)
	}
	if err := b.chunks.Add(id, uint32(size)); err != nil {
		return item.ChunkListEntry{}, err
	}
	if err := b.chunks.UpdatePackInfo(results); err != nil {
		return item.ChunkListEntry{}, err
	}

	b.stats.NewChunks++
	b.stats.DedupedSize += size
	return item.ChunkListEntry{ID: id, Size: size}, nil
}

// ChunkFile splits a reader into content chunks and stores them.
func (b *Builder) ChunkFile(r io.Reader) ([]item.ChunkListEntry, error) {
	ch, err := chunker.New(b.chunkerParams, b.chunkerKey, b.chunkSeed, r)
	if err != nil {
		return nil, err
	}
	var out []item.ChunkListEntry
	for {
		c, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entry, err := b.AddChunk(chunkData(c), repoobj.TypeFileStream)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// chunkData materialises a chunk's bytes, expanding a hole into zeros.
//
// A sparse region is a real run of zeros as far as the archive is concerned: it has a
// chunk id like any other, and every hole of the same length deduplicates to the same
// object, so storing it costs nothing after the first.
func chunkData(c chunker.Chunk) []byte {
	if c.Allocation == chunker.AllocData {
		return c.Data
	}
	return make([]byte, c.Size)
}

// AddItem appends an item to the metadata stream.
func (b *Builder) AddItem(it *item.Item) error {
	if it.IsRegular() {
		b.stats.NFiles++
	}
	return b.items.add(it)
}

// itemStream buffers items, chunks them, and stores the chunks.
//
// Items are packed back to back with no framing, so the chunker sees one long byte
// stream. That is what lets an archive of a million files share metadata chunks with the
// previous archive of the same files: only the parts that changed produce new chunks.
type itemStream struct {
	builder *Builder
	params  chunker.Params
	key     []byte

	buf    bytes.Buffer
	chunks [][]byte
}

func (s *itemStream) add(it *item.Item) error {
	packed, err := it.Marshal()
	if err != nil {
		return err
	}
	s.buf.Write(packed)
	if s.buf.Len() > itemStreamBufferSize {
		return s.flush(false)
	}
	return nil
}

// flush chunks what is buffered.
//
// Unless final is set, the last chunk is put back in the buffer rather than stored: it is
// the only one whose boundary could still move when more items arrive, and storing it now
// would produce a chunk no future archive can match.
func (s *itemStream) flush(final bool) error {
	if s.buf.Len() == 0 {
		return nil
	}
	ch, err := chunker.New(s.params, s.key, s.builder.chunkSeed, bytes.NewReader(s.buf.Bytes()))
	if err != nil {
		return err
	}
	var pieces [][]byte
	for {
		c, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		pieces = append(pieces, append([]byte(nil), chunkData(c)...))
	}
	s.buf.Reset()

	end := len(pieces)
	if !final && len(pieces) > 1 {
		end = len(pieces) - 1
	}
	for _, piece := range pieces[:end] {
		entry, err := s.builder.AddChunk(piece, repoobj.TypeArchiveStream)
		if err != nil {
			return err
		}
		s.chunks = append(s.chunks, entry.ID)
	}
	if end < len(pieces) {
		s.buf.Write(pieces[len(pieces)-1])
	}
	return nil
}

// SaveOptions describe the archive being finished.
type SaveOptions struct {
	Name    string
	Comment string
	Tags    []string
	// Timestamp overrides the archive's nominal time. Zero uses the start time, which is
	// borg's default: an archive is dated when it began, not when it finished.
	Timestamp time.Time
	// CommandLine and CWD are recorded for the user's benefit; they are what "borge info"
	// shows when someone asks what produced an archive.
	CommandLine string
	CWD         string
	Hostname    string
	Username    string
}

// Save writes the item pointers and the archive metadata object, adds the archive to the
// directory and rewrites the manifest.
//
// The order matters and is borg's: everything the archive references is durable before
// the pointer that makes it visible is written. An interruption therefore leaves
// unreferenced chunks, which a later compaction reclaims - never an archive naming
// objects that are not there.
func (b *Builder) Save(opts SaveOptions) (*item.ArchiveItem, []byte, error) {
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("archive: an archive needs a name")
	}
	if err := b.items.flush(true); err != nil {
		return nil, nil, err
	}

	itemPtrs, err := b.writeItemPointers()
	if err != nil {
		return nil, nil, err
	}

	end := time.Now().UTC()
	nominal := opts.Timestamp
	if nominal.IsZero() {
		nominal = b.start
	}

	meta := &item.ArchiveItem{
		Version:     2,
		Name:        opts.Name,
		ItemPtrs:    itemPtrs,
		ItemPtrsSet: true,
	}
	setString := func(dst **string, v string) { s := v; *dst = &s }
	setString(&meta.Comment, opts.Comment)
	setString(&meta.CommandLine, opts.CommandLine)
	setString(&meta.Hostname, b.resolveHostname(opts.Hostname))
	setString(&meta.Username, b.resolveUsername(opts.Username))
	setString(&meta.Time, manifest.FormatTimestamp(nominal))
	setString(&meta.Start, manifest.FormatTimestamp(b.start))
	setString(&meta.End, manifest.FormatTimestamp(end))
	if opts.CWD != "" {
		setString(&meta.CWD, opts.CWD)
	}
	meta.Tags = append([]string(nil), opts.Tags...)
	sort.Strings(meta.Tags)
	meta.TagsSet = true
	meta.ChunkerParams = chunkerParamsList(b.chunkerParams)
	meta.ChunkerParamsSet = true

	size, nfiles := b.stats.OriginalSize, b.stats.NFiles
	meta.Size = &size
	meta.NFiles = &nfiles

	// Flush first, so the archive object lands in a pack of its own. borg does this
	// deliberately: "borge repo-list" reads every archive object and nothing else, and a
	// tiny dedicated pack makes that one small read instead of a large one.
	if err := b.repo.Flush(); err != nil {
		return nil, nil, err
	}

	data, err := meta.Marshal()
	if err != nil {
		return nil, nil, err
	}
	id := b.manifest.Key().IDHash(data)
	if _, err := b.AddChunk(data, repoobj.TypeArchiveMeta); err != nil {
		return nil, nil, err
	}

	if err := b.manifest.Archives.Create(id); err != nil {
		return nil, nil, err
	}
	if err := b.manifest.Write(); err != nil {
		return nil, nil, err
	}
	return meta, id, nil
}

// writeItemPointers stores the blocks of item stream chunk ids and returns their ids.
func (b *Builder) writeItemPointers() ([][]byte, error) {
	var ptrs [][]byte
	ids := b.items.chunks
	for start := 0; start < len(ids) || start == 0; start += IDsPerChunk {
		end := start + IDsPerChunk
		if end > len(ids) {
			end = len(ids)
		}
		block := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			block = append(block, id)
		}
		packed, err := msgpackx.Marshal(block)
		if err != nil {
			return nil, err
		}
		entry, err := b.AddChunk(packed, repoobj.TypeArchiveChunkIDs)
		if err != nil {
			return nil, err
		}
		ptrs = append(ptrs, entry.ID)
		if end >= len(ids) {
			break
		}
	}
	return ptrs, nil
}

// chunkerParamsList renders the chunker parameters the way borg stores them: a list whose
// first element is the algorithm name and whose rest are the numbers, in the order the
// constructor takes them.
func chunkerParamsList(p chunker.Params) []any {
	switch p.Algorithm {
	case chunker.AlgoFastCDC:
		return []any{p.Algorithm, int64(p.ChunkMinExp), int64(p.ChunkMaxExp), int64(p.HashMaskBits), int64(p.NCLevel)}
	case chunker.AlgoBuzhash64:
		return []any{p.Algorithm, int64(p.ChunkMinExp), int64(p.ChunkMaxExp), int64(p.HashMaskBits),
			int64(p.WindowSize), int64(p.NCLevel)}
	case chunker.AlgoBuzhash:
		return []any{p.Algorithm, int64(p.ChunkMinExp), int64(p.ChunkMaxExp), int64(p.HashMaskBits), int64(p.WindowSize)}
	case chunker.AlgoFixed:
		return []any{p.Algorithm, int64(p.BlockSize), int64(p.HeaderSize)}
	default:
		return []any{p.Algorithm}
	}
}

func (b *Builder) resolveHostname(given string) string {
	if given != "" {
		return given
	}
	if v, ok := lookupBorgEnv("HOSTNAME"); ok && v != "" {
		return v
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	// borg records the short hostname, not the fully qualified one.
	return strings.SplitN(h, ".", 2)[0]
}

func (b *Builder) resolveUsername(given string) string {
	if given != "" {
		return given
	}
	if v, ok := lookupBorgEnv("USERNAME"); ok && v != "" {
		return v
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return strconv.Itoa(os.Getuid())
}

// lookupBorgEnv reads BORGE_<name>, falling back to BORG_<name>.
func lookupBorgEnv(name string) (string, bool) {
	if v, ok := os.LookupEnv("BORGE_" + name); ok {
		return v, true
	}
	return os.LookupEnv("BORG_" + name)
}
