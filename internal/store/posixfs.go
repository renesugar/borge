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
	"strings"
)

// PosixFS stores objects as files under a base directory.
//
// This is the backend every local repository uses, and the only one borge implements
// so far - sftp, rest, s3 and rclone are stage 8 (docs/PORTING_PLAN.md §5).
type PosixFS struct {
	basePath string
	opened   bool

	// doFsync trades durability for speed. borgstore defaults it off and measures the
	// difference at 26x, so it is off here too; borg's own consistency story rests on
	// the temp-file-and-rename below plus content-addressed names, not on fsync.
	doFsync bool

	permissions Permissions
}

// NewPosixFS returns a backend rooted at an absolute path.
func NewPosixFS(path string, permissions Permissions) (*PosixFS, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("store: path must be absolute: %q", path)
	}
	return &PosixFS{basePath: filepath.Clean(path), permissions: permissions}, nil
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

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ObjectNotFoundError{Name: name}
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	defer f.Close()

	if offset != 0 {
		whence := io.SeekStart
		if offset < 0 {
			// A negative offset counts back from the end, matching borgstore.
			whence = io.SeekEnd
		}
		if _, err := f.Seek(offset, whence); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	if size < 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		return data, nil
	}

	data := make([]byte, size)
	n, err := io.ReadFull(f, data)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("store: %w", err)
	}
	// A short read is not an error: the pack reader asks for a header-sized slice at
	// the end of a pack and uses the short result as its signal for a clean EOF.
	return data[:n], nil
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

// ParseFileURL turns a file:// URL into a local path.
//
// Only local absolute paths are supported: the general URL syntax is proto://host/path,
// the empty host means localhost, and the third slash is both the separator and the
// start of the absolute path.
func ParseFileURL(url string) (string, error) {
	const prefix = "file://"
	if !strings.HasPrefix(url, prefix) {
		return "", fmt.Errorf("store: not a file:// URL: %q", url)
	}
	path := strings.TrimPrefix(url, prefix)
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("store: file:// URL must have an empty host and an absolute path: %q", url)
	}
	return path, nil
}
