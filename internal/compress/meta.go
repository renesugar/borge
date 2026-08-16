// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file ports the Auto and ObfuscateSize meta-compressors from borg's
// src/borg/compress.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package compress

import (
	"fmt"
	"math"
	"math/rand"
)

// ----------------------------------------------------------------------------- auto

// Auto decides per chunk whether an expensive compressor is worth running, by probing
// with LZ4 first.
//
// It is a meta-compressor: it never appears in stored metadata, because whichever
// compressor it settles on records its own ctype. That is why ID reports IDNone and
// Auto is absent from the detection table.
type Auto struct{ inner Compressor }

// NewAuto wraps an expensive compressor in the LZ4 probe.
func NewAuto(inner Compressor) Auto { return Auto{inner: inner} }

func (Auto) ID() uint8           { return IDNone } // never stored; see the type comment
func (Auto) Name() string        { return "auto" }
func (Auto) Level() int          { return UnknownLevel }
func (a Auto) Inner() Compressor { return a.inner }

// The two thresholds below are borg's, and they are reproduced exactly because they
// decide which ctype gets stored.
const (
	// autoProbeRatio: if LZ4 got the chunk below 97% of its original size, the data is
	// compressible enough that trying the expensive compressor may pay off.
	autoProbeRatio = 0.97
	// autoKeepExpensiveRatio: only keep the expensive result if it beats LZ4 by more
	// than 1%. Otherwise store the LZ4 data, which decompresses far faster.
	autoKeepExpensiveRatio = 0.99
)

func (a Auto) Compress(meta *Meta, data []byte) ([]byte, error) {
	if a.inner == nil {
		return nil, fmt.Errorf("compress: auto has no inner compressor")
	}

	// Probe with LZ4. cheapMeta is a scratch copy: borg passes dict(meta) here so the
	// probe cannot leave its fields behind if the expensive compressor wins.
	cheapMeta := *meta
	cheap, err := LZ4{}.Compress(&cheapMeta, data)
	if err != nil {
		return nil, err
	}

	// Note the "+2": borg divides by len(data) + 2 because in its legacy format the
	// compressed data carried a two-byte type/level prefix. borg 2 stores those in the
	// metadata instead, so the prefix is gone - but the constant stayed, and it shifts
	// the threshold slightly. Reproduced deliberately: dropping it would make borge
	// choose a different compressor than borg for chunks near the boundary.
	ratio := float64(len(cheap)) / float64(len(data)+2)
	if ratio >= autoProbeRatio {
		// Barely compressible (or not at all): keep whatever the probe decided, which
		// is either lz4 or none.
		*meta = cheapMeta
		return cheap, nil
	}

	// Worth trying the expensive compressor.
	expensiveMeta := *meta
	expensive, err := a.inner.Compress(&expensiveMeta, data)
	if err != nil {
		return nil, err
	}
	if len(cheap) > 0 && float64(len(expensive))/float64(len(cheap)) < autoKeepExpensiveRatio {
		*meta = expensiveMeta
		return expensive, nil
	}
	*meta = cheapMeta
	return cheap, nil
}

// Deliberate divergence from borg, recorded here and in docs/DIVERGENCES.md.
//
// borg's Auto.compress copies only ctype, clevel and csize out of the inner
// compressor's metadata (its get_meta helper), so an object written with
// --compression auto,... carries **no plaintext size**. That costs it on every read:
// without a size, the LZ4 decompressor cannot allocate the output buffer up front and
// falls back to guessing 8 MiB and growing by 50% until the block fits.
//
// borge keeps the size, because *meta is assigned from a metadata copy that the inner
// compressor already filled in completely. This is safe in both directions:
//
//   - borg reading borge's object: its check_fix_size only asserts that a size, if
//     present, matches the decompressed length - which it does - and its LZ4 path then
//     takes the exact-allocation fast path instead of the guessing one.
//   - borge reading borg's object: Meta.SizeSet is false and the decompressor uses the
//     same grow-and-retry fallback borg does.
//
// So this is strictly an improvement that borg understands, not a format change. It
// was found by the differential test: requiring a size made borge unable to read
// anything borg had written with auto.

// ------------------------------------------------------------------------ obfuscate

// Obfuscation level ranges (src/borg/compress.pyx: ObfuscateSize.__init__).
const (
	ObfuscateRelativeMin = 1 // 1..6: pad by a random multiple of the compressed size
	ObfuscateRelativeMax = 6
	ObfuscateAbsoluteMin = 110 // 110..123: pad by a random amount up to 2**(level-100)
	ObfuscateAbsoluteMax = 123
	ObfuscatePadme       = 250 // round the size up using the Padmé scheme
)

// ObfuscateSize hides the true compressed size of file content chunks by appending
// zero padding.
//
// The padding is not stored separately: csize covers the padded payload and psize
// records where the real data ends, so a reader trims to psize before decompressing.
// The inner compressor's ctype and clevel are what get stored, so obfuscation is
// invisible to decompression apart from the trim.
type ObfuscateSize struct {
	level int
	inner Compressor
	// rnd is the randomness source. It is a field so tests can make padding
	// deterministic; production uses the global source.
	rnd *rand.Rand
}

// NewObfuscateSize wraps a compressor in size obfuscation at the given level.
func NewObfuscateSize(level int, inner Compressor) (*ObfuscateSize, error) {
	if inner == nil {
		return nil, fmt.Errorf("compress: obfuscate needs an inner compressor")
	}
	ok := (level >= ObfuscateRelativeMin && level <= ObfuscateRelativeMax) ||
		(level >= ObfuscateAbsoluteMin && level <= ObfuscateAbsoluteMax) ||
		level == ObfuscatePadme
	if !ok {
		return nil, fmt.Errorf("compress: obfuscate level must be 1..6, 110..123 or 250, got %d", level)
	}
	return &ObfuscateSize{level: level, inner: inner}, nil
}

func (o *ObfuscateSize) ID() uint8    { return IDObfuscate }
func (o *ObfuscateSize) Name() string { return "obfuscate" }
func (o *ObfuscateSize) Level() int   { return o.level }

// Inner returns the wrapped compressor.
func (o *ObfuscateSize) Inner() Compressor { return o.inner }

func (o *ObfuscateSize) Compress(meta *Meta, data []byte) ([]byte, error) {
	out, err := o.inner.Compress(meta, data)
	if err != nil {
		return nil, err
	}
	// psize is where the inner compressor's output ends. Everything after it is padding.
	meta.PSize = meta.CSize
	meta.PSizeSet = true

	// Only file content chunks are padded. Metadata objects are not what a size-based
	// attack looks at, and padding them would cost space for nothing.
	var pad int
	if meta.Type == ROBJFileStream {
		pad = o.padding(len(out))
	}
	if pad < 0 {
		pad = 0 // padding can only lengthen, never shorten
	}
	// Stay clear of the maximum object size, or the object becomes unstorable.
	if limit := MaxDataSize - 1024 - len(out); pad > limit {
		pad = limit
	}
	if pad > 0 {
		padded := make([]byte, len(out)+pad)
		copy(padded, out)
		out = padded // the tail stays zero
	}
	meta.CSize = len(out) // csize is the overall output size, padding included
	meta.OLevel = o.level
	meta.OLevelSet = true
	return out, nil
}

// Decompress is never reached in borg 2: obfuscation is undone by trimming to psize in
// Decompress and dispatching on the inner compressor's ctype. It exists so the type
// satisfies no false expectation of being a decompressor.
func (o *ObfuscateSize) Decompress(_ *Meta, _ []byte) ([]byte, error) {
	return nil, fmt.Errorf("compress: obfuscate is undone by trimming to psize, not by dispatch")
}

func (o *ObfuscateSize) random() float64 {
	if o.rnd != nil {
		return o.rnd.Float64()
	}
	return rand.Float64()
}

// padding returns how many bytes to append for a payload of compressedSize bytes.
func (o *ObfuscateSize) padding(compressedSize int) int {
	switch {
	case o.level >= ObfuscateRelativeMin && o.level <= ObfuscateRelativeMax:
		return o.relativeRandomReciprocal(compressedSize)
	case o.level >= ObfuscateAbsoluteMin && o.level <= ObfuscateAbsoluteMax:
		return o.randomPadding()
	case o.level == ObfuscatePadme:
		return padmePadding(compressedSize)
	default:
		return 0
	}
}

// relativeRandomReciprocal pads by compressedSize * factor/r, where r is uniform in
// (0, 1]. The reciprocal makes small paddings common and large ones rare: at level 1,
// 90% of chunks grow by 1-10%, 9% by 10-100%, and 0.9% by 100-1000%.
func (o *ObfuscateSize) relativeRandomReciprocal(compressedSize int) int {
	const minR = 0.0001 // keep r away from zero, which would give unbounded padding
	factor := 0.001 * math.Pow(10, float64(o.level))
	r := math.Max(minR, o.random())
	return int(float64(compressedSize) * (factor / r))
}

// randomPadding pads by a uniformly random amount up to 2**(level-100), i.e. 1 KiB at
// level 110 through 8 MiB at level 123.
func (o *ObfuscateSize) randomPadding() int {
	maxPadding := math.Pow(2, float64(o.level-100))
	return int(maxPadding * o.random())
}

// padmePadding rounds the size up using the Padmé scheme, which bounds the information
// leaked by a padded length while keeping the overhead below about 12%.
func padmePadding(compressedSize int) int {
	if compressedSize < 2 {
		return 0
	}
	e := math.Floor(math.Log2(float64(compressedSize))) // exponent
	s := math.Floor(math.Log2(e)) + 1                   // second log component
	lastBits := int(e - s)                              // bits to zero
	if lastBits < 0 {
		return 0
	}
	bitMask := (1 << lastBits) - 1
	padded := (compressedSize + bitMask) &^ bitMask
	return padded - compressedSize
}
