// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/chunkers/base.pyx and the chunker
// parameters in src/borg/constants.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package chunker cuts files into content-defined chunks, exactly where borg cuts them.
//
// # Why exactness matters here more than anywhere else
//
// Chunk boundaries *are* the deduplication format. Two implementations that disagree
// about where to cut produce entirely different chunk ids for the same file, so they
// deduplicate nothing against each other - while both working perfectly on their own.
// There is no error, no warning, and nothing to notice until a repository written by
// one tool and appended to by the other has stored everything twice.
//
// That is why docs/PORTING_PLAN.md makes the gate for this package byte-exact boundary
// agreement rather than "round-trips correctly", and why the differential test dumps
// and compares every offset rather than sampling.
//
// # Where the cut points come from
//
// Every content-defined chunker here is keyed: the hash tables are derived from the
// repository's chunk seed through CSPRNG. Cut points are therefore unpredictable
// without the key, which stops an observer from fingerprinting known files by their
// chunk-size pattern.
//
// # Normalized chunking
//
// FastCDC's normalized chunking (nc_level) uses two masks: a strict one (lower cut
// probability) until the chunk reaches normal_size, then a loose one. This
// concentrates chunk sizes around the target instead of the exponential distribution a
// single mask produces. nc_level 0 disables it, and the code then reduces exactly to
// the single-mask chunker.
package chunker

import (
	"fmt"
	"io"
)

// Algorithm names, as they appear in --chunker-params and in stored archive metadata
// (src/borg/constants.py).
const (
	AlgoBuzhash       = "buzhash"
	AlgoBuzhash64     = "buzhash64"
	AlgoFastCDC       = "fastcdc"
	AlgoFixed         = "fixed"
	AlgoRabinAES      = "rabin-aes"
	AlgoGoldilocksAES = "goldilocks-aes"
	AlgoToeplitzAES   = "toeplitz-aes"
)

// borg's default chunker parameters (src/borg/constants.py).
const (
	ChunkMinExp    = 19   // 2**19 == 512 KiB
	ChunkMaxExp    = 23   // 2**23 == 8 MiB
	HashWindowSize = 4095 // 0xFFF; buzhash and buzhash64 only - fastcdc is window-less
	HashMaskBits   = 21   // ~2 MiB chunks statistically
	NCLevel        = 2    // normalized chunking level
)

// Allocation describes what a chunk's bytes came from (src/borg/constants.py).
type Allocation int

const (
	// AllocData is ordinary file content.
	AllocData Allocation = 0
	// AllocAlloc is an all-zero chunk. borg cannot tell a hole from stored zeros at
	// this level, so both land here and the data is not stored.
	AllocAlloc Allocation = 1
	// AllocHole is an unallocated region of a sparse file.
	AllocHole Allocation = 2
)

// Chunk is one cut piece of a file.
//
// Data is nil when Allocation is not AllocData: an all-zero chunk is recorded by size
// alone. Size is always the real byte count, so Size is what the caller accounts for,
// never len(Data).
type Chunk struct {
	Data       []byte
	Size       int
	Allocation Allocation
}

// Chunker cuts a stream into chunks. Next returns io.EOF when the stream is exhausted.
//
// The returned Data may alias the chunker's internal buffer and is only valid until
// the next call to Next. Callers that keep it must copy.
type Chunker interface {
	// Next returns the next chunk, or io.EOF.
	Next() (Chunk, error)
	// Algorithm names the chunker, as stored in archive metadata.
	Algorithm() string
}

// Params describes a chunker configuration, the parsed form of --chunker-params.
type Params struct {
	Algorithm string

	// Content-defined chunkers.
	ChunkMinExp  int
	ChunkMaxExp  int
	HashMaskBits int
	WindowSize   int // buzhash, buzhash64 only
	NCLevel      int // buzhash64, fastcdc and the AES chunkers; buzhash has no nc_level
	NormalSize   int // 0 means "derive from HashMaskBits and NCLevel"

	// Fixed chunker.
	BlockSize  int
	HeaderSize int
}

// DefaultParams is borg's default for file content data: FASTCDC_PARAMS.
func DefaultParams() Params {
	return Params{
		Algorithm:    AlgoFastCDC,
		ChunkMinExp:  ChunkMinExp,
		ChunkMaxExp:  ChunkMaxExp,
		HashMaskBits: HashMaskBits,
		NCLevel:      NCLevel,
	}
}

// ItemsParams is borg's ITEMS_CHUNKER_PARAMS: finer granularity for the item metadata
// stream, which is much smaller than file content.
func ItemsParams() Params {
	return Params{
		Algorithm:    AlgoFastCDC,
		ChunkMinExp:  15,
		ChunkMaxExp:  19,
		HashMaskBits: 17,
		NCLevel:      NCLevel,
	}
}

// New builds a chunker for the given parameters, reading from r.
//
// key is the repository's 32-byte chunk seed for the keyed algorithms. The 32-bit
// buzhash takes a plain integer seed instead, for borg 1.x compatibility; pass it via
// Params and a nil key.
func New(p Params, key []byte, seed uint32, r io.Reader) (Chunker, error) {
	switch p.Algorithm {
	case AlgoFastCDC:
		return newFastCDC(p, key, r)
	case AlgoBuzhash64:
		return newBuzhash64(p, key, r)
	case AlgoBuzhash:
		return newBuzhash(p, seed, r)
	case AlgoFixed:
		return newFixed(p, r)
	case AlgoRabinAES, AlgoGoldilocksAES, AlgoToeplitzAES:
		return nil, fmt.Errorf("chunker: %s is not implemented yet; it is an upstream "+
			"experiment and is not borg's default (see docs/PORTING_PLAN.md stage 1.4)", p.Algorithm)
	default:
		return nil, fmt.Errorf("chunker: unknown algorithm %q", p.Algorithm)
	}
}

// maskBits builds a mask with `bits` one-bits, at the least or most significant end.
//
// Which end is not a detail: it depends on where the hash concentrates its
// information. buzhash64 and the AES chunkers have uniform output and use the low
// bits; Gear-style hashes (fastcdc) accumulate in their high bits, because the low
// bits only depend on the most recent few bytes, so they must use the high bits or the
// cut decision would ignore most of the window.
func maskBits(bits int, high bool) uint64 {
	if bits <= 0 {
		return 0
	}
	if bits >= 64 {
		return ^uint64(0)
	}
	low := uint64(1)<<uint(bits) - 1
	if high {
		return low << uint(64-bits)
	}
	return low
}

// config holds the parameters shared by every content-defined chunker.
type config struct {
	algo       string
	minSize    int
	maxSize    int
	chunkMask  uint64
	maskS      uint64 // strict mask, used below normalSize
	maskL      uint64 // loose mask, used at or above normalSize
	ncLevel    int
	normalSize int
}

// newConfig validates the parameters and derives the masks, mirroring
// ChunkerBase._setup_common.
func newConfig(algo string, p Params, highMasks bool) (config, error) {
	if p.ChunkMinExp < 0 || p.ChunkMaxExp < 0 {
		return config{}, fmt.Errorf("chunker: negative chunk size exponent")
	}
	if p.ChunkMaxExp > 31 {
		return config{}, fmt.Errorf("chunker: chunk_max_exp %d is too large", p.ChunkMaxExp)
	}
	minSize := 1 << uint(p.ChunkMinExp)
	maxSize := 1 << uint(p.ChunkMaxExp)
	if minSize+1 > maxSize {
		return config{}, fmt.Errorf("chunker: max chunk size %d must exceed min %d", maxSize, minSize)
	}
	if p.NCLevel < 0 {
		return config{}, fmt.Errorf("chunker: negative nc_level %d", p.NCLevel)
	}
	if p.HashMaskBits-p.NCLevel < 1 {
		return config{}, fmt.Errorf("chunker: nc_level %d is too large for hash_mask_bits %d",
			p.NCLevel, p.HashMaskBits)
	}
	if p.HashMaskBits+p.NCLevel > 48 {
		return config{}, fmt.Errorf("chunker: nc_level %d is too large for hash_mask_bits %d",
			p.NCLevel, p.HashMaskBits)
	}

	c := config{
		algo:      algo,
		minSize:   minSize,
		maxSize:   maxSize,
		chunkMask: maskBits(p.HashMaskBits, highMasks),
		ncLevel:   p.NCLevel,
	}
	if p.NCLevel != 0 {
		c.maskS = maskBits(p.HashMaskBits+p.NCLevel, highMasks)
		c.maskL = maskBits(p.HashMaskBits-p.NCLevel, highMasks)
		if p.NormalSize != 0 {
			c.normalSize = p.NormalSize
		} else {
			// The default lands the mean chunk size near the nominal target rather than
			// overshooting it: the nominal size minus the expected loose-phase tail.
			c.normalSize = (1 << uint(p.HashMaskBits)) - (1 << uint(p.HashMaskBits-p.NCLevel))
		}
	} else {
		c.maskS = c.chunkMask
		c.maskL = c.chunkMask
		c.normalSize = 0
	}
	return c, nil
}

// maskFor picks the mask for a chunk that is chunkLen bytes long so far.
func (c *config) maskFor(chunkLen int) uint64 {
	if c.ncLevel != 0 && chunkLen < c.normalSize {
		return c.maskS
	}
	return c.maskL
}

// classify decides a chunk's Allocation. borg checks whether the bytes are all zero;
// it cannot tell a hole from stored zeros at this point, and does not try.
func classify(data []byte) Chunk {
	for _, b := range data {
		if b != 0 {
			return Chunk{Data: data, Size: len(data), Allocation: AllocData}
		}
	}
	return Chunk{Data: nil, Size: len(data), Allocation: AllocAlloc}
}
