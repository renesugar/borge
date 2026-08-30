// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of AEADKeyBase and its subclasses in borg's
// src/borg/crypto/key.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package key

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/renesugar/borge/internal/crypto"
)

// The AEAD envelope, in bytes:
//
//	type(1) || reserved(1) || messageIV(6) || sessionID(24) || tag(16) || ciphertext
//	|<------------------- header, all authenticated ------------------>|
//
// borg's docstring gives the same layout in bits. Everything up to the tag travels in
// the clear and is covered by the tag, together with the caller's AAD and the chunk id.
const (
	aeadTypeOffset      = 0
	aeadReservedOffset  = 1
	aeadIVOffset        = 2
	aeadIVSize          = 6
	aeadSessionOffset   = 8
	aeadSessionIDSize   = 24
	aeadHeaderSize      = 1 + 1 + aeadIVSize + aeadSessionIDSize // 32
	aeadPayloadOverhead = aeadHeaderSize + crypto.MACSize        // 48
)

// maxAEADIV is the largest value the 48-bit message counter can hold. Reaching it would
// need 2^48 messages under one session id, which the session limits below make
// impossible in practice; the check exists so a bug cannot silently repeat a nonce.
const maxAEADIV = 1<<48 - 1

// aesOCBMaxSessionBlocks is how much data one AES-OCB session key may cover, in 128-bit
// cipher blocks.
//
// OCB has a birthday bound: an attacker's advantage in distinguishing ciphertexts from
// random is about 6*sigma^2/2^128 for sigma blocks under one key. RFC 7253 derives its
// "at most 2^48 blocks" rule of thumb from that (advantage 2^-32); borg aims higher and
// uses 2^37 blocks (2 TiB), for an advantage of about 2^-51.
//
// Rolling the session key is one sha256 and is fully transparent, because the session id
// travels in every header - so old objects stay readable. It also helps in the multi-key
// setting: the advantages add per session key, so what matters over the lifetime of the
// repository key is sum(sigma_i^2) rather than (sum sigma_i)^2.
const aesOCBMaxSessionBlocks int64 = 1 << 37

// ChaCha20-Poly1305 needs no such limit: its confidentiality bound does not depend on
// how much data is encrypted at all, and its integrity bound limits only the number of
// *failed* decryptions, which is counted across all keys and so cannot be improved by
// using more session keys. maxSessionBlocks is left at zero for it, meaning unlimited.

// Session key derivation domains. The domain is borg's
// b"borg-session-key-" + CIPHERSUITE.__name__, so the strings below are the Python class
// names from low_level.pyx and must not be "tidied up" - they are format.
var (
	sessionDomainAESOCB = []byte("borg-session-key-AES256_OCB")
	sessionDomainCHPO   = []byte("borg-session-key-CHACHA20_POLY1305")
)

// sessionCacheMax bounds the decryption-side session key cache. Reads of one archive
// touch few sessions, so a small cache turns the per-object key derivation into a map
// lookup; when it fills, it is dropped wholesale rather than evicted one at a time,
// which costs one rederivation per live session and needs no bookkeeping.
const sessionCacheMax = 64

// aeadKey is a key whose objects are encrypted and authenticated with an AEAD cipher.
//
// # Sessions
//
// Every message needs a unique nonce, and the nonce is only 48 bits of counter. Rather
// than persisting that counter - which would make a restored backup or a second writer
// catastrophic - borg draws a random 192-bit session id per key instance, derives a
// session key from it, and counts messages from 1 within that session. The session id is
// stored in every object's header, so decryption can rederive the key.
//
// The consequence is that a nonce collision needs a session id collision, which is a
// birthday problem over 192 bits rather than a counter that must never be reused across
// runs.
//
// # Concurrency
//
// Unlike borg's, this type is safe to use from several goroutines. Stage 6 backs up with
// a worker pool, and the counter must not be handed out twice: a repeated (session key,
// nonce) pair breaks both ciphers completely. The mutex covers only the counter and the
// session rotation; the cipher itself is immutable after construction and does the actual
// work outside the lock.
type aeadKey struct {
	typeByte byte
	name     string
	suite    crypto.Suite

	sessionDomain []byte
	// maxSessionBlocks is the session key's data budget, or 0 for unlimited.
	maxSessionBlocks int64

	cryptKey []byte
	idKey    []byte
	idHash   func(idKey, data []byte) []byte

	mu sync.Mutex
	// Write-side session state, all guarded by mu.
	sessionID     []byte
	cipher        *crypto.AEAD
	nextIV        uint64
	sessionBlocks int64
	// sessions caches read-side ciphers by session id, also guarded by mu.
	sessions map[string]*crypto.AEAD
}

func (k *aeadKey) Type() byte     { return k.typeByte }
func (k *aeadKey) Name() string   { return k.name }
func (k *aeadKey) Encrypts() bool { return true }

// IDCheckIsAuthentication is false: the AEAD tag authenticates every read on its own,
// with the chunk id in the AAD. See AssertID for the one thing the id check still adds.
func (k *aeadKey) IDCheckIsAuthentication() bool { return false }

func (k *aeadKey) IDHash(data []byte) []byte { return k.idHash(k.idKey, data) }

// DeriveIDKey derives from the id key; see the Key interface for why it is the id key and
// not the crypt key.
func (k *aeadKey) DeriveIDKey(domain []byte, size int) ([]byte, error) {
	return DeriveKey(k.idKey, nil, domain, size)
}

// newSession draws a fresh session id and derives its key. Callers hold k.mu.
func (k *aeadKey) newSession() error {
	id := make([]byte, aeadSessionIDSize)
	if _, err := rand.Read(id); err != nil {
		return fmt.Errorf("key: could not draw a session id: %w", err)
	}
	c, err := k.cipherFor(id)
	if err != nil {
		return err
	}
	k.sessionID = id
	k.cipher = c
	k.nextIV = 1 // borg's first message uses IV 1: the cipher starts at 0 and next_iv() pre-increments.
	k.sessionBlocks = 0
	return nil
}

// cipherFor derives the session key for a session id and builds the envelope for it.
func (k *aeadKey) cipherFor(sessionID []byte) (*crypto.AEAD, error) {
	if len(sessionID) != aeadSessionIDSize {
		return nil, fmt.Errorf("key: session id must be %d bytes, got %d", aeadSessionIDSize, len(sessionID))
	}
	sessionKey, err := DeriveKey(k.cryptKey, sessionID, k.sessionDomain, crypto.KeySize)
	if err != nil {
		return nil, err
	}
	return crypto.NewAEAD(k.suite, sessionKey, aeadHeaderSize, 0)
}

// cachedCipherFor is cipherFor with the read-side cache in front of it. Callers hold
// k.mu.
func (k *aeadKey) cachedCipherFor(sessionID []byte) (*crypto.AEAD, error) {
	if c, ok := k.sessions[string(sessionID)]; ok {
		return c, nil
	}
	c, err := k.cipherFor(sessionID)
	if err != nil {
		return nil, err
	}
	if len(k.sessions) >= sessionCacheMax {
		k.sessions = make(map[string]*crypto.AEAD, sessionCacheMax)
	}
	k.sessions[string(sessionID)] = c
	return c, nil
}

// messageBlocks is how many cipher blocks a message will occupy, borg's accounting.
//
// The payload and the associated data are separate strings to the cipher and so round up
// separately; the +2 covers the per-message setup. The divisor is 16 even for
// ChaCha20-Poly1305, whose block is 64 bytes - which is harmless, because only AES-OCB
// has a session limit at all.
func messageBlocks(dataLen, aadLen int) int64 {
	blocks := int64((dataLen+15)/16) + int64((aeadHeaderSize+aadLen+15)/16) + 2
	return blocks
}

// Encrypt wraps a payload in the AEAD envelope.
func (k *aeadKey) Encrypt(id, data, aad []byte) ([]byte, error) {
	iv, sessionID, cipher, err := k.reserve(len(data), len(aad)+len(id))
	if err != nil {
		return nil, err
	}

	header := make([]byte, aeadHeaderSize)
	header[aeadTypeOffset] = k.typeByte
	header[aeadReservedOffset] = 0
	putUint48(header[aeadIVOffset:aeadIVOffset+aeadIVSize], iv)
	copy(header[aeadSessionOffset:], sessionID)

	full := make([]byte, 0, len(aad)+len(id))
	full = append(full, aad...)
	full = append(full, id...)

	return cipher.Encrypt(nonceFor(iv), data, header, full)
}

// reserve takes the next message counter, rotating the session first if this message
// would exceed its budget. It is the only place the counter is handed out, and it is the
// reason this type has a mutex.
func (k *aeadKey) reserve(dataLen, aadLen int) (iv uint64, sessionID []byte, cipher *crypto.AEAD, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.cipher == nil {
		if err := k.newSession(); err != nil {
			return 0, nil, nil, err
		}
	}
	if k.maxSessionBlocks > 0 {
		blocks := messageBlocks(dataLen, aadLen)
		if k.sessionBlocks+blocks > k.maxSessionBlocks {
			if err := k.newSession(); err != nil {
				return 0, nil, nil, err
			}
		}
		k.sessionBlocks += blocks
	}
	if k.nextIV > maxAEADIV {
		// Unreachable with the session limits above, and a correctness disaster if it
		// were not: refuse rather than wrap.
		return 0, nil, nil, errors.New("key: message counter overflow, which should never happen")
	}
	iv = k.nextIV
	k.nextIV++
	return iv, k.sessionID, k.cipher, nil
}

// Decrypt unwraps and verifies an AEAD envelope.
func (k *aeadKey) Decrypt(id, envelope, aad []byte) ([]byte, error) {
	if len(envelope) < aeadPayloadOverhead {
		return nil, &IntegrityError{ChunkID: id, Reason: "truncated envelope"}
	}
	if envelope[aeadTypeOffset] != k.typeByte {
		return nil, &IntegrityError{ChunkID: id, Reason: "invalid encryption envelope"}
	}

	iv := uint48(envelope[aeadIVOffset : aeadIVOffset+aeadIVSize])
	sessionID := envelope[aeadSessionOffset : aeadSessionOffset+aeadSessionIDSize]

	k.mu.Lock()
	cipher, err := k.cachedCipherFor(sessionID)
	k.mu.Unlock()
	if err != nil {
		return nil, err
	}

	full := make([]byte, 0, len(aad)+len(id))
	full = append(full, aad...)
	full = append(full, id...)

	plaintext, err := cipher.Decrypt(nonceFor(iv), envelope, full)
	if err != nil {
		// Deliberately not distinguishing *why* - see crypto.ErrAuthentication.
		return nil, &IntegrityError{ChunkID: id, Reason: "could not decrypt"}
	}
	return plaintext, nil
}

// nonceFor turns the message counter into the cipher's 12-byte nonce: the same integer,
// big-endian, in the full nonce width. Only its low 48 bits are stored in the header,
// because the rest can never be non-zero.
func nonceFor(iv uint64) []byte {
	nonce := make([]byte, crypto.IVSize)
	binary.BigEndian.PutUint64(nonce[crypto.IVSize-8:], iv)
	return nonce
}

func putUint48(dst []byte, v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	copy(dst, buf[2:])
}

func uint48(src []byte) uint64 {
	var buf [8]byte
	copy(buf[2:], src)
	return binary.BigEndian.Uint64(buf[:])
}

// SessionID exposes the current write-side session id, for tests and for debug output.
// It is nil until the first Encrypt.
func (k *aeadKey) SessionID() []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.sessionID == nil {
		return nil
	}
	return append([]byte(nil), k.sessionID...)
}

// ------------------------------------------------------------------ constructors

// newAEADKey is the shared constructor. cryptKey is the long-term secret every session
// key is derived from; idKey keys the chunk id hash.
func newAEADKey(typeByte byte, name string, suite crypto.Suite, domain []byte, maxBlocks int64,
	cryptKey, idKey []byte, idHash func(idKey, data []byte) []byte,
) (Key, error) {
	// borg accepts 32+32 and 32+128; the second is a borg 1.x blake2b key, which borge
	// does not read (plans/PORTING_PLAN.md §0.6), so 64 is the only valid length here.
	if len(cryptKey) != 64 {
		return nil, fmt.Errorf("key: %s needs a 64 byte crypt key, got %d", name, len(cryptKey))
	}
	if len(idKey) != 32 {
		return nil, fmt.Errorf("key: %s needs a 32 byte id key, got %d", name, len(idKey))
	}
	return &aeadKey{
		typeByte:         typeByte,
		name:             name,
		suite:            suite,
		sessionDomain:    domain,
		maxSessionBlocks: maxBlocks,
		cryptKey:         append([]byte(nil), cryptKey...),
		idKey:            append([]byte(nil), idKey...),
		idHash:           idHash,
		sessions:         make(map[string]*crypto.AEAD, 8),
	}, nil
}

func idHashHMACSHA256(idKey, data []byte) []byte { return crypto.HMACSHA256(idKey, data) }

func idHashBlake3(idKey, data []byte) []byte {
	out, err := crypto.Blake3Keyed(idKey, data)
	if err != nil {
		// Unreachable: newAEADKey validates the key length.
		panic("key: blake3 id hash: " + err.Error())
	}
	return out
}

// NewAESOCB returns the aes256-ocb mode: AES-256-OCB with HMAC-SHA-256 chunk ids.
func NewAESOCB(cryptKey, idKey []byte) (Key, error) {
	return newAEADKey(TypeAESOCB, "aes256-ocb", crypto.SuiteAESOCB, sessionDomainAESOCB,
		aesOCBMaxSessionBlocks, cryptKey, idKey, idHashHMACSHA256)
}

// NewCHPO returns the chacha20-poly1305 mode with HMAC-SHA-256 chunk ids.
func NewCHPO(cryptKey, idKey []byte) (Key, error) {
	return newAEADKey(TypeCHPO, "chacha20-poly1305", crypto.SuiteChaCha20Poly1305, sessionDomainCHPO,
		0, cryptKey, idKey, idHashHMACSHA256)
}

// NewBlake3AESOCB returns the aes256-ocb mode with BLAKE3 chunk ids.
func NewBlake3AESOCB(cryptKey, idKey []byte) (Key, error) {
	return newAEADKey(TypeBlake3AESOCB, "blake3-aes256-ocb", crypto.SuiteAESOCB, sessionDomainAESOCB,
		aesOCBMaxSessionBlocks, cryptKey, idKey, idHashBlake3)
}

// NewBlake3CHPO returns the chacha20-poly1305 mode with BLAKE3 chunk ids.
func NewBlake3CHPO(cryptKey, idKey []byte) (Key, error) {
	return newAEADKey(TypeBlake3CHPO, "blake3-chacha20-poly1305", crypto.SuiteChaCha20Poly1305,
		sessionDomainCHPO, 0, cryptKey, idKey, idHashBlake3)
}
