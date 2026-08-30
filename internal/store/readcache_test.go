// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The open-handle cache is a performance fix in the one place a performance fix can
// silently return the wrong bytes: a descriptor outlives the file it was opened on, so a
// cached handle can serve data that has been deleted or replaced.
//
// That is not hypothetical. TestWritethroughCache failed when this was first written
// unconditionally, because flushing the local cache removes files behind the backend's
// back and the stale handle went on answering. Hence SetReadCache, and hence these.

func newCachedFS(t *testing.T) (*PosixFS, string) {
	t.Helper()
	dir := t.TempDir()
	b, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Create(); err != nil {
		t.Fatal(err)
	}
	if err := b.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.SetReadCache(true)
	return b, dir
}

const objName = "packs/" + "ab" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

// TestReadCacheServesTheSameBytes over repeated reads, ranges and the negative offset
// borgstore defines, because the cache replaced Seek with ReadAt and a positional bug
// would show up as the wrong slice rather than as an error.
func TestReadCacheServesTheSameBytes(t *testing.T) {
	b, _ := newCachedFS(t)
	value := make([]byte, 5000)
	for i := range value {
		value[i] = byte(i * 7)
	}
	if err := b.Store(objName, value); err != nil {
		t.Fatal(err)
	}

	for round := range 3 {
		for _, r := range []struct{ offset, size int64 }{
			{0, 49}, {49, 49}, {0, -1}, {4000, 1000}, {-100, 100}, {4990, 50},
		} {
			got, err := b.Load(objName, r.offset, r.size)
			if err != nil {
				t.Fatalf("round %d (%d, %d): %v", round, r.offset, r.size, err)
			}
			start := r.offset
			if start < 0 {
				start = int64(len(value)) + start
			}
			end := int64(len(value))
			if r.size >= 0 && start+r.size < end {
				end = start + r.size
			}
			if want := value[start:end]; !bytes.Equal(got, want) {
				t.Errorf("round %d (%d, %d): got %d bytes, want %d",
					round, r.offset, r.size, len(got), len(want))
			}
		}
	}
}

// TestReadCacheForgetsADeletedObject. A descriptor kept past an unlink still reads on
// POSIX, so without invalidation Delete would leave the object readable.
func TestReadCacheForgetsADeletedObject(t *testing.T) {
	b, _ := newCachedFS(t)
	if err := b.Store(objName, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Load(objName, 0, -1); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(objName); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Load(objName, 0, -1); err == nil {
		t.Fatal("a deleted object was still readable; the cached handle outlived it")
	}
}

// TestReadCacheSeesAReplacedObject. Store writes a temp file and renames over the name, so
// a held descriptor refers to the old inode. The bytes must be the new ones.
func TestReadCacheSeesAReplacedObject(t *testing.T) {
	b, _ := newCachedFS(t)
	if err := b.Store(objName, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Load(objName, 0, -1); err != nil {
		t.Fatal(err)
	}
	if err := b.Store(objName, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := b.Load(objName, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("after a rewrite the cache served %q, want %q", got, "second")
	}
}

// TestReadCacheOffByDefault is the guard on the decision itself. A backend whose files are
// removed underneath it - the local writethrough cache - must not hold descriptors, and
// the default has to be the safe one so that a new caller gets it right by doing nothing.
func TestReadCacheOffByDefault(t *testing.T) {
	dir := t.TempDir()
	b, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Create(); err != nil {
		t.Fatal(err)
	}
	if err := b.Open(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := b.Store(objName, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Load(objName, 0, -1); err != nil {
		t.Fatal(err)
	}
	// Removed behind the backend's back, as a cache flush does.
	path := filepath.Join(dir, filepath.FromSlash(objName[:len(objName)-60]), objName[len(objName)-60:])
	_ = os.RemoveAll(filepath.Join(dir, "packs"))
	_ = path
	if _, err := b.Load(objName, 0, -1); err == nil {
		t.Fatal("an object removed behind the backend was still readable with the cache off")
	}
}

// TestReadCacheIsBounded, so a listing of many objects cannot exhaust the descriptor
// table. The bound is the reason this is a cache rather than a map that only grows.
func TestReadCacheIsBounded(t *testing.T) {
	b, _ := newCachedFS(t)
	for i := range maxOpenHandles * 3 {
		name := "packs/" + hexName(i)
		if err := b.Store(name, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Load(name, 0, -1); err != nil {
			t.Fatal(err)
		}
	}
	b.handleMu.Lock()
	defer b.handleMu.Unlock()
	if len(b.handles) > maxOpenHandles {
		t.Errorf("the cache holds %d handles, over the %d bound", len(b.handles), maxOpenHandles)
	}
	if len(b.handleAge) != len(b.handles) {
		t.Errorf("the age list has %d entries and the map %d; eviction is losing track",
			len(b.handleAge), len(b.handles))
	}
}

func hexName(i int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for j := range out {
		out[j] = digits[(i+j)%16]
	}
	return string(out)
}
