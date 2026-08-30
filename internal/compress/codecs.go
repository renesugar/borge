// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the compressor classes in borg's src/borg/compress.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package compress

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
	"sync"
)

// ---------------------------------------------------------------------------- none

// None stores data unchanged. It is also the fallback every DecidingCompressor uses
// when compression would not shrink the chunk.
type None struct{}

func (None) ID() uint8    { return IDNone }
func (None) Name() string { return "none" }
func (None) Level() int   { return UnknownLevel } // no levels are defined for none

func (c None) Compress(meta *Meta, data []byte) ([]byte, error) {
	meta.Size, meta.SizeSet = len(data), true
	return finish(meta, c, data)
}

func (None) Decompress(_ *Meta, data []byte) ([]byte, error) {
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// ----------------------------------------------------------------------------- lz4

// LZ4 is borg's fastest compressor and the one Auto probes with.
//
// This is the **raw LZ4 block format**, not the LZ4 frame format. borg calls
// LZ4_compress_default / LZ4_decompress_safe directly, which produce and consume a
// bare block with no magic number, no frame header and no length prefix. Using
// pierrec/lz4's Reader/Writer instead would produce a frame that borg cannot read, and
// the mistake is easy to make because the frame API is the more prominent one.
type LZ4 struct{}

func (LZ4) ID() uint8    { return IDLZ4 }
func (LZ4) Name() string { return "lz4" }
func (LZ4) Level() int   { return UnknownLevel } // no levels are defined for lz4

func (c LZ4) Compress(meta *Meta, data []byte) ([]byte, error) {
	return decideCompress(meta, c, data, func() ([]byte, error) {
		return c.attempt(data)
	})
}

// lz4Compressors holds compressors between calls.
//
// lz4.Compressor is 136 KiB - a 128 KiB hash table and an 8 KiB in-use bitmap - and borge
// built one per chunk. Over the 118,866-file corpus of §12.1b that was 16.1 GB allocated
// in a single create, 91% of everything the run allocated, and it is what paid for 22.2
// seconds of garbage collection, a quarter of the run.
//
// # Why this is safe
//
// CompressBlock calls the compressor's own reset before it does anything, precisely so a
// compressor can be reused: "Zero out reused table to avoid non-deterministic output"
// (pierrec/lz4 issue #65). The in-use bitmap exists to make that reset cheap. Reuse is the
// arrangement the library is designed for, and TestLZ4PooledOutputMatchesFresh checks that
// the bytes are identical either way.
//
// # Why a pool rather than one instance
//
// The chunker of §12.1a is reused as a single instance, which is correct only because the
// write path is serial and is noted there as something step 2 must revisit. This does not
// need revisiting: a pool hands each goroutine its own, so parallelising create changes
// nothing here. LZ4 is a value type with no field to hang state on in any case.
var lz4Compressors sync.Pool

// attempt returns the LZ4 block, or nil when LZ4 did not shrink the input.
func (LZ4) attempt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		// An empty input cannot shrink; borg short-circuits here too, because an empty
		// view has no address to hand to the C function.
		return nil, nil
	}
	buf := make([]byte, lz4.CompressBlockBound(len(data)))
	compressor, _ := lz4Compressors.Get().(*lz4.Compressor)
	if compressor == nil {
		compressor = new(lz4.Compressor)
	}
	defer lz4Compressors.Put(compressor)
	n, err := compressor.CompressBlock(data, buf)
	if err != nil {
		return nil, fmt.Errorf("compress: lz4 compress failed: %w", err)
	}
	// n == 0 means the data is incompressible: pierrec reports it that way, where the
	// C library returns 0 on failure. Either way there is nothing worth storing.
	if n == 0 || n >= len(data) {
		return nil, nil
	}
	return buf[:n], nil
}

func (LZ4) Decompress(meta *Meta, data []byte) ([]byte, error) {
	// Fast path: the plaintext size is in the authenticated metadata, so the output
	// buffer is allocated exactly once at the right size - no guessing, no growing
	// retry loop, and a size mismatch is a reliable corruption signal.
	if meta.SizeSet {
		if meta.Size < 0 {
			return nil, fmt.Errorf("compress: lz4 plaintext size is negative: %d", meta.Size)
		}
		out := make([]byte, meta.Size)
		n, err := lz4.UncompressBlock(data, out)
		if err != nil {
			return nil, fmt.Errorf("compress: lz4 decompress failed: %w", err)
		}
		if n != meta.Size {
			return nil, fmt.Errorf("compress: lz4 produced %d byte(s), metadata says %d", n, meta.Size)
		}
		return out, nil
	}

	// Slow path: no size recorded, which is what an object written with
	// --compression auto,... looks like (see Meta.Size). LZ4 blocks carry no
	// uncompressed length, so the only option is to guess an output size and grow.
	// borg does the same, with the same starting point and growth factor.
	const initialGuess = 9226752 // int(1.1 * 2**23): a bit over 8 MiB, borg's starting point
	osize := initialGuess        // covers the chunker's usual output without a resize
	if n := len(data) * 3; n > osize {
		osize = n
	}
	const maxOSize = 1 << 27 // 128 MiB; far beyond any legitimate repository object
	for {
		out := make([]byte, osize)
		n, err := lz4.UncompressBlock(data, out)
		if err == nil {
			return out[:n], nil
		}
		if osize > maxOSize {
			return nil, fmt.Errorf("compress: lz4 decompress failed even with a %d byte buffer "+
				"and no plaintext size in the metadata: %w", osize, err)
		}
		osize = osize * 3 / 2
	}
}

// ---------------------------------------------------------------------------- zstd

// DefaultZstdLevel is zstd's own default, and borg's (src/borg/compress.pyx).
const DefaultZstdLevel = 3

// Zstd is borg's best ratio-for-speed compressor and the likely future default
// (borg issue #10085).
//
// Levels run from -128 to 22. The negative ones are zstd's "fast" levels, which is why
// the level byte needs its own encoding: see EncodeLevel.
type Zstd struct{ level int }

// NewZstd returns a zstd compressor at the given level.
func NewZstd(level int) (Zstd, error) {
	if level < -128 || level > 22 {
		return Zstd{}, fmt.Errorf("compress: zstd level must be -128..22, got %d", level)
	}
	return Zstd{level: level}, nil
}

func (Zstd) ID() uint8    { return IDZstd }
func (Zstd) Name() string { return "zstd" }
func (c Zstd) Level() int { return c.level }

// EncodeLevel stores the level as an int8 in the clevel byte, which is what makes
// zstd's negative fast levels representable at all. Levels 1..22 encode to the same
// byte they always did, so repositories written by older borg versions keep working.
func (Zstd) EncodeLevel(level int) (uint8, error) {
	if level < -128 || level > 127 {
		return 0, fmt.Errorf("compress: zstd level %d is not storable in one byte", level)
	}
	return uint8(int8(level)), nil
}

// DecodeLevel reverses EncodeLevel, reading the byte as a signed int8.
func (Zstd) DecodeLevel(clevel uint8) int { return int(int8(clevel)) }

// zstdEncoders holds one pool of encoders per level.
//
// §12.2 measured a fresh encoder per chunk at 183.7 MB/s against 871.8 for a reused one -
// 4.7x, "the single largest win available and the cheapest to take" - and then it was not
// taken, because the default compression is lz4 and the benchmark corpus never reached
// this path. Found on the way into the create pipeline, where it would have been worse
// still: klauspost's encoder starts goroutines when it is constructed, so a worker pool
// would have been building and tearing them down per chunk on every worker.
//
// Keyed by level because an encoder is built for one. EncodeAll on a reused encoder is
// safe for concurrent use, which is what makes a single pool per level correct rather than
// one per goroutine.
var zstdEncoders sync.Map // zstd.EncoderLevel -> *sync.Pool of *zstd.Encoder

func zstdEncoderPool(level zstd.EncoderLevel) *sync.Pool {
	if p, ok := zstdEncoders.Load(level); ok {
		return p.(*sync.Pool)
	}
	p := &sync.Pool{New: func() any {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
		if err != nil {
			// NewWriter only fails on a bad option, and the option is this level.
			return err
		}
		return enc
	}}
	actual, _ := zstdEncoders.LoadOrStore(level, p)
	return actual.(*sync.Pool)
}

func (c Zstd) Compress(meta *Meta, data []byte) ([]byte, error) {
	return decideCompress(meta, c, data, func() ([]byte, error) {
		level := zstdLevel(c.level)
		pool := zstdEncoderPool(level)
		got := pool.Get()
		if err, isErr := got.(error); isErr {
			return nil, fmt.Errorf("compress: zstd: %w", err)
		}
		enc := got.(*zstd.Encoder)
		// Returned rather than closed: Close releases the encoder's goroutines, which is
		// the cost this pool exists to stop paying per chunk.
		defer pool.Put(enc)
		out := enc.EncodeAll(data, nil)
		if len(out) >= len(data) {
			return nil, nil
		}
		return out, nil
	})
}

func (Zstd) Decompress(_ *Meta, data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("compress: zstd: %w", err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("compress: zstd decompress failed: %w", err)
	}
	return out, nil
}

// zstdLevel maps a libzstd numeric level onto klauspost/compress's four levels.
//
// The mapping is necessarily approximate: klauspost's pure-Go encoder does not expose
// libzstd's full 1..22 range. That is acceptable because the compressed bytes need not
// match borg's - only the format and the recorded clevel do, and the clevel byte
// records the level the user asked for, so borg reports the same thing borge was told.
// The ratio may differ slightly from libzstd at the same nominal level; stage 9
// benchmarks measure it rather than assuming.
func zstdLevel(level int) zstd.EncoderLevel {
	switch {
	case level <= 2: // includes zstd's negative "fast" levels
		return zstd.SpeedFastest
	case level <= 5:
		return zstd.SpeedDefault
	case level <= 11:
		return zstd.SpeedBetterCompression
	default:
		return zstd.SpeedBestCompression
	}
}

// ---------------------------------------------------------------------------- zlib

// DefaultZlibLevel matches Python's zlib default, which is what borg uses.
const DefaultZlibLevel = 6

// Zlib is the zlib format (RFC 1950: a two-byte header, deflate data, an Adler-32
// checksum), the same thing Python's zlib.compress produces. It is not raw deflate.
type Zlib struct{ level int }

// NewZlib returns a zlib compressor at the given level.
func NewZlib(level int) (Zlib, error) {
	if level < 0 || level > 9 {
		return Zlib{}, fmt.Errorf("compress: zlib level must be 0..9, got %d", level)
	}
	return Zlib{level: level}, nil
}

func (Zlib) ID() uint8    { return IDZlib }
func (Zlib) Name() string { return "zlib" }
func (c Zlib) Level() int { return c.level }

func (c Zlib) Compress(meta *Meta, data []byte) ([]byte, error) {
	return decideCompress(meta, c, data, func() ([]byte, error) {
		var buf bytes.Buffer
		w, err := zlib.NewWriterLevel(&buf, c.level)
		if err != nil {
			return nil, fmt.Errorf("compress: zlib: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			return nil, fmt.Errorf("compress: zlib: %w", err)
		}
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("compress: zlib: %w", err)
		}
		if buf.Len() >= len(data) {
			return nil, nil
		}
		return buf.Bytes(), nil
	})
}

func (Zlib) Decompress(_ *Meta, data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compress: zlib decompress failed: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("compress: zlib decompress failed: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------- lzma

// DefaultLZMALevel matches Python's lzma default preset, which is what borg uses.
const DefaultLZMALevel = 6

// LZMA is the xz container format with no integrity check, matching Python's
// lzma.compress(data, preset=level, check=lzma.CHECK_NONE).
//
// The check is disabled deliberately: borg authenticates every object itself, so an
// xz-level CRC would only cost bytes and time to duplicate a stronger guarantee.
type LZMA struct{ level int }

// NewLZMA returns an lzma compressor at the given preset level.
func NewLZMA(level int) (LZMA, error) {
	if level < 0 || level > 9 {
		return LZMA{}, fmt.Errorf("compress: lzma level must be 0..9, got %d", level)
	}
	return LZMA{level: level}, nil
}

func (LZMA) ID() uint8    { return IDLZMA }
func (LZMA) Name() string { return "lzma" }
func (c LZMA) Level() int { return c.level }

func (c LZMA) Compress(meta *Meta, data []byte) ([]byte, error) {
	return decideCompress(meta, c, data, func() ([]byte, error) {
		var buf bytes.Buffer
		cfg := xz.WriterConfig{
			// CheckNone: borg passes check=lzma.CHECK_NONE. A stream written with a
			// different check would still decompress, but this keeps borge's output
			// the same shape as borg's.
			CheckSum: xz.None,
			DictCap:  lzmaDictCap(c.level),
		}
		if err := cfg.Verify(); err != nil {
			return nil, fmt.Errorf("compress: lzma: %w", err)
		}
		w, err := cfg.NewWriter(&buf)
		if err != nil {
			return nil, fmt.Errorf("compress: lzma: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			return nil, fmt.Errorf("compress: lzma: %w", err)
		}
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("compress: lzma: %w", err)
		}
		if buf.Len() >= len(data) {
			return nil, nil
		}
		return buf.Bytes(), nil
	})
}

func (LZMA) Decompress(_ *Meta, data []byte) ([]byte, error) {
	r, err := xz.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compress: lzma decompress failed: %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("compress: lzma decompress failed: %w", err)
	}
	return out, nil
}

// lzmaDictCap maps a preset level onto a dictionary size, following the table xz uses
// for presets 0..9. ulikunitz/xz has no preset concept, so the level has to be turned
// into the parameter that actually matters for ratio.
func lzmaDictCap(level int) int {
	switch level {
	case 0:
		return 256 * 1024
	case 1:
		return 1024 * 1024
	case 2:
		return 2 * 1024 * 1024
	case 3:
		return 4 * 1024 * 1024
	case 4:
		return 4 * 1024 * 1024
	case 5:
		return 8 * 1024 * 1024
	case 6:
		return 8 * 1024 * 1024
	case 7:
		return 16 * 1024 * 1024
	case 8:
		return 32 * 1024 * 1024
	default:
		return 64 * 1024 * 1024
	}
}

// ------------------------------------------------------------------ zstd, as a stream

// The repository never stores a zstd *stream*: every stored object is compressed whole,
// which is what lets a chunk be decompressed without reading anything before it. A tarball
// is the opposite - one stream from beginning to end - and "export-tar backup.tar.zst"
// needs exactly that.
//
// It lives here rather than in the command layer because this is where the zstd dependency
// and the level mapping already are, and because a second import of the same library
// somewhere else would be a second place to keep those decisions.

// ZstdStreamLevel is borg's ZSTD_TAR_LEVEL: the zstd command line tool's own default,
// which borg uses for a tarball rather than the compression level chosen for the archive.
const ZstdStreamLevel = 3

// NewZstdStreamWriter wraps w in a zstd encoder.
//
// workers is how many compression threads to use; zero or less means one per CPU, which is
// this library's default. borg exposes the same choice as BORG_ZSTD_MT_WORKERS, and
// multithreaded zstd changes only the speed, never the bytes a decompressor sees.
func NewZstdStreamWriter(w io.Writer, workers int) (io.WriteCloser, error) {
	opts := []zstd.EOption{zstd.WithEncoderLevel(zstdLevel(ZstdStreamLevel))}
	if workers > 0 {
		opts = append(opts, zstd.WithEncoderConcurrency(workers))
	}
	enc, err := zstd.NewWriter(w, opts...)
	if err != nil {
		return nil, fmt.Errorf("compress: zstd stream: %w", err)
	}
	return enc, nil
}

// NewZstdStreamReader wraps r in a zstd decoder.
//
// The returned Closer releases the decoder's goroutines; it does not close r, which is the
// caller's. A zstd stream that ends early reports an error on Read rather than being taken
// for a complete one, which is what makes a truncated tarball an error and not a short
// archive.
func NewZstdStreamReader(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("compress: zstd stream: %w", err)
	}
	return dec.IOReadCloser(), nil
}
