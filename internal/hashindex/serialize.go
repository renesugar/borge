// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the write()/read() methods in borghash/HashTableNT.pyx
// (borghash 0.2.0, BSD 3-Clause, Copyright (C) 2024-2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borghash.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package hashindex

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// The serialised chunk index, stored at index/<sha256 of contents> in the repository.
//
//	magic     "BORGHASH"        8 bytes
//	version   uint32 LE         currently 1
//	meta_size uint32 LE         length of the JSON metadata that follows
//	meta      JSON, UTF-8       key/value sizes, field names and formats, capacity, used
//	body      used x (key || value)
//
// Verified against an index written by borg 2.0.0b23 (docs/FORMAT.md).
//
// # What has to match, and what does not
//
// The entries are written in the table's internal bucket order, which depends on the
// capacity and on insertion history. Reproducing borg's exact byte output would mean
// reproducing its whole resize history, and it buys nothing: the reader inserts each
// entry by key, so any order round-trips to the same index. What must match is the
// header layout, the metadata field names and the fixed-width value encoding.

const (
	fileMagic   = "BORGHASH"
	fileVersion = 1
	headerSize  = 8 + 4 + 4
)

// fileMeta is the JSON metadata block. The field names and their order reproduce
// borghash's dict exactly; the names are what borg parses, and keeping the order makes
// a byte diff against a borg-written header readable.
type fileMeta struct {
	KeySize           int      `json:"key_size"`
	ValueSize         int      `json:"value_size"`
	ByteOrder         string   `json:"byte_order"`
	ValueTypeName     string   `json:"value_type_name"`
	ValueTypeFields   []string `json:"value_type_fields"`
	ValueFormatName   string   `json:"value_format_name"`
	ValueFormatFields []string `json:"value_format_fields"`
	ValueFormat       []string `json:"value_format"`
	Capacity          int      `json:"capacity"`
	Used              int      `json:"used"`
}

// Layout describes the value structure a table serialises, so the metadata can name it
// the way borg expects.
type Layout struct {
	ValueTypeName   string
	ValueFormatName string
	Fields          []string
	Formats         []string
}

// ChunkIndexLayout is the layout borg uses for the chunk index
// (src/borg/hashindex.pyx: ChunkIndexEntry / ChunkIndexEntryFormat).
var ChunkIndexLayout = Layout{
	ValueTypeName:   "ChunkIndexEntry",
	ValueFormatName: "ChunkIndexEntryFormatT",
	Fields:          []string{"flags", "size", "pack_id", "obj_offset", "obj_size"},
	Formats:         []string{"I", "I", "32s", "I", "I"},
}

// WriteTable serialises a table in borghash's format.
func WriteTable(w io.Writer, t *Table, layout Layout) error {
	meta := fileMeta{
		KeySize:   t.keySize,
		ValueSize: t.valueSize,
		// borg always writes little-endian; the field exists so a reader can tell.
		ByteOrder:         "little",
		ValueTypeName:     layout.ValueTypeName,
		ValueTypeFields:   layout.Fields,
		ValueFormatName:   layout.ValueFormatName,
		ValueFormatFields: layout.Fields,
		ValueFormat:       layout.Formats,
		Capacity:          t.capacity,
		Used:              t.used,
	}
	metaBytes, err := marshalPythonJSON(meta)
	if err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}

	var header [headerSize]byte
	copy(header[:8], fileMagic)
	binary.LittleEndian.PutUint32(header[8:], fileVersion)
	binary.LittleEndian.PutUint32(header[12:], uint32(len(metaBytes)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}
	if _, err := w.Write(metaBytes); err != nil {
		return fmt.Errorf("hashindex: %w", err)
	}

	written := 0
	var writeErr error
	t.Iterate(func(key, value []byte) bool {
		if _, err := w.Write(key); err != nil {
			writeErr = err
			return false
		}
		if _, err := w.Write(value); err != nil {
			writeErr = err
			return false
		}
		written++
		return true
	})
	if writeErr != nil {
		return fmt.Errorf("hashindex: %w", writeErr)
	}
	if written != t.used {
		// The header already claims t.used entries, so a mismatch would produce a file
		// that reads short. This can only happen if the table was mutated mid-write.
		return fmt.Errorf("hashindex: wrote %d entries but the header says %d", written, t.used)
	}
	return nil
}

// ReadTable parses a serialised table.
func ReadTable(r io.Reader) (*Table, Layout, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, Layout{}, fmt.Errorf("hashindex: reading header: %w", err)
	}
	if string(header[:8]) != fileMagic {
		return nil, Layout{}, fmt.Errorf("hashindex: not a borghash file (magic %q)", header[:8])
	}
	if v := binary.LittleEndian.Uint32(header[8:]); v != fileVersion {
		return nil, Layout{}, fmt.Errorf("hashindex: unsupported file version %d, expected %d", v, fileVersion)
	}
	metaSize := binary.LittleEndian.Uint32(header[12:])
	// A crafted length must not become a huge allocation. Real metadata is a few
	// hundred bytes; a megabyte is already absurd.
	if metaSize > 1<<20 {
		return nil, Layout{}, fmt.Errorf("hashindex: metadata length %d is implausible", metaSize)
	}

	metaBytes := make([]byte, metaSize)
	if _, err := io.ReadFull(r, metaBytes); err != nil {
		return nil, Layout{}, fmt.Errorf("hashindex: reading metadata: %w", err)
	}
	var meta fileMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, Layout{}, fmt.Errorf("hashindex: parsing metadata: %w", err)
	}

	if meta.ByteOrder != "little" {
		return nil, Layout{}, fmt.Errorf("hashindex: byte order %q is not supported; borg writes little", meta.ByteOrder)
	}
	if meta.KeySize < 4 || meta.ValueSize <= 0 {
		return nil, Layout{}, fmt.Errorf("hashindex: implausible sizes in metadata (key %d, value %d)",
			meta.KeySize, meta.ValueSize)
	}
	if meta.Used < 0 || meta.Capacity < 0 {
		return nil, Layout{}, fmt.Errorf("hashindex: negative used/capacity in metadata")
	}

	capacity := meta.Capacity
	if capacity < minCapacity {
		capacity = minCapacity
	}
	t, err := NewTable(meta.KeySize, meta.ValueSize, capacity)
	if err != nil {
		return nil, Layout{}, err
	}

	entry := make([]byte, meta.KeySize+meta.ValueSize)
	for i := 0; i < meta.Used; i++ {
		if _, err := io.ReadFull(r, entry); err != nil {
			return nil, Layout{}, fmt.Errorf("hashindex: reading entry %d of %d: %w", i, meta.Used, err)
		}
		if err := t.Set(entry[:meta.KeySize], entry[meta.KeySize:]); err != nil {
			return nil, Layout{}, err
		}
	}

	layout := Layout{
		ValueTypeName:   meta.ValueTypeName,
		ValueFormatName: meta.ValueFormatName,
		Fields:          meta.ValueTypeFields,
		Formats:         meta.ValueFormat,
	}
	return t, layout, nil
}

// marshalPythonJSON encodes v the way json.dumps does by default: a space after each
// ':' and ', ' between items.
//
// Nothing parses the whitespace, so this is purely so a byte diff between a
// borge-written and a borg-written header shows only the values that genuinely differ.
// Go's encoding/json emits no spaces at all, which would otherwise make every header
// look different.
func marshalPythonJSON(v any) ([]byte, error) {
	compact, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	inString := false
	escaped := false
	for _, c := range compact {
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case !inString && (c == ':' || c == ','):
			out.WriteByte(c)
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(c)
	}
	return out.Bytes(), nil
}
