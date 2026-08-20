// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ItemDiff in borg's src/borg/item.pyx and
// Archive.compare_archives_iter in src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"encoding/hex"
	"fmt"
	"iter"
	"time"

	"github.com/renesugar/borge/internal/item"
)

// Comparing two archives.
//
// # Why the chunk lists can usually answer it
//
// Two files are the same if their chunk lists are the same - no content has to be read at
// all. That holds only while both archives were chunked the same way: with different
// --chunker-params the same bytes produce different chunk ids, and the comparison would
// report every file as modified. So the caller says whether the chunk ids are comparable,
// and when they are not, a differing file is reported as "modified" without the byte
// counts, rather than by reading both archives in full.

// ChangeKind names one kind of difference.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
	ChangeLink     ChangeKind = "link"
	ChangeMode     ChangeKind = "mode"
	ChangeType     ChangeKind = "type"
	ChangeOwner    ChangeKind = "owner"
	ChangeMTime    ChangeKind = "mtime"
	ChangeCTime    ChangeKind = "ctime"
)

// Change is one difference between two versions of a path.
type Change struct {
	Kind ChangeKind
	// Description is the human-readable form, e.g. "changed mode" or "+1.2 kB -300 B".
	Description string
	// Added and Removed are byte counts, set only for a content change where the chunk
	// ids could be compared.
	Added, Removed int64
	// From and To are the old and new values, for the metadata changes.
	From, To string
	// ItemKind is borg's name for the kind of thing that appeared or disappeared, set
	// only for a presence change: "link", "directory", "blkdev", "chrdev" or "fifo", and
	// empty for a regular file. borg files those under a key of their own rather than
	// under "content" - "borg diff --format '{directory}'" reports a directory that came
	// or went and nothing else - so the kind has to survive as more than prose in the
	// description. See DIVERGENCES.md #47.
	ItemKind string
	// FromJSON and ToJSON are those same values in the form borg's --json-lines carries,
	// where it differs from the text one: a timestamp is ISO-8601 rather than borg's
	// human layout, and an owner change is a two-element array rather than "user:group".
	// Nil means the text form is also the JSON form. Kept beside From and To rather than
	// derived later because only the comparison still has the unformatted values.
	FromJSON, ToJSON any
}

// Diff is the set of changes to one path.
type Diff struct {
	Path    string
	Changes []Change

	// Item1 and Item2 are the two versions, nil where the path is absent from that
	// archive. They are carried because --sort-by sorts by fields no Change records -
	// the owner, the timestamps, the size in the second archive - and borg's ItemDiff
	// holds both items for exactly that reason.
	Item1, Item2 *item.Item
}

// Empty reports whether the two versions are the same.
func (d *Diff) Empty() bool { return len(d.Changes) == 0 }

// DiffOptions control a comparison.
type DiffOptions struct {
	// CanCompareChunkIDs says whether both archives used the same chunker parameters. When
	// false, content differences are reported without byte counts.
	CanCompareChunkIDs bool
	// NumericIDs compares uid and gid rather than user and group names.
	NumericIDs bool
	// ContentOnly leaves out the metadata-only changes: mode, owner, times.
	ContentOnly bool
	// Filter selects which paths are compared. Nil compares everything.
	Filter func(path string) bool
}

// Compare walks two archives and reports the differences.
//
// # Order, and why it is not sorted
//
// borg zips the two item streams positionally and yields a comparison the moment both
// sides produce the same path; a path that appears in only one stream is held aside as an
// *orphan* until the other stream produces it, or until both streams end. Whatever is
// still orphaned then is emitted at the end - archive2's leftovers first, as added, then
// archive1's, as removed.
//
// borge used to collect both streams into maps and walk the union sorted by path. The
// comment here called that "the same answer with less state", and it was not: it is borg's
// "--sort-by path" output, not borg's default, so every line matched borg's and the order
// did not. Two archives of the same tree usually stream in the same order, which is why it
// went unnoticed - the difference only shows when a path is added or removed, or when the
// tree was archived from roots given in a different order.
//
// Sorting now belongs to --sort-by, where borg puts it, and this function's job is to
// produce borg's order.
//
// # Streaming both sides at once
//
// The positional zip needs both streams stepped in lockstep, which a callback iterator
// cannot do. iter.Pull2 turns each into a pull iterator; only one of them runs at a time,
// and each holds its own unpacker and reads whole objects through repo.Get, so their reads
// interleave exactly as two sequential walks' would. What this buys is borg's memory
// profile: only the orphans are held, where the old map walk held every item of both
// archives - for two archives of the same tree, everything against nothing.
func Compare(a, b *Archive, opts DiffOptions, fn func(Diff) error) error {
	next1, stop1 := iter.Pull2(itemSeq(a, opts.Filter))
	defer stop1()
	next2, stop2 := iter.Pull2(itemSeq(b, opts.Filter))
	defer stop2()

	// Orphans keep insertion order: they are emitted in the order the stream produced
	// them, as Python's dicts do, so a tail of added files reads the way the archive was
	// written rather than in a hash order that changes between runs.
	orphans1 := newOrphans()
	orphans2 := newOrphans()

	emit := func(path string, x, y *item.Item) error {
		d := diffItems(path, x, y, opts)
		if d.Empty() {
			return nil
		}
		return fn(d)
	}

	for {
		it1, err1, ok1 := next1()
		if err1 != nil {
			return err1
		}
		it2, err2, ok2 := next2()
		if err2 != nil {
			return err2
		}
		if !ok1 && !ok2 {
			break
		}

		if ok1 && ok2 && it1.Path == it2.Path {
			if err := emit(it1.Path, it1, it2); err != nil {
				return err
			}
			continue
		}
		if ok1 {
			if partner := orphans2.take(it1.Path); partner != nil {
				if err := emit(it1.Path, it1, partner); err != nil {
					return err
				}
			} else {
				orphans1.put(it1)
			}
		}
		if ok2 {
			if partner := orphans1.take(it2.Path); partner != nil {
				if err := emit(it2.Path, partner, it2); err != nil {
					return err
				}
			} else {
				orphans2.put(it2)
			}
		}
	}

	// What is left had no partner in the other archive: present only in archive2 is an
	// addition, present only in archive1 is a removal.
	for _, added := range orphans2.rest() {
		if err := emit(added.Path, nil, added); err != nil {
			return err
		}
	}
	for _, deleted := range orphans1.rest() {
		if err := emit(deleted.Path, deleted, nil); err != nil {
			return err
		}
	}
	return nil
}

// itemSeq turns an archive's callback walk into a pull-able sequence.
//
// The sentinel is ErrStopIteration, which Items treats as "stop, not a failure": without
// it, abandoning the walk early - which iter.Pull2's stop does on every return path -
// would read the rest of the item stream for nothing.
func itemSeq(a *Archive, filter func(string) bool) iter.Seq2[*item.Item, error] {
	return func(yield func(*item.Item, error) bool) {
		stopped := false
		err := a.Items(func(it *item.Item) error {
			if filter != nil && !filter(it.Path) {
				return nil
			}
			if !yield(it, nil) {
				stopped = true
				return ErrStopIteration
			}
			return nil
		})
		if err != nil && !stopped {
			yield(nil, err)
		}
	}
}

// orphans is an insertion-ordered set of items keyed by path.
type orphans struct {
	order []*item.Item
	index map[string]int
}

func newOrphans() *orphans { return &orphans{index: map[string]int{}} }

func (o *orphans) put(it *item.Item) {
	// The same path twice in one stream is not something a borg archive holds, but the
	// behaviour is defined rather than left to chance: Python's "orphans[path] = item" on
	// a key already present replaces the value and leaves the position alone, so the later
	// item is kept and it is kept where the first one was.
	if i, dup := o.index[it.Path]; dup {
		o.order[i] = it
		return
	}
	o.index[it.Path] = len(o.order)
	o.order = append(o.order, it)
}

// take removes and returns the item at path, or nil.
func (o *orphans) take(path string) *item.Item {
	i, ok := o.index[path]
	if !ok {
		return nil
	}
	it := o.order[i]
	o.order[i] = nil
	delete(o.index, path)
	return it
}

// rest returns the items still held, in the order they were added.
func (o *orphans) rest() []*item.Item {
	out := make([]*item.Item, 0, len(o.index))
	for _, it := range o.order {
		if it != nil {
			out = append(out, it)
		}
	}
	return out
}

// diffItems compares one path's two versions. Either may be nil, meaning absent.
func diffItems(path string, a, b *item.Item, opts DiffOptions) Diff {
	d := Diff{Path: path, Item1: a, Item2: b}
	add := func(c Change) { d.Changes = append(d.Changes, c) }

	switch {
	case a == nil && b == nil:
		return d
	case a == nil:
		add(Change{Kind: ChangeAdded, Description: "added " + describeKind(b),
			Added: contentSize(b), ItemKind: borgItemKind(b)})
		return d
	case b == nil:
		add(Change{Kind: ChangeRemoved, Description: "removed " + describeKind(a),
			Removed: contentSize(a), ItemKind: borgItemKind(a)})
		return d
	}

	// A symlink whose target changed is a content change, not a metadata one: what the
	// path resolves to is the whole of what a symlink is.
	if a.IsSymlink() || b.IsSymlink() {
		ta, tb := "", ""
		if a.Target != nil {
			ta = *a.Target
		}
		if b.Target != nil {
			tb = *b.Target
		}
		if ta != tb {
			add(Change{Kind: ChangeLink, Description: "changed link", From: ta, To: tb})
		}
	}

	if a.ChunksSet && b.ChunksSet {
		if c, changed := contentChange(a, b, opts.CanCompareChunkIDs); changed {
			add(c)
		}
	}

	if opts.ContentOnly {
		return d
	}

	// Mode, and the type character separately: a file replaced by a directory is a
	// different thing from a file whose permissions changed, and reporting both as
	// "changed mode" hides it.
	if a.Mode != nil && b.Mode != nil && *a.Mode != *b.Mode {
		ma, mb := item.FormatMode(*a.Mode), item.FormatMode(*b.Mode)
		add(Change{Kind: ChangeMode, Description: "changed mode", From: ma, To: mb})
		if ma[0] != mb[0] {
			add(Change{Kind: ChangeType, Description: "changed type", From: ma[:1], To: mb[:1]})
		}
	}

	if from, to, changed := ownerChange(a, b, opts.NumericIDs); changed {
		add(Change{
			Kind: ChangeOwner, Description: "changed owner", From: from, To: to,
			FromJSON: ownerPair(a, opts.NumericIDs), ToJSON: ownerPair(b, opts.NumericIDs),
		})
	}

	for _, tc := range []struct {
		kind ChangeKind
		a, b *int64
	}{
		{ChangeMTime, a.MTime, b.MTime},
		{ChangeCTime, a.CTime, b.CTime},
	} {
		if tc.a != nil && tc.b != nil && *tc.a != *tc.b {
			add(Change{
				Kind:        tc.kind,
				Description: "changed " + string(tc.kind),
				From:        formatDiffTime(*tc.a),
				To:          formatDiffTime(*tc.b),
				FromJSON:    isoDiffTime(*tc.a),
				ToJSON:      isoDiffTime(*tc.b),
			})
		}
	}
	return d
}

// contentChange compares two files' contents.
func contentChange(a, b *item.Item, canCompareIDs bool) (Change, bool) {
	if !canCompareIDs {
		// Different chunker parameters: the ids are not comparable, so the only honest
		// answer without reading both archives in full is whether the sizes differ.
		if a.ContentSize() != b.ContentSize() {
			return Change{Kind: ChangeModified, Description: "modified"}, true
		}
		return Change{}, false
	}

	if sameChunks(a.Chunks, b.Chunks) {
		return Change{}, false
	}
	inA := map[string]int64{}
	for _, c := range a.Chunks {
		inA[string(c.ID)] = c.Size
	}
	inB := map[string]int64{}
	for _, c := range b.Chunks {
		inB[string(c.ID)] = c.Size
	}
	var added, removed int64
	for id, size := range inB {
		if _, ok := inA[id]; !ok {
			added += size
		}
	}
	for id, size := range inA {
		if _, ok := inB[id]; !ok {
			removed += size
		}
	}
	return Change{
		Kind:        ChangeModified,
		Description: fmt.Sprintf("+%d B -%d B", added, removed),
		Added:       added,
		Removed:     removed,
	}, true
}

func sameChunks(a, b []item.ChunkListEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Size != b[i].Size || hex.EncodeToString(a[i].ID) != hex.EncodeToString(b[i].ID) {
			return false
		}
	}
	return true
}

func ownerChange(a, b *item.Item, numericIDs bool) (string, string, bool) {
	if numericIDs {
		ua, ga := derefInt(a.UID), derefInt(a.GID)
		ub, gb := derefInt(b.UID), derefInt(b.GID)
		if ua == ub && ga == gb {
			return "", "", false
		}
		return fmt.Sprintf("%d:%d", ua, ga), fmt.Sprintf("%d:%d", ub, gb), true
	}
	ua, ga := derefStr(a.User), derefStr(a.Group)
	ub, gb := derefStr(b.User), derefStr(b.Group)
	if ua == ub && ga == gb {
		return "", "", false
	}
	return ua + ":" + ga, ub + ":" + gb, true
}

func derefInt(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func contentSize(it *item.Item) int64 {
	if it == nil {
		return 0
	}
	return it.ContentSize()
}

// describeKind names what an added or removed path was, so "added directory" and "added
// file" are distinguishable in a listing.
func describeKind(it *item.Item) string {
	switch {
	case it == nil:
		return "entry"
	case it.IsDir():
		return "directory"
	case it.IsSymlink():
		return "link"
	case it.IsFIFO():
		return "fifo"
	case it.IsDevice():
		return "device"
	default:
		return fmt.Sprintf("%d B", it.ContentSize())
	}
}

func formatDiffTime(ns int64) string {
	return time.Unix(0, ns).Local().Format("Mon, 2006-01-02 15:04:05 -0700")
}

// isoDiffTime is a timestamp as the JSON forms carry it: ISO-8601 with microseconds, the
// same spelling every other JSON timestamp uses. The text form (formatDiffTime) is borg's
// human layout, and the two are not interchangeable.
func isoDiffTime(ns int64) string {
	return time.Unix(0, ns).Local().Format("2006-01-02T15:04:05.000000-07:00")
}

// ownerPair is an item's owner as borg's JSON reports it: a two-element array of user and
// group, where the text form is one "user:group" string.
func ownerPair(it *item.Item, numeric bool) []any {
	if numeric {
		return []any{derefInt(it.UID), derefInt(it.GID)}
	}
	return []any{derefStr(it.User), derefStr(it.Group)}
}

// borgItemKind is borg's name for an item's type, as its diff uses it for a presence
// change. A regular file has no name here: borg reports it under "content" with sizes,
// where the others get a key each.
//
// Block and character devices are separate names, unlike borge's own describeKind which
// calls both "device": borg's format keys are {blkdev} and {chrdev}, so the distinction
// has to be kept even though nothing else in borge needs it.
func borgItemKind(it *item.Item) string {
	switch {
	case it == nil:
		return ""
	case it.IsSymlink():
		return "link"
	case it.IsDir():
		return "directory"
	case it.IsFIFO():
		return "fifo"
	case it.IsDevice():
		if it.ModeOr(0)&item.SIFMT == item.SIFBLK {
			return "blkdev"
		}
		return "chrdev"
	default:
		return ""
	}
}
