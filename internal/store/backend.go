// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/backends/_base.py and
// borgstore/backends/errors.py (borgstore 0.6.1, BSD 3-Clause, Copyright (C) 2026
// Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package store implements borg's object store: the layer a borg 2 repository is built
// on.
//
// A repository is a set of namespaces (archives/, packs/, index/, keys/, config/,
// locks/, cache/) holding one object per file. This package knows nothing about what
// the objects contain - that is repoobj's job - only how to put bytes under a name,
// get them back, list them, and move them.
//
// # Nesting
//
// Namespaces that can hold very many objects are nested: packs/ uses one level, so an
// object lands at packs/<xx>/<full key> rather than packs/<full key>. Filesystems cope
// badly with millions of entries in one directory, which is the same problem borge is
// trying to solve on the restore side.
//
// # Soft deletion
//
// Deleting is a rename to <name>.del, not an unlink. That is what makes borg's
// undelete work, and it means a listing has to filter by suffix rather than assume
// everything it sees is live.
package store

import (
	"errors"
	"fmt"
	"time"
)

// Errors the backends report. Callers distinguish them with errors.Is.
var (
	// ErrObjectNotFound means the named object does not exist.
	ErrObjectNotFound = errors.New("store: object not found")
	// ErrAlreadyExists means a store was created where one already exists.
	ErrAlreadyExists = errors.New("store: already exists")
	// ErrDoesNotExist means the store itself is missing.
	ErrDoesNotExist = errors.New("store: does not exist")
	// ErrPermissionDenied means the operation is not allowed by the configured
	// permissions. It is borge's own restriction, not the filesystem's.
	ErrPermissionDenied = errors.New("store: permission denied")
	// ErrMustBeOpen and ErrMustNotBeOpen are misuse: an operation was attempted in the
	// wrong lifecycle state.
	ErrMustBeOpen    = errors.New("store: backend is not open")
	ErrMustNotBeOpen = errors.New("store: backend is already open")
)

// ObjectNotFoundError names the object that was missing.
type ObjectNotFoundError struct{ Name string }

func (e *ObjectNotFoundError) Error() string {
	return fmt.Sprintf("store: object not found: %s", e.Name)
}
func (e *ObjectNotFoundError) Unwrap() error { return ErrObjectNotFound }

// PermissionDeniedError names what was refused and why.
type PermissionDeniedError struct {
	Name     string
	Required string
}

func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("store: one of the permissions %q is required for %q", e.Required, e.Name)
}
func (e *PermissionDeniedError) Unwrap() error { return ErrPermissionDenied }

// ItemInfo describes one entry of the store.
type ItemInfo struct {
	// Name is the entry's own name within its directory, not a full path - except from
	// Store.List, which fills in the un-nested store name.
	Name string
	// Exists is false for a name that was queried but is not there. Size is then zero.
	Exists bool
	// Directory is true for a nesting directory or a namespace, never for an object.
	Directory bool
	Size      int64
	ATime     time.Time
	MTime     time.Time
}

// Backend stores bytes under names. It is the interface every storage implementation
// satisfies; only PosixFS exists so far (sftp, rest, s3 and rclone are stage 8).
//
// Names given to a Backend are *nested* names - the Store has already applied the
// namespace's nesting level and any soft-delete suffix. A Backend does no nesting of
// its own.
type Backend interface {
	// Create makes the store. It fails if one already exists and is not empty.
	Create() error
	// Destroy removes the store and everything in it.
	Destroy() error

	// Open and Close bracket every other operation.
	Open() error
	Close() error

	// Load reads an object. A size of -1 means "to the end"; a negative offset is
	// measured from the end.
	//
	// Range reads are not an optimisation here - the pack reader locates objects by
	// reading only their 49-byte headers, so a backend that ignored the range would
	// turn every header read into a whole-pack download.
	Load(name string, offset int64, size int64) ([]byte, error)
	// Store writes an object, replacing any existing one.
	Store(name string, value []byte) error
	// Delete removes an object permanently. Soft deletion is Store.Move's job.
	Delete(name string) error
	// Move renames an object.
	Move(oldName, newName string) error
	// Info reports on one name without reading it.
	Info(name string) (ItemInfo, error)
	// List reports the entries of one directory, sorted by name, non-recursively.
	// Entries whose names are not valid store names are skipped: the directory may
	// hold files that did not come from us, or objects still being written.
	List(name string) ([]ItemInfo, error)
	// Mkdir creates a directory (and its parents).
	Mkdir(name string) error
	// Rmdir removes an empty directory.
	Rmdir(name string) error
}

// Permission characters, as borgstore spells them:
//
//	l  list object names in a namespace
//	r  read object contents
//	w  write a NEW object (one that does not exist yet)
//	W  write any object, including overwriting an existing one
//	D  delete an object
//
// A move needs D on the source and w or W on the destination, because the object
// vanishes from its old name - which, to anything looking for it, is the same as
// having been deleted.
const permissionChars = "lrwWD"

// Permissions maps a name prefix to the permissions granted below it.
//
// A nil or empty map means no restriction at all. This is borge's own guard rail, for
// the case where a repository is served to a client that should not be able to
// destroy it; the filesystem's own permissions are separate and still apply.
type Permissions map[string]string

// check reports whether any of the required permissions is granted for name.
//
// The lookup walks from the full name up to the root and stops at the *first* prefix
// that appears in the map, whether or not it grants the permission. A more specific
// entry therefore overrides a broader one rather than adding to it - which is what
// lets borg grant "lrwWD" on cache/ inside an otherwise read-only repository.
func (p Permissions) check(name, required string) error {
	if len(p) == 0 {
		return nil
	}
	for _, c := range required {
		if !containsRune(permissionChars, c) {
			return fmt.Errorf("store: unknown permission character %q", c)
		}
	}

	parts := splitAll(name)
	for i := len(parts); i >= 0; i-- {
		path := joinFirst(parts, i)
		granted, ok := p[path]
		if !ok {
			continue
		}
		for _, c := range required {
			if containsRune(granted, c) {
				return nil
			}
		}
		// Found the governing entry and it does not grant this; a broader entry must
		// not be consulted, or the more specific rule would be pointless.
		break
	}
	return &PermissionDeniedError{Name: name, Required: required}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// splitAll splits a store name into its path components. An empty name has none.
func splitAll(name string) []string {
	if name == "" {
		return nil
	}
	var parts []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			parts = append(parts, name[start:i])
			start = i + 1
		}
	}
	return append(parts, name[start:])
}

// joinFirst rejoins the first n components. n == 0 gives the root, "".
func joinFirst(parts []string, n int) string {
	if n <= 0 || len(parts) == 0 {
		return ""
	}
	if n > len(parts) {
		n = len(parts)
	}
	out := parts[0]
	for i := 1; i < n; i++ {
		out += "/" + parts[i]
	}
	return out
}
