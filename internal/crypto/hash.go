// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file ports the hash and KDF helpers from borg's src/borg/crypto/low_level.pyx
// and the argon2 parameters from src/borg/constants.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/blake2b"
	"lukechampine.com/blake3"
)

// The hash functions borg uses for chunk ids and envelope MACs. Which one applies
// depends on the key type - see internal/crypto/key - but all of them are keyed, so
// chunk ids cannot be computed by someone without the repository key.

// HMACSHA256 is borg's hmac_sha256. It is the id hash for the aes256-ocb,
// chacha20-poly1305 and authenticated-sha256 key types.
func HMACSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// Blake3Keyed is borg's keyed BLAKE3, used as the id hash for the blake3 key types.
//
// borg calls blake3(data, key=id_key).digest(length=32); the key must be exactly 32
// bytes, which is BLAKE3's keyed mode. Note the borg 1.x blake2b id keys were 128
// bytes and are not supported (src/borg/crypto/key.py).
//
// borg may hash large inputs with an internal thread pool above a tuned threshold. The
// digest is identical either way, so borge does not reproduce the threshold - it is a
// throughput knob, not part of the format.
func Blake3Keyed(key, data []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: blake3 key must be 32 bytes, got %d", len(key))
	}
	var k [32]byte
	copy(k[:], key)
	h := blake3.New(32, k[:])
	h.Write(data)
	return h.Sum(nil), nil
}

// Blake2b256 is borg's blake2b_256: an unkeyed BLAKE2b over key||data, 32-byte digest.
//
// Despite the name it is *not* BLAKE2b's keyed mode - borg prepends the key to the
// message and hashes it unkeyed. Reproducing that exactly matters; using BLAKE2b's
// real keyed mode would give different digests for the same inputs.
func Blake2b256(key, data []byte) []byte {
	h, err := blake2b.New256(nil)
	if err != nil {
		// Unreachable: New256(nil) cannot fail.
		panic("crypto: blake2b.New256: " + err.Error())
	}
	h.Write(key)
	h.Write(data)
	return h.Sum(nil)
}

// Blake2b128 is borg's blake2b_128: an unkeyed BLAKE2b with a 16-byte digest.
func Blake2b128(data []byte) []byte {
	h, err := blake2b.New(16, nil)
	if err != nil {
		panic("crypto: blake2b.New(16): " + err.Error())
	}
	h.Write(data)
	return h.Sum(nil)
}

// borg's argon2 parameters (src/borg/constants.py: ARGON2_ARGS, ARGON2_SALT_BYTES).
// These are format-visible: they are stored in the key blob so an existing repository
// can still be unlocked if the defaults ever change.
const (
	Argon2TimeCost    = 3
	Argon2MemoryCost  = 1 << 16 // KiB, i.e. 64 MiB
	Argon2Parallelism = 4
	Argon2SaltBytes   = 16
	// Argon2Type is "id" in borg's config, i.e. Argon2id.
	Argon2Type = "id"
)

// Argon2ID derives a key from a passphrase, with the parameters given.
//
// The parameters are arguments rather than constants because they are read back from
// the stored key blob: a repository created with older defaults must still open with
// the values it recorded, not with today's.
func Argon2ID(passphrase, salt []byte, timeCost, memoryCostKiB, parallelism uint32, outputLen uint32) ([]byte, error) {
	if len(salt) == 0 {
		return nil, fmt.Errorf("crypto: argon2 needs a salt")
	}
	if timeCost == 0 || memoryCostKiB == 0 || parallelism == 0 || outputLen == 0 {
		return nil, fmt.Errorf("crypto: invalid argon2 parameters (time=%d memory=%d parallelism=%d len=%d)",
			timeCost, memoryCostKiB, parallelism, outputLen)
	}
	if parallelism > 255 {
		return nil, fmt.Errorf("crypto: argon2 parallelism must be 1..255, got %d", parallelism)
	}
	return argon2.IDKey(passphrase, salt, timeCost, memoryCostKiB, uint8(parallelism), outputLen), nil
}

// Argon2IDDefault derives a key using borg's current default parameters.
func Argon2IDDefault(passphrase, salt []byte, outputLen uint32) ([]byte, error) {
	return Argon2ID(passphrase, salt, Argon2TimeCost, Argon2MemoryCost, Argon2Parallelism, outputLen)
}
