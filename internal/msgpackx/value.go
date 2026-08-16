// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file ports StableDict from borg's src/borg/helpers/datastruct.py and the
// Timestamp helpers from src/borg/helpers/msgpack.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package msgpackx

import (
	"fmt"
	"sort"
)

// Map is a msgpack map. It keeps entries in order, because msgpack maps are ordered
// on the wire and borg's choice of order is format-visible: chunk ids are computed
// over packed bytes, so reordering a map changes the id of the object containing it.
//
// A Go map cannot be used for this. Its iteration order is deliberately randomised,
// which would make borge's output non-deterministic across runs.
type Map struct {
	// Stable makes Encode sort the entries by key before writing, reproducing borg's
	// StableDict (src/borg/helpers/datastruct.py), whose items() returns
	// sorted(super().items()).
	//
	// This is not cosmetic. borg uses StableDict exactly where packed bytes get
	// hashed - manifest config, archive metadata, item xattrs - so that the same
	// logical content always packs to the same bytes and therefore the same chunk id.
	// msgpack-python's packer calls items() on dict subclasses (with strict_types
	// False, which borg sets), which is how the sorting reaches the wire.
	Stable bool

	entries []MapEntry
}

// MapEntry is one key/value pair of a Map.
type MapEntry struct {
	Key   any
	Value any
}

// NewMap returns an insertion-ordered map, the equivalent of a plain Python dict.
func NewMap(entries ...MapEntry) *Map {
	return &Map{entries: append([]MapEntry(nil), entries...)}
}

// NewStableMap returns a map that sorts its keys when encoded, the equivalent of
// borg's StableDict.
func NewStableMap(entries ...MapEntry) *Map {
	return &Map{Stable: true, entries: append([]MapEntry(nil), entries...)}
}

// Len reports the number of entries.
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// Entries returns the entries in their current order. The result aliases the Map's
// storage; do not modify it.
func (m *Map) Entries() []MapEntry {
	if m == nil {
		return nil
	}
	return m.entries
}

// Set stores a value, replacing an existing entry with an equal key and otherwise
// appending. Keys are compared with keysEqual, matching Python's dict semantics for
// the key types borg uses.
func (m *Map) Set(key, value any) {
	for i := range m.entries {
		if keysEqual(m.entries[i].Key, key) {
			m.entries[i].Value = value
			return
		}
	}
	m.entries = append(m.entries, MapEntry{Key: key, Value: value})
}

// Get returns the value stored under key, and whether it was present.
func (m *Map) Get(key any) (any, bool) {
	if m == nil {
		return nil, false
	}
	for _, e := range m.entries {
		if keysEqual(e.Key, key) {
			return e.Value, true
		}
	}
	return nil, false
}

// Delete removes the entry with the given key, reporting whether it was present.
func (m *Map) Delete(key any) bool {
	for i, e := range m.entries {
		if keysEqual(e.Key, key) {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)
			return true
		}
	}
	return false
}

// GetString returns a string-typed value stored under a string key.
func (m *Map) GetString(key string) (string, bool) {
	v, ok := m.Get(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// GetBytes returns a []byte-typed value stored under a string key.
func (m *Map) GetBytes(key string) ([]byte, bool) {
	v, ok := m.Get(key)
	if !ok {
		return nil, false
	}
	b, ok := v.([]byte)
	return b, ok
}

// GetInt returns an integer value stored under a string key, accepting either
// signedness as the decoder may produce.
func (m *Map) GetInt(key string) (int64, bool) {
	v, ok := m.Get(key)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		if n > 1<<63-1 {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// sortedEntries returns the entries in Python's sorted() order. It is used by Encode
// when Stable is set.
func (m *Map) sortedEntries() ([]MapEntry, error) {
	out := append([]MapEntry(nil), m.entries...)
	var sortErr error
	sort.SliceStable(out, func(i, j int) bool {
		less, err := keyLess(out[i].Key, out[j].Key)
		if err != nil && sortErr == nil {
			sortErr = err
		}
		return less
	})
	if sortErr != nil {
		return nil, sortErr
	}
	return out, nil
}

// Ext is a msgpack extension value other than a timestamp. borg does not write any,
// but the decoder must be able to represent one rather than failing, so that a
// diagnostic can say what was found.
type Ext struct {
	Type int8
	Data []byte
}

// TimestampExtType is the msgpack extension type reserved for timestamps by the
// msgpack spec.
const TimestampExtType int8 = -1

// timestampExtByte is TimestampExtType as it appears on the wire. Go refuses to
// convert the negative typed constant to a byte directly, and the two's-complement
// byte is what actually gets written.
const timestampExtByte byte = 0xff

// Timestamp is the msgpack timestamp extension, used by borg for item atime, ctime,
// mtime and birthtime (src/borg/item.pyx: int_to_timestamp / timestamp_to_int).
//
// Nanoseconds is always in [0, 1e9), so a time before the epoch has a negative
// Seconds and a positive Nanoseconds - the same normalisation Python's floor division
// produces in Timestamp.from_unix_nano.
type Timestamp struct {
	Seconds     int64
	Nanoseconds uint32
}

const nanosPerSecond = 1_000_000_000

// TimestampFromUnixNano converts nanoseconds since the epoch to a Timestamp, matching
// msgpack-python's Timestamp.from_unix_nano. Python's // and % floor toward negative
// infinity, so -1ns becomes {Seconds: -1, Nanoseconds: 999999999}; Go's / and %
// truncate toward zero and would give {0, -1}, which is not representable.
func TimestampFromUnixNano(ns int64) Timestamp {
	sec := ns / nanosPerSecond
	nsec := ns % nanosPerSecond
	if nsec < 0 {
		sec--
		nsec += nanosPerSecond
	}
	return Timestamp{Seconds: sec, Nanoseconds: uint32(nsec)}
}

// UnixNano converts back to nanoseconds since the epoch, matching
// msgpack-python's Timestamp.to_unix_nano.
func (t Timestamp) UnixNano() int64 {
	return t.Seconds*nanosPerSecond + int64(t.Nanoseconds)
}

func (t Timestamp) String() string {
	return fmt.Sprintf("Timestamp(seconds=%d, nanoseconds=%d)", t.Seconds, t.Nanoseconds)
}
