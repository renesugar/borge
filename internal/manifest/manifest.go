// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the Manifest class in borg's src/borg/manifest.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package manifest is the repository's table of contents: the single object every
// operation reads first, and the directory of archives it points at.
//
// # What the manifest is, and what it stopped being
//
// In borg 1 the manifest carried the whole archive list, so adding an archive meant
// rewriting a blob that grew with the repository, and two clients adding archives at the
// same time could lose each other's work. In borg 2 the object store's archives/
// namespace *is* the directory: one empty object per archive, named by the archive's
// chunk id. `manifest["archives"]` is still written, and is always empty.
//
// What remains in the manifest is small and slow-changing: the format version, a
// timestamp, and a config map whose only load-bearing entry is `item_keys`.
package manifest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/msgpackx"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
)

// Version is the manifest version borge writes. borg reads 1 and 2; 1 is borg 1.x.
const Version = 2

// MaxArchives is borg's limit on how many archives a repository may hold. It exists
// because the msgpack unpacker used for the manifest is deliberately limited, so a
// corrupt or hostile manifest cannot make the reader allocate without bound.
const MaxArchives = 400000

// Errors.
var (
	// ErrNoManifest means the repository has no manifest object.
	ErrNoManifest = errors.New("manifest: repository has no manifest")
	// ErrUnsupportedFeature means the manifest demands a feature borge does not have.
	ErrUnsupportedFeature = errors.New("manifest: unsupported repository feature")
	// ErrInvalidManifest means the manifest is not a shape borge understands.
	ErrInvalidManifest = errors.New("manifest: invalid manifest")
)

// Operation is a class of repository access, used for the feature-flag check.
//
// The point of the split is that a repository written by a newer borg may still be
// safely *readable* by an older one while not being safely writable. Refusing everything
// on any unknown feature would be needlessly strict; refusing nothing would corrupt
// repositories.
type Operation string

const (
	// OpRead is listing and extracting archives.
	OpRead Operation = "read"
	// OpCheck is anything that has to understand every detail of the repository.
	OpCheck Operation = "check"
	// OpWrite is adding archives.
	OpWrite Operation = "write"
	// OpDelete is anything needing a complete and correct set of chunk references.
	OpDelete Operation = "delete"
)

// supportedFeatures is what borge implements beyond the base format. It is empty, as it
// is in borg: no optional feature has ever been defined.
var supportedFeatures = map[string]bool{}

// ItemKeys is the set of item metadata keys borge knows (src/borg/constants.py).
//
// The manifest records the keys a repository actually uses, and a reader takes the union
// of that and its own list. Writing the union back is how a repository written by two
// different versions stays readable by both.
var ItemKeys = []string{
	"acl_access", "acl_default", "acl_extended", "acl_nfs4", "atime", "birthtime",
	"bsdflags", "chunks", "chunks_healthy", "ctime", "gid", "group", "hardlink_master",
	"hlid", "inode", "mode", "mtime", "part", "path", "rdev", "size", "source", "target",
	"uid", "user", "xattrs",
}

// Manifest is a loaded repository manifest.
type Manifest struct {
	repo *repository.Repository
	ro   *repoobj.RepoObj
	key  key.Key

	// ID is the id hash of the manifest payload. It is not where the manifest is stored -
	// that is the fixed all-zero key.ManifestID - it is a fingerprint of the content, used
	// to tell one manifest from another.
	ID []byte

	// Timestamp is borg's ISO-8601 string with microsecond precision. It is kept as text
	// rather than a time.Time because the exact spelling is what gets written back, and
	// re-rendering a parsed time would not reproduce it byte for byte.
	Timestamp string

	// Config is the manifest's config map, preserved verbatim so unknown entries written
	// by another version survive a round trip.
	Config *msgpackx.Map

	// ItemKeys is the union of borge's own key set and the repository's.
	ItemKeys []string

	// Archives is the archive directory.
	Archives *Archives

	// version is what was read; borge writes Version.
	version int64
	// unknown holds manifest keys borge does not know, so writing preserves them.
	unknown []msgpackx.MapEntry
}

// Load reads and decrypts the manifest.
//
// operations is what the caller intends to do; the feature flags are checked against it,
// so a repository that is readable but not safely writable fails only when a write was
// actually intended.
func Load(r *repository.Repository, k key.Key, operations ...Operation) (*Manifest, error) {
	ro, err := repoobj.New(k)
	if err != nil {
		return nil, err
	}
	obj, err := r.Manifest()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoManifest, err)
	}

	_, data, err := ro.Parse(key.ManifestID, obj, repoobj.TypeManifest, repoobj.ParseOptions{})
	if err != nil {
		return nil, err
	}

	mi, err := unpackManifest(data)
	if err != nil {
		return nil, err
	}
	if mi.Version != 1 && mi.Version != 2 {
		return nil, fmt.Errorf("%w: version %d", ErrInvalidManifest, mi.Version)
	}

	m := &Manifest{
		repo:    r,
		ro:      ro,
		key:     k,
		ID:      k.IDHash(data),
		version: mi.Version,
		unknown: mi.Unknown,
	}
	if mi.Timestamp != nil {
		m.Timestamp = *mi.Timestamp
	}
	m.Config = mi.Config
	if m.Config == nil {
		m.Config = msgpackx.NewStableMap()
	}

	// The union of three sources: what borge knows, what the config records (borg 2), and
	// the legacy top-level field (borg 1.x).
	m.ItemKeys = unionKeys(ItemKeys, configItemKeys(m.Config), mi.ItemKeys)

	m.Archives = &Archives{repo: r, manifest: m}

	if err := m.checkCompatibility(operations); err != nil {
		return nil, err
	}
	return m, nil
}

// unpackManifest decodes the manifest payload.
//
// A manifest "from the future" is marked by four 0xc1 bytes - a msgpack byte that is
// never valid - so an old reader fails with a clear message instead of a decoding error.
func unpackManifest(data []byte) (*item.ManifestItem, error) {
	if len(data) >= 4 && data[0] == 0xc1 && data[1] == 0xc1 && data[2] == 0xc1 && data[3] == 0xc1 {
		return nil, fmt.Errorf("%w: this manifest was written by a newer borg", ErrUnsupportedFeature)
	}
	mi, err := item.UnmarshalManifestItem(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	return mi, nil
}

func configItemKeys(config *msgpackx.Map) []string {
	if config == nil {
		return nil
	}
	v, ok := config.Get("item_keys")
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range list {
		switch s := e.(type) {
		case string:
			out = append(out, s)
		case []byte:
			out = append(out, string(s))
		}
	}
	return out
}

func unionKeys(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range sets {
		for _, k := range set {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// checkCompatibility refuses to proceed when the manifest demands a feature borge lacks.
//
// Only *mandatory* requirements are enforced, and only for the operations the caller
// intends: a repository may be readable but not writable by this version, and saying so
// precisely is more useful than refusing it outright.
func (m *Manifest) checkCompatibility(operations []Operation) error {
	if m.Config == nil {
		return nil
	}
	v, ok := m.Config.Get("feature_flags")
	if !ok {
		return nil
	}
	flags, ok := v.(*msgpackx.Map)
	if !ok {
		return nil
	}
	for _, op := range operations {
		req, ok := flags.Get(string(op))
		if !ok {
			continue
		}
		reqMap, ok := req.(*msgpackx.Map)
		if !ok {
			continue
		}
		mandatory, ok := reqMap.Get("mandatory")
		if !ok {
			continue
		}
		list, ok := mandatory.([]any)
		if !ok {
			continue
		}
		var missing []string
		for _, f := range list {
			name := ""
			switch s := f.(type) {
			case string:
				name = s
			case []byte:
				name = string(s)
			}
			if name != "" && !supportedFeatures[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%w: %s requires %s; a newer borg or borge is needed",
				ErrUnsupportedFeature, op, strings.Join(missing, ", "))
		}
	}
	return nil
}

// MandatoryFeatures lists every mandatory feature the manifest declares, per operation.
func (m *Manifest) MandatoryFeatures() map[Operation][]string {
	out := map[Operation][]string{}
	if m.Config == nil {
		return out
	}
	v, ok := m.Config.Get("feature_flags")
	if !ok {
		return out
	}
	flags, ok := v.(*msgpackx.Map)
	if !ok {
		return out
	}
	for _, e := range flags.Entries() {
		name, ok := e.Key.(string)
		if !ok {
			continue
		}
		reqMap, ok := e.Value.(*msgpackx.Map)
		if !ok {
			continue
		}
		mandatory, ok := reqMap.Get("mandatory")
		if !ok {
			continue
		}
		list, ok := mandatory.([]any)
		if !ok {
			continue
		}
		var features []string
		for _, f := range list {
			switch s := f.(type) {
			case string:
				features = append(features, s)
			case []byte:
				features = append(features, string(s))
			}
		}
		out[Operation(name)] = features
	}
	return out
}

// Version reports the manifest version that was read.
func (m *Manifest) Version() int64 { return m.version }

// Key is the repository key the manifest was decrypted with.
func (m *Manifest) Key() key.Key { return m.key }

// RepoObj is the object codec bound to that key.
func (m *Manifest) RepoObj() *repoobj.RepoObj { return m.ro }

// Repository is the repository this manifest belongs to.
func (m *Manifest) Repository() *repository.Repository { return m.repo }

// LastTimestamp parses the recorded timestamp.
func (m *Manifest) LastTimestamp() (time.Time, error) { return ParseTimestamp(m.Timestamp) }

// timestampLayouts are the spellings borg's datetime.fromisoformat accepts for the
// values borg itself writes: with an offset, and without.
var timestampLayouts = []string{
	"2006-01-02T15:04:05.999999-07:00",
	"2006-01-02T15:04:05.999999Z07:00",
	"2006-01-02T15:04:05.999999",
	"2006-01-02T15:04:05",
}

// ParseTimestamp reads one of borg's ISO-8601 timestamps.
//
// A timestamp with no zone is read as UTC, which is what borg's parse_timestamp does.
// That default is not cosmetic: borg 1.x wrote local times without a zone, and reading
// them as local would shift every old archive by the reader's offset.
func ParseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("manifest: empty timestamp")
	}
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("manifest: cannot parse timestamp %q", s)
}

// FormatTimestamp renders a time the way borg writes it:
// isoformat(timespec="microseconds") on a UTC-aware value.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

// Write encrypts and stores the manifest.
//
// The timestamp is forced to advance. Clocks are not reliable - they get set backwards,
// they differ between machines writing to one repository - and several operations
// compare manifest timestamps to decide what is newer. Taking max(now, last + 1µs)
// keeps that comparison meaningful even when the clock is not.
func (m *Manifest) Write() error {
	next := time.Now().UTC()
	if m.Timestamp != "" {
		last, err := ParseTimestamp(m.Timestamp)
		if err != nil {
			return err
		}
		if incremented := last.Add(time.Microsecond); incremented.After(next) {
			next = incremented
		}
	}
	m.Timestamp = FormatTimestamp(next)

	count, err := m.Archives.Count()
	if err != nil {
		return err
	}
	if count > MaxArchives {
		return fmt.Errorf("manifest: %d archives exceeds the limit of %d", count, MaxArchives)
	}
	if len(m.ItemKeys) > 100 {
		return fmt.Errorf("manifest: %d item keys exceeds the limit of 100", len(m.ItemKeys))
	}

	config := msgpackx.NewStableMap()
	if m.Config != nil {
		for _, e := range m.Config.Entries() {
			if k, ok := e.Key.(string); ok && k == "item_keys" {
				continue // rewritten below from the union
			}
			config.Set(e.Key, e.Value)
		}
	}
	keys := make([]any, 0, len(m.ItemKeys))
	sorted := append([]string(nil), m.ItemKeys...)
	sortStrings(sorted)
	for _, k := range sorted {
		keys = append(keys, k)
	}
	config.Set("item_keys", keys)
	m.Config = config

	ts := m.Timestamp
	mi := &item.ManifestItem{
		Version: Version,
		// Always empty: the archives/ namespace is the directory. The key is still
		// written because borg writes it, and a missing key is a difference.
		Archives:    msgpackx.NewStableMap(),
		ArchivesSet: true,
		Timestamp:   &ts,
		Config:      config,
		ConfigSet:   true,
		Unknown:     m.unknown,
	}

	data, err := mi.Marshal()
	if err != nil {
		return err
	}
	m.ID = m.key.IDHash(data)

	obj, err := m.ro.Format(key.ManifestID, &repoobj.Meta{Type: repoobj.TypeManifest}, data)
	if err != nil {
		return err
	}
	return m.repo.PutManifest(obj)
}
