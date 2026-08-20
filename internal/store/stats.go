// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the statistics borgstore keeps, as borg renders them in
// format_store_stats (src/borg/archive.py).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"sync"
	"time"
)

// What the store did, which is what "borg extract --stats" reports and what
// "create --json" carries as store_stats.
//
// # Why measure at all
//
// borge omitted store_stats, hashing_time and chunking_time from its JSON rather than
// sending zeroes, and the reason was right: "a frontend charting hashing_time would draw a
// flat line and believe it, where a missing key is a question it can answer"
// (PORTING_PLAN §11.4). The way out of that is not to invent the numbers but to measure
// them, which is what this file starts.
//
// # What each group means
//
// The three groups answer different questions, and they differ only when a cache is in
// play:
//
//   - the per-method counters are what the *caller* asked the store to do;
//   - the backend counters are what actually reached storage, so backend_load_calls below
//     load_calls is the cache doing its job;
//   - the cache counters are that job from the other side.
//
// With no cache configured, the first two are equal and the third is zeroes - which is not
// a fabrication but the truth about a store that has no cache: cache_disabled says so.

// MethodStats is one store method's tally.
type MethodStats struct {
	Calls int64
	Time  time.Duration
	// Volume is bytes moved, and is only meaningful for load and store. borgstore leaves
	// it out of its report for the other four rather than printing zeroes, and so does
	// the renderer in the command layer.
	Volume int64
}

// Stats is a snapshot of everything the store has counted.
type Stats struct {
	Info, List, Load, Store, Delete, Move MethodStats

	BackendLoadCalls   int64
	BackendLoadVolume  int64
	BackendStoreCalls  int64
	BackendStoreVolume int64
	BackendDeleteCalls int64

	// CacheDisabled reports that no cache backend is configured, which is why every cache
	// counter below it is zero.
	CacheDisabled    bool
	CacheHits        int64
	CacheMisses      int64
	CacheErrors      int64
	CacheLoadCalls   int64
	CacheLoadVolume  int64
	CacheStoreCalls  int64
	CacheStoreVolume int64
	CacheDeleteCalls int64
}

// HitRatio is hits over hits+misses, in the range 0..1. With neither, it is zero - borg
// prints "0.0%" there rather than leaving the line out.
func (s Stats) HitRatio() float64 {
	total := s.CacheHits + s.CacheMisses
	if total == 0 {
		return 0
	}
	return float64(s.CacheHits) / float64(total)
}

// statsRecorder accumulates the counters. It has its own mutex rather than reusing the
// Store's: Stats() must be callable while the store is in use, and a caller reading the
// numbers should not have to wait behind a pack write to get them.
type statsRecorder struct {
	mu sync.Mutex
	s  Stats
}

// observe records one call of a method: how long it took and how much it moved.
func (r *statsRecorder) observe(m *MethodStats, started time.Time, volume int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m.Calls++
	m.Time += time.Since(started)
	m.Volume += volume
}

func (r *statsRecorder) add(counter *int64, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*counter += n
}

// snapshot copies the counters out.
func (r *statsRecorder) snapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

// Stats returns what this store has done so far.
func (s *Store) Stats() Stats {
	out := s.stats.snapshot()
	out.CacheDisabled = s.cache == nil
	return out
}
