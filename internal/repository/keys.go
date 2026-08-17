// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the key storage methods of Repository in borg's
// src/borg/repository.py (store_key, save_key, load_keys, load_key, delete_key).
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/store"
)

// The repository implements key.RepoKeyStore, which is how a repokey repository's keys
// are read and written. The interface lives in the key package rather than here because
// the layering runs the other way: the key layer must not import the store.

// ListKeys returns every key blob in the repository's keys/ namespace.
//
// Blobs belonging to another repository are not filtered out here - the key layer does
// that, because it is the one that knows what a blob's header means.
func (r *Repository) ListKeys() ([]key.NamedBlob, error) {
	names, err := r.store.ListNames("keys", false)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil, nil // the namespace is created lazily
		}
		return nil, err
	}
	var out []key.NamedBlob
	for _, name := range names {
		data, err := r.store.Load("keys/"+name, 0, -1, false)
		if err != nil {
			if errors.Is(err, store.ErrObjectNotFound) {
				continue // deleted under us, which is normal
			}
			return nil, err
		}
		out = append(out, key.NamedBlob{Name: name, Data: data})
	}
	return out, nil
}

// StoreKey writes a key blob and returns the name it was stored under.
//
// Keys are content-addressed, so storing is additive: a repository may hold several keys,
// one per passphrase, and writing one never disturbs the others. Removing a key is
// DeleteKey's job and nothing else's.
func (r *Repository) StoreKey(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	if err := r.store.Store("keys/"+name, data); err != nil {
		return "", err
	}
	return name, nil
}

// DeleteKey removes one key blob. A key that is already gone is not an error: the caller
// wanted it absent, and it is.
func (r *Repository) DeleteKey(name string) error {
	err := r.store.Delete("keys/"+name, false)
	if err != nil && errors.Is(err, store.ErrObjectNotFound) {
		return nil
	}
	return err
}

// KeyManager returns a key manager for this repository, with the repository wired in as
// the repokey store.
func (r *Repository) KeyManager() (*key.Manager, error) {
	return key.NewManager(r.id, r)
}
