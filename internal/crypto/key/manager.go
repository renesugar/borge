// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the key discovery, loading and storage half of FlexiKey and
// of KeyManager, in borg's src/borg/crypto/key.py and src/borg/crypto/keymanager.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package key

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/renesugar/borge/internal/item"
)

// Storage is where an individual key blob lives.
//
// It is a property of the key, not of the mode: the type byte in an object identifies
// the ciphersuite and says nothing about where the key is kept. borg arrived at that
// separation after coupling them for years, and it is easy to re-couple by accident, so
// nothing in this package derives one from the other.
type Storage string

const (
	// StorageKeyfile keeps the blob in a file outside the repository, so that whoever
	// holds the repository does not hold the key.
	StorageKeyfile Storage = "keyfile"
	// StorageRepo keeps the blob in the repository's keys/ namespace, where it travels
	// with the repository. Convenient, and worth exactly as much as the passphrase.
	StorageRepo Storage = "repokey"
	// StorageNone is the none-* modes, which have no key material to store.
	StorageNone Storage = "none"
)

// RepoKeyStore is the repository half of key storage.
//
// It is an interface rather than a direct dependency because internal/store sits *above*
// this package in the layering (scripts/check-layering.sh): the key layer must not import
// the repository. The repository implements these three methods and passes itself in.
type RepoKeyStore interface {
	// ListKeys returns every key blob in the repository, by store name.
	ListKeys() ([]NamedBlob, error)
	// StoreKey writes a key blob and returns the name it was stored under.
	StoreKey(data []byte) (string, error)
	// DeleteKey removes one key blob by name.
	DeleteKey(name string) error
}

// NamedBlob is one stored key blob and the name it lives under.
type NamedBlob struct {
	Name string
	Data []byte
}

// Blob is one of a repository's keys, as seen from outside: enough to name it, describe
// it and delete it, without knowing the passphrase.
type Blob struct {
	// ID is the sha256 of the blob text; it is also the store name or file name.
	ID string
	// Label is the human-readable name, empty if the key carries none.
	Label string
	// Algorithm is the KDF/cipher combination the blob records.
	Algorithm string
	// Storage says where this blob lives, and Path is its file name when it is a keyfile.
	Storage Storage
	Path    string
	// Text is the full blob, including the BORG_KEY header line.
	Text []byte
	// Corrupt is set when the envelope could not be parsed. Such a key is still listed
	// and still removable - hiding it would leave the user unable to clean it up.
	Corrupt bool
}

// Unlocked is a key blob that has been opened with a passphrase.
type Unlocked struct {
	// Material is the secret. Callers should not log it.
	Material *item.Key
	// Blob is the key it came from.
	Blob Blob
}

// Manager finds, opens and stores the keys of one repository.
//
// # Search order
//
// Keyfiles first, then the repository's keys/ namespace, and within the keyfiles the
// directories in KeysDirs in order. The first key a passphrase opens wins. That ordering
// is borg's and it matters: a user who has taken the trouble to put a keyfile on removable
// media expects it to be used rather than a copy that happens to sit in the repository.
type Manager struct {
	// RepoID is the repository's 32-byte id. Blobs naming a different repository are
	// skipped rather than tried.
	RepoID []byte
	// Repo is the repository's key namespace, or nil when there is none (an export, a
	// repository not open for this purpose).
	Repo RepoKeyStore
	// KeysDirs is the keyfile search path. New keyfiles go into the first entry; the
	// rest are read-only. Empty means "resolve from the environment", see KeysDirs.
	KeysDirs []string
}

// NewManager returns a Manager with the default keyfile search path.
func NewManager(repoID []byte, repo RepoKeyStore) (*Manager, error) {
	if len(repoID) != 32 {
		return nil, fmt.Errorf("key: repository id must be 32 bytes, got %d", len(repoID))
	}
	dirs, err := KeysDirs()
	if err != nil {
		return nil, err
	}
	return &Manager{RepoID: append([]byte(nil), repoID...), Repo: repo, KeysDirs: dirs}, nil
}

// RepoIDHex is the repository id as it appears in a blob's header line.
func (m *Manager) RepoIDHex() string { return hex.EncodeToString(m.RepoID) }

func (m *Manager) keysDirs() ([]string, error) {
	if len(m.KeysDirs) > 0 {
		return m.KeysDirs, nil
	}
	return KeysDirs()
}

// ---------------------------------------------------------------- keys directory

// helpText marks a declaration that exists only to carry user-facing documentation.
//
// The doc comment above such a declaration is help text and nothing else: docgen renders
// it into "borge help", so a maintainer's note in it would be printed at a user. Notes
// belong in the code below it.
const helpText = "user-facing help text"

// With none of those set, keyfiles are looked for in the user configuration directory,
// under borge/keys and then borg/keys - so a borg installation's keys are found without
// being moved.
//
//borge:doc user
//borge:help environment/keyfile-search
//borge:about KeysDirs
var _ = helpText

// KeysDirs returns the keyfile search path: where borge writes first, then the places it
// will also read from.
//
// # Why there is more than one
//
// borge is not borg, so it keeps its keys in its own configuration directory rather than
// writing into borg's. But a user porting a repository already has keyfiles in borg's
// directory, and a keyfile is byte-for-byte the same in either tool. Refusing to look
// there would mean "borge cannot open the repository borg just created", which is the
// opposite of the point. So borge reads both and writes only its own.
//
// See docs/DIVERGENCES.md; borg looks in exactly one directory.
func KeysDirs() ([]string, error) {
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, seen := range dirs {
			if seen == p {
				return
			}
		}
		dirs = append(dirs, p)
	}

	// An explicit setting wins outright, and pins the search to that one directory: a
	// user who says where the keys are is not asking for a search.
	if v, ok := os.LookupEnv("BORGE_KEYS_DIR"); ok && v != "" {
		return []string{v}, nil
	}
	if v, ok := os.LookupEnv("BORG_KEYS_DIR"); ok && v != "" {
		return []string{v}, nil
	}

	if v, ok := os.LookupEnv("BORGE_CONFIG_DIR"); ok && v != "" {
		add(filepath.Join(v, "keys"))
	}
	if v, ok := os.LookupEnv("BORG_CONFIG_DIR"); ok && v != "" {
		add(filepath.Join(v, "keys"))
	}
	if v, ok := os.LookupEnv("BORGE_BASE_DIR"); ok && v != "" {
		add(filepath.Join(v, ".config", "borge", "keys"))
	}
	if v, ok := os.LookupEnv("BORG_BASE_DIR"); ok && v != "" {
		add(filepath.Join(v, ".config", "borg", "keys"))
	}

	if len(dirs) == 0 {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("key: cannot determine the configuration directory: %w", err)
		}
		add(filepath.Join(cfg, "borge", "keys"))
		add(filepath.Join(cfg, "borg", "keys"))
	}
	return dirs, nil
}

// keyFileFromEnv is BORG_KEY_FILE: a single explicit keyfile, which overrides the search
// entirely.
func keyFileFromEnv() string {
	v, ok := lookupEnv("KEY_FILE")
	if !ok || v == "" {
		return ""
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return v
	}
	return abs
}

// ---------------------------------------------------------------- listing

// List returns every key of this repository, keyfiles first.
//
// A blob whose envelope cannot be parsed is returned with Corrupt set rather than
// dropped: a corrupt key still occupies a name, and the user needs to see it to remove
// it.
func (m *Manager) List() ([]Blob, error) {
	var out []Blob

	files, err := m.keyfileBlobs()
	if err != nil {
		return nil, err
	}
	out = append(out, files...)

	repo, err := m.repoBlobs()
	if err != nil {
		return nil, err
	}
	out = append(out, repo...)
	return out, nil
}

func (m *Manager) describe(text []byte, storage Storage, path string) Blob {
	b := Blob{
		ID:      BlobName(text),
		Storage: storage,
		Path:    path,
		Text:    text,
	}
	env, err := ParseEnvelope(text, m.RepoIDHex())
	if err != nil {
		b.Corrupt = true
		b.Algorithm = "(unparseable)"
		return b
	}
	b.Label = env.Label
	b.Algorithm = env.Algorithm
	return b
}

func (m *Manager) keyfileBlobs() ([]Blob, error) {
	if explicit := keyFileFromEnv(); explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			// A named-but-missing keyfile is not an error here: the key may be a repokey,
			// and the variable may be pointing at where a new one is to be written.
			return nil, nil
		}
		if !IsKeyfile(data, m.RepoIDHex()) {
			return nil, fmt.Errorf("%w: %s", ErrRepositoryMismatch, explicit)
		}
		return []Blob{m.describe(data, StorageKeyfile, explicit)}, nil
	}

	dirs, err := m.keysDirs()
	if err != nil {
		return nil, err
	}
	var out []Blob
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a directory that does not exist yet is not an error
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // stable order, so "the first key that opens" is deterministic
		for _, name := range names {
			path := filepath.Join(dir, name)
			// Read in binary and check the magic before anything else: the directory may
			// hold files that are not keys at all.
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if !IsKeyfile(data, m.RepoIDHex()) {
				continue
			}
			out = append(out, m.describe(data, StorageKeyfile, path))
		}
	}
	return out, nil
}

func (m *Manager) repoBlobs() ([]Blob, error) {
	if m.Repo == nil {
		return nil, nil
	}
	blobs, err := m.Repo.ListKeys()
	if err != nil {
		return nil, err
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Name < blobs[j].Name })
	var out []Blob
	for _, nb := range blobs {
		if !IsKeyfile(nb.Data, m.RepoIDHex()) {
			continue
		}
		b := m.describe(nb.Data, StorageRepo, "")
		// The store name is authoritative: it is what DeleteKey takes. It equals the
		// content digest for anything borg or borge wrote, but do not assume that.
		b.ID = nb.Name
		out = append(out, b)
	}
	return out, nil
}

// ---------------------------------------------------------------- unlocking

// Unlock tries a passphrase against every key of this repository and returns the first
// one it opens.
//
// Trying them all is what makes several passphrases per repository work: the passphrase
// selects the key, rather than the user having to say which key they are holding.
func (m *Manager) Unlock(passphrase string) (*Unlocked, error) {
	blobs, err := m.List()
	if err != nil {
		return nil, err
	}
	if len(blobs) == 0 {
		return nil, fmt.Errorf("key: no key found for repository %s", m.RepoIDHex())
	}
	for _, b := range blobs {
		if b.Corrupt {
			continue
		}
		material, env, err := OpenMaterial(b.Text, m.RepoIDHex(), passphrase)
		if err != nil {
			if errors.Is(err, ErrPassphraseWrong) {
				continue
			}
			// A key borge cannot read at all must not stop the others from being tried.
			continue
		}
		b.Label = env.Label
		return &Unlocked{Material: material, Blob: b}, nil
	}
	return nil, ErrPassphraseWrong
}

// ---------------------------------------------------------------- saving

// SaveOptions control where and how a key blob is written.
type SaveOptions struct {
	// Storage selects keyfile or repokey.
	Storage Storage
	// Label names the key. Required when adding a key beside an existing one.
	Label string
	// Path overrides the keyfile location. Empty means "name it by its content in the
	// first keys directory".
	Path string
	// Create refuses to overwrite an existing keyfile. It is what repository creation
	// passes, so that a careless BORG_KEY_FILE cannot destroy another repository's key
	// (borg #6036).
	Create bool
}

// Save writes a key blob for the given material and passphrase.
func (m *Manager) Save(material *item.Key, passphrase string, opts SaveOptions) (Blob, error) {
	text, err := SealMaterial(material, passphrase, opts.Label)
	if err != nil {
		return Blob{}, err
	}
	data := []byte(text)

	switch opts.Storage {
	case StorageRepo:
		if m.Repo == nil {
			return Blob{}, errors.New("key: this repository cannot store keys")
		}
		name, err := m.Repo.StoreKey(data)
		if err != nil {
			return Blob{}, err
		}
		b := m.describe(data, StorageRepo, "")
		b.ID = name
		return b, nil

	case StorageKeyfile:
		path := opts.Path
		if path == "" {
			path = keyFileFromEnv()
		}
		if path == "" {
			dirs, err := m.keysDirs()
			if err != nil {
				return Blob{}, err
			}
			if err := os.MkdirAll(dirs[0], 0o700); err != nil {
				return Blob{}, fmt.Errorf("key: %w", err)
			}
			path = filepath.Join(dirs[0], BlobName(data))
		}
		if opts.Create {
			if _, err := os.Stat(path); err == nil {
				return Blob{}, fmt.Errorf("key: refusing to overwrite the existing key file %s", path)
			}
		}
		if err := writeKeyfile(path, data); err != nil {
			return Blob{}, err
		}
		return m.describe(data, StorageKeyfile, path), nil

	default:
		return Blob{}, fmt.Errorf("key: cannot save a key with storage %q", opts.Storage)
	}
}

// writeKeyfile writes a key file readable only by its owner, via a temporary file so a
// crash cannot leave a truncated key behind.
func writeKeyfile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".borge-key-*")
	if err != nil {
		return fmt.Errorf("key: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("key: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	return nil
}

// Delete removes one key blob.
func (m *Manager) Delete(b Blob) error {
	switch b.Storage {
	case StorageRepo:
		if m.Repo == nil {
			return errors.New("key: this repository cannot store keys")
		}
		return m.Repo.DeleteKey(b.ID)
	case StorageKeyfile:
		if b.Path == "" {
			return errors.New("key: this key has no file to remove")
		}
		return secureErase(b.Path)
	default:
		return fmt.Errorf("key: cannot delete a key with storage %q", b.Storage)
	}
}

// secureErase overwrites a key file with random bytes before unlinking it.
//
// On a copy-on-write or log-structured filesystem this does not actually destroy the old
// bytes, and on an SSD it may not either - so it is a best effort that raises the cost of
// casual recovery, not a guarantee. It is still worth doing: the alternative leaves the
// key material intact in free space for certain.
func secureErase(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("key: %w", err)
	}
	if info.Mode().IsRegular() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err == nil {
			noise := make([]byte, info.Size())
			if _, err := rand.Read(noise); err == nil {
				_, _ = f.Write(noise)
				_ = f.Sync()
			}
			_ = f.Close()
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- key management

// ChangePassphrase rewrites an unlocked key under a new passphrase and removes the old
// blob.
//
// The order is write-then-delete, so an interruption leaves two working keys rather than
// none. The label is carried over, because it names the key rather than the passphrase.
func (m *Manager) ChangePassphrase(u *Unlocked, newPassphrase string) (Blob, error) {
	if u == nil {
		return Blob{}, errors.New("key: no unlocked key to change")
	}
	opts := SaveOptions{Storage: u.Blob.Storage, Label: u.Blob.Label}
	if u.Blob.Storage == StorageKeyfile && !isContentNamed(u.Blob) {
		// The user pinned this path (BORG_KEY_FILE, or a file they placed themselves), so
		// rewrite it in place rather than leaving the key somewhere they did not choose.
		opts.Path = u.Blob.Path
	}

	created, err := m.Save(u.Material, newPassphrase, opts)
	if err != nil {
		return Blob{}, err
	}
	// A blob written to the same path has already replaced the old one. A content-named
	// one has not: new content means a new name, so the old file or object is still there
	// and has to go, or the old passphrase would keep working.
	if created.Path != u.Blob.Path || created.ID != u.Blob.ID {
		if err := m.Delete(u.Blob); err != nil {
			return created, fmt.Errorf("key: the new key was written but the old one could not be removed: %w", err)
		}
	}
	return created, nil
}

// isContentNamed reports whether a keyfile carries the name borge gave it, rather than
// one the user chose. Only the former may be renamed out from under them.
func isContentNamed(b Blob) bool {
	return b.Path != "" && filepath.Base(b.Path) == BlobName(b.Text)
}

// AddKey adds a second key protecting the same material with another passphrase.
//
// This is how a repository gets a shared passphrase, or a break-glass one, without anyone
// having to learn the original.
func (m *Manager) AddKey(u *Unlocked, passphrase, label string) (Blob, error) {
	if u == nil {
		return Blob{}, errors.New("key: no unlocked key to copy")
	}
	if label == "" {
		return Blob{}, errors.New("key: a label is required when adding a key")
	}
	if label == AdminLabel {
		return Blob{}, fmt.Errorf("key: the %q label belongs to the key created with the repository", AdminLabel)
	}
	existing, err := m.List()
	if err != nil {
		return Blob{}, err
	}
	for _, b := range existing {
		if b.Label == label {
			return Blob{}, fmt.Errorf("key: a key labelled %q already exists", label)
		}
	}
	return m.Save(u.Material, passphrase, SaveOptions{Storage: u.Blob.Storage, Label: label})
}

// RemoveKey deletes one key, selected by label or by an id prefix.
//
// Two things are refused: removing the last key, which would make the repository
// unreadable, and removing the admin key, which is the one created with the repository.
func (m *Manager) RemoveKey(selector string) (Blob, error) {
	blobs, err := m.List()
	if err != nil {
		return Blob{}, err
	}
	if len(blobs) <= 1 {
		return Blob{}, errors.New("key: refusing to remove the only key of this repository")
	}

	var matches []Blob
	for _, b := range blobs {
		if b.Label == selector || (selector != "" && strings.HasPrefix(b.ID, selector)) {
			matches = append(matches, b)
		}
	}
	if len(matches) != 1 {
		return Blob{}, fmt.Errorf("key: %q matches %d keys, it has to match exactly one", selector, len(matches))
	}
	victim := matches[0]
	if victim.Label == AdminLabel {
		return Blob{}, fmt.Errorf("key: the %q key is protected and cannot be removed", AdminLabel)
	}
	return victim, m.Delete(victim)
}

// ---------------------------------------------------------------- export and import

// Export returns the blob text of one key, ready to be written to a file or printed.
//
// selector picks the key by label or id prefix; it may be empty when there is only one.
func (m *Manager) Export(selector string) (Blob, error) {
	blobs, err := m.List()
	if err != nil {
		return Blob{}, err
	}
	if len(blobs) == 0 {
		return Blob{}, fmt.Errorf("key: no key found for repository %s", m.RepoIDHex())
	}
	if selector == "" {
		if len(blobs) != 1 {
			var labels []string
			for _, b := range blobs {
				name := b.Label
				if name == "" {
					name = b.ID[:12]
				}
				labels = append(labels, name)
			}
			return Blob{}, fmt.Errorf("key: this repository has %d keys (%s); say which one to export",
				len(blobs), strings.Join(labels, ", "))
		}
		return blobs[0], nil
	}
	var matches []Blob
	for _, b := range blobs {
		if b.Label == selector || strings.HasPrefix(b.ID, selector) {
			matches = append(matches, b)
		}
	}
	if len(matches) != 1 {
		return Blob{}, fmt.Errorf("key: %q matches %d keys, it has to match exactly one", selector, len(matches))
	}
	return matches[0], nil
}

// Import stores an exported key blob into this repository.
//
// The blob is not decrypted: importing a key is not the same as unlocking it, and
// requiring the passphrase here would stop someone from installing a key they are meant
// to hold but not use.
func (m *Manager) Import(text []byte, storage Storage) (Blob, error) {
	if !IsKeyfile(text, "") {
		return Blob{}, ErrNotAKeyfile
	}
	if !IsKeyfile(text, m.RepoIDHex()) {
		return Blob{}, fmt.Errorf("%w: this key is not for repository %s", ErrRepositoryMismatch, m.RepoIDHex())
	}
	// Parse before storing, so a truncated or mangled export is refused rather than
	// installed and discovered later.
	if _, err := ParseEnvelope(text, m.RepoIDHex()); err != nil {
		return Blob{}, err
	}

	// Normalise: an exported file may have picked up trailing whitespace, and the blob's
	// name is its content digest.
	repoID, b64, err := KeyfileParse(text, m.RepoIDHex())
	if err != nil {
		return Blob{}, err
	}
	data := []byte(KeyfileFormat(repoID, strings.TrimSpace(b64)))

	switch storage {
	case StorageRepo:
		if m.Repo == nil {
			return Blob{}, errors.New("key: this repository cannot store keys")
		}
		name, err := m.Repo.StoreKey(data)
		if err != nil {
			return Blob{}, err
		}
		b := m.describe(data, StorageRepo, "")
		b.ID = name
		return b, nil
	case StorageKeyfile:
		path := keyFileFromEnv()
		if path == "" {
			dirs, err := m.keysDirs()
			if err != nil {
				return Blob{}, err
			}
			path = filepath.Join(dirs[0], BlobName(data))
		}
		if err := writeKeyfile(path, data); err != nil {
			return Blob{}, err
		}
		return m.describe(data, StorageKeyfile, path), nil
	default:
		return Blob{}, fmt.Errorf("key: cannot import a key with storage %q", storage)
	}
}
