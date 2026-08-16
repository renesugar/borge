// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/utils/nesting.py and the validate_name function
// in borgstore/backends/_base.py (borgstore 0.6.1, BSD 3-Clause, Copyright (C) 2026
// Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"fmt"
	"strings"
)

// Suffixes borgstore reserves (borgstore/constants.py).
const (
	// TmpSuffix marks a file being written. Names ending in it are rejected on the way
	// in and skipped on the way out of a listing, so a half-written object is never
	// mistaken for a real one.
	TmpSuffix = ".tmp"
	// DelSuffix marks a soft-deleted object. Soft deletion is a rename, which is what
	// makes borg's undelete possible.
	DelSuffix = ".del"
	// HidSuffix marks an internal file users never see.
	HidSuffix = ".hid"

	// MaxNameLength is deliberately conservative: names have to survive every backend
	// and platform borgstore supports, and suffixes may still be appended.
	MaxNameLength = 100

	// RootNS is the namespace of the store root.
	RootNS = ""
)

// validateName checks a store name, applying borgstore's rules.
//
// It runs on the way in *and* on the way out: a name read from a directory listing is
// validated too, because the directory may hold files that did not come from us. The
// rules are deliberately strict - lowercase ASCII, no spaces or backslashes - so a name
// means the same thing on a case-insensitive filesystem, over a shell, and through
// every backend.
func validateName(name string) error {
	if len(name) > MaxNameLength {
		return fmt.Errorf("store: name is longer than %d bytes: %q", MaxNameLength, name)
	}
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return fmt.Errorf("store: name must be plain ASCII: %q", name)
		}
	}
	// Security: the name must stay inside the store. A leading slash or a ".."
	// component would let a caller address anything on the filesystem.
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("store: name must be relative and must not contain '..': %q", name)
	}
	// Separators are always "/", never "\", so a name means the same thing if this is
	// ever ported to Windows. No spaces, which keeps names usable from a shell.
	if strings.ContainsAny(name, `\ `) {
		return fmt.Errorf("store: name must not contain backslashes or spaces: %q", name)
	}
	// Lowercase, so "config" and "CONFIG" cannot address different objects on a
	// case-insensitive backend - and so a hex id is unambiguous.
	if name != strings.ToLower(name) {
		return fmt.Errorf("store: name must be lowercase: %q", name)
	}
	if strings.HasSuffix(name, TmpSuffix) {
		return fmt.Errorf("store: name must not end with %s, which is reserved for files being written: %q",
			TmpSuffix, name)
	}
	return nil
}

// splitKey splits a name into its namespace and its key. A name with no slash has no
// namespace.
func splitKey(name string) (namespace string, key string, hasNamespace bool) {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i], name[i+1:], true
	}
	return "", name, false
}

// Nest inserts intermediate directories derived from the key, so a namespace holding
// millions of objects does not put them all in one directory:
//
//	Nest("packs/0123456789abcdef", 2) == "packs/01/23/0123456789abcdef"
//
// The last element is the *whole* key, not a remainder. That costs a little space and
// buys a lot: a directory listing yields usable keys directly, a sorted listing is in
// the same order as a sorted key list, and a file recovered into lost+found still
// carries its own name.
//
// suffix, when non-empty, is appended after nesting - which is how a soft-deleted
// object keeps its place in the tree.
func Nest(name string, levels int, suffix string) string {
	if levels > 0 && name != "" {
		namespace, key, hasNamespace := splitKey(name)
		parts := make([]string, 0, levels+2)
		if hasNamespace {
			parts = append(parts, namespace)
		}
		for level := 0; level < levels; level++ {
			// Two hex characters per level. A key shorter than that yields a shorter or
			// empty component, which is what borgstore does too.
			start, end := 2*level, 2*level+2
			if start > len(key) {
				start = len(key)
			}
			if end > len(key) {
				end = len(key)
			}
			parts = append(parts, key[start:end])
		}
		parts = append(parts, key)
		name = strings.Join(parts, "/")
	}
	return name + suffix
}

// Unnest reverses Nest:
//
//	Unnest("packs/01/23/0123456789abcdef", "packs") == "packs/0123456789abcdef"
//
// suffix, when non-empty, is stripped from the key.
func Unnest(name, namespace, suffix string) (string, error) {
	if namespace != "" {
		if !strings.HasSuffix(namespace, "/") {
			namespace += "/"
		}
		if !strings.HasPrefix(name, namespace) {
			return "", fmt.Errorf("store: name %q is not in namespace %q", name, namespace)
		}
		name = strings.TrimPrefix(name, namespace)
	}
	key := name
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		key = key[i+1:]
	}
	key = strings.TrimSuffix(key, suffix)
	return namespace + key, nil
}
