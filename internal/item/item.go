// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the Item class in borg's src/borg/item.pyx.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package item implements borg's archive metadata structures: items (one per archived
// filesystem object), archive and manifest headers, and key blobs.
//
// # Optional fields
//
// borg stores these as Python dicts where a key is simply absent when it does not
// apply - a symlink has no chunk list, a file on a filesystem without birthtime has no
// birthtime. The distinction between "absent" and "zero" is format-visible: writing
// mode 0 is not the same as not writing mode at all.
//
// Go has no absent, so every optional field is a pointer. That is verbose at the call
// site but it makes the distinction impossible to lose by accident, which a struct of
// plain values would do silently. The Opt* helpers keep construction readable.
//
// # Unknown keys are preserved
//
// Decode keeps any key it does not recognise in Unknown and writes it back out. A borge
// that dropped unknown keys would silently strip metadata written by a newer borg the
// moment anything rewrote an item - borg recreate, borg transfer, a repair. Losing data
// while "successfully" processing it is the worst failure mode available here.
package item

import (
	"fmt"
	"sort"

	"github.com/renesugar/borge/internal/msgpackx"
)

// ChunkListEntry is one entry of an item's chunk list: a chunk id and the size of the
// plaintext it stands for.
//
// borg 1.x also stored a compressed size as a third element. borg drops it on read
// (fix_list_of_chunkentries keeps only id and size), and so does borge - which means a
// borg 1.x item re-encodes with two-element entries, exactly as borg would write it.
type ChunkListEntry struct {
	ID   []byte
	Size int64
}

// Item is one archived filesystem object.
//
// Only Path and MTime are always present (REQUIRED_ITEM_KEYS in src/borg/constants.py);
// everything else depends on what the object is and what the filesystem reported.
type Item struct {
	// Path is the archived path: relative, normalised, forward slashes. It is
	// sanitised on both encode and decode; see path.go.
	Path string

	// Target is a symlink's target. Source is the borg 1.x spelling, read only when
	// transferring borg 1 archives.
	Target *string
	Source *string

	User  *string
	Group *string

	Mode     *int64
	UID      *int64
	GID      *int64
	RDev     *int64
	BSDFlags *int64
	Size     *int64
	Inode    *int64
	NLink    *int64

	// Times are nanoseconds since the epoch, stored on the wire as msgpack timestamps.
	ATime     *int64
	CTime     *int64
	MTime     *int64
	BirthTime *int64

	ACLAccess   []byte
	ACLDefault  []byte
	ACLExtended []byte
	ACLNFS4     []byte

	// HLID identifies a hard link group: items with the same value are the same inode.
	HLID []byte
	// HardlinkMaster is a borg 1.x key, read only when transferring borg 1 archives.
	HardlinkMaster *bool

	// Chunks is the content, as a list of chunk ids and sizes. ChunksHealthy is the
	// pre-damage list kept by borg check --repair.
	Chunks        []ChunkListEntry
	ChunksHealthy []ChunkListEntry
	// ChunksSet and ChunksHealthySet distinguish an empty list from an absent key: an
	// empty file has a chunk list of length zero, which is not the same as a symlink
	// having none at all.
	ChunksSet        bool
	ChunksHealthySet bool

	// XAttrs maps extended attribute names to values, both raw byte strings. Order is
	// not preserved: borg stores them in a StableDict, so they are sorted on encode.
	XAttrs    map[string][]byte
	XAttrsSet bool

	// Deleted marks an item removed by borg delete without a compact.
	Deleted *bool
	// Part is a borg 1.x key for split files.
	Part *int64

	// Unknown holds keys borge does not recognise, so they survive a decode/encode
	// round trip. See the package comment.
	Unknown []msgpackx.MapEntry
}

// Optional-field helpers, so building an Item does not need a named variable per field.
func OptString(s string) *string { return &s }
func OptInt(i int64) *int64      { return &i }
func OptBool(b bool) *bool       { return &b }

// itemFieldOrder lists the keys borge knows, so Decode can tell an unknown key from a
// known one. It is not the encode order: encoding sorts, matching borg's StableDict.
var itemKnownKeys = map[string]bool{}

func init() {
	for _, k := range []string{
		"path", "source", "target", "user", "group",
		"acl_access", "acl_default", "acl_extended", "acl_nfs4",
		"mode", "uid", "gid", "rdev", "bsdflags",
		"atime", "ctime", "mtime", "birthtime",
		"size", "inode", "hlid", "hardlink_master",
		"chunks", "chunks_healthy", "xattrs",
		"deleted", "nlink", "part",
	} {
		itemKnownKeys[k] = true
	}
}

// DecodeItem reads an item from its decoded msgpack map.
func DecodeItem(m *msgpackx.Map) (*Item, error) {
	if m == nil {
		return nil, fmt.Errorf("item: cannot decode a nil map")
	}
	it := &Item{}
	var sawPath, sawMTime bool

	for _, e := range m.Entries() {
		key, err := mapKey(e.Key)
		if err != nil {
			return nil, err
		}
		if !itemKnownKeys[key] {
			it.Unknown = append(it.Unknown, e)
			continue
		}

		switch key {
		case "path":
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			safe, err := ToSanitizedPath(s)
			if err != nil {
				return nil, err
			}
			it.Path, sawPath = safe, true
		case "source":
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			it.Source = &s
		case "target":
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			// borg applies map_chars on decode, which is the identity on POSIX.
			mapped := MapChars(s)
			it.Target = &mapped
		case "user":
			// borg 1 stored "not known" as None; borg 2's policy is to omit the key,
			// so a nil value is dropped rather than becoming an empty string.
			if e.Value == nil {
				continue
			}
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			it.User = &s
		case "group":
			if e.Value == nil {
				continue
			}
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			it.Group = &s

		case "mode", "uid", "gid", "rdev", "bsdflags", "size", "inode", "nlink", "part":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "mode":
				it.Mode = &n
			case "uid":
				it.UID = &n
			case "gid":
				it.GID = &n
			case "rdev":
				it.RDev = &n
			case "bsdflags":
				it.BSDFlags = &n
			case "size":
				it.Size = &n
			case "inode":
				it.Inode = &n
			case "nlink":
				it.NLink = &n
			case "part":
				it.Part = &n
			}

		case "atime", "ctime", "mtime", "birthtime":
			ns, err := wantTimestamp(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "atime":
				it.ATime = &ns
			case "ctime":
				it.CTime = &ns
			case "mtime":
				it.MTime, sawMTime = &ns, true
			case "birthtime":
				it.BirthTime = &ns
			}

		case "acl_access", "acl_default", "acl_extended", "acl_nfs4", "hlid":
			b, err := wantBytes(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "acl_access":
				it.ACLAccess = b
			case "acl_default":
				it.ACLDefault = b
			case "acl_extended":
				it.ACLExtended = b
			case "acl_nfs4":
				it.ACLNFS4 = b
			case "hlid":
				it.HLID = b
			}

		case "hardlink_master", "deleted":
			b, ok := e.Value.(bool)
			if !ok {
				return nil, fmt.Errorf("item: %s must be a bool, got %T", key, e.Value)
			}
			if key == "deleted" {
				it.Deleted = &b
			} else {
				it.HardlinkMaster = &b
			}

		case "chunks", "chunks_healthy":
			list, err := decodeChunkList(key, e.Value)
			if err != nil {
				return nil, err
			}
			if key == "chunks" {
				it.Chunks, it.ChunksSet = list, true
			} else {
				it.ChunksHealthy, it.ChunksHealthySet = list, true
			}

		case "xattrs":
			x, err := decodeXAttrs(e.Value)
			if err != nil {
				return nil, err
			}
			it.XAttrs, it.XAttrsSet = x, true
		}
	}

	if !sawPath {
		return nil, fmt.Errorf("item: required key \"path\" is missing")
	}
	if !sawMTime {
		return nil, fmt.Errorf("item: required key \"mtime\" is missing")
	}
	return it, nil
}

// Encode renders the item back to a msgpack map with sorted keys, matching borg's
// StableDict. The sort order is format-visible: item bytes are hashed into the metadata
// stream, so a different order gives a different chunk id for identical content.
func (it *Item) Encode() (*msgpackx.Map, error) {
	m := msgpackx.NewStableMap()

	safe, err := AssertSanitizedPath(it.Path)
	if err != nil {
		return nil, err
	}
	m.Set("path", safe)

	if it.Source != nil {
		m.Set("source", *it.Source)
	}
	if it.Target != nil {
		m.Set("target", Slashify(*it.Target))
	}
	if it.User != nil {
		m.Set("user", *it.User)
	}
	if it.Group != nil {
		m.Set("group", *it.Group)
	}

	for _, f := range []struct {
		key string
		val *int64
	}{
		{"mode", it.Mode}, {"uid", it.UID}, {"gid", it.GID}, {"rdev", it.RDev},
		{"bsdflags", it.BSDFlags}, {"size", it.Size}, {"inode", it.Inode},
		{"nlink", it.NLink}, {"part", it.Part},
	} {
		if f.val != nil {
			m.Set(f.key, *f.val)
		}
	}

	for _, f := range []struct {
		key string
		val *int64
	}{
		{"atime", it.ATime}, {"ctime", it.CTime}, {"mtime", it.MTime}, {"birthtime", it.BirthTime},
	} {
		if f.val != nil {
			m.Set(f.key, msgpackx.TimestampFromUnixNano(*f.val))
		}
	}

	for _, f := range []struct {
		key string
		val []byte
	}{
		{"acl_access", it.ACLAccess}, {"acl_default", it.ACLDefault},
		{"acl_extended", it.ACLExtended}, {"acl_nfs4", it.ACLNFS4}, {"hlid", it.HLID},
	} {
		if f.val != nil {
			m.Set(f.key, f.val)
		}
	}

	if it.HardlinkMaster != nil {
		m.Set("hardlink_master", *it.HardlinkMaster)
	}
	if it.Deleted != nil {
		m.Set("deleted", *it.Deleted)
	}
	if it.ChunksSet {
		m.Set("chunks", encodeChunkList(it.Chunks))
	}
	if it.ChunksHealthySet {
		m.Set("chunks_healthy", encodeChunkList(it.ChunksHealthy))
	}
	if it.XAttrsSet {
		m.Set("xattrs", encodeXAttrs(it.XAttrs))
	}

	for _, e := range it.Unknown {
		m.Set(e.Key, e.Value)
	}
	return m, nil
}

// Marshal encodes the item to msgpack bytes.
func (it *Item) Marshal() ([]byte, error) {
	m, err := it.Encode()
	if err != nil {
		return nil, err
	}
	return msgpackx.Marshal(m)
}

// UnmarshalItem decodes an item from msgpack bytes.
func UnmarshalItem(b []byte) (*Item, error) {
	v, err := msgpackx.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, fmt.Errorf("item: expected a map, got %T", v)
	}
	return DecodeItem(m)
}

// ------------------------------------------------------------------ shared decoding

func mapKey(k any) (string, error) {
	switch v := k.(type) {
	case string:
		return v, nil
	case []byte:
		// borg's fix_key: a bytes key from an old msgpack becomes a str key.
		return string(v), nil
	default:
		return "", fmt.Errorf("item: map key must be a string, got %T", k)
	}
}

// wantString accepts either a str or a bytes value, matching borg's want_str.
//
// Both spellings occur because borg < 1.3 packed with use_bin_type=False, which made
// str and bytes indistinguishable on the wire. Since a Go string is exactly the wire
// bytes (see internal/msgpackx), the conversion is free and lossless.
func wantString(key string, v any) (string, error) {
	switch s := v.(type) {
	case string:
		return s, nil
	case []byte:
		return string(s), nil
	default:
		return "", fmt.Errorf("item: %s must be a string, got %T", key, v)
	}
}

// wantBytes accepts either spelling, matching borg's want_bytes.
func wantBytes(key string, v any) ([]byte, error) {
	switch b := v.(type) {
	case []byte:
		return b, nil
	case string:
		return []byte(b), nil
	default:
		return nil, fmt.Errorf("item: %s must be bytes, got %T", key, v)
	}
}

func wantInt(key string, v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case uint64:
		if n > 1<<63-1 {
			return 0, fmt.Errorf("item: %s value %d does not fit in an int64", key, n)
		}
		return int64(n), nil
	default:
		return 0, fmt.Errorf("item: %s must be an integer, got %T", key, v)
	}
}

// wantTimestamp accepts a msgpack timestamp, a plain integer, or a byte string.
//
// The last two are borg 1.x spellings that borg's fix_timestamp still accepts: an int
// of nanoseconds, or the bigint_to_int encoding, which is a *little-endian signed*
// integer - the opposite byte order from everything else in the format.
func wantTimestamp(key string, v any) (int64, error) {
	switch t := v.(type) {
	case msgpackx.Timestamp:
		return t.UnixNano(), nil
	case int64:
		return t, nil
	case uint64:
		if t > 1<<63-1 {
			return 0, fmt.Errorf("item: %s value %d does not fit in an int64", key, t)
		}
		return int64(t), nil
	case []byte:
		return littleEndianSigned(t), nil
	default:
		return 0, fmt.Errorf("item: %s must be a timestamp, got %T", key, v)
	}
}

// littleEndianSigned reads borg 1.x's bigint encoding: int.from_bytes(v, "little",
// signed=True), i.e. two's complement in little-endian order.
func littleEndianSigned(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	// Sign-extend from the width actually supplied.
	if len(b) < 8 && b[len(b)-1]&0x80 != 0 {
		v |= ^uint64(0) << (uint(len(b)) * 8)
	}
	return int64(v)
}

func decodeChunkList(key string, v any) ([]ChunkListEntry, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("item: %s must be a list, got %T", key, v)
	}
	out := make([]ChunkListEntry, 0, len(list))
	for i, raw := range list {
		entry, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("item: %s[%d] must be a list, got %T", key, i, raw)
		}
		// borg accepts 2 or 3 elements (id, size[, csize]) and keeps only the first two.
		if len(entry) != 2 && len(entry) != 3 {
			return nil, fmt.Errorf("item: %s[%d] has %d elements, want 2 or 3", key, i, len(entry))
		}
		id, err := wantBytes(fmt.Sprintf("%s[%d].id", key, i), entry[0])
		if err != nil {
			return nil, err
		}
		size, err := wantInt(fmt.Sprintf("%s[%d].size", key, i), entry[1])
		if err != nil {
			return nil, err
		}
		out = append(out, ChunkListEntry{ID: id, Size: size})
	}
	return out, nil
}

func encodeChunkList(entries []ChunkListEntry) []any {
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = []any{e.ID, e.Size}
	}
	return out
}

// decodeXAttrs reads the extended attribute dict. borg normalises it to bytes keys and
// bytes values, turning the None that old borg wrote for an empty value into b"".
func decodeXAttrs(v any) (map[string][]byte, error) {
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, fmt.Errorf("item: xattrs must be a map, got %T", v)
	}
	out := make(map[string][]byte, m.Len())
	for _, e := range m.Entries() {
		k, err := wantBytes("xattrs key", e.Key)
		if err != nil {
			return nil, err
		}
		if e.Value == nil {
			out[string(k)] = []byte{}
			continue
		}
		val, err := wantBytes("xattrs value", e.Value)
		if err != nil {
			return nil, err
		}
		out[string(k)] = val
	}
	return out, nil
}

// encodeXAttrs writes the attribute dict with bytes keys, sorted.
//
// borg stores xattrs in a StableDict, and its keys are *bytes*, so they sort by byte
// value rather than by Python code point. Sorting here rather than relying on the
// StableMap's key comparison keeps that explicit.
func encodeXAttrs(x map[string][]byte) *msgpackx.Map {
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	sort.Strings(keys) // byte order, which is what Python's bytes comparison does

	m := msgpackx.NewMap()
	for _, k := range keys {
		m.Set([]byte(k), x[k])
	}
	return m
}
