// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the CSPRNG class in borg's src/borg/crypto/low_level.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package chunker

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

// CSPRNG is borg's deterministic random source: AES-256 in CTR mode with an
// all-zero IV, keyed by the repository's chunk seed.
//
// It is not a general-purpose RNG and must not be used as one. Its only job is to turn
// the repository key into the chunker's hash tables, deterministically, so that two
// borg instances with the same key cut chunks at the same places - and so that anyone
// without the key cannot predict where the cuts fall. That unpredictability is what
// stops an observer from fingerprinting known files by their chunk-size pattern.
//
// **This is format-defining code.** The tables it produces decide every chunk boundary
// in a repository. A port that produces a different byte stream here still works
// perfectly on its own, and deduplicates nothing against borg.
type CSPRNG struct {
	stream cipher.Stream
	buf    [4096]byte // borg refills in 4096-byte blocks; the block size is not observable
	pos    int
}

// NewCSPRNG seeds the generator with a 256-bit key.
func NewCSPRNG(key []byte) (*CSPRNG, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("chunker: csprng seed key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("chunker: %w", err)
	}
	// The IV is all zeros. That is safe only because the key is used for nothing else:
	// it is a key-stretching function, not an encryption of anything secret.
	var iv [aes.BlockSize]byte
	r := &CSPRNG{stream: cipher.NewCTR(block, iv[:])}
	r.pos = len(r.buf) // force a refill on first use
	return r, nil
}

func (r *CSPRNG) refill() {
	for i := range r.buf {
		r.buf[i] = 0
	}
	// CTR keystream: encrypting zeros yields the keystream itself, which is what borg
	// does (it feeds a zero buffer through EVP_EncryptUpdate).
	r.stream.XORKeyStream(r.buf[:], r.buf[:])
	r.pos = 0
}

// RandomBytes returns n deterministic pseudo-random bytes.
func (r *CSPRNG) RandomBytes(n int) []byte {
	out := make([]byte, n)
	for filled := 0; filled < n; {
		if r.pos >= len(r.buf) {
			r.refill()
		}
		c := copy(out[filled:], r.buf[r.pos:])
		r.pos += c
		filled += c
	}
	return out
}

// RandomInt returns a value in [0, n) using rejection sampling.
//
// The sampling procedure is part of the format, not an implementation choice: Shuffle
// consumes RandomInt, Shuffle builds the buzhash64 table, and the table decides chunk
// boundaries. Draw a different number of bytes per call, or mask a different number of
// bits, and every boundary moves.
func (r *CSPRNG) RandomInt(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("chunker: csprng upper bound must be positive, got %d", n)
	}
	if n == 1 {
		return 0, nil
	}

	// Bits needed to represent n-1, then bytes to hold those bits.
	bitsNeeded := 0
	for tmp := n - 1; tmp > 0; tmp >>= 1 {
		bitsNeeded++
	}
	bytesNeeded := (bitsNeeded + 7) / 8
	mask := (1 << bitsNeeded) - 1

	// borg caps the retries and then falls back to a slightly biased modulo. The cap is
	// unreachable in practice (each attempt succeeds with probability > 1/2), but it is
	// reproduced so the two implementations cannot diverge even in that corner.
	const maxAttempts = 1000
	for attempt := 0; attempt < maxAttempts; attempt++ {
		b := r.RandomBytes(bytesNeeded)
		v := bigEndianInt(b) & mask
		if v < n {
			return v, nil
		}
	}
	b := r.RandomBytes(bytesNeeded)
	return bigEndianInt(b) % n, nil
}

// Shuffle permutes items in place with Fisher-Yates, drawing from this generator.
//
// borg iterates downward from len-1 to 1 and swaps with RandomInt(i+1). The direction
// and the bound are both format-visible through the buzhash64 table.
func (r *CSPRNG) Shuffle(items []int) error {
	for i := len(items) - 1; i > 0; i-- {
		j, err := r.RandomInt(i + 1)
		if err != nil {
			return err
		}
		items[i], items[j] = items[j], items[i]
	}
	return nil
}

// bigEndianInt reads a byte slice as a big-endian integer, matching Python's
// int.from_bytes(data, byteorder="big"). At most 8 bytes are ever passed here, since
// bytesNeeded is derived from a table index.
func bigEndianInt(b []byte) int {
	v := 0
	for _, x := range b {
		v = v<<8 | int(x)
	}
	return v
}
