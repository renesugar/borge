// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the key blob handling in FlexiKey, in borg's
// src/borg/crypto/key.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package key

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/renesugar/borge/internal/crypto"
	"github.com/renesugar/borge/internal/item"
)

// The key blob is what stands between a passphrase and the repository's key material.
//
// # Shape
//
//	BORG_KEY <repository id in hex>\n
//	<base64 of msgpack(EncryptedKey), wrapped at 70 columns>\n
//
// and the EncryptedKey inside it holds the argon2id parameters, the salt, and the key
// material sealed with ChaCha20-Poly1305 under the passphrase-derived key.
//
// The same text is used whether the blob lives in a file or in the repository's keys/
// namespace - the storage location is not encoded anywhere in it. That is what makes
// "borge key export" from a repokey repository and "borg key import" into a keyfile one
// (and every other combination) work without conversion.
//
// # Why the KDF parameters are stored rather than assumed
//
// A repository created years ago must still open after the defaults change. Reading the
// parameters back from the blob is the whole point of writing them down; deriving with
// today's constants instead would lock the user out of their own backup.

// KeyfileID is the magic first token of a key blob.
const KeyfileID = "BORG_KEY"

// AdminLabel is the label of the key created together with the repository. It is
// reserved: it cannot be assigned to a key added later, and that key cannot be removed.
const AdminLabel = "admin"

// AlgorithmArgon2 is the only key blob algorithm borg 2 writes: argon2id for the
// passphrase, ChaCha20-Poly1305 for the key material.
const AlgorithmArgon2 = "argon2 chacha20-poly1305"

// keyfileWrapColumn is where the base64 is wrapped. It is Python textwrap's default
// width, which is what borg relies on - the value is visible in every key file, so it
// is format, not formatting.
const keyfileWrapColumn = 70

// materialVersion is the version borge writes into new key material. borg accepts 1 and
// 2; version 1 is a borg 1.x layout with enc_key/enc_hmac_key instead of crypt_key.
const materialVersion = 2

// encryptedKeyVersion is the version of the outer, passphrase-protected envelope.
const encryptedKeyVersion = 1

// Blob errors.
var (
	// ErrNotAKeyfile means the data does not start with the BORG_KEY magic.
	ErrNotAKeyfile = errors.New("key: not a borg key file")
	// ErrRepositoryMismatch means the blob belongs to a different repository.
	ErrRepositoryMismatch = errors.New("key: this key belongs to a different repository")
	// ErrPassphraseWrong means no key of this repository could be unlocked with the
	// passphrase given.
	ErrPassphraseWrong = errors.New("key: passphrase is wrong, or there is no key for it")
	// ErrUnsupportedKeyFormat means the blob is a version or algorithm borge cannot read.
	ErrUnsupportedKeyFormat = errors.New("key: unsupported key format")
)

// IsKeyfile reports whether data looks like a key blob. If repoIDHex is non-empty, the
// blob must also name that repository.
func IsKeyfile(data []byte, repoIDHex string) bool {
	return strings.HasPrefix(string(data), KeyfileID+" "+repoIDHex)
}

// KeyfileFormat renders a key blob: the magic line, then the wrapped base64.
func KeyfileFormat(repoIDHex, b64 string) string {
	return fmt.Sprintf("%s %s\n%s\n", KeyfileID, repoIDHex, b64)
}

// KeyfileParse splits a key blob into the repository id it names and its base64 body.
//
// repoIDHex, when non-empty, is required to match - a keyfile for another repository is
// rejected here rather than failing later with a decryption error that says nothing
// useful.
func KeyfileParse(data []byte, repoIDHex string) (repoID, b64 string, err error) {
	if !IsKeyfile(data, "") {
		return "", "", ErrNotAKeyfile
	}
	if repoIDHex != "" && !IsKeyfile(data, repoIDHex) {
		return "", "", fmt.Errorf("%w (wanted %s)", ErrRepositoryMismatch, repoIDHex)
	}
	header, body, found := strings.Cut(string(data), "\n")
	if !found {
		return "", "", ErrNotAKeyfile
	}
	return header[len(KeyfileID)+1:], body, nil
}

// BlobName is the name a blob is stored under, in the keys/ namespace or as a file: the
// sha256 of its own text.
//
// Naming a key by its content means writing the same key twice is idempotent, and that
// changing a passphrase produces a different name - which is what lets the old key be
// deleted afterwards rather than overwritten in place.
func BlobName(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// wrapBase64 encodes and wraps, reproducing Python's
// "\n".join(textwrap.wrap(b2a_base64(data))).
func wrapBase64(data []byte) string {
	s := base64.StdEncoding.EncodeToString(data)
	var lines []string
	for len(s) > keyfileWrapColumn {
		lines = append(lines, s[:keyfileWrapColumn])
		s = s[keyfileWrapColumn:]
	}
	lines = append(lines, s)
	return strings.Join(lines, "\n")
}

// unwrapBase64 decodes a wrapped base64 body, ignoring the line structure.
func unwrapBase64(body string) ([]byte, error) {
	clean := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(body)
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("key: key data is not valid base64: %w", err)
	}
	// borg's own sanity check: a real blob is around 400 bytes, so anything this short is
	// certainly not one, and saying so beats a confusing msgpack error.
	if len(raw) < 20 {
		return nil, fmt.Errorf("key: key data is only %d bytes, far too short to be a key", len(raw))
	}
	return raw, nil
}

// ---------------------------------------------------------------- KDF parameters

// Argon2Params are the passphrase KDF settings stored in a key blob.
type Argon2Params struct {
	TimeCost    uint32
	MemoryCost  uint32 // KiB
	Parallelism uint32
	Type        string // "id"; borge refuses anything else
	SaltBytes   int
}

// DefaultArgon2Params are borg's ARGON2_ARGS.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{
		TimeCost:    crypto.Argon2TimeCost,
		MemoryCost:  crypto.Argon2MemoryCost,
		Parallelism: crypto.Argon2Parallelism,
		Type:        crypto.Argon2Type,
		SaltBytes:   crypto.Argon2SaltBytes,
	}
}

// effectiveArgon2Params applies the BORG_TESTONLY_WEAKEN_KDF=1 override.
//
// The override exists because the real parameters cost 64 MiB and hundreds of
// milliseconds per attempt, which makes a test suite that unlocks keys repeatedly
// unusable. It must never be set outside a test: a blob written under it is protected by
// an 8 KiB single-pass argon2, which is close to no protection at all.
//
// Note what borg does here, and what borge therefore does too: the override applies to
// the *derivation* only, while the blob still records the real parameters. So a blob
// written with the flag set cannot be opened without it - the recorded parameters are a
// lie. That is one more reason it belongs nowhere near production, and it is why this
// function is the single chokepoint both sealing and opening go through, rather than
// something that edits the parameters on the way into the blob.
func effectiveArgon2Params(p Argon2Params) Argon2Params {
	if v, ok := lookupEnv("TESTONLY_WEAKEN_KDF"); !ok || v != "1" {
		return p
	}
	p.TimeCost = 1
	p.Parallelism = 1
	// 8 KiB is the smallest value argon2 accepts.
	p.MemoryCost = 8
	return p
}

// lookupEnv reads BORGE_<name>, falling back to BORG_<name> (docs/PORTING_PLAN.md §0.5).
func lookupEnv(name string) (string, bool) {
	if v, ok := os.LookupEnv("BORGE_" + name); ok {
		return v, true
	}
	return os.LookupEnv("BORG_" + name)
}

// derivePassphraseKey runs the passphrase KDF.
//
// The parameters come from the blob being opened, never from today's defaults - see the
// note at the top of this file.
func derivePassphraseKey(passphrase string, salt []byte, p Argon2Params) ([]byte, error) {
	if p.Type != "id" {
		// borg only ever writes argon2id. Refusing the other variants is not a limitation
		// so much as a refusal to guess: argon2i and argon2d have different trade-offs and
		// a blob claiming one was not written by borg.
		return nil, fmt.Errorf("%w: argon2 type %q (only \"id\" is supported)", ErrUnsupportedKeyFormat, p.Type)
	}
	e := effectiveArgon2Params(p)
	return crypto.Argon2ID([]byte(passphrase), salt, e.TimeCost, e.MemoryCost, e.Parallelism, crypto.KeySize)
}

// passphraseCipher builds the envelope the key material is sealed with.
//
// The nonce is all zeros, and that is safe here only because the key is not: every blob
// draws a fresh random salt, so the argon2 output - and hence the key - differs per blob
// even for the same passphrase. Reusing the salt would reuse the (key, nonce) pair, which
// is why the salt is drawn with crypto/rand and never derived from anything.
func passphraseCipher(derived []byte) (*crypto.AEAD, error) {
	return crypto.NewAEAD(crypto.SuiteChaCha20Poly1305, derived, 0, 0)
}

// zeroNonce is the 12-byte all-zero nonce borg's key blob cipher uses.
func zeroNonce() []byte { return make([]byte, crypto.IVSize) }

// ---------------------------------------------------------------- material

// NewMaterial draws fresh random key material for a repository.
//
// borg draws 100 bytes at once and slices them: 64 for the crypt key, 32 for the id key,
// 4 for the chunker seed. borge does the same, so that a key created by either tool has
// the same shape and the same entropy budget.
func NewMaterial(repoID []byte) (*item.Key, error) {
	if len(repoID) != 32 {
		return nil, fmt.Errorf("key: repository id must be 32 bytes, got %d", len(repoID))
	}
	var buf [100]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("key: could not draw key material: %w", err)
	}
	seed := int64(int32(binary.BigEndian.Uint32(buf[96:100])))
	return &item.Key{
		Version:      materialVersion,
		RepositoryID: append([]byte(nil), repoID...),
		CryptKey:     append([]byte(nil), buf[0:64]...),
		IDKey:        append([]byte(nil), buf[64:96]...),
		ChunkSeed:    &seed,
	}, nil
}

// checkMaterial rejects key material that could not have come from a borg 2 key.
func checkMaterial(m *item.Key) error {
	if m == nil {
		return errors.New("key: no key material")
	}
	if m.Version != 1 && m.Version != 2 {
		return fmt.Errorf("%w: key material version %d", ErrUnsupportedKeyFormat, m.Version)
	}
	// 32+128 is the borg 1.x blake2b layout, which borge does not read (§0.6).
	if len(m.CryptKey) != 64 {
		return fmt.Errorf("%w: crypt key is %d bytes, want 64", ErrUnsupportedKeyFormat, len(m.CryptKey))
	}
	if len(m.IDKey) != 32 {
		return fmt.Errorf("%w: id key is %d bytes, want 32", ErrUnsupportedKeyFormat, len(m.IDKey))
	}
	return nil
}

// ChunkSeed is the chunker seed from key material, zero if it carries none.
//
// It is a signed 32-bit value in borg, so it is returned as int32 rather than left as the
// int64 msgpack decodes to - the sign matters, because it is mixed into the chunker's
// table and a repository chunked with the wrong sign would deduplicate against nothing.
func ChunkSeed(m *item.Key) int32 {
	if m == nil || m.ChunkSeed == nil {
		return 0
	}
	return int32(*m.ChunkSeed)
}

// ---------------------------------------------------------------- seal and open

// SealMaterial produces the key blob text for the given material and passphrase.
//
// label is optional and travels in the clear: it names the key so a repository with
// several passphrases can be managed without unlocking each one in turn.
func SealMaterial(m *item.Key, passphrase, label string) (string, error) {
	if err := checkMaterial(m); err != nil {
		return "", err
	}
	if len(m.RepositoryID) != 32 {
		return "", fmt.Errorf("key: key material has a %d byte repository id, want 32", len(m.RepositoryID))
	}

	packed, err := m.Marshal()
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}

	// The blob always records the real parameters; see effectiveArgon2Params.
	p := DefaultArgon2Params()
	salt := make([]byte, p.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("key: could not draw a salt: %w", err)
	}
	derived, err := derivePassphraseKey(passphrase, salt, p)
	if err != nil {
		return "", err
	}
	cipher, err := passphraseCipher(derived)
	if err != nil {
		return "", err
	}
	sealed, err := cipher.Encrypt(zeroNonce(), packed, nil, nil)
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}

	algorithm := AlgorithmArgon2
	argonType := p.Type
	timeCost, memoryCost, parallelism := int64(p.TimeCost), int64(p.MemoryCost), int64(p.Parallelism)
	ek := &item.EncryptedKey{
		Version:           encryptedKeyVersion,
		Algorithm:         &algorithm,
		Salt:              salt,
		Data:              sealed,
		Argon2TimeCost:    &timeCost,
		Argon2MemoryCost:  &memoryCost,
		Argon2Parallelism: &parallelism,
		Argon2Type:        &argonType,
	}
	if label != "" {
		l := label
		ek.Label = &l
	}

	blob, err := ek.Marshal()
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	return KeyfileFormat(hex.EncodeToString(m.RepositoryID), wrapBase64(blob)), nil
}

// Envelope is what can be read off a key blob without knowing the passphrase.
type Envelope struct {
	Version   int64
	Algorithm string
	Label     string
	Argon2    Argon2Params
	Salt      []byte
	Data      []byte
}

// ParseEnvelope decodes the outer envelope of a key blob without decrypting it.
//
// This is what "borge key list" reports: which keys a repository has, what they are
// called and how they are protected, without asking for a passphrase.
func ParseEnvelope(blobText []byte, repoIDHex string) (*Envelope, error) {
	var body string
	if IsKeyfile(blobText, "") {
		_, b64, err := KeyfileParse(blobText, repoIDHex)
		if err != nil {
			return nil, err
		}
		body = b64
	} else {
		// A borg 1.x repokey is raw base64 with no BORG_KEY header. borge does not read
		// borg 1.x repositories, but it can still say what such a blob is rather than
		// calling it corrupt.
		body = string(blobText)
	}

	raw, err := unwrapBase64(body)
	if err != nil {
		return nil, err
	}
	ek, err := item.UnmarshalEncryptedKey(raw)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	if ek.Version != encryptedKeyVersion {
		return nil, fmt.Errorf("%w: key envelope version %d", ErrUnsupportedKeyFormat, ek.Version)
	}

	env := &Envelope{Version: ek.Version, Salt: ek.Salt, Data: ek.Data}
	if ek.Algorithm != nil {
		env.Algorithm = *ek.Algorithm
	}
	if ek.Label != nil {
		env.Label = *ek.Label
	}
	if ek.Argon2TimeCost != nil {
		env.Argon2.TimeCost = uint32(*ek.Argon2TimeCost)
	}
	if ek.Argon2MemoryCost != nil {
		env.Argon2.MemoryCost = uint32(*ek.Argon2MemoryCost)
	}
	if ek.Argon2Parallelism != nil {
		env.Argon2.Parallelism = uint32(*ek.Argon2Parallelism)
	}
	if ek.Argon2Type != nil {
		env.Argon2.Type = *ek.Argon2Type
	}
	env.Argon2.SaltBytes = len(ek.Salt)
	return env, nil
}

// OpenMaterial unlocks a key blob with a passphrase.
//
// It returns ErrPassphraseWrong when the passphrase does not open this particular blob,
// which is not an error at the caller's level: a repository may hold several keys and
// the passphrase is tried against each in turn.
func OpenMaterial(blobText []byte, repoIDHex, passphrase string) (*item.Key, *Envelope, error) {
	env, err := ParseEnvelope(blobText, repoIDHex)
	if err != nil {
		return nil, nil, err
	}
	if env.Algorithm != AlgorithmArgon2 {
		return nil, env, fmt.Errorf("%w: key algorithm %q; borge reads only %q",
			ErrUnsupportedKeyFormat, env.Algorithm, AlgorithmArgon2)
	}

	derived, err := derivePassphraseKey(passphrase, env.Salt, env.Argon2)
	if err != nil {
		return nil, env, err
	}
	cipher, err := passphraseCipher(derived)
	if err != nil {
		return nil, env, err
	}
	packed, err := cipher.Decrypt(zeroNonce(), env.Data, nil)
	if err != nil {
		return nil, env, ErrPassphraseWrong
	}

	m, err := item.UnmarshalKey(packed)
	if err != nil {
		return nil, env, fmt.Errorf("key: key material is corrupt: %w", err)
	}
	if err := checkMaterial(m); err != nil {
		return nil, env, err
	}
	return m, env, nil
}

// ---------------------------------------------------------------- mode from material

// FromMaterial builds a Key of the named mode from unlocked key material.
//
// This is the join between the two halves of stage 4: the blob says *what the key is*,
// the manifest's type byte says *which mode reads the repository*, and this puts them
// together.
func FromMaterial(name string, m *item.Key) (Key, error) {
	if err := checkMaterial(m); err != nil {
		return nil, err
	}
	return ByName(name, m.CryptKey, m.IDKey)
}

// FromMaterialForType is FromMaterial addressed by the type byte found in an object,
// which is how a repository is actually opened: read the manifest, look at byte 0.
func FromMaterialForType(t byte, m *item.Key) (Key, error) {
	name := TypeName(t)
	if !IsImplementedType(t) {
		return nil, fmt.Errorf("key: cannot open a repository in %s", name)
	}
	if RequiresKeyMaterial(t) {
		return FromMaterial(name, m)
	}
	return ByName(name, nil, nil)
}

// IsImplementedType reports whether borge can read objects of this key type.
func IsImplementedType(t byte) bool {
	switch t {
	case TypeSHA256None, TypeBlake3None, TypeSHA256Authenticated, TypeBlake3Authenticated,
		TypeAESOCB, TypeCHPO, TypeBlake3AESOCB, TypeBlake3CHPO:
		return true
	default:
		return false
	}
}

// RequiresKeyMaterial reports whether a mode needs a stored key at all.
//
// The none-* modes do not: their chunk ids and envelope checksums are unkeyed, so a
// repository in one of those modes has no keys/ entry and no passphrase. Asking for one
// would be a bug, not a safety measure.
func RequiresKeyMaterial(t byte) bool {
	return t != TypeSHA256None && t != TypeBlake3None
}

// describeArgon2 renders the KDF parameters for "borge key list".
func (e *Envelope) describeArgon2() string {
	if e.Argon2.TimeCost == 0 {
		return e.Algorithm
	}
	return e.Algorithm + " (t=" + strconv.Itoa(int(e.Argon2.TimeCost)) +
		", m=" + strconv.Itoa(int(e.Argon2.MemoryCost)) +
		", p=" + strconv.Itoa(int(e.Argon2.Parallelism)) + ")"
}

// String describes the envelope without revealing anything secret.
func (e *Envelope) String() string {
	label := e.Label
	if label == "" {
		label = "(no label)"
	}
	return label + ": " + e.describeArgon2()
}
