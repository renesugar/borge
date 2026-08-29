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

	// HashingTime and ChunkingTime are borg's two timings, reported by "create --stats"
	// and carried in "create --json".
	//
	// They were the reason two keys were missing from borge's JSON: sending zeroes would
	// have been worse than sending nothing, "because a frontend charting hashing_time
	// would draw a flat line and believe it" (PORTING_PLAN §11.4). Measuring is the way
	// out of that, and this is the measurement. Each covers what borg's covers: hashing
	// is the chunk id computation, chunking is the chunker producing a chunk.
	HashingTime  time.Duration
	ChunkingTime time.Duration
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

	// CountItemStreamSize includes the item metadata stream in the archive's recorded
	// size. It is false for create and import-tar and true for recreate, because that is
	// what borg does - not by design, but see AddChunk: borg's create loses the counter
	// its item buffer writes into and its recreate does not. The same tree gives
	// size=341 through borg's create and size=1284447 through its recreate.
	//
	// So the recorded size depends on which command wrote the archive, in both tools.
	// borge reproduces that rather than picking one, because the number is stored and
	// read back: an archive borge recreated should report what an archive borg recreated
	// reports. docs/DIVERGENCES.md #36.
	CountItemStreamSize bool
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
	// chunkerCache is the content chunker, reused across files; see contentChunker.
	chunkerCache       chunker.Chunker
	chunkerCacheParams chunker.Params
	chunkerCacheSeed   uint32

	// countItemStreamSize is BuilderOptions.CountItemStreamSize.
	countItemStreamSize bool
	chunkSeed           uint32

	// workers is how many goroutines hash and format chunks; 0 or 1 is serial.
	workers int

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
		manifest:            m,
		countItemStreamSize: opts.CountItemStreamSize,
		repo:                repo,
		ro:                  ro,
		chunks:              chunks,
		chunkerParams:       params,
		chunkerKey:          chunkerKey,
		chunkSeed:           opts.ChunkSeed,
		workers:             createWorkers(),
		start:               time.Now().UTC(),
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
//
// # Why the item stream is usually not counted
//
// OriginalSize is stored in the archive metadata as "size" and read back by both tools,
// so it has to be borg's number and not a better one. On the create and import-tar paths
// borg's is the sum of the file content chunks and the item-*pointer* chunks, and
// excludes the item metadata stream - which its own code says it includes:
//
//	self.items_buffer.flush(flush=True)  # this adds the size of metadata stream chunks
//	                                     # to stats.osize
//
// The comment is untrue, and the reason is worth writing down because nothing about the
// number looks wrong until you measure it. borg's create does "archive.stats += fso.stats"
// (create_cmd.py:252) to fold the file processor's counts into the archive's, and
// Statistics.__add__ builds and returns a *new* object. archive.stats is rebound to it;
// archive.items_buffer.stats still refers to the old one. Every item-stream chunk written
// after that point increments a counter nobody reads.
//
// Measured rather than deduced, because the code reads the other way: 5000 empty files
// give an item stream of a few hundred KB and borg still records size=341 - the item
// pointer chunks alone. 100 empty files and one 1 MB file both record exactly 35 bytes of
// overhead, which is msgpack's encoding of a one-element list holding one 32-byte id.
//
// borg's recreate has no such fold - it uses target.stats throughout - so its item buffer
// keeps writing into the counter that is read, and a recreated archive records a size that
// includes the item stream. The same 5000-file tree records 341 through borg's create and
// 1284447 through its recreate. Hence CountItemStreamSize, set by the recreate path only:
// the number depends on which command wrote the archive, and borge reproduces that rather
// than choosing one and disagreeing with borg half the time.
//
// So borge matches the behaviour, not the comment. See docs/DIVERGENCES.md #36.
func (b *Builder) AddChunk(data []byte, roType string) (item.ChunkListEntry, error) {
	startedHashing := time.Now()
	id := b.manifest.Key().IDHash(data)
	b.stats.HashingTime += time.Since(startedHashing)
	size := int64(len(data))

	b.stats.Chunks++
	if roType != repoobj.TypeArchiveStream || b.countItemStreamSize {
		b.stats.OriginalSize += size
	}

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

// ReuseChunk records a reference to a chunk the repository already holds.
//
// It is transfer's fast path and the reason a second transfer of the same archives is
// cheap: the chunk is not read, not decrypted and not written, only counted. Reports false
// when the chunk is not there, so the caller can copy it.
func (b *Builder) ReuseChunk(id []byte, size int64) (item.ChunkListEntry, bool) {
	if _, ok := b.chunks.Get(id); !ok {
		return item.ChunkListEntry{}, false
	}
	b.stats.Chunks++
	b.stats.OriginalSize += size
	return item.ChunkListEntry{ID: id, Size: size}, true
}

// AddCompressedChunk stores a payload that is already compressed, under an id the caller
// has already verified.
//
// This is "transfer --recompress never": the chunk is re-encrypted for the destination and
// otherwise crosses untouched. The id is the source's, which is the point - a transfer that
// recomputed ids would deduplicate against nothing.
func (b *Builder) AddCompressedChunk(id []byte, meta *repoobj.Meta, compressed []byte, size int64) (
	item.ChunkListEntry, error) {

	b.stats.Chunks++
	b.stats.OriginalSize += size

	if _, seen := b.chunks.Get(id); seen {
		return item.ChunkListEntry{ID: id, Size: size}, nil
	}
	obj, err := b.ro.FormatCompressed(id, meta, compressed)
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

// contentChunker returns a chunker for these parameters, pointed at r.
//
// One chunker is kept and reset per file, because building one is not free: the keyed
// Gear and buzhash tables come from a CSPRNG, measured at 1.75 ms for fastcdc and 4.35 ms
// for buzhash64, which over a 118,866-file directory is about 3.5 minutes of table
// construction before a byte is chunked (docs/PORTING_PLAN.md §12.1). borg builds one per
// archive; borge was building one per file.
//
// The cache is keyed on the parameters and the seed rather than assumed: transfer
// re-chunks through the *destination's* configuration, which is not necessarily the one
// this Builder was made with, and silently reusing tables derived from a different key
// would produce chunk boundaries that match neither repository.
//
// # This is safe because the write path is serial
//
// One chunker cannot serve two goroutines: it owns a buffer that Next hands out by
// reference. Stage 9 step 2 pipelines create, and at that point this needs to become one
// chunker per worker rather than one per Builder. Reusing it here is correct today and is
// a thing to revisit then, not a thing to inherit.
func (b *Builder) contentChunker(params chunker.Params, seed uint32, r io.Reader) (chunker.Chunker, error) {
	if b.chunkerCache != nil && b.chunkerCacheParams == params && b.chunkerCacheSeed == seed {
		b.chunkerCache.Reset(r)
		return b.chunkerCache, nil
	}
	ch, err := chunker.New(params, b.chunkerKey, seed, r)
	if err != nil {
		return nil, err
	}
	b.chunkerCache, b.chunkerCacheParams, b.chunkerCacheSeed = ch, params, seed
	return ch, nil
}

// ChunkFile splits a reader into content chunks and stores them.
//
// With more than one worker the hashing and formatting go to a pool; see pipeline.go for
// what is parallel, what stays serial and why. The serial path below remains the reference
// and is what BORGE_CREATE_WORKERS=1 selects.
func (b *Builder) ChunkFile(r io.Reader) ([]item.ChunkListEntry, error) {
	return b.ChunkFileSized(r, -1)
}

// ChunkFileSized is ChunkFile for a caller that knows how much it is about to hand over.
//
// size is -1 when unknown. The hint decides whether the worker pool is used at all, and
// getting it wrong only costs speed: see pipelineMinFileSize for the measurement. An
// unknown size takes the pipeline, because the callers that do not know - import-tar,
// recreate, a stream on standard input - are the ones handing over whole archives, while
// the case the pipeline is bad at is many small files, which arrives through create where
// the size is always known.
func (b *Builder) ChunkFileSized(r io.Reader, size int64) ([]item.ChunkListEntry, error) {
	if b.workers > 1 && (size < 0 || size >= pipelineMinFileSize) {
		return b.chunkFilePipelined(r, b.workers)
	}
	return b.chunkFileSerial(r)
}

// chunkFileSerial is the one-goroutine path.
func (b *Builder) chunkFileSerial(r io.Reader) ([]item.ChunkListEntry, error) {
	ch, err := b.contentChunker(b.chunkerParams, b.chunkSeed, r)
	if err != nil {
		return nil, err
	}
	var out []item.ChunkListEntry
	for {
		// Timed around Next alone: what follows it is hashing and storing, which are
		// counted separately, and a timer around the whole loop would report the sum
		// three times over.
		startedChunking := time.Now()
		c, err := ch.Next()
		b.stats.ChunkingTime += time.Since(startedChunking)
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
// StreamOptions describe the single item a stream becomes.
type StreamOptions struct {
	// Name is the path stored for it. borg's default is "stdin".
	Name string
	// Mode is the permission bits; the regular-file type is added here.
	Mode int64
	// User and Group are stored when non-empty, with UID and GID beside them.
	User, Group string
	UID, GID    *int64
}

// AddStream archives a stream of bytes as one regular file.
//
// This is how "borge create ARCHIVE -" and --content-from-command store what they read:
// there is no file on disk, so there is nothing to stat, and every piece of metadata is
// either given on the command line or invented here.
//
// # The timestamps are all "now", and all three are stored
//
// borg's process_pipe sets atime, ctime and mtime to the moment of the backup, and it does
// so whatever --atime and --noctime say - those options are about what to copy from a
// file's inode, and a pipe has no inode. Reproduced, because an archive of a database dump
// that borg and borge disagree about is one that cannot be compared.
func (b *Builder) AddStream(r io.Reader, opts StreamOptions) (int64, error) {
	chunks, err := b.ChunkFile(r)
	if err != nil {
		return 0, err
	}
	var size int64
	for _, c := range chunks {
		size += int64(c.Size)
	}

	now := time.Now().UnixNano()
	mode := opts.Mode | item.SIFREG
	it := &item.Item{
		Path:      opts.Name,
		Mode:      &mode,
		Size:      &size,
		Chunks:    chunks,
		ChunksSet: true,
		MTime:     &now,
		ATime:     &now,
		CTime:     &now,
	}
	if opts.User != "" {
		user := opts.User
		it.User = &user
	}
	if opts.Group != "" {
		group := opts.Group
		it.Group = &group
	}
	it.UID, it.GID = opts.UID, opts.GID

	if err := b.AddItem(it); err != nil {
		return 0, err
	}
	b.stats.NFiles++
	b.stats.OriginalSize += size
	return size, nil
}

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
	// chunker is built once and reset per flush; the stream's parameters never change.
	chunker chunker.Chunker
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
	// The metadata stream is flushed repeatedly as items accumulate, so this was its own
	// per-flush table construction. Its parameters never change, so one chunker serves
	// the whole stream.
	if s.chunker == nil {
		ch, err := chunker.New(s.params, s.key, s.builder.chunkSeed, bytes.NewReader(s.buf.Bytes()))
		if err != nil {
			return err
		}
		s.chunker = ch
	} else {
		s.chunker.Reset(bytes.NewReader(s.buf.Bytes()))
	}
	ch := s.chunker
	// The item stream is chunked too, and borg counts that in the same total: its
	// chunking_time comes from the chunker object, not from the caller.
	var pieces [][]byte
	for {
		startedChunking := time.Now()
		c, err := ch.Next()
		s.builder.stats.ChunkingTime += time.Since(startedChunking)
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
