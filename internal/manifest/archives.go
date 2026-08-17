// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the Archives class in borg's src/borg/manifest.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package manifest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/patterns"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
	"github.com/renesugar/borge/internal/store"
)

// Archives is a repository's archive directory.
//
// # How an archive is listed
//
// Each archive has one object at `archives/<chunk id in hex>` whose **content is empty**.
// The name is the whole record: it is the chunk id of the archive's metadata object, so
// listing the namespace gives the archive ids, and the details are read from the objects
// they point at.
//
// That indirection is what makes creating and deleting archives concurrent-safe. Adding
// an archive writes one new object rather than rewriting a shared list, so two clients
// backing up at once cannot lose each other's work - which is exactly what borg 1's
// manifest-held archive list could do.
//
// # Soft deletion
//
// Deleting renames the pointer to `<name>.del` rather than removing it, so the archive
// is invisible to a listing but still recoverable until a compaction removes it. See
// docs/FORMAT.md §1.2.
type Archives struct {
	repo     *repository.Repository
	manifest *Manifest
}

// Info is what a listing knows about one archive.
type Info struct {
	// ID is the chunk id of the archive's metadata object.
	ID []byte
	// Name is the archive name. Names are not unique - several archives may share one,
	// distinguished by their id and time.
	Name string
	// Time is the archive's nominal time.
	Time time.Time
	// TimeString is that timestamp exactly as stored, for output that has to match borg's.
	TimeString string
	// Start and End bracket the run that produced the archive.
	Start, End time.Time

	Host string
	User string
	Tags []string

	Comment string
	Size    int64
	NFiles  int64

	// Exists is false when the directory holds a pointer but the archive object behind it
	// is missing or unreadable. Such an entry is still listed: it is exactly what a check
	// needs to see, and hiding it would make the damage invisible.
	Exists bool
	// Problem says why Exists is false.
	Problem string
}

// archiveName is the store name of an archive's directory entry.
func archiveName(id []byte) string { return "archives/" + hex.EncodeToString(id) }

// IDs returns the chunk ids of the repository's archives.
//
// With deleted set, it returns the soft-deleted ones instead - which is how an undelete
// finds something to undelete.
func (a *Archives) IDs(deleted bool) ([][]byte, error) {
	names, err := a.repo.Store().ListNames("archives", deleted)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil, nil // the namespace is created lazily
		}
		return nil, err
	}
	sort.Strings(names)
	out := make([][]byte, 0, len(names))
	for _, name := range names {
		id, err := hex.DecodeString(name)
		if err != nil || len(id) != repoobj.ChunkIDSize {
			// Not an archive pointer. Skip it rather than failing: the namespace belongs
			// to borg and borge, and refusing to list because of one stray name would
			// make the whole repository unusable.
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// Count returns how many archives the repository holds.
func (a *Archives) Count() (int, error) {
	ids, err := a.IDs(false)
	return len(ids), err
}

// Get reads one archive's metadata by id.
//
// A missing or corrupt archive object is reported as an Info with Exists false rather
// than as an error, because a listing has to be able to show a damaged archive.
func (a *Archives) Get(id []byte) (*Info, error) {
	info := &Info{ID: append([]byte(nil), id...)}

	obj, err := a.repo.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrObjectNotFound) {
			info.Name = "archive-does-not-exist"
			info.Problem = "the directory has a pointer, but the archive object is missing"
			return info, nil
		}
		return nil, err
	}

	_, data, err := a.manifest.ro.Parse(id, obj, repoobj.TypeArchiveMeta, repoobj.ParseOptions{})
	if err != nil {
		info.Name = "archive-metadata-has-integrity-error"
		info.Problem = "the archive object failed its integrity check: " + err.Error()
		return info, nil
	}

	ai, err := item.UnmarshalArchiveItem(data)
	if err != nil {
		info.Name = "archive-metadata-is-unreadable"
		info.Problem = "the archive metadata could not be decoded: " + err.Error()
		return info, nil
	}
	if ai.Version != 1 && ai.Version != 2 {
		info.Name = "archive-metadata-has-unknown-version"
		info.Problem = fmt.Sprintf("archive metadata version %d", ai.Version)
		return info, nil
	}

	info.Exists = true
	info.Name = ai.Name
	if ai.Time != nil {
		info.TimeString = *ai.Time
		if t, err := ParseTimestamp(*ai.Time); err == nil {
			info.Time = t
		}
	}
	if ai.Start != nil {
		if t, err := ParseTimestamp(*ai.Start); err == nil {
			info.Start = t
		}
	}
	if ai.End != nil {
		if t, err := ParseTimestamp(*ai.End); err == nil {
			info.End = t
		}
	}
	if ai.Hostname != nil {
		info.Host = *ai.Hostname
	}
	if ai.Username != nil {
		info.User = *ai.Username
	}
	if ai.Comment != nil {
		info.Comment = *ai.Comment
	}
	if ai.Size != nil {
		info.Size = *ai.Size
	}
	if ai.NFiles != nil {
		info.NFiles = *ai.NFiles
	}
	// Sorted, because borg sorts them and a tag set has no inherent order.
	info.Tags = append([]string(nil), ai.Tags...)
	sort.Strings(info.Tags)
	return info, nil
}

// Item reads an archive's full metadata object.
func (a *Archives) Item(id []byte) (*item.ArchiveItem, error) {
	obj, err := a.repo.Get(id)
	if err != nil {
		return nil, err
	}
	_, data, err := a.manifest.ro.Parse(id, obj, repoobj.TypeArchiveMeta, repoobj.ParseOptions{})
	if err != nil {
		return nil, err
	}
	return item.UnmarshalArchiveItem(data)
}

// ListOptions filter and order a listing.
type ListOptions struct {
	// Match selects archives. Each entry is one of borg's selectors: "aid:<hex prefix>",
	// "tags:a,b", "user:<name>", "host:<name>", or a name pattern (optionally "name:").
	// They are applied in order, each narrowing the result.
	Match []string
	// Deleted lists soft-deleted archives instead of live ones.
	Deleted bool
	// SortBy names fields to sort on, applied in reverse order so the first is primary.
	// Valid: "timestamp"/"ts", "archive"/"name", "id", "host", "user", "tags".
	SortBy []string
	// Reverse flips the final order.
	Reverse bool
	// First and Last keep only that many entries from the respective end.
	First, Last int
	// Newer and Older bound the timestamp, inclusive.
	Newer, Older time.Time
}

// IsZero reports whether no filter or ordering was asked for, so this is the plain
// "every archive" list.
//
// A command whose meaning is repository-wide uses it to refuse an archive filter rather
// than silently applying one: "shared between names" and "unreferenced" are only true
// statements about the whole repository.
func (o ListOptions) IsZero() bool {
	return len(o.Match) == 0 && !o.Deleted && len(o.SortBy) == 0 && !o.Reverse &&
		o.First == 0 && o.Last == 0 && o.Newer.IsZero() && o.Older.IsZero()
}

// List returns the archives matching the options, sorted.
//
// Every filter defaults to "do not filter", so a zero ListOptions produces the complete
// list. Several callers depend on that: a partial list where a full one was meant is the
// kind of bug that silently skips archives.
func (a *Archives) List(opts ListOptions) ([]Info, error) {
	ids, err := a.IDs(opts.Deleted)
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(ids))
	for _, id := range ids {
		info, err := a.Get(id)
		if err != nil {
			return nil, err
		}
		infos = append(infos, *info)
	}

	for _, match := range opts.Match {
		infos, err = applyMatch(infos, match)
		if err != nil {
			return nil, err
		}
	}

	if !opts.Newer.IsZero() || !opts.Older.IsZero() {
		var kept []Info
		for _, info := range infos {
			if !opts.Newer.IsZero() && info.Time.Before(opts.Newer) {
				continue
			}
			if !opts.Older.IsZero() && info.Time.After(opts.Older) {
				continue
			}
			kept = append(kept, info)
		}
		infos = kept
	}

	// borg's default order is by timestamp, and it is the only order that makes a listing
	// readable; sorting keys are applied in reverse so the first named key wins.
	sortKeys := opts.SortBy
	if len(sortKeys) == 0 {
		sortKeys = []string{"timestamp"}
	}
	for i := len(sortKeys) - 1; i >= 0; i-- {
		if err := sortInfos(infos, sortKeys[i]); err != nil {
			return nil, err
		}
	}

	if opts.First > 0 && len(infos) > opts.First {
		infos = infos[:opts.First]
	}
	if opts.Last > 0 && len(infos) > opts.Last {
		infos = infos[len(infos)-opts.Last:]
	}
	if opts.Reverse {
		for i, j := 0, len(infos)-1; i < j; i, j = i+1, j-1 {
			infos[i], infos[j] = infos[j], infos[i]
		}
	}
	return infos, nil
}

func applyMatch(infos []Info, match string) ([]Info, error) {
	var kept []Info
	switch {
	case strings.HasPrefix(match, "aid:"):
		want := strings.TrimPrefix(match, "aid:")
		for _, info := range infos {
			if strings.HasPrefix(hex.EncodeToString(info.ID), want) {
				kept = append(kept, info)
			}
		}
		// borg insists an id match is unambiguous, and so does borge: an id prefix is
		// used where exactly one archive is meant, so matching two is a mistake the
		// caller needs told about rather than a shorter list.
		if len(kept) != 1 {
			return nil, fmt.Errorf("manifest: %q matches %d archives, an archive id match has to match exactly one",
				match, len(kept))
		}
	case strings.HasPrefix(match, "tags:"):
		want := splitNonEmpty(strings.TrimPrefix(match, "tags:"), ",")
		for _, info := range infos {
			have := map[string]bool{}
			for _, t := range info.Tags {
				have[t] = true
			}
			all := true
			for _, t := range want {
				if !have[t] {
					all = false
					break
				}
			}
			if all {
				kept = append(kept, info)
			}
		}
	case strings.HasPrefix(match, "user:"):
		want := strings.TrimPrefix(match, "user:")
		for _, info := range infos {
			if info.User == want {
				kept = append(kept, info)
			}
		}
	case strings.HasPrefix(match, "host:"):
		want := strings.TrimPrefix(match, "host:")
		for _, info := range infos {
			if info.Host == want {
				kept = append(kept, info)
			}
		}
	default:
		want := strings.TrimPrefix(match, "name:")
		re, err := patterns.CompileName(want)
		if err != nil {
			return nil, err
		}
		for _, info := range infos {
			if re.MatchString(info.Name) {
				kept = append(kept, info)
			}
		}
	}
	return kept, nil
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortInfos(infos []Info, key string) error {
	var less func(i, j int) bool
	switch key {
	case "timestamp", "ts":
		less = func(i, j int) bool { return infos[i].Time.Before(infos[j].Time) }
	case "archive", "name":
		less = func(i, j int) bool { return infos[i].Name < infos[j].Name }
	case "id":
		less = func(i, j int) bool { return string(infos[i].ID) < string(infos[j].ID) }
	case "host":
		less = func(i, j int) bool { return infos[i].Host < infos[j].Host }
	case "user":
		less = func(i, j int) bool { return infos[i].User < infos[j].User }
	case "tags":
		less = func(i, j int) bool {
			return strings.Join(infos[i].Tags, ",") < strings.Join(infos[j].Tags, ",")
		}
	default:
		return fmt.Errorf("manifest: cannot sort archives by %q", key)
	}
	sort.SliceStable(infos, less)
	return nil
}

// Names returns the names of the repository's archives, in listing order.
func (a *Archives) Names() ([]string, error) {
	infos, err := a.List(ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Name)
	}
	return out, nil
}

// ByName returns the archive with this name, or nil.
//
// Names are not unique. When several archives share one, the last in listing order wins,
// which - the default order being by timestamp - means the most recent. That is what a
// user naming an archive without an id almost always means.
func (a *Archives) ByName(name string) (*Info, error) {
	infos, err := a.List(ListOptions{})
	if err != nil {
		return nil, err
	}
	var found *Info
	for i := range infos {
		if infos[i].Exists && infos[i].Name == name {
			found = &infos[i]
		}
	}
	return found, nil
}

// Exists reports whether an archive of this name exists.
func (a *Archives) Exists(name string) (bool, error) {
	info, err := a.ByName(name)
	return info != nil, err
}

// ExistsID reports whether this archive id is in the directory.
func (a *Archives) ExistsID(id []byte, deleted bool) (bool, error) {
	ids, err := a.IDs(deleted)
	if err != nil {
		return false, err
	}
	for _, have := range ids {
		if string(have) == string(id) {
			return true, nil
		}
	}
	return false, nil
}

// Create adds a directory entry pointing at an archive metadata object.
//
// The repository is flushed first. The pointer must not become visible while the object
// it names is still buffered in the pack writer, or a concurrent reader would find an
// archive it cannot read.
func (a *Archives) Create(id []byte) error {
	if len(id) != repoobj.ChunkIDSize {
		return fmt.Errorf("manifest: archive id must be %d bytes, got %d", repoobj.ChunkIDSize, len(id))
	}
	if err := a.repo.Flush(); err != nil {
		return err
	}
	return a.repo.Store().Store(archiveName(id), nil)
}

// Delete soft-deletes an archive: the pointer is renamed, not removed.
func (a *Archives) Delete(id []byte) error {
	return a.repo.Store().SoftDelete(archiveName(id))
}

// Undelete restores a soft-deleted archive.
func (a *Archives) Undelete(id []byte) error {
	return a.repo.Store().Undelete(archiveName(id))
}

// Nuke removes a soft-deleted archive's pointer for good.
//
// It only touches the directory entry. The archive's objects stay until a compaction
// finds them unreferenced, which is what makes this safe to call without a reference
// count in hand.
func (a *Archives) Nuke(id []byte) error {
	return a.repo.Store().Delete(archiveName(id), true)
}
