// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the key classes in borg's src/borg/crypto/key.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package key implements borg's repository keys: the id hash and the per-slot envelope
// that every repository object is wrapped in.
//
// # What a key does
//
// Three things, and they are separable:
//
//   - **Id hash.** A chunk's id is a hash of its plaintext. In the keyed modes it is a
//     *keyed* hash, so only someone with the repository key can compute an id - which
//     is what stops an observer from confirming that a known file is in the repository.
//   - **Envelope.** Encrypt and Decrypt wrap a payload so it can be authenticated (and
//     in the AEAD modes, hidden).
//   - **Storage.** Where the key material itself lives - a keyfile or the repository.
//     That is stage 4; this package currently implements the modes that need no stored
//     key material and the ones whose material is supplied directly.
//
// # Which modes are implemented
//
// The MAC-based family, which covers everything that does not encrypt:
//
//	none-sha256            unkeyed checksum       type 0x80
//	none-blake3            unkeyed checksum       type 0x90
//	authenticated-sha256   keyed MAC              type 0x60
//	authenticated-blake3   keyed MAC              type 0x70
//
// docs/PORTING_PLAN.md §7 calls for exactly this order: these modes exercise the whole
// object and archive path with no crypto risk, so stages 3, 5 and 6 can be built and
// interop-tested before the AEAD modes and their key derivation land.
//
// The AEAD modes (aes256-ocb, chacha20-poly1305, and their blake3 variants) are
// stage 4. internal/crypto already has the ciphers; what is missing is the session key
// derivation and the key blob handling.
package key

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/renesugar/borge/internal/crypto"
)

// Key type bytes (src/borg/constants.py: KeyType). The byte identifies the ciphersuite
// only - where the key is stored is deliberately not encoded in it.
const (
	TypeSHA256Authenticated byte = 0x60
	TypeBlake3Authenticated byte = 0x70
	TypeSHA256None          byte = 0x80
	TypeBlake3None          byte = 0x90

	// Legacy borg 1.x types and the AEAD types, listed so borge can recognise and
	// refuse them with something better than "unknown".
	TypeKeyfile             byte = 0x00
	TypePassphrase          byte = 0x01
	TypePlaintext           byte = 0x02
	TypeRepo                byte = 0x03
	TypeBlake2Keyfile       byte = 0x04
	TypeBlake2Repo          byte = 0x05
	TypeBlake2Authenticated byte = 0x06
	TypeAuthenticated       byte = 0x07

	TypeAESOCB       byte = 0x10
	TypeCHPO         byte = 0x20
	TypeBlake3AESOCB byte = 0x30
	TypeBlake3CHPO   byte = 0x40
	// TypeDroppedBlake3Authenticated was a borg 2 beta format, removed before release
	// (borg #9104). The byte stays reserved so it is never reused.
	TypeDroppedBlake3Authenticated byte = 0x50
)

// ErrIntegrity means an object failed its integrity check: a bad tag, a truncated
// envelope, a wrong type byte, or an id that does not match its plaintext.
var ErrIntegrity = errors.New("key: integrity error")

// IntegrityError describes what failed. The message names the chunk, because a repair
// needs to know which object to look at.
type IntegrityError struct {
	ChunkID []byte
	Reason  string
}

func (e *IntegrityError) Error() string {
	id := "(unknown)"
	if len(e.ChunkID) > 0 {
		id = hex.EncodeToString(e.ChunkID)
	}
	return fmt.Sprintf("key: chunk %s: %s", id, e.Reason)
}
func (e *IntegrityError) Unwrap() error { return ErrIntegrity }

// ManifestID is the fixed chunk id of the manifest: 32 zero bytes.
//
// It is not a hash of anything, so AssertID skips it - checking it would fail every
// time.
var ManifestID = make([]byte, 32)

// Key is what a repository object needs to be written or read.
type Key interface {
	// Type is the key type byte stored in every envelope.
	Type() byte
	// Name is the mode's name as borg spells it, e.g. "none-sha256".
	Name() string

	// IDHash computes a chunk id from its plaintext.
	IDHash(data []byte) []byte

	// Encrypt wraps a payload in this mode's envelope. aad is authenticated but not
	// stored; the caller reconstructs it.
	Encrypt(id, data, aad []byte) ([]byte, error)
	// Decrypt unwraps and verifies an envelope.
	Decrypt(id, envelope, aad []byte) ([]byte, error)

	// IDCheckIsAuthentication reports whether verifying the chunk id is what
	// authenticates a read.
	//
	// It is true for the unkeyed modes, whose envelope checksum anyone can recompute:
	// there, skipping the id check would remove all integrity checking from reads. It
	// is false for the keyed modes, where the envelope tag already authenticates the
	// payload for that specific chunk id, so the id check is an extra full-plaintext
	// hash pass that BORG_ASSERT_ID makes optional.
	IDCheckIsAuthentication() bool

	// Encrypts reports whether this mode hides the payload. The MAC modes do not.
	Encrypts() bool
}

// AssertID verifies that data hashes to id.
//
// An empty id, and the manifest's all-zero id, are skipped - neither is a hash of the
// data it accompanies.
func AssertID(k Key, id, data []byte) error {
	if len(id) == 0 || subtle.ConstantTimeCompare(id, ManifestID) == 1 {
		return nil
	}
	computed := k.IDHash(data)
	if subtle.ConstantTimeCompare(computed, id) != 1 {
		return &IntegrityError{ChunkID: id, Reason: "id verification failed"}
	}
	return nil
}

// DeriveKey produces new key material from existing material, a salt and a domain.
//
//	sha256(fromKey || salt || domain)[:size]
//
// A one-step KDF is enough because the input is already a pseudorandom key rather than
// a passphrase, so PRF security suffices (NIST SP 800-56C rev 2 §4). size is capped at
// 32, the width of the hash.
//
// The domain is what keeps derived keys apart: the chunker's table key, the envelope
// MAC key and the session keys all come from the same material and must never collide.
func DeriveKey(fromKey, salt, domain []byte, size int) ([]byte, error) {
	if size > sha256.Size {
		return nil, fmt.Errorf("key: cannot derive %d bytes from sha256, the maximum is %d", size, sha256.Size)
	}
	h := sha256.New()
	h.Write(fromKey)
	h.Write(salt)
	h.Write(domain)
	return h.Sum(nil)[:size], nil
}

// ---------------------------------------------------------------- MAC-based modes

// macKey is the shared body of every mode that tags rather than encrypts.
//
// Envelope layout:
//
//	TYPE(1) || reserved(1) || tag(32) || payload
//
// The payload is stored as-is, so anyone can read it; the tag is computed over the
// header, the AAD and the payload.
//
// The tag is deterministic - no nonce, no session, no state. Two repositories with the
// same key material therefore write byte-identical objects for identical input, which
// lets them deduplicate against each other at the filesystem level.
type macKey struct {
	typeByte byte
	name     string
	// tagKey is nil for the unkeyed (none-*) modes.
	tagKey []byte
	// mac computes the tag over prefix || payload.
	mac func(tagKey, prefix, payload []byte) []byte
	// idHash computes a chunk id, keyed or not depending on the mode.
	idHash func(idKey, data []byte) []byte
	idKey  []byte
	// unkeyed marks the none-* modes, whose checksum is not an authentication.
	unkeyed bool
}

const (
	macTagSize         = 32
	macHeaderSize      = 2
	macPayloadOverhead = macHeaderSize + macTagSize
)

func (k *macKey) Type() byte     { return k.typeByte }
func (k *macKey) Name() string   { return k.name }
func (k *macKey) Encrypts() bool { return false }

// IDCheckIsAuthentication is true only for the unkeyed modes; see the interface comment.
func (k *macKey) IDCheckIsAuthentication() bool { return k.unkeyed }

func (k *macKey) IDHash(data []byte) []byte { return k.idHash(k.idKey, data) }

// tagPrefix is everything the tag covers except the payload.
//
// The AAD is length-prefixed, so the boundary between it and the payload is
// unambiguous. borg notes that the AAD has a fixed length today but the tag must not
// depend on that staying true - a tag that could be reinterpreted with a different
// AAD/payload split would be forgeable by shifting bytes across the boundary.
func (k *macKey) tagPrefix(header, aad, id []byte) ([]byte, error) {
	aadFull := make([]byte, 0, len(aad)+len(id))
	aadFull = append(aadFull, aad...)
	aadFull = append(aadFull, id...)
	if len(aadFull) > 0xFFFF {
		return nil, fmt.Errorf("key: aad is %d bytes, too long to length-prefix", len(aadFull))
	}
	out := make([]byte, 0, len(header)+2+len(aadFull))
	out = append(out, header...)
	out = append(out, byte(len(aadFull)>>8), byte(len(aadFull)))
	out = append(out, aadFull...)
	return out, nil
}

func (k *macKey) Encrypt(id, data, aad []byte) ([]byte, error) {
	header := []byte{k.typeByte, 0} // TYPE + reserved; reserved is authenticated, so it
	//                                 can carry a format flag later without a size change.
	prefix, err := k.tagPrefix(header, aad, id)
	if err != nil {
		return nil, err
	}
	tag := k.mac(k.tagKey, prefix, data)

	out := make([]byte, 0, macPayloadOverhead+len(data))
	out = append(out, header...)
	out = append(out, tag...)
	out = append(out, data...)
	return out, nil
}

func (k *macKey) Decrypt(id, envelope, aad []byte) ([]byte, error) {
	if len(envelope) < macPayloadOverhead {
		return nil, &IntegrityError{ChunkID: id, Reason: "truncated envelope"}
	}
	if envelope[0] != k.typeByte {
		return nil, &IntegrityError{ChunkID: id, Reason: "invalid encryption envelope"}
	}

	header := envelope[:macHeaderSize]
	tag := envelope[macHeaderSize:macPayloadOverhead]
	payload := envelope[macPayloadOverhead:]

	prefix, err := k.tagPrefix(header, aad, id)
	if err != nil {
		return nil, err
	}
	computed := k.mac(k.tagKey, prefix, payload)
	if subtle.ConstantTimeCompare(computed, tag) != 1 {
		return nil, &IntegrityError{ChunkID: id, Reason: "envelope tag verification failed"}
	}
	// Copy: the caller must not hold a slice into the envelope buffer, which may be a
	// window into a memory-mapped pack.
	out := make([]byte, len(payload))
	copy(out, payload)
	return out, nil
}

// NewNoneSHA256 returns the none-sha256 mode: no encryption, no key, and an *unkeyed*
// checksum.
//
// The checksum detects accidental corruption - a bad disk, a read that returned the
// wrong bytes - but it is not protection against tampering, because whoever changes an
// object can recompute it. Chunk ids are unkeyed too, so every none-sha256 repository
// deduplicates identically with no key sharing.
func NewNoneSHA256() Key {
	return &macKey{
		typeByte: TypeSHA256None,
		name:     "none-sha256",
		unkeyed:  true,
		idHash:   func(_, data []byte) []byte { s := sha256.Sum256(data); return s[:] },
		mac: func(_, prefix, payload []byte) []byte {
			h := sha256.New()
			h.Write(prefix)
			h.Write(payload)
			return h.Sum(nil)
		},
	}
}

// NewNoneBlake3 returns the none-blake3 mode: as none-sha256, with BLAKE3.
func NewNoneBlake3() Key {
	return &macKey{
		typeByte: TypeBlake3None,
		name:     "none-blake3",
		unkeyed:  true,
		idHash:   func(_, data []byte) []byte { return crypto.Blake3Unkeyed(data) },
		mac: func(_, prefix, payload []byte) []byte {
			return crypto.Blake3UnkeyedTwo(prefix, payload)
		},
	}
}

// MAC key derivation domains (src/borg/crypto/key.py). Each mode has its own, so the
// same crypt_key cannot produce the same tag key for two different tag algorithms.
var (
	macKeyDomainHMACSHA256 = []byte("borg-repoobj-mac-hmac-sha256")
	macKeyDomainBlake3     = []byte("borg-repoobj-mac-blake3")
)

// NewAuthenticatedSHA256 returns the authenticated-sha256 mode: no encryption, but a
// real MAC, so tampering is detected whether accidental or malicious.
//
// cryptKey and idKey are the key material. Note the tag key is derived from *cryptKey*,
// not from idKey: chunk ids are public and related repositories share the id key, which
// must not give them the ability to forge each other's objects.
func NewAuthenticatedSHA256(cryptKey, idKey []byte) (Key, error) {
	tagKey, err := DeriveKey(cryptKey, nil, macKeyDomainHMACSHA256, 32)
	if err != nil {
		return nil, err
	}
	return &macKey{
		typeByte: TypeSHA256Authenticated,
		name:     "authenticated-sha256",
		tagKey:   tagKey,
		idKey:    idKey,
		idHash:   func(idKey, data []byte) []byte { return crypto.HMACSHA256(idKey, data) },
		mac: func(tagKey, prefix, payload []byte) []byte {
			h := hmac.New(sha256.New, tagKey)
			h.Write(prefix)
			h.Write(payload)
			return h.Sum(nil)
		},
	}, nil
}

// NewAuthenticatedBlake3 returns the authenticated-blake3 mode.
func NewAuthenticatedBlake3(cryptKey, idKey []byte) (Key, error) {
	tagKey, err := DeriveKey(cryptKey, nil, macKeyDomainBlake3, 32)
	if err != nil {
		return nil, err
	}
	return &macKey{
		typeByte: TypeBlake3Authenticated,
		name:     "authenticated-blake3",
		tagKey:   tagKey,
		idKey:    idKey,
		idHash: func(idKey, data []byte) []byte {
			out, err := crypto.Blake3Keyed(idKey, data)
			if err != nil {
				// Unreachable: the constructors validate the key length.
				panic("key: blake3 id hash: " + err.Error())
			}
			return out
		},
		mac: func(tagKey, prefix, payload []byte) []byte {
			out, err := crypto.Blake3KeyedTwo(tagKey, prefix, payload)
			if err != nil {
				panic("key: blake3 mac: " + err.Error())
			}
			return out
		},
	}, nil
}

// ByName builds a key by its borg mode name. cryptKey and idKey are ignored by the
// unkeyed modes.
func ByName(name string, cryptKey, idKey []byte) (Key, error) {
	switch name {
	case "none-sha256":
		return NewNoneSHA256(), nil
	case "none-blake3":
		return NewNoneBlake3(), nil
	case "authenticated-sha256":
		return NewAuthenticatedSHA256(cryptKey, idKey)
	case "authenticated-blake3":
		return NewAuthenticatedBlake3(cryptKey, idKey)
	case "aes256-ocb", "chacha20-poly1305", "blake3-aes256-ocb", "blake3-chacha20-poly1305":
		return nil, fmt.Errorf("key: %s is not implemented yet (stage 4); "+
			"internal/crypto has the cipher, the key derivation and blob handling are missing", name)
	default:
		return nil, fmt.Errorf("key: unknown mode %q", name)
	}
}

// TypeName describes a key type byte, including the ones borge does not implement, so
// an error message can say what was found rather than only that it was unknown.
func TypeName(t byte) string {
	switch t {
	case TypeSHA256None:
		return "none-sha256"
	case TypeBlake3None:
		return "none-blake3"
	case TypeSHA256Authenticated:
		return "authenticated-sha256"
	case TypeBlake3Authenticated:
		return "authenticated-blake3"
	case TypeAESOCB:
		return "aes256-ocb"
	case TypeCHPO:
		return "chacha20-poly1305"
	case TypeBlake3AESOCB:
		return "blake3-aes256-ocb"
	case TypeBlake3CHPO:
		return "blake3-chacha20-poly1305"
	case TypeDroppedBlake3Authenticated:
		return "a dropped borg 2 beta format (reserved, never valid)"
	case TypeKeyfile, TypePassphrase, TypePlaintext, TypeRepo,
		TypeBlake2Keyfile, TypeBlake2Repo, TypeBlake2Authenticated, TypeAuthenticated:
		return fmt.Sprintf("a borg 1.x key type (0x%02x), which borge does not read", t)
	default:
		return fmt.Sprintf("unknown key type 0x%02x", t)
	}
}
