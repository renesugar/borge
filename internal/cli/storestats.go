// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of format_store_stats and _STORE_STATS_ORDER in borg's
// src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"strings"

	"github.com/renesugar/borge/internal/store"
)

// The store statistics, as "borg extract --stats" prints them and "create --json" carries
// them.
//
// borge sent none of this, and the omission was recorded as deliberate: sending zeroes
// would be worse than sending nothing, because "a frontend charting hashing_time would draw
// a flat line and believe it, where a missing key is a question it can answer"
// (PORTING_PLAN §11.4). That argument was right, and it argues for *measuring*, which is
// what internal/store/stats.go now does. See DIVERGENCES.md #51.

// storeStatsKey is one line of the report: the key borg puts in the JSON, and the value.
type storeStatsKey struct {
	name  string
	value any
}

// storeStatsData renders a snapshot into borg's key order.
//
// borg's _STORE_STATS_ORDER lists four keys per method, and its renderer prints only those
// the stats dict actually holds - which for info, list, delete and move is calls and time
// alone, since borgstore records no volume for them. The same keys are produced here, in
// the same order, so a diff against borg's output lines up.
func storeStatsData(s store.Stats) []storeStatsKey {
	var out []storeStatsKey
	add := func(name string, value any) { out = append(out, storeStatsKey{name, value}) }

	for _, m := range []struct {
		name    string
		stats   store.MethodStats
		volumes bool
	}{
		{"info", s.Info, false},
		{"list", s.List, false},
		{"load", s.Load, true},
		{"store", s.Store, true},
		{"delete", s.Delete, false},
		{"move", s.Move, false},
	} {
		add(m.name+"_calls", m.stats.Calls)
		add(m.name+"_time", m.stats.Time.Seconds())
		if !m.volumes {
			continue
		}
		add(m.name+"_volume", m.stats.Volume)
		// Throughput is derived, as borgstore derives it: volume over the time this
		// method spent. A method never called has no time and no volume, and dividing
		// gives a zero rather than a NaN.
		throughput := 0.0
		if secs := m.stats.Time.Seconds(); secs > 0 {
			throughput = float64(m.stats.Volume) / secs
		}
		add(m.name+"_throughput", throughput)
	}

	add("backend_load_calls", s.BackendLoadCalls)
	add("backend_load_volume", s.BackendLoadVolume)
	add("backend_store_calls", s.BackendStoreCalls)
	add("backend_store_volume", s.BackendStoreVolume)
	add("backend_delete_calls", s.BackendDeleteCalls)

	add("cache_disabled", s.CacheDisabled)
	add("cache_hits", s.CacheHits)
	add("cache_misses", s.CacheMisses)
	add("cache_hit_ratio", s.HitRatio())
	add("cache_errors", s.CacheErrors)
	add("cache_load_calls", s.CacheLoadCalls)
	add("cache_load_volume", s.CacheLoadVolume)
	add("cache_store_calls", s.CacheStoreCalls)
	add("cache_store_volume", s.CacheStoreVolume)
	add("cache_delete_calls", s.CacheDeleteCalls)
	return out
}

// formatStoreStats renders the report as borg's text, one "Store <name>: <value>" line per
// entry with the underscores turned into spaces.
//
// The value formatting is borg's format_value: a throughput gets "/s", a volume gets the
// size units, a time gets three decimals and the word "seconds", a ratio gets a percentage
// with one decimal, and anything else is printed as it is. Python's False is capitalised,
// and that is reproduced rather than tidied - somebody grepping this output has borg's
// spelling in their fingers.
func formatStoreStats(s store.Stats, units string) string {
	var b strings.Builder
	for _, k := range storeStatsData(s) {
		fmt.Fprintf(&b, "Store %s: %s\n", strings.ReplaceAll(k.name, "_", " "),
			formatStoreStatsValue(k.name, k.value, units))
	}
	return b.String()
}

func formatStoreStatsValue(name string, value any, units string) string {
	switch {
	case strings.HasSuffix(name, "_throughput"):
		return formatBytesIn(int64(value.(float64)), units) + "/s"
	case strings.HasSuffix(name, "_volume"):
		return formatBytesIn(value.(int64), units)
	case strings.HasSuffix(name, "_time"):
		return fmt.Sprintf("%.3f seconds", value.(float64))
	case strings.HasSuffix(name, "_ratio"):
		return fmt.Sprintf("%.1f%%", value.(float64)*100)
	}
	if b, ok := value.(bool); ok {
		// Python's str(False).
		if b {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", value)
}

// storeStatsJSON is the same numbers as the map borg puts under "store_stats".
func storeStatsJSON(s store.Stats) map[string]any {
	out := map[string]any{}
	for _, k := range storeStatsData(s) {
		out[k.name] = k.value
	}
	return out
}
