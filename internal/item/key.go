// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the Key and EncryptedKey classes in borg's
// src/borg/item.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package item

import (
	"fmt"

	"github.com/renesugar/borge/internal/msgpackx"
)

// EncryptedKey is the passphrase-protected key blob, as stored in keys/<digest> for a
// repokey or in the keyfile for a keyfile repository.
//
// The KDF parameters are stored alongside the ciphertext rather than being taken from
// today's defaults, so a repository created with older parameters still opens after
// the defaults change. Reading them back rather than assuming is the whole point.
type EncryptedKey struct {
	Version   int64
	Algorithm *string
	// Iterations is the legacy PBKDF2 count; argon2 uses the argon2_* fields instead.
	Iterations *int64
	Salt       []byte
	Hash       []byte
	Data       []byte

	Argon2TimeCost    *int64
	Argon2MemoryCost  *int64
	Argon2Parallelism *int64
	Argon2Type        *string

	// Label is an optional human-readable name for the key, e.g. "admin".
	Label *string

	Unknown []msgpackx.MapEntry
}

var encryptedKeyKnownKeys = map[string]bool{
	"version": true, "algorithm": true, "iterations": true, "salt": true,
	"hash": true, "data": true, "argon2_time_cost": true, "argon2_memory_cost": true,
	"argon2_parallelism": true, "argon2_type": true, "label": true,
}

// DecodeEncryptedKey reads an encrypted key blob.
func DecodeEncryptedKey(m *msgpackx.Map) (*EncryptedKey, error) {
	if m == nil {
		return nil, fmt.Errorf("item: cannot decode a nil key map")
	}
	k := &EncryptedKey{}
	for _, e := range m.Entries() {
		key, err := mapKey(e.Key)
		if err != nil {
			return nil, err
		}
		if !encryptedKeyKnownKeys[key] {
			k.Unknown = append(k.Unknown, e)
			continue
		}
		switch key {
		case "version":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			k.Version = n
		case "algorithm", "argon2_type", "label":
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "algorithm":
				k.Algorithm = &s
			case "argon2_type":
				k.Argon2Type = &s
			case "label":
				k.Label = &s
			}
		case "salt", "hash", "data":
			b, err := wantBytes(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "salt":
				k.Salt = b
			case "hash":
				k.Hash = b
			case "data":
				k.Data = b
			}
		case "iterations", "argon2_time_cost", "argon2_memory_cost", "argon2_parallelism":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "iterations":
				k.Iterations = &n
			case "argon2_time_cost":
				k.Argon2TimeCost = &n
			case "argon2_memory_cost":
				k.Argon2MemoryCost = &n
			case "argon2_parallelism":
				k.Argon2Parallelism = &n
			}
		}
	}
	return k, nil
}

// Encode renders the key blob with sorted keys.
func (k *EncryptedKey) Encode() *msgpackx.Map {
	m := msgpackx.NewStableMap()
	m.Set("version", k.Version)
	for _, f := range []struct {
		key string
		val *string
	}{
		{"algorithm", k.Algorithm}, {"argon2_type", k.Argon2Type}, {"label", k.Label},
	} {
		if f.val != nil {
			m.Set(f.key, *f.val)
		}
	}
	for _, f := range []struct {
		key string
		val []byte
	}{
		{"salt", k.Salt}, {"hash", k.Hash}, {"data", k.Data},
	} {
		if f.val != nil {
			m.Set(f.key, f.val)
		}
	}
	for _, f := range []struct {
		key string
		val *int64
	}{
		{"iterations", k.Iterations},
		{"argon2_time_cost", k.Argon2TimeCost},
		{"argon2_memory_cost", k.Argon2MemoryCost},
		{"argon2_parallelism", k.Argon2Parallelism},
	} {
		if f.val != nil {
			m.Set(f.key, *f.val)
		}
	}
	for _, e := range k.Unknown {
		m.Set(e.Key, e.Value)
	}
	return m
}

// Marshal encodes the key blob to msgpack bytes.
func (k *EncryptedKey) Marshal() ([]byte, error) { return msgpackx.Marshal(k.Encode()) }

// UnmarshalEncryptedKey decodes a key blob from msgpack bytes.
func UnmarshalEncryptedKey(b []byte) (*EncryptedKey, error) {
	v, err := msgpackx.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, fmt.Errorf("item: expected a map, got %T", v)
	}
	return DecodeEncryptedKey(m)
}

// ----------------------------------------------------------------------- Key

// Key is the decrypted key material.
//
// This structure holds secrets. Callers should not log it, and should zero CryptKey and
// IDKey when done with them.
type Key struct {
	Version      int64
	RepositoryID []byte
	// CryptKey is the encryption key material; IDKey keys the chunk id hash.
	CryptKey []byte
	IDKey    []byte
	// ChunkSeed keys the chunker's hash tables. borg keeps it to 32 bits.
	ChunkSeed *int64
	// TAMRequired is a borg 1.x flag. borg 2 always requires TAM implicitly.
	TAMRequired *bool

	Unknown []msgpackx.MapEntry
}

var keyKnownKeys = map[string]bool{
	"version": true, "repository_id": true, "crypt_key": true, "id_key": true,
	"chunk_seed": true, "tam_required": true,
	// borg 1.x split what borg 2 calls crypt_key into two fields; they are recognised
	// so DecodeKey can join them rather than treating them as unknown extras.
	"enc_key": true, "enc_hmac_key": true,
}

// DecodeKey reads decrypted key material.
func DecodeKey(m *msgpackx.Map) (*Key, error) {
	if m == nil {
		return nil, fmt.Errorf("item: cannot decode a nil key map")
	}
	k := &Key{}
	var encKey, encHMACKey []byte

	for _, e := range m.Entries() {
		key, err := mapKey(e.Key)
		if err != nil {
			return nil, err
		}
		if !keyKnownKeys[key] {
			k.Unknown = append(k.Unknown, e)
			continue
		}
		switch key {
		case "version":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			k.Version = n
		case "repository_id", "crypt_key", "id_key", "enc_key", "enc_hmac_key":
			b, err := wantBytes(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "repository_id":
				k.RepositoryID = b
			case "crypt_key":
				k.CryptKey = b
			case "id_key":
				k.IDKey = b
			case "enc_key":
				encKey = b
			case "enc_hmac_key":
				encHMACKey = b
			}
		case "chunk_seed":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			k.ChunkSeed = &n
		case "tam_required":
			b, ok := e.Value.(bool)
			if !ok {
				return nil, fmt.Errorf("item: tam_required must be a bool, got %T", e.Value)
			}
			k.TAMRequired = &b
		}
	}

	// A borg 1.x key has no crypt_key; it is the concatenation of enc_key and
	// enc_hmac_key. borg accepts 32+32 or 32+128 bytes, the latter being the old
	// blake2b variant.
	if k.CryptKey == nil && encKey != nil {
		joined := append(append([]byte{}, encKey...), encHMACKey...)
		if n := len(joined); n != 64 && n != 160 {
			return nil, fmt.Errorf("item: legacy enc_key+enc_hmac_key is %d bytes, want 64 or 160", n)
		}
		k.CryptKey = joined
	}
	return k, nil
}

// Encode renders the key material with sorted keys.
func (k *Key) Encode() *msgpackx.Map {
	m := msgpackx.NewStableMap()
	m.Set("version", k.Version)
	for _, f := range []struct {
		key string
		val []byte
	}{
		{"repository_id", k.RepositoryID}, {"crypt_key", k.CryptKey}, {"id_key", k.IDKey},
	} {
		if f.val != nil {
			m.Set(f.key, f.val)
		}
	}
	if k.ChunkSeed != nil {
		m.Set("chunk_seed", *k.ChunkSeed)
	}
	if k.TAMRequired != nil {
		m.Set("tam_required", *k.TAMRequired)
	}
	for _, e := range k.Unknown {
		m.Set(e.Key, e.Value)
	}
	return m
}

// Marshal encodes the key material to msgpack bytes.
func (k *Key) Marshal() ([]byte, error) { return msgpackx.Marshal(k.Encode()) }

// UnmarshalKey decodes key material from msgpack bytes.
func UnmarshalKey(b []byte) (*Key, error) {
	v, err := msgpackx.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, fmt.Errorf("item: expected a map, got %T", v)
	}
	return DecodeKey(m)
}
