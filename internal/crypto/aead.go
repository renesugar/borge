// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the _AEAD_BASE envelope in borg's src/borg/crypto/low_level.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package crypto implements borg's low-level cryptographic primitives: the AEAD
// envelope, the id-hash and MAC functions, and the passphrase KDF.
//
// The AEAD ciphers themselves come from elsewhere - ChaCha20-Poly1305 from
// golang.org/x/crypto, AES-256-OCB from internal/crypto/ocb, which had to be written
// from scratch because Go has no OCB anywhere. See that package's documentation; it is
// the highest-risk component in the port.
//
// Key derivation and key storage live one layer up, in the key package (stage 4).
// This package only knows how to encrypt a message once someone hands it a key.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/renesugar/borge/internal/crypto/ocb"
)

// Envelope sizes, fixed by the format.
const (
	// KeySize is the AEAD key size. Both of borg's suites use 256-bit keys.
	KeySize = 32
	// IVSize is the nonce size borg gives its AEAD ciphers.
	IVSize = 12
	// MACSize is the authentication tag size.
	MACSize = 16
)

// ErrAuthentication means the envelope failed authentication: it was corrupted,
// truncated, or tampered with. borg raises IntegrityError here.
//
// It deliberately does not say which, and callers must not either. Distinguishing a
// bad tag from a bad length gives an attacker a decryption oracle.
var ErrAuthentication = errors.New("crypto: authentication failed")

// Suite identifies an AEAD ciphersuite.
type Suite int

const (
	// SuiteAESOCB is AES-256-OCB, borg's aes256-ocb mode (key type 0x10).
	SuiteAESOCB Suite = iota
	// SuiteChaCha20Poly1305 is borg's chacha20-poly1305 mode (key type 0x20).
	SuiteChaCha20Poly1305
)

func (s Suite) String() string {
	switch s {
	case SuiteAESOCB:
		return "aes256-ocb"
	case SuiteChaCha20Poly1305:
		return "chacha20-poly1305"
	default:
		return fmt.Sprintf("unknown suite %d", int(s))
	}
}

// cipherBlockSize is the block size used only for the message-length limit below.
// It is the cipher's own notion of a block: 16 bytes for AES, 64 for ChaCha20.
func (s Suite) cipherBlockSize() int {
	if s == SuiteChaCha20Poly1305 {
		return 64
	}
	return 16
}

// maxCipherBlocks bounds how much data may be encrypted under one (key, IV) pair.
//
// ChaCha20 has an internal 32-bit block counter, so exceeding 2^32 blocks would repeat
// keystream. AES-OCB has no such counter - it derives per-block offsets from the IV -
// but borg applies the same limit to both, because the check costs nothing and cannot
// trigger for real messages, which are bounded by MAX_DATA_SIZE.
const maxCipherBlocks = 1 << 32

// AEAD is borg's authenticated-encryption envelope.
//
// # Envelope layout
//
//	header (HeaderLen bytes) || tag (16 bytes) || ciphertext (len(plaintext) bytes)
//
// Note the tag comes *before* the ciphertext, which is not Go's convention - the
// standard library appends it. The header travels in the clear.
//
// # What gets authenticated
//
//	AAD = aad || header[AADOffset:]
//
// in that order. `aad` is authenticated but not transmitted (the caller reconstructs
// it); the header is transmitted. AADOffset lets a prefix of the header be excluded
// from authentication, which borg uses for header bytes that legitimately change.
//
// Both parts matter: a port that authenticates only one of them, or concatenates them
// the other way round, produces envelopes borg rejects with no useful diagnostic.
type AEAD struct {
	suite     Suite
	aead      cipher.AEAD
	headerLen int
	aadOffset int
}

// NewAEAD builds an envelope for the given suite.
//
// headerLen is the exact header length every call must supply; aadOffset is where in
// that header the authenticated part starts, and must not exceed headerLen.
func NewAEAD(suite Suite, key []byte, headerLen, aadOffset int) (*AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}
	if headerLen < 0 {
		return nil, fmt.Errorf("crypto: negative header length %d", headerLen)
	}
	if aadOffset < 0 || aadOffset > headerLen {
		return nil, fmt.Errorf("crypto: aad offset %d outside the %d byte header", aadOffset, headerLen)
	}

	var inner cipher.AEAD
	var err error
	switch suite {
	case SuiteAESOCB:
		var block cipher.Block
		block, err = aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("crypto: %w", err)
		}
		inner, err = ocb.New(block)
	case SuiteChaCha20Poly1305:
		inner, err = chacha20poly1305.New(key)
	default:
		return nil, fmt.Errorf("crypto: unknown ciphersuite %d", int(suite))
	}
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	if inner.NonceSize() != IVSize {
		return nil, fmt.Errorf("crypto: %s wants a %d byte nonce, borg uses %d",
			suite, inner.NonceSize(), IVSize)
	}
	if inner.Overhead() != MACSize {
		return nil, fmt.Errorf("crypto: %s produces a %d byte tag, borg uses %d",
			suite, inner.Overhead(), MACSize)
	}

	return &AEAD{suite: suite, aead: inner, headerLen: headerLen, aadOffset: aadOffset}, nil
}

// Suite reports which ciphersuite this envelope uses.
func (a *AEAD) Suite() Suite { return a.suite }

// HeaderLen is the exact header length Encrypt requires and Decrypt expects.
func (a *AEAD) HeaderLen() int { return a.headerLen }

// EnvelopeOverhead is how many bytes an envelope adds to the plaintext.
func (a *AEAD) EnvelopeOverhead() int { return a.headerLen + MACSize }

// Encrypt produces header || tag || ciphertext.
//
// iv must be 12 bytes and must never repeat for a given key: both OCB and
// ChaCha20-Poly1305 fail catastrophically on nonce reuse. This function cannot check
// that; nonce management belongs to the key layer.
func (a *AEAD) Encrypt(iv, plaintext, header, aad []byte) ([]byte, error) {
	if len(iv) != IVSize {
		return nil, fmt.Errorf("crypto: iv must be %d bytes, got %d", IVSize, len(iv))
	}
	if len(header) != a.headerLen {
		return nil, fmt.Errorf("crypto: header must be %d bytes, got %d", a.headerLen, len(header))
	}
	if blocks := blockCount(len(plaintext), a.suite.cipherBlockSize()); blocks > maxCipherBlocks {
		return nil, fmt.Errorf("crypto: message too large: %d cipher blocks exceeds the limit of %d",
			blocks, int64(maxCipherBlocks))
	}

	sealed := a.aead.Seal(nil, iv, plaintext, a.authenticatedData(header, aad))

	// Go's AEAD appends the tag; borg's envelope puts it between the header and the
	// ciphertext. Rearrange rather than teaching the cipher a different layout.
	ct, tag := sealed[:len(plaintext)], sealed[len(plaintext):]

	out := make([]byte, 0, a.headerLen+MACSize+len(ct))
	out = append(out, header...)
	out = append(out, tag...)
	out = append(out, ct...)
	return out, nil
}

// Decrypt authenticates an envelope and returns its plaintext.
//
// It returns ErrAuthentication for anything wrong with the envelope - a bad tag,
// truncation, altered associated data. No plaintext is returned in that case.
func (a *AEAD) Decrypt(iv, envelope, aad []byte) ([]byte, error) {
	if len(iv) != IVSize {
		return nil, fmt.Errorf("crypto: iv must be %d bytes, got %d", IVSize, len(iv))
	}
	if len(envelope) < a.headerLen+MACSize {
		// Truncated. borg treats this like any other corruption rather than reporting a
		// distinct error, and so does borge: the difference is not the caller's business
		// and telling them would be a side channel.
		return nil, ErrAuthentication
	}
	if blocks := blockCount(len(envelope), a.suite.cipherBlockSize()); blocks > maxCipherBlocks {
		return nil, fmt.Errorf("crypto: message too large: %d cipher blocks exceeds the limit of %d",
			blocks, int64(maxCipherBlocks))
	}

	header := envelope[:a.headerLen]
	tag := envelope[a.headerLen : a.headerLen+MACSize]
	ct := envelope[a.headerLen+MACSize:]

	// Reassemble into Go's ciphertext || tag order.
	sealed := make([]byte, 0, len(ct)+MACSize)
	sealed = append(sealed, ct...)
	sealed = append(sealed, tag...)

	plaintext, err := a.aead.Open(nil, iv, sealed, a.authenticatedData(header, aad))
	if err != nil {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

// authenticatedData builds the AEAD's associated data: the caller's aad followed by
// the authenticated part of the header. The order is part of the format.
func (a *AEAD) authenticatedData(header, aad []byte) []byte {
	authHeader := header[a.aadOffset:]
	if len(authHeader) == 0 {
		return aad
	}
	out := make([]byte, 0, len(aad)+len(authHeader))
	out = append(out, aad...)
	out = append(out, authHeader...)
	return out
}

// blockCount is borg's num_cipher_blocks: the number of cipher blocks a message of
// this length occupies, rounding up.
func blockCount(length, blockSize int) int64 {
	if length <= 0 {
		return 0
	}
	return (int64(length) + int64(blockSize) - 1) / int64(blockSize)
}
