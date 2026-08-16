// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/compress.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package compress implements borg's compression layer.
//
// Compression happens after the chunk id is computed (so the id is a function of the
// plaintext alone) and before encryption.
//
// # What has to match borg, and what does not
//
// Only three things are format-visible, and it is worth being precise about which,
// because it decides how much of this package is risky:
//
//   - The **compressor ids** stored in the object metadata (see the ID constants).
//     These are fixed by the format.
//   - The **metadata fields** ctype, clevel, csize, size and psize. borg reads these
//     to know how to decompress, so they must be exactly right.
//   - **Decompressibility in both directions.** borg must decompress what borge writes
//     and vice versa.
//
// The compressed bytes themselves need *not* be identical to borg's. Chunk ids are
// computed over plaintext, and a pack file's name is the hash of that pack's own
// contents, so nothing downstream depends on borge and borg producing the same
// compressed output for the same input. This is why using Go's zlib or klauspost's
// zstd rather than reimplementing zlib and libzstd is safe: any valid stream in the
// right format decompresses correctly.
//
// What *is* worth matching, and what this package does match, is the *decision*: which
// compressor ends up being used for a given chunk. That is what DecidingCompressor and
// Auto reproduce, because it determines the ctype that gets stored and therefore what a
// user sees and what borg recreate does.
//
// # The deciding behaviour
//
// borg never stores data that got bigger. Every real compressor is a
// DecidingCompressor: it compresses, and if the result is not smaller than the input it
// stores the input uncompressed with ctype = none instead. A port that skips this
// writes larger repositories and reports the wrong ctype.
package compress

import (
	"fmt"
	"sort"
)

// Compressor ids, stored in the object metadata as "ctype". These are part of the
// on-disk format (src/borg/compress.pyx) and cannot change.
const (
	IDNone       uint8 = 0x00
	IDLZ4        uint8 = 0x01
	IDLZMA       uint8 = 0x02
	IDZstd       uint8 = 0x03
	IDObfuscate  uint8 = 0x04
	IDZlib       uint8 = 0x05
	IDZlibLegacy uint8 = 0x08 // borg < 1.3; never written, detected by content
)

// UnknownLevel is the clevel byte stored by compressors that have no meaningful level
// (none, lz4), and by zlib_legacy where the level cannot be recovered.
const UnknownLevel = 255

// ROBJFileStream is the repo object type for file content chunks. ObfuscateSize only
// pads these; padding metadata objects would waste space for no privacy gain, since
// their sizes are not what a size-based attack looks at. Mirrors ROBJ_FILE_STREAM in
// src/borg/constants.py; duplicated here so this package does not depend upward.
const ROBJFileStream = "F"

// MaxDataSize is the largest plaintext borg puts in one repository object
// (src/borg/constants.py: MAX_DATA_SIZE). ObfuscateSize needs it to know how much
// padding it may add.
const MaxDataSize = 20971479

// Meta is the per-object compression metadata. borg carries these as keys in the
// msgpack metadata dict of a repository object; a struct is used here so a missing
// field is a compile error rather than a KeyError at runtime.
type Meta struct {
	// Type is the repo object type (a ROBJ_* value), set by the caller before
	// compressing. Only ObfuscateSize reads it.
	Type string

	// Size is the plaintext size, and SizeSet says whether it is known.
	//
	// Every plain compressor records it, which lets the LZ4 decompressor allocate
	// exactly the right output buffer instead of guessing. The Auto meta-compressor
	// does *not*: borg's Auto.compress copies only ctype, clevel and csize out of the
	// inner compressor's metadata (get_meta in src/borg/compress.pyx), so an object
	// written with --compression auto,... has no size at all.
	//
	// That is not a hypothetical: auto is a commonly used setting, so the decompressor
	// has to work without a size, and SizeSet is what distinguishes "unknown" from a
	// genuine zero-length chunk. borge's own Auto does record the size - see the note
	// there - but it must still read what borg wrote.
	Size    int
	SizeSet bool

	// CType is the compressor id actually used, and CLevel the level byte as that
	// compressor encodes it (see Compressor.EncodeLevel).
	CType  uint8
	CLevel uint8

	// CSize is the overall size of the compressed payload, including any obfuscation
	// padding.
	CSize int

	// PSize is the payload size before obfuscation padding. It is only set when
	// ObfuscateSize is in use; PSizeSet distinguishes "0 bytes" from "not set".
	PSize    int
	PSizeSet bool

	// OLevel is the obfuscation level, recorded so repo-compress can reproduce it.
	OLevel    int
	OLevelSet bool
}

// Compressor compresses one chunk. Compress fills in the metadata fields describing
// what it did, mirroring borg's convention of mutating the meta dict in place.
type Compressor interface {
	// ID is the compressor id stored as ctype. Meta compressors that never appear in
	// the metadata (Auto) return IDNone here and are not registered for detection.
	ID() uint8

	// Name is the name used in a --compression spec.
	Name() string

	// Level is the compression level as this compressor understands it.
	Level() int

	// Compress compresses data and updates meta. The returned slice may alias data
	// when nothing was compressed.
	Compress(meta *Meta, data []byte) ([]byte, error)
}

// Decompressor reverses one compressor. meta describes what was done; data is the
// compressed payload with any obfuscation padding already trimmed by the caller.
type Decompressor interface {
	Decompress(meta *Meta, data []byte) ([]byte, error)
}

// levelCodec lets a compressor use a different byte encoding for its level than the
// level itself. Only zstd needs this, to store its negative "fast" levels.
type levelCodec interface {
	EncodeLevel(level int) (uint8, error)
	DecodeLevel(clevel uint8) int
}

// encodeLevel is the default level encoding: the level is the byte.
func encodeLevel(level int) (uint8, error) {
	if level < 0 || level > 255 {
		return 0, fmt.Errorf("compress: invalid level %d, must be 0..255", level)
	}
	return uint8(level), nil
}

// finish records a successful compression in meta. It is the Go equivalent of
// CompressorBase.compress's non-legacy branch, which sets ctype, clevel and csize.
func finish(meta *Meta, c Compressor, out []byte) ([]byte, error) {
	clevel, err := levelByte(c)
	if err != nil {
		return nil, err
	}
	meta.CType = c.ID()
	meta.CLevel = clevel
	meta.CSize = len(out)
	return out, nil
}

func levelByte(c Compressor) (uint8, error) {
	if lc, ok := c.(levelCodec); ok {
		return lc.EncodeLevel(c.Level())
	}
	return encodeLevel(c.Level())
}

// decideCompress is the shared body of every DecidingCompressor: run the compressor's
// own attempt, and fall back to storing the data uncompressed when compression did not
// actually shrink it.
//
// attempt returns the compressed bytes, or nil to mean "do not use me" - which is how
// borg's _decide signals that NONE_COMPRESSOR should take over.
func decideCompress(meta *Meta, c Compressor, data []byte, attempt func() ([]byte, error)) ([]byte, error) {
	meta.Size, meta.SizeSet = len(data), true
	out, err := attempt()
	if err != nil {
		return nil, err
	}
	if out == nil {
		// Not worth compressing: store it as-is, with ctype none.
		return None{}.Compress(meta, data)
	}
	return finish(meta, c, out)
}

// registry of compressors that can appear in stored metadata, in the order borg checks
// them (COMPRESSOR_LIST in src/borg/compress.pyx: fast ones first). The order matters
// only for zlib_legacy, whose detection is content-based rather than id-based.
var detectOrder = []uint8{IDLZ4, IDZstd, IDNone, IDZlib, IDZlibLegacy, IDLZMA, IDObfuscate}

var namesByID = map[uint8]string{
	IDNone:       "none",
	IDLZ4:        "lz4",
	IDLZMA:       "lzma",
	IDZstd:       "zstd",
	IDObfuscate:  "obfuscate",
	IDZlib:       "zlib",
	IDZlibLegacy: "zlib_legacy",
}

// Name returns the human-readable name of a compressor id.
func Name(id uint8) string {
	if n, ok := namesByID[id]; ok {
		return n
	}
	return fmt.Sprintf("unknown(0x%02x)", id)
}

// Names lists every known compressor name, sorted. Used for error messages and shell
// completion.
func Names() []string {
	out := make([]string, 0, len(namesByID))
	for _, n := range namesByID {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Detect maps a stored (ctype, clevel) pair to a decompressor and the level it
// implies, mirroring Compressor.detect in src/borg/compress.pyx.
//
// borg passes a two-byte header here. In borg 2 that header is synthesised from the
// metadata dict (bytes((ctype, clevel))) rather than read from the payload, because
// borg 2 keeps the compression type in the authenticated metadata instead of prefixing
// it to the data. Taking the two values separately makes that explicit.
func Detect(ctype, clevel uint8) (Decompressor, int, error) {
	for _, id := range detectOrder {
		if id != ctype {
			continue
		}
		switch id {
		case IDNone:
			return None{}, int(clevel), nil
		case IDLZ4:
			return LZ4{}, int(clevel), nil
		case IDZstd:
			return Zstd{level: Zstd{}.DecodeLevel(clevel)}, Zstd{}.DecodeLevel(clevel), nil
		case IDZlib:
			return Zlib{level: int(clevel)}, int(clevel), nil
		case IDLZMA:
			return LZMA{level: int(clevel)}, int(clevel), nil
		case IDObfuscate:
			// Obfuscation is transparent on read in borg 2: the caller trims the
			// payload to psize and dispatches on the inner compressor's own ctype, so
			// a stored ctype of 0x04 should not reach here.
			return nil, 0, fmt.Errorf("compress: ctype 0x04 (obfuscate) is not a stored type in borg 2; " +
				"the object metadata should name the inner compressor and carry psize")
		case IDZlibLegacy:
			return nil, 0, fmt.Errorf("compress: zlib_legacy (borg 1.x) data is not supported; " +
				"borge reads borg 2 repositories only")
		}
	}
	return nil, 0, fmt.Errorf("compress: no decompressor for ctype 0x%02x (clevel %d)", ctype, clevel)
}

// Decompress reverses whatever compression the metadata describes.
//
// It handles the obfuscation trim: when psize is set and smaller than csize, the bytes
// past psize are padding added to hide the real size, and must not be fed to the
// decompressor.
func Decompress(meta *Meta, data []byte) ([]byte, error) {
	if meta.CSize != 0 && len(data) != meta.CSize {
		return nil, fmt.Errorf("compress: csize says %d byte(s), got %d", meta.CSize, len(data))
	}
	payload := data
	if meta.PSizeSet {
		if meta.PSize > len(data) {
			return nil, fmt.Errorf("compress: psize %d exceeds the %d byte(s) available", meta.PSize, len(data))
		}
		payload = data[:meta.PSize]
	}
	dec, _, err := Detect(meta.CType, meta.CLevel)
	if err != nil {
		return nil, err
	}
	out, err := dec.Decompress(meta, payload)
	if err != nil {
		return nil, err
	}
	// borg's check_fix_size: when the plaintext size is present it is authenticated
	// metadata, so a mismatch means the payload is not what the metadata says it is.
	// When it is absent (an Auto-compressed object, see Meta.Size) there is nothing to
	// check against and borg does not check either.
	if meta.SizeSet && meta.Size != len(out) {
		return nil, fmt.Errorf("compress: decompressed to %d byte(s), metadata says %d",
			len(out), meta.Size)
	}
	return out, nil
}
