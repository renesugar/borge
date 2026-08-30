// SPDX-License-Identifier: Apache-2.0

package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/store"
)

// Unit tests that run without the borg venv. The pack writer's concurrency invariant is
// the reason most of them exist: plans/PORTING_PLAN.md §16 calls a mistake there a race
// that corrupts repositories under load and reproduces rarely, so it is pinned down
// deliberately rather than left to chance.

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo")
	backend, err := store.NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(backend, NamespaceConfig(false, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(); err != nil {
		t.Fatal(err)
	}
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func objectFor(i int) (id, obj []byte) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("chunk-%d", i)))
	// A stand-in for a repository object: the tests here only need bytes with a
	// distinguishable length, not a real envelope.
	return sum[:], bytes.Repeat([]byte{byte(i)}, 100+i)
}

// TestPackWriterResultsAreOnePackBehind: with the background store on, Add returns the
// *previous* pack's results while the current one is in flight. Callers must not assume
// the results they get describe the chunk they just added.
func TestPackWriterResultsAreOnePackBehind(t *testing.T) {
	s := newTestStore(t)
	idx, err := hashindex.NewChunkIndex(0)
	if err != nil {
		t.Fatal(err)
	}
	// One chunk per pack, so every Add rolls a pack over.
	w, err := NewPackWriter(s, idx, 1, 0, true)
	if err != nil {
		t.Fatal(err)
	}

	id0, obj0 := objectFor(0)
	results, err := w.Add(id0, obj0)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Errorf("the first Add returned %d results; there was no previous pack", len(results))
	}

	id1, obj1 := objectFor(1)
	results, err = w.Add(id1, obj1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !bytes.Equal(results[0].ChunkID, id0) {
		t.Fatalf("the second Add should report the first chunk, got %v", results)
	}

	// Flush is a barrier: afterwards nothing is buffered or in flight, and no chunk is
	// still pending.
	if _, err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if w.Buffered() != 0 {
		t.Errorf("%d chunks still buffered after Flush", w.Buffered())
	}
	for _, id := range [][]byte{id0, id1} {
		if idx.IsPending(id) {
			t.Errorf("chunk %x is still pending after Flush", id[:8])
		}
		e, ok := idx.Get(id)
		if !ok {
			t.Fatalf("chunk %x is missing from the index", id[:8])
		}
		if e.PackID == [32]byte{} {
			t.Errorf("chunk %x has no pack id after Flush", id[:8])
		}
	}
}

// failingBackend fails Store for names matching a prefix, so a pack store can be made to
// fail on demand.
type failingBackend struct {
	store.Backend
	mu       sync.Mutex
	failFrom int
	stores   int
}

func (b *failingBackend) Store(name string, value []byte) error {
	b.mu.Lock()
	b.stores++
	n := b.stores
	b.mu.Unlock()
	if b.failFrom > 0 && n >= b.failFrom && strings.HasPrefix(name, "packs/") {
		return errors.New("simulated store failure")
	}
	return b.Backend.Store(name, value)
}

// TestPackStoreFailureDropsIndexEntries: when a pack cannot be stored, its chunks must
// be removed from the index. Leaving them would point the index at a pack that does not
// exist, and the next read would fail with a missing object rather than a write error.
func TestPackStoreFailureDropsIndexEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	backend, err := store.NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Create(); err != nil {
		t.Fatal(err)
	}
	failing := &failingBackend{Backend: backend, failFrom: 1}
	s, err := store.New(failing, NamespaceConfig(false, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	idx, err := hashindex.NewChunkIndex(0)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewPackWriter(s, idx, 1, 0, true)
	if err != nil {
		t.Fatal(err)
	}

	id0, obj0 := objectFor(0)
	if _, err := w.Add(id0, obj0); err != nil {
		t.Fatal(err)
	}
	// The chunk is in the index, pending, while its store is in flight.
	if !idx.IsPending(id0) {
		t.Error("a chunk handed to the background store should be pending")
	}

	// The failure surfaces one pack later, from whichever call joins.
	id1, obj1 := objectFor(1)
	_, err = w.Add(id1, obj1)
	if err == nil {
		_, err = w.Flush()
	}
	if err == nil {
		t.Fatal("a failing pack store was not reported")
	}

	// Both the failed pack's chunk and anything still buffered must be gone from the
	// index, so no pending leftovers survive into what gets persisted.
	if idx.Contains(id0) {
		t.Error("a chunk from a failed pack is still in the index")
	}
	if idx.Contains(id1) {
		t.Error("a buffered chunk survived the failure")
	}
}

// TestPackWriterSyncAndAsyncAgree: turning the background store off must not change
// what ends up in the repository, only when it happens.
func TestPackWriterSyncAndAsyncAgree(t *testing.T) {
	run := func(async bool) (string, int) {
		s := newTestStore(t)
		idx, err := hashindex.NewChunkIndex(0)
		if err != nil {
			t.Fatal(err)
		}
		w, err := NewPackWriter(s, idx, 0, 2000, async)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			id, obj := objectFor(i)
			if _, err := w.Add(id, obj); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		_, digest := indexDigest(idx)
		return digest, idx.Len()
	}

	asyncDigest, asyncLen := run(true)
	syncDigest, syncLen := run(false)
	if asyncLen != syncLen {
		t.Errorf("entry counts differ: async %d, sync %d", asyncLen, syncLen)
	}
	if asyncDigest != syncDigest {
		t.Errorf("index digests differ between the async and sync paths\n  async: %s\n  sync:  %s",
			asyncDigest, syncDigest)
	}
}

// TestIterHeadersErrorRules covers the two rules borg spells out, because getting the
// second one wrong would let a truncated walk silently produce an index missing the rest
// of a pack - and a repair would then "fix" the archives by dropping those chunks.
func TestIterHeadersErrorRules(t *testing.T) {
	// Build a small pack of two real objects.
	var pack []byte
	var want [][]byte
	for i := 0; i < 2; i++ {
		id, obj := realObject(t, i)
		pack = append(pack, obj...)
		want = append(want, id)
	}
	packID := sha256.Sum256(pack)

	t.Run("clean walk", func(t *testing.T) {
		r := NewPackReaderFromBytes(packID[:], pack)
		var got [][]byte
		if err := r.IterHeaders(func(e PackEntry) bool {
			got = append(got, e.ChunkID)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("walked %d objects, want %d", len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Errorf("object %d id differs", i)
			}
		}
	})

	t.Run("a trailing partial header is a clean end", func(t *testing.T) {
		// Append fewer bytes than a header: not corruption, just the end of the pack.
		truncated := append(append([]byte(nil), pack...), 1, 2, 3)
		r := NewPackReaderFromBytes(packID[:], truncated)
		n := 0
		if err := r.IterHeaders(func(PackEntry) bool { n++; return true }); err != nil {
			t.Errorf("a trailing partial header should end the walk cleanly, got %v", err)
		}
		if n != 2 {
			t.Errorf("walked %d objects, want 2", n)
		}
	})

	t.Run("a bad magic is corruption", func(t *testing.T) {
		bad := append([]byte(nil), pack...)
		bad[0] = 'X'
		r := NewPackReaderFromBytes(packID[:], bad)
		if err := r.IterHeaders(func(PackEntry) bool { return true }); err == nil {
			t.Error("a bad object magic must stop the walk with an error, not end it quietly")
		}
	})

	t.Run("an object past the end is corruption", func(t *testing.T) {
		// Truncate mid-object, so the first header claims more than is there.
		bad := pack[:len(pack)-10]
		r := NewPackReaderFromBytes(packID[:], bad)
		err := r.IterHeaders(func(PackEntry) bool { return true })
		if err == nil {
			t.Error("an object extending past the pack must be reported as corruption")
		} else if !strings.Contains(err.Error(), "past the end") {
			t.Errorf("unhelpful error: %v", err)
		}
	})
}

func TestCheckPackObjects(t *testing.T) {
	ok := []PackEntry{{Offset: 0, Size: 100}, {Offset: 100, Size: 50}}
	if err := CheckPackObjects("abc", ok, 150); err != nil {
		t.Errorf("a valid range set was rejected: %v", err)
	}
	overlapping := []PackEntry{{Offset: 0, Size: 100}, {Offset: 50, Size: 50}}
	if err := CheckPackObjects("abc", overlapping, 150); err == nil {
		t.Error("overlapping objects were accepted")
	}
	pastEnd := []PackEntry{{Offset: 0, Size: 100}, {Offset: 100, Size: 100}}
	if err := CheckPackObjects("abc", pastEnd, 150); err == nil {
		t.Error("an object ending past the pack was accepted")
	}
}

// realObject builds an actual repository object, so the pack tests walk real headers.
func realObject(t *testing.T, i int) (id, obj []byte) {
	t.Helper()
	k := testKey(t)
	r, err := repoobj.New(k)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte(fmt.Sprintf("object-%d ", i)), 10)
	id = k.IDHash(data)
	obj, err = r.Format(id, &repoobj.Meta{Type: repoobj.TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}
	return id, obj
}

func TestRepositoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	r, err := Create(location.MustLocal(path), smallPackOptions())
	if err != nil {
		t.Fatal(err)
	}

	if len(r.ID()) != 32 {
		t.Errorf("repository id is %d bytes, want 32", len(r.ID()))
	}
	if r.Version() != Version {
		t.Errorf("version = %d, want %d", r.Version(), Version)
	}

	var ids [][]byte
	var objects [][]byte
	for i := 0; i < 30; i++ {
		id, obj := realObject(t, i)
		ids = append(ids, id)
		objects = append(objects, obj)
		if _, err := r.Put(id, obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		got, err := r.Get(ids[i])
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if !bytes.Equal(got, objects[i]) {
			t.Errorf("object %d differs", i)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must find everything through the persisted index.
	r2, err := Open(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	for i := range ids {
		got, err := r2.Get(ids[i])
		if err != nil {
			t.Fatalf("get %d after reopen: %v", i, err)
		}
		if !bytes.Equal(got, objects[i]) {
			t.Errorf("object %d differs after reopen", i)
		}
	}
	if _, err := r2.Get(sha256Of("nothing")); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("a missing object gave %v, want ErrObjectNotFound", err)
	}
}

func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// TestConfigReadmeMustMatchBorg: borg compares config/readme for exact equality, so a
// borge-specific readme would make the repository unopenable by borg.
func TestConfigReadmeMustMatchBorg(t *testing.T) {
	const want = "This is a Borg Backup repository.\nSee https://borgbackup.readthedocs.io/\n"
	if RepositoryReadme != want {
		t.Errorf("config/readme is %q, but borg requires exactly %q", RepositoryReadme, want)
	}

	path := filepath.Join(t.TempDir(), "repo")
	r, err := Create(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	data, err := os.ReadFile(filepath.Join(path, "config", "readme"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("the written readme is %q", data)
	}

	// A changed readme must be refused on open, rather than half-working.
	if err := os.WriteFile(filepath.Join(path, "config", "readme"),
		[]byte("This is a borge repository.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(location.MustLocal(path), Options{}); !errors.Is(err, ErrInvalidRepository) {
		t.Errorf("a repository with a wrong readme opened: %v", err)
	}
}

func TestOpenRejectsWrongVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	r, err := Create(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	if err := os.WriteFile(filepath.Join(path, "config", "version"), []byte("3"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(location.MustLocal(path), Options{})
	if !errors.Is(err, ErrInvalidRepository) {
		t.Errorf("a version 3 repository opened: %v", err)
	} else if !strings.Contains(err.Error(), "version 3") {
		t.Errorf("the error should name the version found: %v", err)
	}
}

func TestCreateWritesAnEmptyIndex(t *testing.T) {
	// Repository creation writes an empty index fragment so the first operation does not
	// have to rebuild the index by listing every packs/ subdirectory.
	path := filepath.Join(t.TempDir(), "repo")
	r, err := Create(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	entries, err := os.ReadDir(filepath.Join(path, "index"))
	if err != nil {
		t.Fatalf("no index directory after create: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("index/ holds %d objects after create, want 1", len(entries))
	}
}

// ---------------------------------------------------------------------------- locks

func TestExclusiveLockExcludes(t *testing.T) {
	s := newTestStore(t)

	first, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	first.SetTimeout(50 * time.Millisecond)
	if err := first.Acquire(); err != nil {
		t.Fatal(err)
	}

	// Another holder must be refused, exclusive or shared. It has to be given a distinct
	// identity: locks taken by the same process do not conflict with each other, which is
	// borg's behaviour and what lets one process hold a lock across nested operations.
	for _, exclusive := range []bool{true, false} {
		second, err := NewLock(s, exclusive)
		if err != nil {
			t.Fatal(err)
		}
		second.setHolder("another-host", 424242, 0)
		second.SetTimeout(50 * time.Millisecond)
		if err := second.Acquire(); !errors.Is(err, ErrLockTimeout) {
			t.Errorf("exclusive=%v: acquired alongside an exclusive lock (%v)", exclusive, err)
			_ = second.Release(true)
		}
	}

	if err := first.Release(false); err != nil {
		t.Fatal(err)
	}
	// Once released, the next holder gets in.
	third, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	third.setHolder("another-host", 424242, 0)
	third.SetTimeout(50 * time.Millisecond)
	if err := third.Acquire(); err != nil {
		t.Errorf("could not acquire after a release: %v", err)
	}
	_ = third.Release(true)
}

func TestSharedLocksCoexist(t *testing.T) {
	s := newTestStore(t)
	var locks []*Lock
	for i := 0; i < 3; i++ {
		l, err := NewLock(s, false)
		if err != nil {
			t.Fatal(err)
		}
		l.setHolder("shared-host", 1000+i, 0)
		l.SetTimeout(50 * time.Millisecond)
		if err := l.Acquire(); err != nil {
			t.Fatalf("shared lock %d was refused: %v", i, err)
		}
		locks = append(locks, l)
	}

	// An exclusive lock must not get in while shared ones are held.
	ex, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	ex.setHolder("exclusive-host", 2000, 0)
	ex.SetTimeout(50 * time.Millisecond)
	if err := ex.Acquire(); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("an exclusive lock was taken alongside shared ones: %v", err)
		_ = ex.Release(true)
	}

	for _, l := range locks {
		_ = l.Release(true)
	}
}

// TestStaleLocksAreRemoved: a crashed client must not block the repository forever.
func TestStaleLocksAreRemoved(t *testing.T) {
	s := newTestStore(t)

	abandoned, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	abandoned.setHolder("dead-host", 999999, 0)
	if err := abandoned.Acquire(); err != nil {
		t.Fatal(err)
	}
	// Do not release it: simulate a client that died.

	fresh, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	fresh.SetTimeout(50 * time.Millisecond)
	// With the staleness threshold at zero, the abandoned lock is immediately eligible
	// for removal.
	fresh.SetStale(0)
	if err := fresh.Acquire(); err != nil {
		t.Errorf("a stale lock was not cleared: %v", err)
	}
	_ = fresh.Release(true)
}

func TestBreakLock(t *testing.T) {
	s := newTestStore(t)
	l, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(); err != nil {
		t.Fatal(err)
	}

	if err := BreakLock(s); err != nil {
		t.Fatal(err)
	}
	names, err := s.ListNames("locks", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("%d locks survived BreakLock", len(names))
	}
}

func TestRepositoryLocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	r, err := Create(location.MustLocal(path), Options{Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}

	// Another *client* must be refused. It is written directly into locks/, because a
	// Lock built here would carry this process's identity - and locks taken by the same
	// process deliberately do not conflict (borg behaves the same way, and it is what
	// lets one process hold a lock across nested operations).
	foreign := fmt.Sprintf(
		`{"exclusive": true, "hostid": "elsewhere", "processid": 4242, "threadid": 0, "time": %q}`,
		time.Now().UTC().Format(lockTimeLayout))
	sum := sha256.Sum256([]byte(foreign))
	foreignName := filepath.Join(path, "locks", hex.EncodeToString(sum[:]))
	if err := os.WriteFile(foreignName, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if r2, err := Open(location.MustLocal(path), Options{Exclusive: true}); err == nil {
		r2.Close()
		t.Error("an open succeeded while another client held an exclusive lock")
	}
	if err := os.Remove(foreignName); err != nil {
		t.Fatal(err)
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// And an open succeeds once the repository is free.
	r2, err := Open(location.MustLocal(path), Options{Exclusive: true})
	if err != nil {
		t.Fatalf("could not open after close: %v", err)
	}
	r2.Close()
}

// TestSameProcessLocksDoNotConflict pins the consequence of using borg's identity tuple:
// the thread id is always zero, so two locks taken by one process see each other as
// "ours". borg is the same. It is recorded here because it is easy to mistake for a bug.
func TestSameProcessLocksDoNotConflict(t *testing.T) {
	s := newTestStore(t)
	first, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	first.SetTimeout(50 * time.Millisecond)
	if err := first.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer first.Release(true)

	second, err := NewLock(s, true)
	if err != nil {
		t.Fatal(err)
	}
	second.SetTimeout(50 * time.Millisecond)
	if err := second.Acquire(); err != nil {
		t.Errorf("a second lock in the same process was refused: %v", err)
	} else {
		_ = second.Release(true)
	}
}

// ---------------------------------------------------------------------------- index

func TestIndexFragmentsAreDeterministic(t *testing.T) {
	// The fragment name is its content hash, so writing the same entries twice must
	// produce the same object - that is what makes writing idempotent and stops
	// near-duplicate fragments piling up.
	build := func() []string {
		s := newTestStore(t)
		idx, err := hashindex.NewChunkIndex(0)
		if err != nil {
			t.Fatal(err)
		}
		// Insert in a different order each time, to prove the sort is doing the work.
		order := []int{5, 1, 9, 3, 7, 0, 8, 2, 6, 4}
		for _, i := range order {
			id := sha256Of(fmt.Sprintf("chunk-%d", i))
			var packID [32]byte
			copy(packID[:], sha256Of("pack"))
			if err := idx.Set(id, hashindex.Entry{
				Flags: hashindex.FUsed, PackID: packID,
				ObjOffset: uint32(i * 100), ObjSize: 100,
			}); err != nil {
				t.Fatal(err)
			}
		}
		hashes, err := WriteChunkIndex(s, idx, WriteIndexOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return hashes
	}

	first, second := build(), build()
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("fragment hashes are not deterministic:\n  %v\n  %v", first, second)
	}
}

func TestIncrementalIndexWriteOnlyWritesNewEntries(t *testing.T) {
	s := newTestStore(t)
	idx, err := hashindex.NewChunkIndex(0)
	if err != nil {
		t.Fatal(err)
	}
	var packID [32]byte
	copy(packID[:], sha256Of("pack"))

	for i := 0; i < 5; i++ {
		if err := idx.Set(sha256Of(fmt.Sprintf("a-%d", i)),
			hashindex.Entry{Flags: hashindex.FUsed, PackID: packID, ObjSize: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteChunkIndex(s, idx, WriteIndexOptions{Incremental: true}); err != nil {
		t.Fatal(err)
	}
	// Everything written is no longer new, so a second incremental write has nothing to
	// do and must not create an empty fragment.
	before, _ := listIndexHashes(s)
	if _, err := WriteChunkIndex(s, idx, WriteIndexOptions{Incremental: true}); err != nil {
		t.Fatal(err)
	}
	after, _ := listIndexHashes(s)
	if len(after) != len(before) {
		t.Errorf("a second incremental write added %d fragment(s)", len(after)-len(before))
	}

	// Adding a chunk and writing again must record just that one.
	if err := idx.Set(sha256Of("b-0"),
		hashindex.Entry{Flags: hashindex.FUsed, PackID: packID, ObjSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteChunkIndex(s, idx, WriteIndexOptions{Incremental: true}); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := BuildChunkIndex(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Len() != 6 {
		t.Errorf("the merged index has %d entries, want 6", rebuilt.Len())
	}
}

func TestPendingChunksAreNotPersisted(t *testing.T) {
	// A chunk still buffered in the pack writer has no location. Serialising it would
	// record a pointer to a pack that may never exist.
	s := newTestStore(t)
	idx, err := hashindex.NewChunkIndex(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add(sha256Of("pending"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteChunkIndex(s, idx, WriteIndexOptions{}); err == nil {
		t.Error("an index with a pending chunk was persisted")
	}
}

func TestInvalidMarkerForcesRebuild(t *testing.T) {
	// Leftover fragments after an interrupted deletion may be incomplete or stale, so
	// the marker must send the reader to the packs instead of trusting them.
	s := newTestStore(t)
	idx, err := hashindex.NewChunkIndex(0)
	if err != nil {
		t.Fatal(err)
	}
	var packID [32]byte
	copy(packID[:], sha256Of("pack"))
	if err := idx.Set(sha256Of("x"), hashindex.Entry{Flags: hashindex.FUsed, PackID: packID}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteChunkIndex(s, idx, WriteIndexOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := writeInvalidMarker(s); err != nil {
		t.Fatal(err)
	}

	// There are no packs, so a rebuild yields an empty index rather than the stale
	// fragment's contents.
	rebuilt, err := BuildChunkIndex(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Len() != 0 {
		t.Errorf("the stale fragment was trusted: %d entries", rebuilt.Len())
	}
	invalid, err := indexIsInvalid(s)
	if err != nil {
		t.Fatal(err)
	}
	if invalid {
		t.Error("the invalid marker survived the rebuild")
	}
}

func TestPackNameNesting(t *testing.T) {
	packID := sha256Of("a pack")
	name := PackName(packID)
	if !strings.HasPrefix(name, "packs/") {
		t.Errorf("pack name %q is not in the packs namespace", name)
	}
	if got := strings.TrimPrefix(name, "packs/"); got != hex.EncodeToString(packID) {
		t.Errorf("pack name = %q, want the hex pack id", got)
	}
}

// testKey builds the key the object helpers use.
func testKey(t *testing.T) key.Key {
	t.Helper()
	return key.NewNoneSHA256()
}
