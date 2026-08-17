// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"bytes"
	"testing"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/msgpackx"
)

// packItem builds one item exactly as the stream stores it: a msgpack map whose keys are
// sorted, so "path" is not necessarily first.
func packItem(t *testing.T, path string, mtime int64) []byte {
	t.Helper()
	it := &item.Item{Path: path, MTime: &mtime}
	b, err := it.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestUnpacker(t *testing.T) *RobustUnpacker {
	t.Helper()
	u, err := NewRobustUnpacker(manifest.ItemKeys, func(m *msgpackx.Map) bool {
		// borg's validator for the item stream: it has to have a path.
		_, ok := m.Get("path")
		return ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func drain(t *testing.T, u *RobustUnpacker) []string {
	t.Helper()
	var paths []string
	for {
		m, ok, err := u.Next()
		if err != nil {
			t.Fatalf("unpack: %v", err)
		}
		if !ok {
			return paths
		}
		it, err := item.DecodeItem(m)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		paths = append(paths, it.Path)
	}
}

func TestRobustUnpackerReadsAnUndamagedStream(t *testing.T) {
	u := newTestUnpacker(t)
	var stream []byte
	want := []string{"a", "b", "c"}
	for i, p := range want {
		stream = append(stream, packItem(t, p, int64(i))...)
	}
	u.Feed(stream)

	got := drain(t, u)
	if len(got) != len(want) {
		t.Fatalf("read %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRobustUnpackerResyncsAfterAGap is the whole point: a chunk goes missing and the
// items after it are still recovered, rather than the rest of the archive being lost.
func TestRobustUnpackerResyncsAfterAGap(t *testing.T) {
	u := newTestUnpacker(t)

	// Before the gap.
	u.Feed(packItem(t, "before-1", 1))
	u.Feed(packItem(t, "before-2", 2))
	if got := drain(t, u); len(got) != 2 {
		t.Fatalf("read %v before the gap, want 2 items", got)
	}

	// A chunk is missing. What arrives next starts mid-item.
	u.Resync()
	broken := packItem(t, "damaged-item-that-was-cut", 3)
	tail := broken[len(broken)/2:] // the second half of an item, with no header
	after := append(append([]byte(nil), tail...), packItem(t, "after-1", 4)...)
	after = append(after, packItem(t, "after-2", 5)...)
	u.Feed(after)

	got := drain(t, u)
	if len(got) != 2 || got[0] != "after-1" || got[1] != "after-2" {
		t.Errorf("after a gap the unpacker read %v, want [after-1 after-2]", got)
	}
	if u.Resyncing() {
		t.Error("the unpacker is still resyncing after finding items")
	}
}

// TestRobustUnpackerResyncsAcrossAFeedBoundary: the item that ends the gap may itself be
// split, so the scan must not walk past a partial header at the end of the buffer.
func TestRobustUnpackerResyncsAcrossAFeedBoundary(t *testing.T) {
	u := newTestUnpacker(t)
	u.Resync()

	next := packItem(t, "recovered", 7)
	u.Feed(next[:3])
	if got := drain(t, u); len(got) != 0 {
		t.Fatalf("read %v from a partial item", got)
	}
	u.Feed(next[3:])

	got := drain(t, u)
	if len(got) != 1 || got[0] != "recovered" {
		t.Errorf("read %v, want [recovered]", got)
	}
}

func TestRobustUnpackerRejectsNonItems(t *testing.T) {
	u := newTestUnpacker(t)
	u.Resync()

	// A map that decodes cleanly but is not an item: the validator has to reject it, or
	// a resync could latch onto arbitrary data.
	notAnItem := msgpackx.NewStableMap()
	notAnItem.Set("nonsense", int64(1))
	packed, err := msgpackx.Marshal(notAnItem)
	if err != nil {
		t.Fatal(err)
	}
	u.Feed(append(packed, packItem(t, "real", 1)...))

	got := drain(t, u)
	if len(got) != 1 || got[0] != "real" {
		t.Errorf("read %v, want only the real item", got)
	}
}

func TestValidMsgpackedDict(t *testing.T) {
	pathKey, err := msgpackx.Marshal("path")
	if err != nil {
		t.Fatal(err)
	}
	keys := [][]byte{pathKey}

	good := packItem(t, "x", 1)
	// The item's keys are sorted, so "path" is not first; build one where it is.
	m := msgpackx.NewMap()
	m.Set("path", "x")
	firstIsPath, err := msgpackx.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	if !ValidMsgpackedDict(firstIsPath, keys) {
		t.Error("a map starting with 'path' was rejected")
	}
	if ValidMsgpackedDict(nil, keys) {
		t.Error("empty data was accepted")
	}
	if ValidMsgpackedDict([]byte{0x91, 0x01}, keys) {
		t.Error("an array was accepted as a map")
	}
	if ValidMsgpackedDict([]byte{0x81, 0x01}, keys) {
		t.Error("a map with an integer key was accepted")
	}
	// A map32 header is refused on purpose, even though it is valid msgpack.
	if ValidMsgpackedDict(append([]byte{0xDF, 0, 0, 0, 1}, pathKey...), keys) {
		t.Error("a map32 was accepted")
	}
	// And a real item, whose first key is not 'path', is correctly not a candidate for
	// this key set - which is why the caller passes the whole item_keys set.
	if bytes.HasPrefix(good[1:], pathKey) {
		t.Skip("item encoding changed; this assertion no longer means anything")
	}
	if ValidMsgpackedDict(good, keys) {
		t.Error("a map whose first key is not in the set was accepted")
	}
}
