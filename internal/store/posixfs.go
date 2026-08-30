// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/backends/posixfs.py (borgstore 0.6.1,
// BSD 3-Clause, Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// PosixFS stores objects as files under a base directory.
//
// This is the backend every local repository uses, and the only one borge implements
// so far - sftp, rest, s3 and rclone are stage 8 (plans/PORTING_PLAN.md §5).
type PosixFS struct {
	basePath string
	opened   bool

	// doFsync trades durability for speed. borgstore defaults it off and measures the
	// difference at 26x, so it is off here too; borg's own consistency story rests on
	// the temp-file-and-rename below plus content-addressed names, not on fsync.
	doFsync bool

	permissions Permissions

	// readCache enables the open-handle cache; see SetReadCache.
	readCache bool
	// handles keeps recently read files open. See the note on Load.
	handles   map[string]*os.File
	handleAge []string
	handleMu  sync.Mutex
}

// maxOpenHandles bounds the read cache.
//
// A repository has few packs and an extract reads them over and over, so a handful of
// handles catches nearly everything; the bound is what keeps a listing of ten thousand
// objects from holding ten thousand descriptors. Evicted handles are closed.
const maxOpenHandles = 16

// NewPosixFS returns a backend rooted at an absolute path.
func NewPosixFS(path string, permissions Permissions) (*PosixFS, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("store: path must be absolute: %q", path)
	}
	return &PosixFS{
		basePath:    filepath.Clean(path),
		permissions: permissions,
		handles:     map[string]*os.File{},
	}, nil
}

// SetReadCache turns the open-handle cache on. Off by default; see openForRead.
//
// The caller is asserting that objects in this store are immutable and are removed only
// through Delete and Move. That is true of a repository's own backend - objects are
// content-addressed and written by rename - and false of the local writethrough cache,
// whose whole job is to have files appear and vanish underneath it. Store.New turns it on
// for the first and not the second.
func (b *PosixFS) SetReadCache(on bool) {
	if !on {
		b.closeHandles()
	}
	b.readCache = on
}

// SetFsync turns fsync-before-rename on. See the doFsync field.
func (b *PosixFS) SetFsync(on bool) { b.doFsync = on }

// BasePath is the directory this backend is rooted at.
func (b *PosixFS) BasePath() string { return b.basePath }

// resolve validates a store name and turns it into a filesystem path.
//
// The validation is what keeps a name from escaping basePath; filepath.Join alone
// would happily resolve "../.." for a caller.
func (b *PosixFS) resolve(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	return filepath.Join(b.basePath, filepath.FromSlash(name)), nil
}

func (b *PosixFS) requireOpen() error {
	if !b.opened {
		return ErrMustBeOpen
	}
	return nil
}

// Create makes the store directory.
//
// An existing *empty* directory is accepted, and missing parents are created: some
// repository hosts only give access through borg, so requiring the user to create
// parents out of band would make those unusable. A non-empty directory is refused,
// so a repository cannot be created on top of unrelated files.
func (b *PosixFS) Create() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	if err := b.permissions.check("", "wW"); err != nil {
		return err
	}
	if err := os.MkdirAll(b.basePath, 0o700); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	entries, err := os.ReadDir(b.basePath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s is not empty", ErrAlreadyExists, b.basePath)
	}
	return nil
}

// Destroy removes the store and everything below it.
func (b *PosixFS) Destroy() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	if err := b.permissions.check("", "D"); err != nil {
		return err
	}
	if _, err := os.Stat(b.basePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrDoesNotExist, b.basePath)
		}
		return fmt.Errorf("store: %w", err)
	}
	// The contents go; the base directory itself may stay, because Create accepts an
	// existing empty directory and may not have made this one.
	entries, err := os.ReadDir(b.basePath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(b.basePath, e.Name())); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// Open marks the backend usable.
func (b *PosixFS) Open() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	info, err := os.Stat(b.basePath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrDoesNotExist, b.basePath)
	}
	b.opened = true
	return nil
}

// Close marks the backend unusable.
func (b *PosixFS) Close() error {
	if !b.opened {
		return ErrMustBeOpen
	}
	b.opened = false
	b.closeHandles()
	return nil
}

// Load reads an object, or a range of one.
func (b *PosixFS) Load(name string, offset, size int64) ([]byte, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	path, err := b.resolve(name)
	if err != nil {
		return nil, err
	}
	if err := b.permissions.check(name, "r"); err != nil {
		return nil, err
	}

	f, err := b.openForRead(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ObjectNotFoundError{Name: name}
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	if !b.readCache {
		// Not cached, so this handle is ours to close.
		defer f.Close()
	}

	start := offset
	if offset < 0 {
		// A negative offset counts back from the end, matching borgstore.
		info, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		start = info.Size() + offset
		if start < 0 {
			return nil, fmt.Errorf("store: offset %d is before the start of %s", offset, name)
		}
	}
	if size < 0 {
		data, err := readAllFrom(f, start)
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		return data, nil
	}

	data := make([]byte, size)
	n, err := readFullAt(f, data, start)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("store: %w", err)
	}
	// A short read is not an error: the pack reader asks for a header-sized slice at
	// the end of a pack and uses the short result as its signal for a clean EOF.
	return data[:n], nil
}

// openForRead returns a cached handle for a path, opening it if necessary.
//
// # Why this cache exists
//
// Load opened and closed the file on every object read. An extract of the corpus in
// §12.1b read 118,866 chunks out of a handful of packs, so the same few files were opened
// 118,866 times - each costing an openat, four fcntls as Go's runtime registers and
// deregisters the descriptor with its poller, and a close. `strace -c` counted three
// openat per restored file where one is needed.
//
// It is also evidence for ROADMAP R0.1, which proposes sorting a restore by
// (pack_id, obj_offset) so each pack is read once. Part of what that would buy is
// available without touching the format: keep the pack open.
//
// # Why this is safe
//
// Reads go through ReadAt, so a shared handle carries no file position and two readers
// cannot disturb each other. Objects are immutable once written - Store creates a
// temporary file and renames it into place - so a cached descriptor for a name that is
// replaced still refers to the bytes the reader asked for, which is the old file rather
// than a torn new one. The names that *can* go away are dropped from the cache by Delete
// and Move, and Close drops all of them.
//
// Every path that changes what a name refers to drops the handle first: Store before its
// rename, Delete before its unlink, Move before both, and Close for all of them. That list
// is exhaustive by construction rather than by argument - the first version of this reasoned
// about which names could be rewritten, concluded a cached descriptor was harmless because
// it would serve "the old file rather than a torn new one", and
// TestReadCacheSeesAReplacedObject failed on exactly that.
func (b *PosixFS) openForRead(path string) (*os.File, error) {
	if !b.readCache {
		return os.Open(path)
	}
	b.handleMu.Lock()
	defer b.handleMu.Unlock()
	if f, ok := b.handles[path]; ok {
		return f, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if len(b.handleAge) >= maxOpenHandles {
		oldest := b.handleAge[0]
		b.handleAge = b.handleAge[1:]
		if old, ok := b.handles[oldest]; ok {
			old.Close()
			delete(b.handles, oldest)
		}
	}
	b.handles[path] = f
	b.handleAge = append(b.handleAge, path)
	return f, nil
}

// forgetHandle closes and drops a cached handle, for a name that is being removed or
// renamed. Called with the path, not the object name.
func (b *PosixFS) forgetHandle(path string) {
	b.handleMu.Lock()
	defer b.handleMu.Unlock()
	f, ok := b.handles[path]
	if !ok {
		return
	}
	f.Close()
	delete(b.handles, path)
	for i, p := range b.handleAge {
		if p == path {
			b.handleAge = append(b.handleAge[:i], b.handleAge[i+1:]...)
			break
		}
	}
}

// closeHandles drops every cached handle.
func (b *PosixFS) closeHandles() {
	b.handleMu.Lock()
	defer b.handleMu.Unlock()
	for _, f := range b.handles {
		f.Close()
	}
	b.handles = map[string]*os.File{}
	b.handleAge = nil
}

// readFullAt fills buf from offset, reporting how much it got.
func readFullAt(f *os.File, buf []byte, offset int64) (int, error) {
	n, err := f.ReadAt(buf, offset)
	if errors.Is(err, io.EOF) && n > 0 {
		// A short read at the end of a file is not an error to the caller; see Load.
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// readAllFrom reads to the end of the file from offset.
func readAllFrom(f *os.File, offset int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size() - offset
	if size <= 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// Store writes an object.
//
// The write goes to a temporary file in the same directory and is then renamed into
// place, so a reader never sees a partially written object and an interrupted write
// leaves the previous version intact. The temp name carries TmpSuffix, which
// validateName rejects, so a leftover temp file is skipped by every listing rather
// than being mistaken for an object.
func (b *PosixFS) Store(name string, value []byte) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	path, err := b.resolve(name)
	if err != nil {
		return err
	}
	overwrite := fileExists(path)
	required := "wW"
	if overwrite {
		required = "W"
	}
	if err := b.permissions.check(name, required); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmpPath, err := b.writeTemp(dir, value)
	if errors.Is(err, os.ErrNotExist) {
		// The directory was not there. Retry once after creating it, rather than
		// creating it every time: on a network filesystem the extra round trip per
		// object is expensive and almost always wasted.
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return fmt.Errorf("store: %w", mkErr)
		}
		tmpPath, err = b.writeTemp(dir, value)
	}
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	// The rename swaps the inode, and a cached descriptor would go on serving the old
	// one. Invalidating here rather than reasoning about which names can be rewritten:
	// TestReadCacheSeesAReplacedObject failed when this was left to reasoning.
	b.forgetHandle(path)
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func (b *PosixFS) writeTemp(dir string, value []byte) (string, error) {
	f, err := os.CreateTemp(dir, "*"+TmpSuffix)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(value); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if b.doFsync {
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(name)
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// Delete removes an object permanently.
func (b *PosixFS) Delete(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	path, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := b.permissions.check(name, "D"); err != nil {
		return err
	}
	// Before the unlink, so no reader can pick the handle up in between.
	b.forgetHandle(path)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ObjectNotFoundError{Name: name}
		}
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Move renames an object.
//
// It needs D on the source: the object disappears from its old name, which to anything
// looking for it is indistinguishable from deletion.
func (b *PosixFS) Move(oldName, newName string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	oldPath, err := b.resolve(oldName)
	if err != nil {
		return err
	}
	newPath, err := b.resolve(newName)
	if err != nil {
		return err
	}
	if err := b.permissions.check(oldName, "D"); err != nil {
		return err
	}
	required := "wW"
	if fileExists(newPath) {
		required = "W"
	}
	if err := b.permissions.check(newName, required); err != nil {
		return err
	}

	// Both names change identity here: the source goes away and the target is replaced.
	b.forgetHandle(oldPath)
	b.forgetHandle(newPath)
	if err := os.Rename(oldPath, newPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: %w", err)
		}
		// As in Store: the destination directory may be missing. Retry once with it
		// created, and only then decide the source is genuinely absent.
		if mkErr := os.MkdirAll(filepath.Dir(newPath), 0o700); mkErr != nil {
			return fmt.Errorf("store: %w", mkErr)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &ObjectNotFoundError{Name: oldName}
			}
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// Info reports on a name without reading it. A missing name is not an error; the
// result simply has Exists false.
func (b *PosixFS) Info(name string) (ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return ItemInfo{}, err
	}
	path, err := b.resolve(name)
	if err != nil {
		return ItemInfo{}, err
	}
	// No content is read, so listing permission is enough.
	if err := b.permissions.check(name, "lr"); err != nil {
		return ItemInfo{}, err
	}

	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ItemInfo{Name: filepath.Base(path)}, nil
		}
		return ItemInfo{}, fmt.Errorf("store: %w", err)
	}
	return ItemInfo{
		Name:      filepath.Base(path),
		Exists:    true,
		Directory: st.IsDir(),
		Size:      st.Size(),
		MTime:     st.ModTime(),
	}, nil
}

// List reports one directory's entries, sorted by name.
//
// Names that fail validation are skipped rather than reported: the directory may hold
// a leftover temp file or something that never came from borge, and a listing is not
// the place to fail over it.
func (b *PosixFS) List(name string) ([]ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	path, err := b.resolve(name)
	if err != nil {
		return nil, err
	}
	if err := b.permissions.check(name, "l"); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ObjectNotFoundError{Name: name}
		}
		return nil, fmt.Errorf("store: %w", err)
	}

	out := make([]ItemInfo, 0, len(entries))
	for _, e := range entries {
		if validateName(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// It vanished between the listing and the stat: another process is working
			// in the same store, which is expected rather than exceptional.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("store: %w", err)
		}
		out = append(out, ItemInfo{
			Name:      e.Name(),
			Exists:    true,
			Directory: info.IsDir(),
			Size:      info.Size(),
			MTime:     info.ModTime(),
		})
	}
	// borgstore sorts, and Store.List's ordering guarantee depends on it: with every
	// object at the same nesting level, a sorted directory walk is a sorted key list.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Mkdir creates a directory and its parents.
func (b *PosixFS) Mkdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	path, err := b.resolve(name)
	if err != nil {
		return err
	}
	// Write permission, not just listing: creating unbounded empty directories is a
	// denial-of-service against the store.
	if err := b.permissions.check(name, "w"); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Rmdir removes an empty directory.
func (b *PosixFS) Rmdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	path, err := b.resolve(name)
	if err != nil {
		return err
	}
	// Only empty directories can be removed, so no data can be lost; write permission
	// is enough, and delete permission also allows it.
	if err := b.permissions.check(name, "wD"); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ObjectNotFoundError{Name: name}
		}
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
