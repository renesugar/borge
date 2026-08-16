// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borghash/HashTable.pyx (borghash 0.2.0, BSD 3-Clause,
// Copyright (C) 2024-2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borghash.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package hashindex implements borg's chunk index: the mapping from a 32-byte chunk id
// to what borg knows about that chunk.
//
// # Why not a Go map
//
// At the reference scale - the recipedb corpus has 1,623,610 unique chunks - the index
// holds 32-byte keys and 48-byte values, and it is resident for the whole run of every
// command.
//
// borghash's structure is worth reproducing rather than replacing with a Go map, for
// two reasons. The measured one is memory: 143 MB here against 185 MB for
// map[[32]byte]Entry, a 1.29x saving (TestMemoryFootprint). Real, but on its own it
// would be a thin argument.
//
// The stronger reason is that the layout is load-bearing elsewhere. The hash table
// proper stores only uint32 *indices* into separate dense key and value arrays, so a kv
// index is a stable 32-bit handle for a 256-bit key - which is what borg's k_to_idx /
// idx_to_k abbreviation depends on, and what lets the table be kept at a low load
// factor (0.5) for fast probing while costing 4 bytes per slot. Swapping in a Go map
// would mean reimplementing that separately and losing the index/ serialisation's
// natural shape.
//
// # No hash function
//
// Keys are chunk ids - HMAC-SHA-256 or BLAKE3 outputs - so they are already uniformly
// distributed. borghash takes the first four bytes as a big-endian uint32 and reduces
// modulo the capacity. That is only safe because of where the keys come from; this
// table must not be reused for keys that are not cryptographic hashes.
package hashindex

import (
	"bytes"
	"fmt"
)

// Bucket sentinels. Everything at or above reserved is unavailable as a kv index,
// which caps the table at just under 4 Gi entries - far beyond any practical
// repository (that many 32-byte keys alone would be 128 GiB).
const (
	freeBucket      = 0xFFFFFFFF
	tombstoneBucket = 0xFFFFFFFE
	reservedBucket  = 0xFFFFFF00
)

// Growth parameters, from borghash. They are not tuning knobs here: matching them
// keeps borge's memory profile and resize behaviour the same as borg's, which is what
// makes the stage 9 comparison meaningful.
const (
	minCapacity   = 1000
	maxLoadFactor = 0.5
	minLoadFactor = 0.10
	shrinkFactor  = 0.4
	growFactor    = 2.0
	kvGrowFactor  = 1.3
)

// Table maps fixed-size byte keys to fixed-size byte values.
//
// It is not safe for concurrent use. borg's PackWriter relies on the index being
// touched by one goroutine only (see docs/FORMAT.md §7.2), so adding a mutex here
// would hide a design invariant rather than help.
type Table struct {
	keySize   int
	valueSize int

	// table holds kv indices, or freeBucket / tombstoneBucket.
	table []uint32
	// keys and values are dense arrays indexed by kv index. Deletion zeroes an entry
	// but leaves the slot occupied: borghash never compacts them, so kvUsed only grows
	// within the lifetime of a table.
	keys   []byte
	values []byte

	capacity        int
	used            int
	tombstones      int
	kvUsed          uint32
	kvCapacity      uint32
	initialCapacity int
}

// NewTable returns an empty table for keys and values of the given sizes.
func NewTable(keySize, valueSize, capacity int) (*Table, error) {
	if keySize < 4 {
		return nil, fmt.Errorf("hashindex: key size must be at least 4 bytes, got %d", keySize)
	}
	if valueSize <= 0 {
		return nil, fmt.Errorf("hashindex: value size must be positive, got %d", valueSize)
	}
	if capacity < minCapacity {
		capacity = minCapacity
	}
	t := &Table{keySize: keySize, valueSize: valueSize, initialCapacity: capacity}
	t.resizeTable(capacity)
	t.resizeKV(int(float64(capacity) * maxLoadFactor))
	return t, nil
}

// Len reports how many keys the table holds.
func (t *Table) Len() int { return t.used }

// Capacity reports the current bucket count. It is written into the serialised header,
// which is the only reason it is exposed.
func (t *Table) Capacity() int { return t.capacity }

// KeySize and ValueSize report the fixed sizes this table was built for.
func (t *Table) KeySize() int   { return t.keySize }
func (t *Table) ValueSize() int { return t.valueSize }

// Clear empties the table and returns it to its initial capacity.
func (t *Table) Clear() {
	t.capacity = 0
	t.used = 0
	t.tombstones = 0
	t.resizeTable(t.initialCapacity)
	t.kvUsed = 0
	t.resizeKV(int(float64(t.initialCapacity) * maxLoadFactor))
}

// bucketFor reduces a key to a starting bucket.
//
// There is no hash function: the key is assumed to be perfectly random already. See
// the package comment for why that holds and when it would not.
func (t *Table) bucketFor(key []byte) int {
	key32 := uint32(key[0])<<24 | uint32(key[1])<<16 | uint32(key[2])<<8 | uint32(key[3])
	return int(key32 % uint32(t.capacity))
}

// lookup finds a key. It returns the bucket holding it and true, or a free bucket the
// key could be inserted into and false.
func (t *Table) lookup(key []byte) (int, bool) {
	index := t.bucketFor(key)
	for {
		kv := t.table[index]
		if kv == freeBucket {
			return index, false
		}
		if kv != tombstoneBucket {
			off := int(kv) * t.keySize
			if bytes.Equal(t.keys[off:off+t.keySize], key) {
				return index, true
			}
		}
		// Linear probing: a tombstone is skipped rather than reused, so a key inserted
		// before a deletion is still reachable past the hole it left.
		index++
		if index == t.capacity {
			index = 0
		}
	}
}

// Get returns the value stored under key, and whether it was present. The returned
// slice aliases the table's storage and must not be modified.
func (t *Table) Get(key []byte) ([]byte, bool) {
	if len(key) != t.keySize {
		return nil, false
	}
	index, found := t.lookup(key)
	if !found {
		return nil, false
	}
	off := int(t.table[index]) * t.valueSize
	return t.values[off : off+t.valueSize], true
}

// Contains reports whether the key is present.
func (t *Table) Contains(key []byte) bool {
	if len(key) != t.keySize {
		return false
	}
	_, found := t.lookup(key)
	return found
}

// Set stores a value, replacing any existing one.
func (t *Table) Set(key, value []byte) error {
	if len(key) != t.keySize {
		return fmt.Errorf("hashindex: key must be %d bytes, got %d", t.keySize, len(key))
	}
	if len(value) != t.valueSize {
		return fmt.Errorf("hashindex: value must be %d bytes, got %d", t.valueSize, len(value))
	}

	index, found := t.lookup(key)
	if found {
		off := int(t.table[index]) * t.valueSize
		copy(t.values[off:off+t.valueSize], value)
		return nil
	}

	if t.kvUsed >= t.kvCapacity {
		t.resizeKV(int(float64(t.kvCapacity) * kvGrowFactor))
	}
	if t.kvUsed >= t.kvCapacity {
		// Unreachable in practice: the cap is just under 4 Gi entries. Reported rather
		// than silently wrapping, because a wrapped kv index would corrupt the table.
		return fmt.Errorf("hashindex: key/value array is full (%d entries)", t.kvUsed)
	}

	kv := t.kvUsed
	copy(t.keys[int(kv)*t.keySize:], key)
	copy(t.values[int(kv)*t.valueSize:], value)
	t.kvUsed++
	t.used++
	t.table[index] = kv

	// Tombstones count toward the load: they still cost a probe step, so a table full
	// of them must be rebuilt even though it holds few keys.
	if float64(t.used+t.tombstones) > float64(t.capacity)*maxLoadFactor {
		t.resizeTable(int(float64(t.capacity) * growFactor))
	}
	return nil
}

// Delete removes a key, reporting whether it was present.
func (t *Table) Delete(key []byte) bool {
	if len(key) != t.keySize {
		return false
	}
	index, found := t.lookup(key)
	if !found {
		return false
	}
	kv := int(t.table[index])

	// Zero the storage rather than only unlinking it: the value can hold key material
	// or pack locations, and leaving it readable in a long-lived array is avoidable.
	clearBytes(t.keys[kv*t.keySize : (kv+1)*t.keySize])
	clearBytes(t.values[kv*t.valueSize : (kv+1)*t.valueSize])

	t.table[index] = tombstoneBucket
	t.used--
	t.tombstones++

	if float64(t.used) < float64(t.capacity)*minLoadFactor {
		newCapacity := int(float64(t.capacity) * shrinkFactor)
		if newCapacity < minCapacity {
			newCapacity = minCapacity
		}
		t.resizeTable(newCapacity)
	}
	return true
}

// Iterate calls fn for every key/value pair, in the table's internal bucket order.
//
// The order is not meaningful and is not stable across insertions - it depends on the
// current capacity and on the order things were inserted. Callers that need a defined
// order must sort. fn returning false stops the iteration.
//
// The slices passed to fn alias the table's storage and are only valid for the call.
func (t *Table) Iterate(fn func(key, value []byte) bool) {
	for i := 0; i < t.capacity; i++ {
		kv := t.table[i]
		if kv == freeBucket || kv == tombstoneBucket {
			continue
		}
		k := t.keys[int(kv)*t.keySize : (int(kv)+1)*t.keySize]
		v := t.values[int(kv)*t.valueSize : (int(kv)+1)*t.valueSize]
		if !fn(k, v) {
			return
		}
	}
}

// resizeTable rebuilds the bucket array at a new capacity, dropping all tombstones.
func (t *Table) resizeTable(newCapacity int) {
	if newCapacity < 1 {
		newCapacity = minCapacity
	}
	newTable := make([]uint32, newCapacity)
	for i := range newTable {
		newTable[i] = freeBucket
	}

	oldCapacity, oldTable := t.capacity, t.table
	t.capacity = newCapacity
	for i := 0; i < oldCapacity; i++ {
		kv := oldTable[i]
		if kv == freeBucket || kv == tombstoneBucket {
			continue
		}
		off := int(kv) * t.keySize
		index := t.bucketFor(t.keys[off : off+t.keySize])
		for newTable[index] != freeBucket {
			index++
			if index == newCapacity {
				index = 0
			}
		}
		newTable[index] = kv
	}

	t.table = newTable
	t.tombstones = 0
}

// resizeKV grows the key and value arrays. They never shrink: a kv index is stable for
// the lifetime of the table, and compacting them would invalidate every bucket.
func (t *Table) resizeKV(newCapacity int) {
	if newCapacity > reservedBucket-1 {
		newCapacity = reservedBucket - 1
	}
	if newCapacity <= int(t.kvCapacity) {
		return
	}
	keys := make([]byte, newCapacity*t.keySize)
	copy(keys, t.keys)
	values := make([]byte, newCapacity*t.valueSize)
	copy(values, t.values)
	t.keys, t.values = keys, values
	t.kvCapacity = uint32(newCapacity)
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
