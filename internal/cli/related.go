// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the other_key branch of KeyBase.create in borg's
// src/borg/crypto/key.py, and of uses_same_id_hash / uses_same_chunker_secret.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/item"
)

// What makes two repositories *related*, and why transfer insists on it.
//
// borg's own summary of the feature calls the new key structure "independent", and that is
// the one thing it is not (PORTING_PLAN §11.1). Two of the three secrets are deliberately
// INHERITED:
//
//   - the **id key**, so the same content still hashes to the same chunk id;
//   - the **chunk seed**, so the chunker still cuts at the same boundaries.
//
// Only the **AE key** is new, and that is the re-encryption: every chunk is written under a
// fresh encryption key while keeping the identity that deduplication depends on. Without
// both inherited halves a transfer would store every chunk again under a new id - which
// looks exactly like success, only slower and larger, which is why borg refuses it up front
// rather than letting it happen.

// relatedMaterial builds key material for a repository related to the one at otherPath.
func (e *Env) relatedMaterial(otherPath string, repoID []byte, copyCryptKey bool) (*item.Key, error) {
	// Opened the way transfer opens its source, which matters for the passphrase: the two
	// repositories need not share one, and unlocking the source with the *destination's*
	// passphrase would work in every test where they happen to be equal and fail for a
	// user who set them apart - which is the case --other-repo exists for.
	other, err := e.openOtherRepo(otherPath)
	if err != nil {
		return nil, err
	}
	defer other.Close()

	if other.unlocked == nil || other.unlocked.Material == nil {
		// The unencrypted modes have no key material at all, which is the case borg names
		// explicitly - and it is not a failure to explain as "wrong passphrase".
		return nil, errors.New("Copying key material from an unencrypted repository is not possible.")
	}
	src := other.unlocked.Material
	if len(src.IDKey) == 0 || src.ChunkSeed == nil {
		return nil, errors.New("Copying key material from an unencrypted repository is not possible.")
	}

	material, err := key.NewMaterial(repoID)
	if err != nil {
		return nil, err
	}
	// Inherited: the two secrets that make deduplication work across the pair.
	material.IDKey = append([]byte(nil), src.IDKey...)
	seed := *src.ChunkSeed
	material.ChunkSeed = &seed
	if copyCryptKey {
		// The user asked for the same authenticated-encryption key. borg offers this and
		// borge does not second-guess it: a transfer then re-encrypts nothing, which is
		// faster and is a weaker boundary than the default.
		material.CryptKey = append([]byte(nil), src.CryptKey...)
	} else {
		// A fresh AE key. borg's comment: "borg transfer re-encrypts all data anyway, thus
		// we can default to a new, random AE key". NewMaterial already drew one; this is
		// only here to say that the default is deliberate rather than incidental.
		fresh := make([]byte, len(material.CryptKey))
		if _, err := rand.Read(fresh); err != nil {
			return nil, fmt.Errorf("key: could not draw a new encryption key: %w", err)
		}
		material.CryptKey = fresh
	}
	return material, nil
}

// idHashFamily names the id-hash family of an encryption mode.
//
// Two repositories dedup against each other only if their ids are computed the same way, so
// this is the equivalence borg's uses_same_id_hash tests. The families are borg's, minus the
// borg 1.x classes borge does not read: keyed HMAC-SHA256, keyed BLAKE3, unkeyed SHA-256 and
// unkeyed BLAKE3.
func idHashFamily(mode string) string {
	switch mode {
	case "aes256-ocb", "sha256-aes256-ocb", "chacha20-poly1305", "sha256-chacha20-poly1305",
		"authenticated-sha256":
		return "hmac-sha256"
	case "blake3-aes256-ocb", "blake3-chacha20-poly1305", "authenticated-blake3":
		return "blake3"
	case "none-sha256":
		return "unkeyed-sha256"
	case "none-blake3":
		return "unkeyed-blake3"
	}
	return "unknown:" + mode
}

// usesSameIDHash reports whether chunk ids from one mode mean the same thing in the other.
//
// Note what this allows: aes256-ocb -> chacha20-poly1305 is fine, because both key their
// ids with HMAC-SHA256 and only the encryption differs. aes256-ocb -> none-sha256 is not,
// because one keys the hash and the other does not.
func usesSameIDHash(otherMode, mode string) bool {
	f := idHashFamily(otherMode)
	// An unrecognised mode is never "the same": whatever it hashes with, this build does
	// not know, so it cannot promise the ids will line up.
	if strings.HasPrefix(f, "unknown:") {
		return false
	}
	return f == idHashFamily(mode)
}

// usesSameChunkerSecret is borg's: the chunk seeds must match, or the two repositories cut
// chunks at different boundaries and share nothing.
func usesSameChunkerSecret(other, this *item.Key) bool {
	if other == nil || this == nil || other.ChunkSeed == nil || this.ChunkSeed == nil {
		// An unencrypted repository has no seed. borg's chunk_seed is 0 there for both
		// sides, so they match; a missing seed on one side alone does not.
		return (other == nil || other.ChunkSeed == nil) && (this == nil || this.ChunkSeed == nil)
	}
	return *other.ChunkSeed == *this.ChunkSeed
}
