// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/chunkers/fastcdc.pyx and the sequential
// kernel of src/borg/chunkers/fastcdc_impl.c.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package chunker

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FastCDC is borg 2's default chunker for file content (CHUNKER_PARAMS).
//
// It uses the Gear rolling hash:
//
//	fp = (fp << 1) + Gear[byte]
//
// One shift, one add and one table lookup per byte - no window and no "remove" term,
// which makes it cheaper than buzhash's cyclic polynomial. Being window-less also
// means the hash restarts from zero at the start of every chunk, so there is no warm-up
// pass.
//
// Because Gear accumulates information in the *high* bits (the low bits depend only on
// the last few bytes), the cut mask uses the high bits of the hash. That is the
// high_masks=true argument threaded through newConfig, and getting it backwards would
// make the cut decision depend on a handful of recent bytes instead of the whole window.
//
// Upstream has SIMD kernels (NEON, AVX2, AVX-512) selected by BORG_FASTCDC_KERNEL, but
// they are documented as bit-identical to the sequential loop and the sequential one is
// the default. borge implements only the sequential loop; per plans/PORTING_PLAN.md §0.4
// a faster kernel is a stage 9 question, to be answered by measurement.
type FastCDC struct {
	cfg  config
	gear [256]uint64
	d    driver
}

func newFastCDC(p Params, key []byte, r io.Reader) (*FastCDC, error) {
	cfg, err := newConfig(AlgoFastCDC, p, true) // Gear needs high-bit masks
	if err != nil {
		return nil, err
	}
	gear, err := fastCDCGearTable(key)
	if err != nil {
		return nil, err
	}
	c := &FastCDC{cfg: cfg, gear: gear}
	c.d.init(&c.cfg, r)
	return c, nil
}

// fastCDCGearTable derives the keyed 256-entry Gear table from the repository's chunk
// seed. The values are read from the CSPRNG stream as little-endian uint64s.
func fastCDCGearTable(key []byte) ([256]uint64, error) {
	var gear [256]uint64
	rng, err := NewCSPRNG(key)
	if err != nil {
		return gear, fmt.Errorf("chunker: fastcdc gear table: %w", err)
	}
	raw := rng.RandomBytes(256 * 8)
	for i := 0; i < 256; i++ {
		gear[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return gear, nil
}

func (c *FastCDC) Algorithm() string { return AlgoFastCDC }

// Reset points the chunker at a new stream. The Gear table, which is the expensive part,
// is derived from the repository key and does not depend on the stream.
func (c *FastCDC) Reset(r io.Reader) { c.d.reset(r) }

// Next returns the next chunk.
func (c *FastCDC) Next() (Chunk, error) {
	data, err := c.d.next(c.scan, func() uint64 { return 0 }) // window-less: restart from 0
	if err != nil {
		return Chunk{}, err
	}
	return classify(data), nil
}

// scan is borg's fc_scan_seq: look for a cut point in p, updating the rolling hash.
// It returns the index of the byte at which the hash matched, or -1 for no cut.
func (c *FastCDC) scan(p []byte, fp *uint64, mask uint64) int {
	h := *fp
	for i, b := range p {
		h = h<<1 + c.gear[b]
		if h&mask == 0 {
			*fp = h
			return i
		}
	}
	*fp = h
	return -1
}

// GearTable exposes the derived table for tests and for the differential harness.
func (c *FastCDC) GearTable() [256]uint64 { return c.gear }
