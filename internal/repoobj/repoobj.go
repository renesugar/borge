// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/repoobj.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package repoobj implements the BORG_OBJ envelope: the wrapper every repository object
// is stored in, whether it holds a manifest, archive metadata, an item stream chunk or
// file content.
//
// # Layout
//
//	magic[8] = "BORG_OBJ" | version:u8 | chunk_id[32] | meta_size:u32 | data_size:u32
//	meta_encrypted[meta_size]
//	data_encrypted[data_size]
//
// The header is 49 bytes, little-endian. An object has two independently wrapped slots:
// the metadata (which records how the data was compressed) and the data itself.
//
// # The AAD is where this goes wrong
//
// For version 2 objects each slot is authenticated with
//
//	magic || version || chunk_id || slot_tag
//
// where slot_tag is "M" for the metadata slot and "D" for the data slot. The slot tag is
// what binds a ciphertext to the slot it was written for: without it, the two slots
// could be swapped, or a slot moved to another object, and the tag would still verify.
//
// meta_size and data_size are deliberately *excluded*, because they are only known
// after encryption. Tampering with either still fails, by changing the length of the
// ciphertext slice that gets authenticated.
//
// Getting the AAD wrong produces objects borg rejects with an authentication failure
// and no useful diagnostic, so it is tested on its own before anything else.
package repoobj

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/msgpackx"
)

// Object header constants (src/borg/repoobj.py).
const (
	// Magic is the first eight bytes of every repository object.
	Magic = "BORG_OBJ"

	// VersionNoHeaderAAD authenticates with the chunk id alone. borge reads it; it must
	// never write it.
	VersionNoHeaderAAD byte = 0x01
	// VersionHeaderAAD authenticates with the header prefix plus a slot tag. This is
	// what borge writes.
	VersionHeaderAAD byte = 0x02
	// Version is the version borge writes.
	Version = VersionHeaderAAD

	// HeaderSize is magic(8) + version(1) + chunk_id(32) + meta_size(4) + data_size(4).
	HeaderSize = 8 + 1 + 32 + 4 + 4
	// HeaderAADSize is the part of the header that goes into the AAD: everything up to
	// but not including the two size fields.
	HeaderAADSize = 8 + 1 + 32

	// ChunkIDSize is the length of a chunk id.
	ChunkIDSize = 32
)

// Slot tags, appended to the header prefix so each ciphertext is bound to its slot.
var (
	metaAADTag = []byte("M")
	dataAADTag = []byte("D")
)

// Repository object types (src/borg/constants.py), stored as meta["type"].
const (
	TypeManifest        = "M" // the manifest
	TypeArchiveMeta     = "A" // an archive's metadata object
	TypeArchiveChunkIDs = "C" // a block of item-stream chunk ids
	TypeArchiveStream   = "S" // an item metadata stream chunk
	TypeFileStream      = "F" // a file content chunk
	// TypeDontCare parses without asserting the type. It is never written.
	TypeDontCare = "*"
)

// MaxDataSize is the largest plaintext borg puts in one repository object.
//
// The odd value is historical: 20 MiB minus 41 bytes, because borg < 1.3 counted a
// 41-byte PUT header inside a total of exactly 20 MiB.
const MaxDataSize = 20971479

// ErrIntegrity means an object is malformed or failed authentication.
var ErrIntegrity = errors.New("repoobj: integrity error")

// IntegrityError describes a malformed or unauthentic object.
type IntegrityError struct{ Reason string }

func (e *IntegrityError) Error() string { return "repoobj: " + e.Reason }
func (e *IntegrityError) Unwrap() error { return ErrIntegrity }

// Places the chunk id may be verified at (src/borg/repoobj.py: ASSERT_ID_PLACES).
const (
	PlaceRead     = "read"     // the general read path: extract, mount, export-tar, diff
	PlaceRepair   = "repair"   // borg check --repair
	PlaceTransfer = "transfer" // borg transfer
	PlaceRechunk  = "rechunk"  // borg recreate --chunker-params
	// PlaceVerifyData is borg check --verify-data. It always verifies and cannot be
	// switched off: it is the audit that re-certifies the id/content invariant for the
	// whole repository, which is what makes not verifying elsewhere defensible.
	PlaceVerifyData = "verify_data"
)

// defaultAssertIDPlaces is BORG_ASSERT_ID's default: verify everywhere except on the
// hot read path.
//
// This is a real performance decision rather than a detail. For the keyed modes the
// envelope already authenticates the payload against that specific chunk id, so the id
// check on a read is an extra full-plaintext hash pass over every byte restored. borge
// reproduces both the default and the environment variable, or its stage 9 benchmarks
// would not be comparable with borg's.
var defaultAssertIDPlaces = []string{PlaceRepair, PlaceTransfer, PlaceRechunk}

// Meta is a repository object's metadata: what compression was applied, and what kind
// of object it is.
type Meta struct {
	compress.Meta
	// Type is one of the Type* constants.
	Type string
}

// RepoObj formats and parses repository objects.
//
// It is not safe for concurrent use; borg creates one per repository and the pack
// writer's background goroutine never touches it.
type RepoObj struct {
	key        key.Key
	compressor compress.Compressor

	assertIDPlaces map[string]bool
	assertIDPlace  string
}

// New returns a RepoObj using the given key.
//
// The default compressor is lz4, matching borg's LZ4_COMPRESSOR: some commands write
// chunks without taking a --compression argument (rename, for one), and this is what
// they get.
func New(k key.Key) (*RepoObj, error) {
	if k == nil {
		return nil, errors.New("repoobj: a key is required")
	}
	places, err := assertIDPlacesFromEnv()
	if err != nil {
		return nil, err
	}
	return &RepoObj{
		key:            k,
		compressor:     compress.LZ4{},
		assertIDPlaces: places,
		assertIDPlace:  PlaceRead,
	}, nil
}

// SetCompressor selects the compressor used by Format.
func (r *RepoObj) SetCompressor(c compress.Compressor) { r.compressor = c }

// Compressor is the compressor objects are written with.
func (r *RepoObj) Compressor() compress.Compressor { return r.compressor }

// SetAssertIDPlace attributes subsequent reads to a place, for the BORG_ASSERT_ID
// policy. Commands that are a trust boundary for the id/content invariant - transfer,
// re-chunking, check --repair - set their own.
func (r *RepoObj) SetAssertIDPlace(place string) error {
	switch place {
	case PlaceRead, PlaceRepair, PlaceTransfer, PlaceRechunk, PlaceVerifyData:
		r.assertIDPlace = place
		return nil
	default:
		return fmt.Errorf("repoobj: unknown assert-id place %q", place)
	}
}

// Key exposes the key this RepoObj uses.
func (r *RepoObj) Key() key.Key { return r.key }

// IDHash computes a chunk id from plaintext.
func (r *RepoObj) IDHash(data []byte) []byte { return r.key.IDHash(data) }

// assertIDPlacesFromEnv reads BORG_ASSERT_ID, honouring BORGE_ASSERT_ID first
// (docs/PORTING_PLAN.md §0.5).
func assertIDPlacesFromEnv() (map[string]bool, error) {
	value, ok := os.LookupEnv("BORGE_ASSERT_ID")
	if !ok {
		value, ok = os.LookupEnv("BORG_ASSERT_ID")
	}
	if !ok {
		out := make(map[string]bool, len(defaultAssertIDPlaces))
		for _, p := range defaultAssertIDPlaces {
			out[p] = true
		}
		return out, nil
	}

	out := map[string]bool{}
	for _, p := range strings.Split(value, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case PlaceRead, PlaceRepair, PlaceTransfer, PlaceRechunk:
			out[p] = true
		case PlaceVerifyData:
			return nil, fmt.Errorf("BORG_ASSERT_ID: %s always verifies and cannot be configured", p)
		default:
			return nil, fmt.Errorf("BORG_ASSERT_ID: invalid place %q; valid places are: %s, %s, %s, %s",
				p, PlaceRead, PlaceRepair, PlaceTransfer, PlaceRechunk)
		}
	}
	return out, nil
}

// headerAAD builds the authenticated header prefix for a version 2 object.
func headerAAD(id []byte) []byte {
	out := make([]byte, 0, HeaderAADSize+1)
	out = append(out, Magic...)
	out = append(out, Version)
	out = append(out, id...)
	return out
}

// Format builds a repository object.
//
// meta.Type must be set; the compression fields are filled in here. The chunk id is not
// computed from data - the caller already has it, because the id is what decided
// whether this chunk needed storing at all.
func (r *RepoObj) Format(id []byte, meta *Meta, data []byte) ([]byte, error) {
	if len(id) != ChunkIDSize {
		return nil, fmt.Errorf("repoobj: chunk id must be %d bytes, got %d", ChunkIDSize, len(id))
	}
	if meta == nil {
		return nil, errors.New("repoobj: meta is required")
	}
	if meta.Type == "" || meta.Type == TypeDontCare {
		return nil, fmt.Errorf("repoobj: a concrete object type is required, got %q", meta.Type)
	}
	if len(data) > MaxDataSize {
		return nil, fmt.Errorf("repoobj: %d bytes exceeds the %d byte maximum for one object",
			len(data), MaxDataSize)
	}

	compressed, err := r.compressor.Compress(&meta.Meta, data)
	if err != nil {
		return nil, err
	}
	return r.assemble(id, meta, compressed)
}

// assemble encrypts the metadata and the (already compressed) payload and frames them.
func (r *RepoObj) assemble(id []byte, meta *Meta, compressed []byte) ([]byte, error) {
	aad := headerAAD(id)
	dataEncrypted, err := r.key.Encrypt(id, compressed, append(append([]byte{}, aad...), dataAADTag...))
	if err != nil {
		return nil, err
	}

	metaPacked, err := msgpackx.Marshal(metaToMap(meta))
	if err != nil {
		return nil, fmt.Errorf("repoobj: packing metadata: %w", err)
	}
	metaEncrypted, err := r.key.Encrypt(id, metaPacked, append(append([]byte{}, aad...), metaAADTag...))
	if err != nil {
		return nil, err
	}

	out := make([]byte, HeaderSize, HeaderSize+len(metaEncrypted)+len(dataEncrypted))
	copy(out[0:8], Magic)
	out[8] = Version
	copy(out[9:41], id)
	binary.LittleEndian.PutUint32(out[41:45], uint32(len(metaEncrypted)))
	binary.LittleEndian.PutUint32(out[45:49], uint32(len(dataEncrypted)))
	out = append(out, metaEncrypted...)
	out = append(out, dataEncrypted...)
	return out, nil
}

// FormatCompressed builds an object from a payload that is *already* compressed.
//
// This is what "transfer --recompress never" needs: the source object's compressed bytes
// are kept exactly as they are and only re-encrypted, which is the whole saving - borg's
// comment calls it "keep the compressed payload the same". The metadata has to carry the
// source's ctype, clevel and plaintext size, because nothing here can recompute them: the
// plaintext is never reconstituted on this path.
//
// The id is not recomputed either. The caller has already verified it, by parsing the
// source object with WantCompressed (which decompresses to check id == hash(plaintext) and
// hands back the compressed bytes anyway). Doing that check here would mean decompressing a
// second time.
func (r *RepoObj) FormatCompressed(id []byte, meta *Meta, compressed []byte) ([]byte, error) {
	if len(id) != ChunkIDSize {
		return nil, fmt.Errorf("repoobj: chunk id must be %d bytes, got %d", ChunkIDSize, len(id))
	}
	if meta == nil {
		return nil, errors.New("repoobj: meta is required")
	}
	if meta.Type == "" || meta.Type == TypeDontCare {
		return nil, fmt.Errorf("repoobj: a concrete object type is required, got %q", meta.Type)
	}
	if !meta.SizeSet {
		return nil, errors.New("repoobj: a precompressed object needs the plaintext size in its metadata")
	}
	if len(compressed) > MaxDataSize {
		return nil, fmt.Errorf("repoobj: %d bytes exceeds the %d byte maximum for one object",
			len(compressed), MaxDataSize)
	}
	return r.assemble(id, meta, compressed)
}

// header is a parsed object header.
type header struct {
	version  byte
	chunkID  []byte
	metaSize uint32
	dataSize uint32
}

// parseHeader reads and validates an object header. It does not require the whole
// object to be present, so a caller walking pack headers can use it.
func parseHeader(obj []byte) (header, error) {
	if len(obj) < HeaderSize {
		return header{}, &IntegrityError{Reason: fmt.Sprintf(
			"object too small: expected at least %d header bytes, got %d", HeaderSize, len(obj))}
	}
	if string(obj[:8]) != Magic {
		return header{}, &IntegrityError{Reason: "invalid object magic"}
	}
	h := header{
		version:  obj[8],
		chunkID:  obj[9:41],
		metaSize: binary.LittleEndian.Uint32(obj[41:45]),
		dataSize: binary.LittleEndian.Uint32(obj[45:49]),
	}
	if h.version != VersionNoHeaderAAD && h.version != VersionHeaderAAD {
		return header{}, &IntegrityError{Reason: fmt.Sprintf("unsupported object version: %d", h.version)}
	}
	return h, nil
}

// ParseHeader exposes the header fields of an object, which is what a pack walk needs:
// the chunk id and the total size, without reading or decrypting the payload.
func ParseHeader(obj []byte) (chunkID []byte, totalSize int, err error) {
	h, err := parseHeader(obj)
	if err != nil {
		return nil, 0, err
	}
	return h.chunkID, HeaderSize + int(h.metaSize) + int(h.dataSize), nil
}

// aadFor builds the AAD for one slot of an object of the given version.
//
// Version 1 authenticates with the chunk id alone (the key layer appends it), so the
// AAD is empty there.
func aadFor(version byte, obj []byte, tag []byte) []byte {
	if version != VersionHeaderAAD {
		return nil
	}
	out := make([]byte, 0, HeaderAADSize+len(tag))
	out = append(out, obj[:HeaderAADSize]...)
	out = append(out, tag...)
	return out
}

// ParseMeta decrypts an object's metadata slot without touching the data slot.
//
// Only enough of the object to cover the header and the metadata needs to be supplied;
// more is accepted. This is what lets a caller learn an object's type and compressed
// size from a short range read.
func (r *RepoObj) ParseMeta(id []byte, obj []byte, wantType string) (*Meta, error) {
	h, err := parseHeader(obj)
	if err != nil {
		return nil, err
	}
	if int(h.metaSize) > len(obj)-HeaderSize {
		return nil, &IntegrityError{Reason: fmt.Sprintf(
			"object too small: expected at least %d bytes, got %d", HeaderSize+int(h.metaSize), len(obj))}
	}

	metaEncrypted := obj[HeaderSize : HeaderSize+int(h.metaSize)]
	metaPacked, err := r.key.Decrypt(id, metaEncrypted, aadFor(h.version, obj, metaAADTag))
	if err != nil {
		return nil, err
	}
	meta, err := metaFromBytes(metaPacked)
	if err != nil {
		return nil, err
	}
	if wantType != TypeDontCare && meta.Type != wantType {
		return nil, &IntegrityError{Reason: fmt.Sprintf(
			"expected object type %q, got %q", wantType, meta.Type)}
	}
	return meta, nil
}

// ParseOptions tunes what Parse does.
type ParseOptions struct {
	// WantCompressed returns the payload still compressed, for a caller that means to
	// store it again unchanged.
	WantCompressed bool
	// SkipDecompress returns without decompressing, which also skips the id
	// verification. Only meaningful together with WantCompressed.
	SkipDecompress bool
	// AssertIDPlace attributes this read to a place, overriding the RepoObj's.
	AssertIDPlace string
}

// Parse decrypts an object and returns its metadata and payload.
//
// wantType is checked against the stored type unless it is TypeDontCare. Whether the
// chunk id is verified against the plaintext depends on the place this read is
// attributed to; see defaultAssertIDPlaces.
func (r *RepoObj) Parse(id []byte, obj []byte, wantType string, opts ParseOptions) (*Meta, []byte, error) {
	if opts.SkipDecompress && !opts.WantCompressed {
		return nil, nil, errors.New("repoobj: SkipDecompress without WantCompressed would return nothing")
	}

	h, err := parseHeader(obj)
	if err != nil {
		return nil, nil, err
	}
	expected := HeaderSize + int(h.metaSize) + int(h.dataSize)
	if expected != len(obj) {
		return nil, nil, &IntegrityError{Reason: fmt.Sprintf(
			"object size inconsistent: expected %d bytes, got %d", expected, len(obj))}
	}

	metaEncrypted := obj[HeaderSize : HeaderSize+int(h.metaSize)]
	metaPacked, err := r.key.Decrypt(id, metaEncrypted, aadFor(h.version, obj, metaAADTag))
	if err != nil {
		return nil, nil, err
	}
	meta, err := metaFromBytes(metaPacked)
	if err != nil {
		return nil, nil, err
	}
	if wantType != TypeDontCare && meta.Type != wantType {
		return nil, nil, &IntegrityError{Reason: fmt.Sprintf(
			"expected object type %q, got %q", wantType, meta.Type)}
	}

	dataEncrypted := obj[HeaderSize+int(h.metaSize):]
	compressed, err := r.key.Decrypt(id, dataEncrypted, aadFor(h.version, obj, dataAADTag))
	if err != nil {
		return nil, nil, err
	}

	if opts.SkipDecompress {
		return meta, compressed, nil
	}

	plain, err := compress.Decompress(&meta.Meta, compressed)
	if err != nil {
		return nil, nil, err
	}

	if r.shouldAssertID(opts.AssertIDPlace) {
		if err := key.AssertID(r.key, id, plain); err != nil {
			return nil, nil, err
		}
	}

	if opts.WantCompressed {
		return meta, compressed, nil
	}
	return meta, plain, nil
}

// shouldAssertID applies the BORG_ASSERT_ID policy for one read.
func (r *RepoObj) shouldAssertID(place string) bool {
	if place == "" {
		place = r.assertIDPlace
	}
	// For a key whose id check *is* the read-path authentication, it always happens:
	// skipping it would leave the read with no integrity checking at all.
	if r.key.IDCheckIsAuthentication() {
		return true
	}
	// verify_data is the audit that re-certifies the invariant, so it is not optional.
	if place == PlaceVerifyData {
		return true
	}
	return r.assertIDPlaces[place]
}

// ExtractCryptedData returns the encrypted data slot of an object, which is what borg
// inspects to detect which crypto mode a repository uses before it has a key.
func ExtractCryptedData(obj []byte) ([]byte, error) {
	h, err := parseHeader(obj)
	if err != nil {
		return nil, err
	}
	expected := HeaderSize + int(h.metaSize) + int(h.dataSize)
	if expected != len(obj) {
		return nil, &IntegrityError{Reason: fmt.Sprintf(
			"object size inconsistent: expected %d bytes, got %d", expected, len(obj))}
	}
	return obj[HeaderSize+int(h.metaSize):], nil
}

// ------------------------------------------------------------- metadata encoding

// metaToMap renders the metadata dict borg stores in the metadata slot.
//
// **The key order is insertion order, not sorted.** borg builds this as a plain dict,
// not a StableDict, so the keys come out in the order they were set: "type" by
// RepoObj.format, then "size" by the DecidingCompressor, then "ctype", "clevel" and
// "csize" by the compressor base, then "psize" and "olevel" if the payload was
// size-obfuscated.
//
// Reproducing that is not cosmetic. borg's MAC modes are deterministic - no nonce, no
// session - so two repositories with the same key material store byte-identical objects
// for identical input, and can be deduplicated against each other at the filesystem
// level. A different key order here silently gives that up.
func metaToMap(meta *Meta) *msgpackx.Map {
	m := msgpackx.NewMap()
	// The order below is borg's insertion order, and it is load-bearing - see the
	// comment above. "size" comes second because DecidingCompressor.compress sets it
	// before handing off to the compressor base, which then adds ctype/clevel/csize;
	// ObfuscateSize appends psize and olevel afterwards.
	m.Set("type", meta.Type)
	if meta.SizeSet {
		m.Set("size", int64(meta.Size))
	}
	m.Set("ctype", int64(meta.CType))
	m.Set("clevel", int64(meta.CLevel))
	m.Set("csize", int64(meta.CSize))
	if meta.PSizeSet {
		m.Set("psize", int64(meta.PSize))
	}
	if meta.OLevelSet {
		m.Set("olevel", int64(meta.OLevel))
	}
	return m
}

// metaFromBytes parses the metadata dict.
func metaFromBytes(packed []byte) (*Meta, error) {
	v, err := msgpackx.Unmarshal(packed)
	if err != nil {
		return nil, &IntegrityError{Reason: "metadata is not valid msgpack: " + err.Error()}
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, &IntegrityError{Reason: fmt.Sprintf("metadata is a %T, expected a map", v)}
	}

	meta := &Meta{}
	if s, ok := m.GetString("type"); ok {
		meta.Type = s
	} else {
		return nil, &IntegrityError{Reason: "metadata has no type"}
	}
	if n, ok := m.GetInt("ctype"); ok {
		meta.CType = uint8(n)
	}
	if n, ok := m.GetInt("clevel"); ok {
		meta.CLevel = uint8(n)
	}
	if n, ok := m.GetInt("csize"); ok {
		meta.CSize = int(n)
	}
	// size is absent for objects written with --compression auto,... - borg's Auto
	// drops it (see docs/DIVERGENCES.md §1), so its absence is normal, not corruption.
	if n, ok := m.GetInt("size"); ok {
		meta.Size, meta.SizeSet = int(n), true
	}
	if n, ok := m.GetInt("psize"); ok {
		meta.PSize, meta.PSizeSet = int(n), true
	}
	if n, ok := m.GetInt("olevel"); ok {
		meta.OLevel, meta.OLevelSet = int(n), true
	}
	return meta, nil
}
