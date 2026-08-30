// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/store.py (borgstore 0.6.1, BSD 3-Clause,
// Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// CacheMode selects how a namespace uses the local cache.
type CacheMode int

const (
	// CacheOff does not cache. This is the default for every namespace.
	CacheOff CacheMode = iota
	// CacheWritethrough caches on write and on a read miss. A miss loads the *whole*
	// object, caches it, and serves the requested range from the copy - which is what
	// makes the pack cache worth having: reading twenty object headers out of one pack
	// costs one fetch rather than twenty.
	CacheWritethrough
	// CacheMirror always loads the whole object from the primary backend and refreshes
	// the cache, using the cache only as a mirror rather than a source.
	CacheMirror
)

// NamespaceConfig describes how one namespace is laid out and cached.
type NamespaceConfig struct {
	// Levels is the nesting depths in use, ordered. More than one entry means the
	// store is migrating between depths, so a lookup has to probe each of them; the
	// last is where new objects go.
	Levels []int
	// Cache selects the caching mode for this namespace.
	Cache CacheMode
	// MaxSize bounds the cache for this namespace in bytes. Zero means unbounded.
	MaxSize int64
}

// Store is a namespaced, nesting object store over a Backend.
//
// Every method is serialised on one mutex, matching borgstore >= 0.6, which serialises
// all Store operations internally. That is what lets borg's PackWriter hand a pack to a
// background thread while the caller keeps working (docs/FORMAT.md §7.2) - the Store is
// shared, the ChunkIndex is not.
type Store struct {
	mu      sync.Mutex
	backend Backend

	// namespaces is sorted longest-prefix-first, so the most specific configuration
	// for a name wins.
	namespaces []namespaceEntry

	cache Backend

	// stats counts what the store has been asked to do; see stats.go.
	stats statsRecorder
}

type namespaceEntry struct {
	prefix string
	config NamespaceConfig
}

// New returns a Store over a backend.
//
// config maps a namespace prefix (borg writes them with a trailing slash, e.g.
// "packs/") to its layout. cache, when non-nil, is the backend the caching namespaces
// use; it may be shared between repositories, because the namespaces that enable
// caching name their objects by content hash.
func New(backend Backend, config map[string]NamespaceConfig, cache Backend) (*Store, error) {
	if backend == nil {
		return nil, errors.New("store: a backend is required")
	}
	if len(config) == 0 {
		return nil, errors.New("store: at least one namespace must be configured")
	}

	s := &Store{backend: backend, cache: cache}
	// The primary backend's objects are immutable and content-addressed, so it may keep
	// files open between reads; see PosixFS.SetReadCache. The cache backend may not - its
	// files are meant to appear and vanish underneath it, and a held descriptor would
	// serve bytes from a file that had already been evicted.
	if rc, ok := backend.(interface{ SetReadCache(bool) }); ok {
		rc.SetReadCache(true)
	}
	for prefix, ns := range config {
		if len(ns.Levels) == 0 {
			return nil, fmt.Errorf("store: namespace %q must configure at least one nesting level", prefix)
		}
		for _, l := range ns.Levels {
			if l < 0 {
				return nil, fmt.Errorf("store: namespace %q has a negative nesting level %d", prefix, l)
			}
		}
		s.namespaces = append(s.namespaces, namespaceEntry{prefix: prefix, config: ns})
	}
	// Longest prefix first: a configuration for "packs/" must win over one for "".
	sort.Slice(s.namespaces, func(i, j int) bool {
		return len(s.namespaces[i].prefix) > len(s.namespaces[j].prefix)
	})
	return s, nil
}

// Backend exposes the underlying backend, for operations that are not namespaced.
func (s *Store) Backend() Backend { return s.backend }

func (s *Store) configFor(name string) (NamespaceConfig, error) {
	for _, ns := range s.namespaces {
		if strings.HasPrefix(name, ns.prefix) {
			return ns.config, nil
		}
	}
	return NamespaceConfig{}, fmt.Errorf("store: no namespace is configured for %q", name)
}

// Create makes the store and precreates the nesting directories.
func (s *Store) Create() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.backend.Create(); err != nil {
		return err
	}
	return nil
}

// Destroy removes the store.
func (s *Store) Destroy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backend.Destroy()
}

// Open opens the store, and the cache backend if there is one.
func (s *Store) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.backend.Open(); err != nil {
		return err
	}
	if s.cache != nil {
		// A cache that cannot be opened is not fatal: it is an optimisation, and
		// failing the whole operation over it would be worse than running slowly.
		if err := s.cache.Open(); err != nil {
			s.cache = nil
		}
	}
	return nil
}

// Close closes the store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil {
		_ = s.cache.Close()
	}
	return s.backend.Close()
}

// find returns the nested name an object is stored under.
//
// With a single configured level the name is computed directly - no probe, which
// matters because find runs before every load, store and delete. With several levels
// the store is migrating between depths, so each is probed in order and the last is
// used when nothing is found, which is where a new object goes.
//
// The caller must hold s.mu.
func (s *Store) find(name string, deleted bool) (string, error) {
	cfg, err := s.configFor(name)
	if err != nil {
		return "", err
	}
	suffix := ""
	if deleted {
		suffix = DelSuffix
	}

	if len(cfg.Levels) == 1 {
		return Nest(name, cfg.Levels[0], suffix), nil
	}
	var nested string
	for _, level := range cfg.Levels {
		nested = Nest(name, level, suffix)
		info, err := s.backend.Info(nested)
		if err != nil {
			return "", err
		}
		if info.Exists {
			return nested, nil
		}
	}
	return nested, nil
}

// Find returns the nested name for a store name, without touching the object.
func (s *Store) Find(name string, deleted bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(name, deleted)
}

// Info reports on an object.
func (s *Store) Info(name string, deleted bool) (ItemInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.Info, started, 0) }()
	nested, err := s.find(name, deleted)
	if err != nil {
		return ItemInfo{}, err
	}
	return s.backend.Info(nested)
}

// Load reads an object, or a range of one. size < 0 means "to the end".
func (s *Store) Load(name string, offset, size int64, deleted bool) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	started := time.Now()
	var loaded int64
	defer func() { s.stats.observe(&s.stats.s.Load, started, loaded) }()

	cfg, err := s.configFor(name)
	if err != nil {
		return nil, err
	}
	nested, err := s.find(name, deleted)
	if err != nil {
		return nil, err
	}

	switch cfg.Cache {
	case CacheWritethrough:
		if s.cache != nil {
			s.stats.add(&s.stats.s.CacheLoadCalls, 1)
			if data, err := s.cache.Load(nested, offset, size); err == nil {
				s.stats.add(&s.stats.s.CacheHits, 1)
				s.stats.add(&s.stats.s.CacheLoadVolume, int64(len(data)))
				loaded = int64(len(data))
				return data, nil
			}
			// A cache miss, or a broken cache, is not an error: fall through to the
			// primary backend. The whole object is fetched and cached, so the next
			// range read of the same object is local. Counted as a miss either way -
			// borgstore counts errors separately only where it can tell them apart, and
			// here the miss and the broken read arrive as the same failure.
			s.stats.add(&s.stats.s.CacheMisses, 1)
		}
		full, err := s.loadFromBackend(nested, 0, -1)
		if err != nil {
			return nil, err
		}
		s.cacheStore(nested, full)
		out := sliceRange(full, offset, size)
		loaded = int64(len(out))
		return out, nil

	case CacheMirror:
		full, err := s.loadFromBackend(nested, 0, -1)
		if err != nil {
			return nil, err
		}
		s.cacheStore(nested, full)
		out := sliceRange(full, offset, size)
		loaded = int64(len(out))
		return out, nil

	default:
		data, err := s.loadFromBackend(nested, offset, size)
		loaded = int64(len(data))
		return data, err
	}
}

// loadFromBackend is every read that actually reaches storage, which is what separates the
// backend counters from the per-method ones.
func (s *Store) loadFromBackend(nested string, offset, size int64) ([]byte, error) {
	data, err := s.backend.Load(nested, offset, size)
	s.stats.add(&s.stats.s.BackendLoadCalls, 1)
	s.stats.add(&s.stats.s.BackendLoadVolume, int64(len(data)))
	return data, err
}

// Store writes an object.
func (s *Store) Store(name string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	started := time.Now()
	var stored int64
	defer func() { s.stats.observe(&s.stats.s.Store, started, stored) }()

	cfg, err := s.configFor(name)
	if err != nil {
		return err
	}
	// find here means an existing object keeps its nesting level and a new one goes to
	// the last configured level.
	nested, err := s.find(name, false)
	if err != nil {
		return err
	}
	if err := s.backend.Store(nested, value); err != nil {
		return err
	}
	s.stats.add(&s.stats.s.BackendStoreCalls, 1)
	s.stats.add(&s.stats.s.BackendStoreVolume, int64(len(value)))
	stored = int64(len(value))
	if cfg.Cache == CacheWritethrough || cfg.Cache == CacheMirror {
		s.cacheStore(nested, value)
	}
	return nil
}

// Delete removes an object permanently. For borg's undelete to work, use SoftDelete.
func (s *Store) Delete(name string, deleted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.Delete, started, 0) }()

	cfg, err := s.configFor(name)
	if err != nil {
		return err
	}
	nested, err := s.find(name, deleted)
	if err != nil {
		return err
	}
	if err := s.backend.Delete(nested); err != nil {
		return err
	}
	s.stats.add(&s.stats.s.BackendDeleteCalls, 1)
	if cfg.Cache == CacheWritethrough || cfg.Cache == CacheMirror {
		s.stats.add(&s.stats.s.CacheDeleteCalls, 1)
		_ = s.cache.Delete(nested) // best effort; a stale cache entry is corrected on the next store
	}
	return nil
}

// SoftDelete renames an object to <name>.del, so it can be undeleted.
//
// This is what borg's delete does by default and what makes borg undelete possible at
// all. A port that unlinked here would lose that with no error anywhere.
func (s *Store) SoftDelete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A move at the backend, and counted as one: borg's soft delete, undelete and
	// re-nesting are all renames, and borgstore tallies them under move.
	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.Move, started, 0) }()
	nested, err := s.find(name, false)
	if err != nil {
		return err
	}
	return s.moveLocked(name, nested, nested+DelSuffix)
}

// Undelete reverses SoftDelete.
func (s *Store) Undelete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A move at the backend, and counted as one: borg's soft delete, undelete and
	// re-nesting are all renames, and borgstore tallies them under move.
	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.Move, started, 0) }()
	nested, err := s.find(name, true)
	if err != nil {
		return err
	}
	return s.moveLocked(name, nested, strings.TrimSuffix(nested, DelSuffix))
}

// Move renames an object to another store name.
func (s *Store) Move(name, newName string, deleted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.Move, started, 0) }()
	nested, err := s.find(name, deleted)
	if err != nil {
		return err
	}
	newNested, err := s.find(newName, deleted)
	if err != nil {
		return err
	}
	return s.moveLocked(name, nested, newNested)
}

// ChangeLevel moves an object to the namespace's current (last) nesting level, which is
// how a store migrates between depths.
func (s *Store) ChangeLevel(name string, deleted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A move at the backend, and counted as one: borg's soft delete, undelete and
	// re-nesting are all renames, and borgstore tallies them under move.
	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.Move, started, 0) }()
	cfg, err := s.configFor(name)
	if err != nil {
		return err
	}
	nested, err := s.find(name, deleted)
	if err != nil {
		return err
	}
	suffix := ""
	if deleted {
		suffix = DelSuffix
	}
	target := Nest(name, cfg.Levels[len(cfg.Levels)-1], suffix)
	if target == nested {
		return nil
	}
	return s.moveLocked(name, nested, target)
}

// moveLocked performs a rename and keeps the cache in step. The caller must hold s.mu.
func (s *Store) moveLocked(name, from, to string) error {
	if err := s.backend.Move(from, to); err != nil {
		return err
	}
	cfg, err := s.configFor(name)
	if err == nil && s.cache != nil && (cfg.Cache == CacheWritethrough || cfg.Cache == CacheMirror) {
		_ = s.cache.Move(from, to)
	}
	return nil
}

// List walks a namespace and yields its objects, recursing into nesting directories.
//
// The reported Name is the **bare key**, without the namespace and without the nesting
// path - "0123...cdef", not "packs/01/0123...cdef". That is what borgstore yields and
// what borg's callers expect, since they use the name directly as an object id.
//
// deleted selects which half of the namespace is reported: with deleted false only live
// objects, with deleted true only soft-deleted ones, in both cases with the .del suffix
// removed from the reported name. The two sets are disjoint - an object is in exactly
// one of them - which is what lets borg list an archive directory and a recycle bin
// from the same namespace.
//
// fn returning false stops the walk.
func (s *Store) List(namespace string, deleted bool, fn func(ItemInfo) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// One call, however many directories the nesting makes it read: borgstore counts the
	// method the caller invoked, not the syscalls underneath it.
	started := time.Now()
	defer func() { s.stats.observe(&s.stats.s.List, started, 0) }()
	_, err := s.listLocked(namespace, namespace, deleted, fn)
	return err
}

// listLocked recurses. dir is the directory being read; namespace is the namespace the
// walk started from, used to un-nest the names it reports.
func (s *Store) listLocked(dir, namespace string, deleted bool, fn func(ItemInfo) bool) (bool, error) {
	entries, err := s.backend.List(dir)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			// A namespace directory that was never written to does not exist yet
			// (docs/FORMAT.md §1), and an empty listing is the right answer.
			return true, nil
		}
		return false, err
	}

	for _, info := range entries {
		child := info.Name
		if dir != "" {
			child = dir + "/" + info.Name
		}
		if info.Directory {
			// Only nesting directories are expected here; namespaces are not nested
			// inside one another.
			cont, err := s.listLocked(child, namespace, deleted, fn)
			if err != nil || !cont {
				return cont, err
			}
			continue
		}

		isDeleted := strings.HasSuffix(info.Name, DelSuffix)
		if isDeleted != deleted {
			continue
		}
		// The *bare key* is reported, not a namespaced or nested name: borgstore's
		// recursion yields each directory entry's own name, so both the namespace and
		// the nesting path fall away. Callers ported from borg rely on this - they use
		// the listed name directly as a hex object id.
		info.Name = strings.TrimSuffix(info.Name, DelSuffix)
		if !fn(info) {
			return false, nil
		}
	}
	return true, nil
}

// ListNames is List reduced to the names, collected into a slice.
func (s *Store) ListNames(namespace string, deleted bool) ([]string, error) {
	var names []string
	err := s.List(namespace, deleted, func(info ItemInfo) bool {
		names = append(names, info.Name)
		return true
	})
	return names, err
}

// cacheStore writes to the cache, ignoring failures. The caller must hold s.mu.
func (s *Store) cacheStore(nested string, value []byte) {
	if s.cache == nil {
		return
	}
	// A cache write failure is deliberately silent: the object is already in the
	// primary backend, so the only consequence is a slower read later. It is counted,
	// though, because a cache that fails every write is a cache that is costing time and
	// returning nothing, and the count is the only sign of it.
	s.stats.add(&s.stats.s.CacheStoreCalls, 1)
	if err := s.cache.Store(nested, value); err != nil {
		s.stats.add(&s.stats.s.CacheErrors, 1)
		return
	}
	s.stats.add(&s.stats.s.CacheStoreVolume, int64(len(value)))
}

// sliceRange applies an offset and size to an already-loaded object, matching what a
// backend range read would have returned.
func sliceRange(full []byte, offset, size int64) []byte {
	if offset < 0 {
		offset = int64(len(full)) + offset
		if offset < 0 {
			offset = 0
		}
	}
	if offset > int64(len(full)) {
		return nil
	}
	out := full[offset:]
	if size >= 0 && size < int64(len(out)) {
		out = out[:size]
	}
	return out
}
