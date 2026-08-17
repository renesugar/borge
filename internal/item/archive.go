// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the ArchiveItem and ManifestItem classes in borg's
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

// ArchiveItem is an archive's metadata object (ROBJ_ARCHIVE_META).
//
// Version 2 is borg 2's format. The legacy fields below are read when transferring a
// borg 1.x archive and are never written; they are kept as separate fields rather than
// folded into their modern equivalents so a round trip does not silently rename them.
type ArchiveItem struct {
	Version int64
	Name    string

	// ItemPtrs is a list of chunk ids, each pointing at a block that itself holds a
	// list of chunk ids of the item metadata stream. The extra level of indirection is
	// what keeps the archive object small for an archive with millions of items.
	ItemPtrs    [][]byte
	ItemPtrsSet bool

	CommandLine *string
	Hostname    *string
	Username    *string

	// Time, Start and End are ISO-8601 strings, not timestamps: borg stores archive
	// times as text here, unlike item times.
	//
	// All three are v2 fields. Time is the archive's *nominal* time - the one a listing
	// shows and "--timestamp" sets - while Start and End bracket how long the backup
	// actually ran. Time is one of REQUIRED_ARCHIVE_KEYS; only TimeEnd below is legacy.
	Time  *string
	Start *string
	End   *string

	Comment *string
	Tags    []string
	TagsSet bool

	// ChunkerParams records how the archive was chunked, so borg recreate knows
	// whether re-chunking is needed. Elements are a mix of strings and integers, e.g.
	// ("fastcdc", 19, 23, 21, 2).
	ChunkerParams    []any
	ChunkerParamsSet bool

	Size   *int64
	NFiles *int64
	CWD    *string

	RecreateCommandLine *string

	// Legacy borg 1.x fields, read only.
	Items                 [][]byte
	ItemsSet              bool
	CmdLine               []string
	CmdLineSet            bool
	RecreateCmdLine       []string
	RecreateCmdLineSet    bool
	TimeEnd               *string
	SizeParts             *int64
	NFilesParts           *int64
	RecreateSourceID      []byte
	RecreateArgs          []string
	RecreateArgsSet       bool
	RecreatePartialChunks []any
	RecreatePartialSet    bool

	Unknown []msgpackx.MapEntry
}

var archiveKnownKeys = map[string]bool{}

func init() {
	for _, k := range []string{
		"version", "name", "items", "item_ptrs", "cmdline", "command_line",
		"hostname", "username", "start", "end", "time", "time_end", "comment",
		"tags", "chunker_params", "recreate_cmdline", "recreate_command_line",
		"recreate_source_id", "recreate_args", "recreate_partial_chunks",
		"size", "nfiles", "size_parts", "nfiles_parts", "cwd",
	} {
		archiveKnownKeys[k] = true
	}
}

// DecodeArchiveItem reads an archive metadata object.
func DecodeArchiveItem(m *msgpackx.Map) (*ArchiveItem, error) {
	if m == nil {
		return nil, fmt.Errorf("item: cannot decode a nil archive map")
	}
	a := &ArchiveItem{}
	seen := map[string]bool{}

	for _, e := range m.Entries() {
		key, err := mapKey(e.Key)
		if err != nil {
			return nil, err
		}
		if !archiveKnownKeys[key] {
			a.Unknown = append(a.Unknown, e)
			continue
		}
		seen[key] = true

		switch key {
		case "version":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			a.Version = n
		case "name":
			a.Name, err = wantString(key, e.Value)
			if err != nil {
				return nil, err
			}

		case "command_line", "recreate_command_line", "hostname", "username",
			"comment", "cwd", "start", "end", "time", "time_end":
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "command_line":
				a.CommandLine = &s
			case "recreate_command_line":
				a.RecreateCommandLine = &s
			case "hostname":
				a.Hostname = &s
			case "username":
				a.Username = &s
			case "comment":
				a.Comment = &s
			case "cwd":
				a.CWD = &s
			case "start":
				a.Start = &s
			case "end":
				a.End = &s
			case "time":
				a.Time = &s
			case "time_end":
				a.TimeEnd = &s
			}

		case "size", "nfiles", "size_parts", "nfiles_parts":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "size":
				a.Size = &n
			case "nfiles":
				a.NFiles = &n
			case "size_parts":
				a.SizeParts = &n
			case "nfiles_parts":
				a.NFilesParts = &n
			}

		case "item_ptrs", "items":
			list, err := decodeBytesList(key, e.Value)
			if err != nil {
				return nil, err
			}
			if key == "item_ptrs" {
				a.ItemPtrs, a.ItemPtrsSet = list, true
			} else {
				a.Items, a.ItemsSet = list, true
			}

		case "tags", "cmdline", "recreate_cmdline", "recreate_args":
			list, err := decodeStringList(key, e.Value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "tags":
				a.Tags, a.TagsSet = list, true
			case "cmdline":
				a.CmdLine, a.CmdLineSet = list, true
			case "recreate_cmdline":
				a.RecreateCmdLine, a.RecreateCmdLineSet = list, true
			case "recreate_args":
				a.RecreateArgs, a.RecreateArgsSet = list, true
			}

		case "chunker_params":
			list, ok := e.Value.([]any)
			if !ok {
				return nil, fmt.Errorf("item: chunker_params must be a list, got %T", e.Value)
			}
			// borg's fix_tuple_of_str_and_int: decode bytes elements to str, leave
			// integers alone, reject anything else.
			out := make([]any, len(list))
			for i, v := range list {
				switch x := v.(type) {
				case []byte:
					out[i] = string(x)
				case string, int64, uint64:
					out[i] = x
				default:
					return nil, fmt.Errorf("item: chunker_params[%d] must be a string or an integer, got %T", i, v)
				}
			}
			a.ChunkerParams, a.ChunkerParamsSet = out, true

		case "recreate_source_id":
			b, err := wantBytes(key, e.Value)
			if err != nil {
				return nil, err
			}
			a.RecreateSourceID = b

		case "recreate_partial_chunks":
			list, ok := e.Value.([]any)
			if !ok {
				return nil, fmt.Errorf("item: recreate_partial_chunks must be a list, got %T", e.Value)
			}
			a.RecreatePartialChunks, a.RecreatePartialSet = list, true
		}
	}

	// REQUIRED_ARCHIVE_KEYS in src/borg/constants.py. "time" is required there too, but
	// borg 2 writes "start"/"end" instead and only borg 1.x archives carry "time", so
	// requiring it would reject every archive borg 2 writes.
	for _, k := range []string{"version", "name", "item_ptrs", "command_line"} {
		if !seen[k] {
			return nil, fmt.Errorf("item: archive is missing required key %q", k)
		}
	}
	return a, nil
}

// Encode renders the archive metadata with sorted keys.
func (a *ArchiveItem) Encode() *msgpackx.Map {
	m := msgpackx.NewStableMap()
	m.Set("version", a.Version)
	m.Set("name", a.Name)

	for _, f := range []struct {
		key string
		val *string
	}{
		{"command_line", a.CommandLine}, {"recreate_command_line", a.RecreateCommandLine},
		{"hostname", a.Hostname}, {"username", a.Username}, {"comment", a.Comment},
		{"cwd", a.CWD}, {"start", a.Start}, {"end", a.End},
		{"time", a.Time}, {"time_end", a.TimeEnd},
	} {
		if f.val != nil {
			m.Set(f.key, *f.val)
		}
	}
	for _, f := range []struct {
		key string
		val *int64
	}{
		{"size", a.Size}, {"nfiles", a.NFiles},
		{"size_parts", a.SizeParts}, {"nfiles_parts", a.NFilesParts},
	} {
		if f.val != nil {
			m.Set(f.key, *f.val)
		}
	}

	if a.ItemPtrsSet {
		m.Set("item_ptrs", bytesListToAny(a.ItemPtrs))
	}
	if a.ItemsSet {
		m.Set("items", bytesListToAny(a.Items))
	}
	if a.TagsSet {
		m.Set("tags", stringListToAny(a.Tags))
	}
	if a.CmdLineSet {
		m.Set("cmdline", stringListToAny(a.CmdLine))
	}
	if a.RecreateCmdLineSet {
		m.Set("recreate_cmdline", stringListToAny(a.RecreateCmdLine))
	}
	if a.RecreateArgsSet {
		m.Set("recreate_args", stringListToAny(a.RecreateArgs))
	}
	if a.ChunkerParamsSet {
		m.Set("chunker_params", a.ChunkerParams)
	}
	if a.RecreateSourceID != nil {
		m.Set("recreate_source_id", a.RecreateSourceID)
	}
	if a.RecreatePartialSet {
		m.Set("recreate_partial_chunks", a.RecreatePartialChunks)
	}

	for _, e := range a.Unknown {
		m.Set(e.Key, e.Value)
	}
	return m
}

// Marshal encodes the archive metadata to msgpack bytes.
func (a *ArchiveItem) Marshal() ([]byte, error) { return msgpackx.Marshal(a.Encode()) }

// UnmarshalArchiveItem decodes archive metadata from msgpack bytes.
func UnmarshalArchiveItem(b []byte) (*ArchiveItem, error) {
	v, err := msgpackx.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, fmt.Errorf("item: expected a map, got %T", v)
	}
	return DecodeArchiveItem(m)
}

// ------------------------------------------------------------------- manifest

// ManifestItem is the repository manifest (ROBJ_MANIFEST), stored at config/manifest.
type ManifestItem struct {
	Version int64
	// Archives is always empty in borg 2: the archives/ namespace in the object store
	// *is* the archive directory (src/borg/manifest.py). The field exists so a borg 1.x
	// manifest can still be read and re-encoded unchanged.
	Archives    *msgpackx.Map
	ArchivesSet bool

	Timestamp *string
	Config    *msgpackx.Map
	ConfigSet bool

	// ItemKeys is the legacy location; borg 2 keeps it inside Config.
	ItemKeys    []string
	ItemKeysSet bool

	Unknown []msgpackx.MapEntry
}

var manifestKnownKeys = map[string]bool{
	"version": true, "archives": true, "timestamp": true, "config": true, "item_keys": true,
}

// DecodeManifestItem reads a manifest.
func DecodeManifestItem(m *msgpackx.Map) (*ManifestItem, error) {
	if m == nil {
		return nil, fmt.Errorf("item: cannot decode a nil manifest map")
	}
	mi := &ManifestItem{}
	for _, e := range m.Entries() {
		key, err := mapKey(e.Key)
		if err != nil {
			return nil, err
		}
		if !manifestKnownKeys[key] {
			mi.Unknown = append(mi.Unknown, e)
			continue
		}
		switch key {
		case "version":
			n, err := wantInt(key, e.Value)
			if err != nil {
				return nil, err
			}
			mi.Version = n
		case "timestamp":
			s, err := wantString(key, e.Value)
			if err != nil {
				return nil, err
			}
			mi.Timestamp = &s
		case "archives", "config":
			sub, ok := e.Value.(*msgpackx.Map)
			if !ok {
				return nil, fmt.Errorf("item: manifest %s must be a map, got %T", key, e.Value)
			}
			if key == "archives" {
				mi.Archives, mi.ArchivesSet = sub, true
			} else {
				mi.Config, mi.ConfigSet = sub, true
			}
		case "item_keys":
			list, err := decodeStringList(key, e.Value)
			if err != nil {
				return nil, err
			}
			mi.ItemKeys, mi.ItemKeysSet = list, true
		}
	}
	return mi, nil
}

// Encode renders the manifest with sorted keys.
func (mi *ManifestItem) Encode() *msgpackx.Map {
	m := msgpackx.NewStableMap()
	m.Set("version", mi.Version)
	if mi.ArchivesSet {
		m.Set("archives", mi.Archives)
	}
	if mi.Timestamp != nil {
		m.Set("timestamp", *mi.Timestamp)
	}
	if mi.ConfigSet {
		m.Set("config", mi.Config)
	}
	if mi.ItemKeysSet {
		m.Set("item_keys", stringListToAny(mi.ItemKeys))
	}
	for _, e := range mi.Unknown {
		m.Set(e.Key, e.Value)
	}
	return m
}

// Marshal encodes the manifest to msgpack bytes.
func (mi *ManifestItem) Marshal() ([]byte, error) { return msgpackx.Marshal(mi.Encode()) }

// UnmarshalManifestItem decodes a manifest from msgpack bytes.
func UnmarshalManifestItem(b []byte) (*ManifestItem, error) {
	v, err := msgpackx.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	m, ok := v.(*msgpackx.Map)
	if !ok {
		return nil, fmt.Errorf("item: expected a map, got %T", v)
	}
	return DecodeManifestItem(m)
}

// ------------------------------------------------------------------- helpers

func decodeBytesList(key string, v any) ([][]byte, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("item: %s must be a list, got %T", key, v)
	}
	out := make([][]byte, len(list))
	for i, raw := range list {
		b, err := wantBytes(fmt.Sprintf("%s[%d]", key, i), raw)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

func decodeStringList(key string, v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("item: %s must be a list, got %T", key, v)
	}
	out := make([]string, len(list))
	for i, raw := range list {
		s, err := wantString(fmt.Sprintf("%s[%d]", key, i), raw)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

func bytesListToAny(list [][]byte) []any {
	out := make([]any, len(list))
	for i, b := range list {
		out[i] = b
	}
	return out
}

func stringListToAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
