// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the Repository class in borg's src/borg/repository.py,
// split per borg issue #10017 (the pack machinery lives in pack.go and the index
// persistence in index.go).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package repository is the borg 2 repository: an object store with a chunk index over
// it, plus the pack machinery that keeps small objects out of individual files.
//
// It is deliberately split into three files rather than one, which is borg issue #10017
// ("split the god modules"): repository.go is the lifecycle and the object API, pack.go
// is the pack writer and reader, index.go is the chunk index persistence.
package repository

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/store"
)

// Version is the repository format version borge reads and writes. borg 2's
// acceptable_repo_versions is (4,).
const Version = 4

// RepositoryReadme is the exact content of config/readme.
//
// borg compares this for **string equality** when opening a repository and raises
// InvalidRepository on any difference. borge must therefore write borg's text verbatim -
// it cannot write a "borge repository" readme without making the repository unreadable
// by borg, which would defeat the whole interoperability gate.
const RepositoryReadme = "This is a Borg Backup repository.\nSee https://borgbackup.readthedocs.io/\n"

// Errors.
var (
	// ErrObjectNotFound means the chunk id is not in the index.
	ErrObjectNotFound = errors.New("repository: object not found")
	// ErrInvalidRepository means the directory is not a borg repository, or is a
	// version borge does not read.
	ErrInvalidRepository = errors.New("repository: invalid repository")
	// ErrPackLocationUnknown means a chunk is buffered but not yet flushed. It is a
	// caller bug - a wrong flush ordering - not a missing object.
	ErrPackLocationUnknown = errors.New("repository: chunk has no pack location yet")
)

// ObjectNotFoundError names the missing chunk.
type ObjectNotFoundError struct{ ID []byte }

func (e *ObjectNotFoundError) Error() string {
	return fmt.Sprintf("repository: object not found: %s", hex.EncodeToString(e.ID))
}
func (e *ObjectNotFoundError) Unwrap() error { return ErrObjectNotFound }

// NamespaceConfig is borg's repository layout (src/borg/repository.py:684-692).
//
// packs/ is the only nested namespace. cacheURL, when non-empty, turns on the
// writethrough pack cache for it.
func NamespaceConfig(packCache bool, packCacheSize int64) map[string]store.NamespaceConfig {
	packs := store.NamespaceConfig{Levels: []int{1}}
	if packCache {
		packs.Cache = store.CacheWritethrough
		packs.MaxSize = packCacheSize
	}
	return map[string]store.NamespaceConfig{
		"archives/": {Levels: []int{0}},
		"cache/":    {Levels: []int{0}},
		"config/":   {Levels: []int{0}},
		"index/":    {Levels: []int{0}},
		"keys/":     {Levels: []int{0}},
		"locks/":    {Levels: []int{0}},
		"packs/":    packs,
	}
}

// Options configure a Repository.
type Options struct {
	// Exclusive takes an exclusive lock rather than a shared one.
	Exclusive bool
	// NoLock skips locking entirely. Only for read-only inspection of a repository
	// nobody else is using.
	NoLock bool
	// PackMaxCount and PackMaxSize override the pack sizing, matching
	// BORG_PACK_MAX_COUNT and BORG_PACK_MAX_SIZE.
	PackMaxCount int
	PackMaxSize  int
	// PackAsync hands full packs to a background goroutine. Default on; the
	// BORG_PACK_ASYNC=no debugging switch turns it off.
	PackAsync *bool
	// Cache is the backend the writethrough pack cache uses, or nil.
	Cache store.Backend
	// CacheMaxSize bounds it, in bytes. Zero is unbounded.
	CacheMaxSize int64
}

// Repository is an opened borg repository.
type Repository struct {
	store   *store.Store
	loc     *location.Location
	id      []byte
	version int

	opened     bool
	packWriter *PackWriter
	packCache  *packCache

	// chunks is built lazily on first use and persisted at Close. It is the single
	// owner of the in-memory index: reads resolve through it, the pack writer updates
	// it, and callers must go through .Chunks() rather than keeping their own.
	chunks *hashindex.ChunkIndex

	lock *Lock
	opts Options
}

// Create makes a new repository at a location.
//
// The creation order matters, and the last step is a performance measure rather than a
// correctness one: writing an empty chunk index means the first operation does not have
// to rebuild it by listing every packs/ subdirectory.
func Create(loc *location.Location, opts Options) (*Repository, error) {
	backend, err := store.NewBackend(loc)
	if err != nil {
		return nil, err
	}
	s, err := store.New(backend, NamespaceConfig(opts.Cache != nil, opts.CacheMaxSize), opts.Cache)
	if err != nil {
		return nil, err
	}
	if err := s.Create(); err != nil {
		return nil, err
	}
	if err := s.Open(); err != nil {
		return nil, err
	}
	defer s.Close()

	id := make([]byte, 32)
	if _, err := randRead(id); err != nil {
		return nil, err
	}

	if err := s.Store("config/readme", []byte(RepositoryReadme)); err != nil {
		return nil, err
	}
	if err := s.Store("config/version", []byte(strconv.Itoa(Version))); err != nil {
		return nil, err
	}
	if err := s.Store("config/id", []byte(hex.EncodeToString(id))); err != nil {
		return nil, err
	}

	empty, err := hashindex.NewChunkIndex(0)
	if err != nil {
		return nil, err
	}
	if _, err := WriteChunkIndex(s, empty, WriteIndexOptions{ForceWrite: true}); err != nil {
		return nil, err
	}
	return Open(loc, opts)
}

// Destroy removes a repository through its backend.
//
// This is for the locations that are not directories. A local repository is destroyed by
// the CLI instead, which removes borge's own namespaces and leaves anything else in the
// directory alone (DIVERGENCES #18) - a distinction that needs a filesystem, because the
// Backend interface has no "remove this subtree" that stops at foreign files. Through a
// backend, destroying is all or nothing, which is what borg does everywhere.
func Destroy(loc *location.Location) error {
	backend, err := store.NewBackend(loc)
	if err != nil {
		return err
	}
	return backend.Destroy()
}

// Open opens an existing repository.
func Open(loc *location.Location, opts Options) (*Repository, error) {
	backend, err := store.NewBackend(loc)
	if err != nil {
		return nil, err
	}
	s, err := store.New(backend, NamespaceConfig(opts.Cache != nil, opts.CacheMaxSize), opts.Cache)
	if err != nil {
		return nil, err
	}
	if err := s.Open(); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRepository, err)
	}

	r := &Repository{store: s, loc: loc, opts: opts, packCache: newPackCache(3)}

	readme, err := s.Load("config/readme", 0, -1, false)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%w: no config/readme at %s", ErrInvalidRepository, loc)
	}
	if string(readme) != RepositoryReadme {
		s.Close()
		return nil, fmt.Errorf("%w: config/readme does not match borg's; this is not a borg repository",
			ErrInvalidRepository)
	}

	versionBytes, err := s.Load("config/version", 0, -1, false)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%w: no config/version", ErrInvalidRepository)
	}
	version, err := strconv.Atoi(strings.TrimSpace(string(versionBytes)))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%w: config/version is %q", ErrInvalidRepository, versionBytes)
	}
	if version != Version {
		s.Close()
		return nil, fmt.Errorf("%w: repository version %d is not supported (borge reads version %d)",
			ErrInvalidRepository, version, Version)
	}
	r.version = version

	idHex, err := s.Load("config/id", 0, -1, false)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%w: no config/id", ErrInvalidRepository)
	}
	r.id, err = hex.DecodeString(strings.TrimSpace(string(idHex)))
	if err != nil || len(r.id) != 32 {
		s.Close()
		return nil, fmt.Errorf("%w: config/id is not 32 hex-encoded bytes", ErrInvalidRepository)
	}

	// Lock only after establishing that there is a supported repository here, so a
	// wrong path does not leave a lock behind.
	if !opts.NoLock {
		lock, err := NewLock(s, opts.Exclusive)
		if err != nil {
			s.Close()
			return nil, err
		}
		if err := lock.Acquire(); err != nil {
			s.Close()
			return nil, err
		}
		r.lock = lock
	}

	maxCount, maxSize := packSizing(opts)
	async := true
	if opts.PackAsync != nil {
		async = *opts.PackAsync
	} else if v, ok := lookupEnv("PACK_ASYNC"); ok && v == "no" {
		async = false
	}

	chunks, err := r.Chunks()
	if err != nil {
		r.releaseLock()
		s.Close()
		return nil, err
	}
	pw, err := NewPackWriter(s, chunks, maxCount, maxSize, async)
	if err != nil {
		r.releaseLock()
		s.Close()
		return nil, err
	}
	r.packWriter = pw
	r.opened = true
	return r, nil
}

// packSizing resolves the pack limits.
//
// Setting a count limit switches off the default size bound unless a size is given too,
// matching borg.
func packSizing(opts Options) (maxCount, maxSize int) {
	maxCount = opts.PackMaxCount
	if maxCount == 0 {
		if v, ok := lookupEnv("PACK_MAX_COUNT"); ok {
			if n, err := strconv.Atoi(v); err == nil {
				maxCount = n
			}
		}
	}
	maxSize = opts.PackMaxSize
	if maxSize == 0 {
		if v, ok := lookupEnv("PACK_MAX_SIZE"); ok {
			if n, err := strconv.Atoi(v); err == nil {
				maxSize = n
			}
		}
	}
	if maxSize == 0 && maxCount == 0 {
		maxSize = DefaultPackMaxSize
	}
	return maxCount, maxSize
}

// lookupEnv reads BORGE_<name>, falling back to BORG_<name> (docs/PORTING_PLAN.md §0.5).
func lookupEnv(name string) (string, bool) {
	if v, ok := os.LookupEnv("BORGE_" + name); ok {
		return v, true
	}
	return os.LookupEnv("BORG_" + name)
}

// ID is the repository's 32-byte identity.
func (r *Repository) ID() []byte { return r.id }

// IDString is the repository id in hex.
func (r *Repository) IDString() string { return hex.EncodeToString(r.id) }

// Version is the repository format version.
func (r *Repository) Version() int { return r.version }

// Location is where this repository lives.
//
// It prints as borg's canonical path, with any credentials removed, so a caller that wants
// to show the user which repository it is working on can print this directly.
func (r *Repository) Location() *location.Location { return r.loc }

// Store exposes the underlying object store, for the namespaces the repository does not
// wrap (archives/, keys/, config/manifest).
func (r *Repository) Store() *store.Store { return r.store }

// Chunks returns the chunk index, building it on first use.
func (r *Repository) Chunks() (*hashindex.ChunkIndex, error) {
	if r.chunks == nil {
		chunks, err := BuildChunkIndex(r.store, false)
		if err != nil {
			return nil, err
		}
		r.chunks = chunks
	}
	return r.chunks, nil
}

// IsChunkIndexLoaded reports whether the index has been built this session, without
// building it.
func (r *Repository) IsChunkIndexLoaded() bool { return r.chunks != nil }

// InvalidateChunkIndex drops the in-memory index so Close does not persist a stale copy
// and the next access rebuilds from the repository's actual contents.
func (r *Repository) InvalidateChunkIndex() { r.chunks = nil }

// Put stores a repository object.
//
// The chunk is buffered in the pack writer. When it fills a pack, the results of the
// previously written pack are returned; otherwise nil. See PackWriter for why the
// results are one pack behind.
func (r *Repository) Put(id, data []byte) ([]PackResult, error) {
	if !r.opened {
		return nil, errors.New("repository: not open")
	}
	if len(data) > repoobj.MaxDataSize {
		return nil, fmt.Errorf("repository: object is %d bytes, over the %d maximum",
			len(data), repoobj.MaxDataSize)
	}
	return r.packWriter.Add(id, data)
}

// Get returns a repository object by chunk id.
func (r *Repository) Get(id []byte) ([]byte, error) {
	if !r.opened {
		return nil, errors.New("repository: not open")
	}
	chunks, err := r.Chunks()
	if err != nil {
		return nil, err
	}
	entry, ok := chunks.Get(id)
	if !ok {
		return nil, &ObjectNotFoundError{ID: append([]byte(nil), id...)}
	}

	if chunks.IsPending(id) {
		// The chunk may be in a pack whose background store is still in flight. Joining
		// it resolves the location, and acts as a read barrier.
		if _, err := r.packWriter.JoinInflight(); err != nil {
			return nil, err
		}
		entry, ok = chunks.Get(id)
	}
	if !ok || chunks.IsPending(id) {
		// Still pending means buffered but not flushed. A chunk must be flushed before
		// any read, so this is a caller bug rather than a missing object, and it is
		// reported as such.
		return nil, fmt.Errorf("%w: %s", ErrPackLocationUnknown, hex.EncodeToString(id))
	}

	reader := r.packCache.get(entry.PackID[:])
	if reader == nil {
		reader = NewPackReader(r.store, entry.PackID[:])
	}
	data, err := reader.Read(int(entry.ObjOffset), int(entry.ObjSize))
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil, &ObjectNotFoundError{ID: append([]byte(nil), id...)}
		}
		return nil, err
	}
	if len(data) != int(entry.ObjSize) {
		return nil, fmt.Errorf("repository: object %s is %d bytes in the pack, index says %d",
			hex.EncodeToString(id), len(data), entry.ObjSize)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// GetMany reads several objects, caching whole packs so a run of reads from one pack
// costs a single fetch. That is what makes a restore that is sorted by pack cheap.
func (r *Repository) GetMany(ids [][]byte, fn func(id, data []byte) bool) error {
	chunks, err := r.Chunks()
	if err != nil {
		return err
	}
	for _, id := range ids {
		entry, ok := chunks.Get(id)
		if !ok || chunks.IsPending(id) {
			data, err := r.Get(id)
			if err != nil {
				return err
			}
			if !fn(id, data) {
				return nil
			}
			continue
		}

		reader := r.packCache.get(entry.PackID[:])
		if reader == nil {
			name := PackName(entry.PackID[:])
			contents, err := r.store.Load(name, 0, -1, false)
			if err != nil {
				return err
			}
			reader = NewPackReaderFromBytes(entry.PackID[:], contents)
			r.packCache.put(entry.PackID[:], reader)
		}
		data, err := reader.Read(int(entry.ObjOffset), int(entry.ObjSize))
		if err != nil {
			return err
		}
		out := make([]byte, len(data))
		copy(out, data)
		if !fn(id, out) {
			return nil
		}
	}
	return nil
}

// List returns up to limit chunk ids and their stored sizes, starting after marker.
//
// The ids come from the chunk index, not from listing packs/ - a packs/ listing yields
// pack ids, which are not chunk ids. Chunks still buffered in the pack writer are
// skipped, because they cannot be read yet.
func (r *Repository) List(limit int, marker []byte) ([]PackEntry, error) {
	chunks, err := r.Chunks()
	if err != nil {
		return nil, err
	}
	collect := marker == nil
	var out []PackEntry
	chunks.Iterate(func(id []byte, e hashindex.Entry) bool {
		if e.Flags&hashindex.FPending != 0 {
			return true
		}
		if collect {
			out = append(out, PackEntry{
				ChunkID: append([]byte(nil), id...),
				Size:    int(e.ObjSize),
			})
			return limit <= 0 || len(out) < limit
		}
		if string(id) == string(marker) {
			collect = true // start after the marker, not including it
		}
		return true
	})
	return out, nil
}

// Flush writes any buffered chunks.
func (r *Repository) Flush() error {
	if r.packWriter == nil {
		return nil
	}
	_, err := r.packWriter.Flush()
	return err
}

// Close flushes, persists the chunk index, releases the lock and closes the store.
func (r *Repository) Close() error {
	if !r.opened {
		return nil
	}
	r.opened = false

	var firstErr error
	if r.packWriter != nil {
		// Normally a no-op: Flush is a barrier and runs first. When Close runs while
		// unwinding an error a store may still be in flight, so join it - a stored pack
		// gets recorded and a failed one gets its index entries dropped.
		if _, err := r.packWriter.JoinInflight(); err != nil {
			// Do not return yet: we are closing, probably already unwinding, and the
			// index still needs persisting.
			firstErr = err
		}
		if n := r.packWriter.Buffered(); n > 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("repository: %d chunk(s) were still buffered at close; "+
					"call Flush before Close", n)
			}
		}
	}

	// Persist only what this session added, and only if the index was actually loaded -
	// building it here just to write it back would be pointless work.
	if r.chunks != nil {
		if _, err := WriteChunkIndex(r.store, r.chunks, WriteIndexOptions{Incremental: true}); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	r.releaseLock()
	r.packCache.clear()
	if err := r.store.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ListLocks reports the locks currently held on this repository, without removing any.
// Open the repository with NoLock to inspect locks you do not want to compete with.
func (r *Repository) ListLocks() ([]HeldLock, error) {
	return ListLocks(r.store)
}

// BreakLock removes every lock on this repository, including any this process holds.
//
// It is the escape hatch for a lock left by a killed client, and it is unconditionally
// destructive: if another client really is running, this invites two writers into one
// repository. The caller decides; see cmdBreakLock.
func (r *Repository) BreakLock() error {
	if err := BreakLock(r.store); err != nil {
		return err
	}
	// Our own lock object is gone now, so releasing it on Close would look for something
	// that is not there. Forgetting it here keeps Close quiet.
	r.lock = nil
	return nil
}

// RefreshLock rewrites the lock's timestamp, so a long operation is not declared stale
// underneath itself.
//
// A repository opened without a lock has nothing to refresh, which is not an error: the
// caller asked for no lock and got none.
func (r *Repository) RefreshLock() error {
	if r.lock == nil {
		return nil
	}
	return r.lock.Refresh()
}

func (r *Repository) releaseLock() {
	if r.lock != nil {
		// Ignore a missing lock: Close also runs while unwinding an error, and if the
		// lock is already gone, reporting that would mask the original failure.
		_ = r.lock.Release(true)
		r.lock = nil
	}
}

// Manifest reads the manifest object.
func (r *Repository) Manifest() ([]byte, error) {
	return r.store.Load("config/manifest", 0, -1, false)
}

// PutManifest writes the manifest object.
func (r *Repository) PutManifest(data []byte) error {
	return r.store.Store("config/manifest", data)
}

// WriteFullChunkIndex persists the whole index, replacing every existing fragment. It is
// what a delete or a repair needs, since an incremental write cannot express a removal.
func (r *Repository) WriteFullChunkIndex() error {
	chunks, err := r.Chunks()
	if err != nil {
		return err
	}
	_, err = WriteChunkIndex(r.store, chunks, WriteIndexOptions{DeleteOther: true, ForceWrite: true})
	return err
}
