// SPDX-License-Identifier: Apache-2.0

package hashindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Unit and property tests that run without the borg venv.

func idFor(i int) [ChunkIDSize]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("id-%d", i)))
}

func entryFor(i int) Entry {
	return Entry{
		Flags:     uint32(i%4) | FUsed,
		Size:      uint32(i * 3),
		PackID:    sha256.Sum256([]byte(fmt.Sprintf("pack-%d", i/10))),
		ObjOffset: uint32(i * 17),
		ObjSize:   uint32(i + 1),
	}
}

// TestAgainstReferenceMap is the property test the plan calls for: a million randomised
// operations against a plain Go map, which is the obvious-but-wrong implementation.
//
// The table is a hand-written open-addressed structure with tombstones, growth and
// shrink paths; the operations that break such a thing are specific interleavings of
// insert, overwrite and delete around a resize, which is exactly what a random walk
// finds and a hand-written test does not.
func TestAgainstReferenceMap(t *testing.T) {
	const ops = 1_000_000
	// A small key space so collisions, overwrites and re-inserts of deleted keys all
	// happen often. With a million distinct keys almost every operation would be a
	// first insert, and the tombstone paths would barely run.
	const keySpace = 20000

	table, err := NewTable(ChunkIDSize, EntrySize, minCapacity)
	if err != nil {
		t.Fatal(err)
	}
	reference := make(map[[ChunkIDSize]byte][EntrySize]byte)

	rnd := rand.New(rand.NewSource(20260816))
	var value [EntrySize]byte

	for i := 0; i < ops; i++ {
		key := idFor(rnd.Intn(keySpace))

		switch n := rnd.Intn(100); {
		case n < 55: // set
			rnd.Read(value[:])
			if err := table.Set(key[:], value[:]); err != nil {
				t.Fatalf("op %d: Set: %v", i, err)
			}
			reference[key] = value

		case n < 80: // get
			got, ok := table.Get(key[:])
			want, wantOK := reference[key]
			if ok != wantOK {
				t.Fatalf("op %d: Get(%x) present=%v, want %v", i, key[:8], ok, wantOK)
			}
			if ok && !bytes.Equal(got, want[:]) {
				t.Fatalf("op %d: Get(%x) = %x, want %x", i, key[:8], got, want)
			}

		case n < 90: // contains
			if table.Contains(key[:]) != mapHas(reference, key) {
				t.Fatalf("op %d: Contains(%x) disagrees with the reference", i, key[:8])
			}

		default: // delete
			got := table.Delete(key[:])
			_, want := reference[key]
			if got != want {
				t.Fatalf("op %d: Delete(%x) = %v, want %v", i, key[:8], got, want)
			}
			delete(reference, key)
		}

		if table.Len() != len(reference) {
			t.Fatalf("op %d: table has %d entries, reference has %d", i, table.Len(), len(reference))
		}
	}

	// Finally, the whole contents must agree, in both directions.
	seen := 0
	table.Iterate(func(key, value []byte) bool {
		var k [ChunkIDSize]byte
		copy(k[:], key)
		want, ok := reference[k]
		if !ok {
			t.Fatalf("table holds %x, which the reference does not", key[:8])
		}
		if !bytes.Equal(value, want[:]) {
			t.Fatalf("value for %x differs", key[:8])
		}
		seen++
		return true
	})
	if seen != len(reference) {
		t.Fatalf("iteration produced %d entries, want %d", seen, len(reference))
	}
	t.Logf("%d operations, %d entries remaining, capacity %d", ops, table.Len(), table.Capacity())
}

// TestTombstonesDoNotHideKeys: linear probing means a deleted entry has to leave a
// tombstone rather than a free slot, or keys inserted after it become unreachable.
// This builds the collision chain deliberately instead of hoping the random walk hits it.
func TestTombstonesDoNotHideKeys(t *testing.T) {
	table, err := NewTable(4, 4, minCapacity)
	if err != nil {
		t.Fatal(err)
	}

	// bucketFor uses the first four bytes as a big-endian uint32 modulo capacity, so
	// these three keys all start at bucket 5 and form one probe chain.
	keys := [][]byte{
		{0x00, 0x00, 0x00, 0x05},
		{0x00, 0x00, 0x03, 0xed}, // 1005 = 5 + 1000
		{0x00, 0x00, 0x07, 0xd5}, // 2005 = 5 + 2*1000
	}
	for i, k := range keys {
		if err := table.Set(k, []byte{byte(i), 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}
	if table.bucketFor(keys[0]) != table.bucketFor(keys[1]) ||
		table.bucketFor(keys[1]) != table.bucketFor(keys[2]) {
		t.Fatalf("precondition failed: the keys do not collide (%d, %d, %d)",
			table.bucketFor(keys[0]), table.bucketFor(keys[1]), table.bucketFor(keys[2]))
	}

	// Remove the first link in the chain. The other two must remain reachable.
	if !table.Delete(keys[0]) {
		t.Fatal("delete failed")
	}
	for i := 1; i < len(keys); i++ {
		v, ok := table.Get(keys[i])
		if !ok {
			t.Fatalf("key %d became unreachable after deleting the head of its probe chain", i)
		}
		if v[0] != byte(i) {
			t.Errorf("key %d has value %x", i, v)
		}
	}
	if table.Contains(keys[0]) {
		t.Error("the deleted key is still present")
	}
}

func TestSystemFlagsAreHidden(t *testing.T) {
	c, err := NewChunkIndex(10)
	if err != nil {
		t.Fatal(err)
	}
	id := idFor(1)

	// A caller cannot set a system flag.
	if err := c.Set(id[:], Entry{Flags: FUsed | FNew}); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get(id[:])
	if !ok {
		t.Fatal("entry missing")
	}
	if got.Flags&MaskSystem != 0 {
		t.Errorf("system flags visible to the caller: %#x", got.Flags)
	}
	if got.Flags != FUsed {
		t.Errorf("flags = %#x, want %#x", got.Flags, uint32(FUsed))
	}

	// But the index does track FNew internally: this was a fresh insert.
	if c.NewCount() != 1 {
		t.Errorf("NewCount = %d, want 1", c.NewCount())
	}
}

// TestFNewIsSticky: the flag answers "is this chunk already in the repository's
// index/?", so overwriting the in-memory entry must not clear it.
func TestFNewIsSticky(t *testing.T) {
	c, err := NewChunkIndex(10)
	if err != nil {
		t.Fatal(err)
	}
	id := idFor(1)
	if err := c.Set(id[:], Entry{Flags: FUsed, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if c.NewCount() != 1 {
		t.Fatalf("NewCount = %d after the first insert, want 1", c.NewCount())
	}

	// Overwriting keeps it new, and does not double-count.
	if err := c.Set(id[:], Entry{Flags: FUsed, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if c.NewCount() != 1 {
		t.Errorf("NewCount = %d after an overwrite, want 1", c.NewCount())
	}

	c.ClearNew()
	if c.NewCount() != 0 {
		t.Errorf("NewCount = %d after ClearNew, want 0", c.NewCount())
	}
	// And once cleared, an overwrite must not make it new again.
	if err := c.Set(id[:], Entry{Flags: FUsed, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if c.NewCount() != 0 {
		t.Errorf("NewCount = %d after overwriting a cleared entry, want 0", c.NewCount())
	}

	// Deleting a new entry must decrement the count.
	other := idFor(2)
	if err := c.Set(other[:], Entry{Flags: FUsed}); err != nil {
		t.Fatal(err)
	}
	if c.NewCount() != 1 {
		t.Fatalf("NewCount = %d, want 1", c.NewCount())
	}
	c.Delete(other[:])
	if c.NewCount() != 0 {
		t.Errorf("NewCount = %d after deleting the new entry, want 0", c.NewCount())
	}
}

// TestAddResetsPackLocation: re-adding a chunk invalidates whatever pack location it
// had, until the next flush resolves it. Skipping the reset would leave the index
// pointing at a pack that no longer holds the chunk.
func TestAddResetsPackLocation(t *testing.T) {
	c, err := NewChunkIndex(10)
	if err != nil {
		t.Fatal(err)
	}
	id := idFor(1)

	if err := c.Add(id[:], 100); err != nil {
		t.Fatal(err)
	}
	if !c.IsPending(id[:]) {
		t.Error("a freshly added chunk should be pending")
	}

	packID := sha256.Sum256([]byte("pack"))
	if err := c.UpdatePackInfo([]PackResult{{
		ChunkID: id[:], PackID: packID, ObjOffset: 4096, ObjSize: 120,
	}}); err != nil {
		t.Fatal(err)
	}
	if c.IsPending(id[:]) {
		t.Error("the chunk should not be pending after its location is set")
	}
	got, _ := c.Get(id[:])
	if got.PackID != packID || got.ObjOffset != 4096 || got.ObjSize != 120 {
		t.Errorf("pack location not recorded: %+v", got)
	}

	// Re-adding must clear it again.
	if err := c.Add(id[:], 100); err != nil {
		t.Fatal(err)
	}
	if !c.IsPending(id[:]) {
		t.Error("re-adding a chunk should make it pending again")
	}
	got, _ = c.Get(id[:])
	if got.PackID != [32]byte(UnknownBytes32) || got.ObjOffset != UnknownInt32 || got.ObjSize != UnknownInt32 {
		t.Errorf("re-adding did not reset the pack location: %+v", got)
	}

	// A conflicting size is an error, not a silent overwrite.
	if err := c.Add(id[:], 200); err == nil {
		t.Error("adding a chunk with a different size should fail")
	}
}

func TestEntryEncodingIsFixedWidth(t *testing.T) {
	// The 48-byte layout is the Python struct format "<II32sII", and it is what both
	// tools read out of index/. A change here corrupts every entry.
	e := Entry{Flags: 0x01020304, Size: 0x05060708, ObjOffset: 0x090a0b0c, ObjSize: 0x0d0e0f10}
	for i := range e.PackID {
		e.PackID[i] = byte(i)
	}
	var buf [EntrySize]byte
	e.encode(buf[:])

	if len(buf) != 48 {
		t.Fatalf("entry is %d bytes, want 48", len(buf))
	}
	if binary.LittleEndian.Uint32(buf[0:]) != 0x01020304 {
		t.Error("flags are not little-endian at offset 0")
	}
	if binary.LittleEndian.Uint32(buf[4:]) != 0x05060708 {
		t.Error("size is not little-endian at offset 4")
	}
	if !bytes.Equal(buf[8:40], e.PackID[:]) {
		t.Error("pack_id is not at offset 8")
	}
	if binary.LittleEndian.Uint32(buf[40:]) != 0x090a0b0c {
		t.Error("obj_offset is not little-endian at offset 40")
	}
	if binary.LittleEndian.Uint32(buf[44:]) != 0x0d0e0f10 {
		t.Error("obj_size is not little-endian at offset 44")
	}
	if decodeEntry(buf[:]) != e {
		t.Error("entry did not survive the round trip")
	}
}

func TestSerializeRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 1000, 10000} {
		c, err := NewChunkIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			id := idFor(i)
			if err := c.Set(id[:], entryFor(i)); err != nil {
				t.Fatal(err)
			}
		}

		var buf bytes.Buffer
		if err := c.Write(&buf); err != nil {
			t.Fatal(err)
		}
		back, err := ReadChunkIndex(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if back.Len() != n {
			t.Errorf("n=%d: read back %d entries", n, back.Len())
		}
		for i := 0; i < n; i++ {
			id := idFor(i)
			got, ok := back.Get(id[:])
			if !ok {
				t.Fatalf("n=%d: entry %d missing", n, i)
			}
			if got != entryFor(i) {
				t.Fatalf("n=%d: entry %d differs:\n  got:  %+v\n  want: %+v", n, i, got, entryFor(i))
			}
		}
	}
}

func TestReadRejectsMalformed(t *testing.T) {
	// A valid file to mutate.
	c, _ := NewChunkIndex(10)
	id := idFor(1)
	_ = c.Set(id[:], entryFor(1))
	var buf bytes.Buffer
	if err := c.Write(&buf); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()

	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] = 'X'
		if _, err := ReadChunkIndex(bytes.NewReader(bad)); err == nil {
			t.Error("accepted a file with the wrong magic")
		}
	})
	t.Run("bad version", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		binary.LittleEndian.PutUint32(bad[8:], 99)
		if _, err := ReadChunkIndex(bytes.NewReader(bad)); err == nil {
			t.Error("accepted an unsupported version")
		}
	})
	t.Run("absurd metadata length", func(t *testing.T) {
		// A crafted length must be rejected on its face, not turned into a huge
		// allocation.
		bad := append([]byte(nil), good...)
		binary.LittleEndian.PutUint32(bad[12:], 0xFFFFFFFF)
		if _, err := ReadChunkIndex(bytes.NewReader(bad)); err == nil {
			t.Error("accepted an implausible metadata length")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for n := 0; n < len(good); n++ {
			if _, err := ReadChunkIndex(bytes.NewReader(good[:n])); err == nil {
				t.Errorf("accepted a %d-byte prefix of a %d-byte file", n, len(good))
			}
		}
	})
	t.Run("body shorter than the header claims", func(t *testing.T) {
		bad := good[:len(good)-1]
		if _, err := ReadChunkIndex(bytes.NewReader(bad)); err == nil {
			t.Error("accepted a file whose body is short")
		}
	})
}

func TestWriteFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index")

	c, _ := NewChunkIndex(100)
	for i := 0; i < 100; i++ {
		id := idFor(i)
		_ = c.Set(id[:], entryFor(i))
	}
	if err := c.WriteFile(path); err != nil {
		t.Fatal(err)
	}

	// No temporary files may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "index" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only \"index\"", names)
	}

	back, err := ReadChunkIndexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Len() != 100 {
		t.Errorf("read back %d entries, want 100", back.Len())
	}
}

func TestClear(t *testing.T) {
	c, _ := NewChunkIndex(1000)
	for i := 0; i < 1000; i++ {
		id := idFor(i)
		_ = c.Set(id[:], entryFor(i))
	}
	c.Clear()
	if c.Len() != 0 {
		t.Errorf("Len = %d after Clear", c.Len())
	}
	if c.NewCount() != 0 {
		t.Errorf("NewCount = %d after Clear", c.NewCount())
	}
	id := idFor(1)
	if c.Contains(id[:]) {
		t.Error("an entry survived Clear")
	}
	// It must still be usable.
	if err := c.Set(id[:], entryFor(1)); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d after reinserting", c.Len())
	}
}

func TestDeletedEntriesAreZeroed(t *testing.T) {
	// A deleted value can hold a pack location; leaving it readable in a long-lived
	// array is avoidable, so Delete zeroes the storage.
	table, err := NewTable(ChunkIDSize, EntrySize, minCapacity)
	if err != nil {
		t.Fatal(err)
	}
	id := idFor(1)
	secret := bytes.Repeat([]byte{0xAB}, EntrySize)
	if err := table.Set(id[:], secret); err != nil {
		t.Fatal(err)
	}
	if !table.Delete(id[:]) {
		t.Fatal("delete failed")
	}
	if bytes.Contains(table.values, secret) {
		t.Error("the deleted value is still in the values array")
	}
	if bytes.Contains(table.keys, id[:]) {
		t.Error("the deleted key is still in the keys array")
	}
}

func TestRejectsWrongSizes(t *testing.T) {
	if _, err := NewTable(3, 4, 100); err == nil {
		t.Error("accepted a 3-byte key size; the bucket index needs 4")
	}
	if _, err := NewTable(32, 0, 100); err == nil {
		t.Error("accepted a zero value size")
	}

	table, err := NewTable(32, 48, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Set(make([]byte, 31), make([]byte, 48)); err == nil {
		t.Error("accepted a short key")
	}
	if err := table.Set(make([]byte, 32), make([]byte, 47)); err == nil {
		t.Error("accepted a short value")
	}
}

func mapHas(m map[[ChunkIDSize]byte][EntrySize]byte, k [ChunkIDSize]byte) bool {
	_, ok := m[k]
	return ok
}

func BenchmarkChunkIndexSet(b *testing.B) {
	c, err := NewChunkIndex(b.N)
	if err != nil {
		b.Fatal(err)
	}
	ids := make([][ChunkIDSize]byte, b.N)
	for i := range ids {
		ids[i] = idFor(i)
	}
	e := entryFor(1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ids[i][:], e); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkChunkIndexGet(b *testing.B) {
	const n = 1_000_000
	c, err := NewChunkIndex(n)
	if err != nil {
		b.Fatal(err)
	}
	ids := make([][ChunkIDSize]byte, n)
	for i := range ids {
		ids[i] = idFor(i)
		if err := c.Set(ids[i][:], entryFor(i)); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := c.Get(ids[i%n][:]); !ok {
			b.Fatal("missing")
		}
	}
}

// TestMemoryFootprint records what the indirection actually buys, at the reference
// scale. It is a measurement kept in the record rather than a pass/fail assertion: the
// exact ratio depends on the Go version's map implementation, and the reason to port
// borghash rather than wrap a Go map is only partly memory (see the package comment).
func TestMemoryFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the memory footprint measurement in short mode")
	}
	const n = 1_623_610 // unique chunks in the recipedb corpus, per docs/PORTING_PLAN.md
	var m0, m1, m2 runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&m0)
	c, err := NewChunkIndex(n)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := sha256.Sum256([]byte(fmt.Sprintf("id-%d", i)))
		if err := c.Set(id[:], Entry{Flags: FUsed, Size: uint32(i)}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(c)
	borgeBytes := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)

	gm := make(map[[ChunkIDSize]byte]Entry, n)
	for i := 0; i < n; i++ {
		id := sha256.Sum256([]byte(fmt.Sprintf("id-%d", i)))
		gm[id] = Entry{Flags: FUsed, Size: uint32(i)}
	}
	runtime.GC()
	runtime.ReadMemStats(&m2)
	runtime.KeepAlive(c)
	runtime.KeepAlive(gm)
	goMapBytes := int64(m2.HeapAlloc) - int64(m1.HeapAlloc)

	if borgeBytes <= 0 || goMapBytes <= 0 {
		t.Skipf("measurement was disturbed by the GC (borge %d, map %d)", borgeBytes, goMapBytes)
	}
	t.Logf("%d entries: borge table %.0f MB, Go map %.0f MB (%.2fx)",
		n, float64(borgeBytes)/1e6, float64(goMapBytes)/1e6,
		float64(goMapBytes)/float64(borgeBytes))
}
