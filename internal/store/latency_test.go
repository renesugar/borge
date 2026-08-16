// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The plan's stage 2 gate asks for a run over the rclone-mounted Google Drive, to
// exercise "where naive per-object I/O will show up first". The concern there is really
// about *operation count*, not about the network: on a filesystem with tens of
// milliseconds of latency per operation, doing twenty round trips where one would do is
// what makes a restore unusable.
//
// Operation count can be measured exactly and without a network, which is what
// TestPackCacheCollapsesHeaderReads does. TestHighLatencyFilesystem then runs the same
// pattern over a real high-latency mount when one is available.

// countingOps wraps a backend and counts what passes through it.
type countingOps struct {
	Backend
	loads, stores, infos, lists int
	// latency is added to every operation, standing in for a network round trip.
	latency time.Duration
}

func (b *countingOps) Load(name string, offset, size int64) ([]byte, error) {
	b.loads++
	time.Sleep(b.latency)
	return b.Backend.Load(name, offset, size)
}

func (b *countingOps) Store(name string, value []byte) error {
	b.stores++
	time.Sleep(b.latency)
	return b.Backend.Store(name, value)
}

func (b *countingOps) Info(name string) (ItemInfo, error) {
	b.infos++
	time.Sleep(b.latency)
	return b.Backend.Info(name)
}

func (b *countingOps) List(name string) ([]ItemInfo, error) {
	b.lists++
	time.Sleep(b.latency)
	return b.Backend.List(name)
}

// TestPackCacheCollapsesHeaderReads measures the behaviour the pack cache exists for.
//
// Reading a pack's object headers means many small range reads of the same file - the
// pack reader walks 49-byte headers to locate every object (docs/FORMAT.md §7.3).
// Without a cache each one is a separate backend operation, which on a high-latency
// filesystem is a round trip apiece. The writethrough cache must turn the whole walk
// into a single fetch.
func TestPackCacheCollapsesHeaderReads(t *testing.T) {
	const headerSize = 49
	const objects = 40

	packID := sha256.Sum256([]byte("a pack"))
	packName := "packs/" + hex.EncodeToString(packID[:])
	packData := bytes.Repeat([]byte{0xAB}, headerSize*objects)

	run := func(withCache bool) (loads int) {
		dir := t.TempDir()
		primary, err := NewPosixFS(filepath.Join(dir, "repo"), nil)
		if err != nil {
			t.Fatal(err)
		}
		counted := &countingOps{Backend: primary}

		var cache Backend
		mode := CacheOff
		if withCache {
			cb, err := NewPosixFS(filepath.Join(dir, "cache"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := cb.Create(); err != nil {
				t.Fatal(err)
			}
			cache, mode = cb, CacheWritethrough
		}

		s, err := New(counted, map[string]NamespaceConfig{
			"packs/": {Levels: []int{1}, Cache: mode},
		}, cache)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Create(); err != nil {
			t.Fatal(err)
		}
		if err := s.Open(); err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		if err := s.Store(packName, packData); err != nil {
			t.Fatal(err)
		}
		counted.loads = 0 // count only the header walk

		// Walk every object header, as PackReader.iter_headers does.
		for i := 0; i < objects; i++ {
			got, err := s.Load(packName, int64(i*headerSize), headerSize, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != headerSize {
				t.Fatalf("header %d: got %d bytes", i, len(got))
			}
		}
		return counted.loads
	}

	withoutCache := run(false)
	withCache := run(true)

	if withoutCache != objects {
		t.Errorf("without a cache: %d backend loads for %d headers, expected one apiece",
			withoutCache, objects)
	}
	if withCache > 1 {
		t.Errorf("with the writethrough cache: %d backend loads for %d headers, expected at most 1",
			withCache, objects)
	}
	t.Logf("walking %d object headers: %d backend loads without the cache, %d with it",
		objects, withoutCache, withCache)
}

// TestSimulatedHighLatency puts a concrete number on the same thing, at a latency
// typical of a network mount.
func TestSimulatedHighLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the simulated latency measurement in short mode")
	}
	const headerSize = 49
	const objects = 20
	const latency = 5 * time.Millisecond // conservative for a cloud mount

	packID := sha256.Sum256([]byte("latency pack"))
	packName := "packs/" + hex.EncodeToString(packID[:])
	packData := bytes.Repeat([]byte{0xCD}, headerSize*objects)

	run := func(withCache bool) time.Duration {
		dir := t.TempDir()
		primary, err := NewPosixFS(filepath.Join(dir, "repo"), nil)
		if err != nil {
			t.Fatal(err)
		}
		counted := &countingOps{Backend: primary, latency: latency}

		var cache Backend
		mode := CacheOff
		if withCache {
			cb, _ := NewPosixFS(filepath.Join(dir, "cache"), nil)
			if err := cb.Create(); err != nil {
				t.Fatal(err)
			}
			cache, mode = cb, CacheWritethrough
		}
		s, err := New(counted, map[string]NamespaceConfig{
			"packs/": {Levels: []int{1}, Cache: mode},
		}, cache)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Create(); err != nil {
			t.Fatal(err)
		}
		if err := s.Open(); err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.Store(packName, packData); err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		for i := 0; i < objects; i++ {
			if _, err := s.Load(packName, int64(i*headerSize), headerSize, false); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start)
	}

	slow, fast := run(false), run(true)
	t.Logf("%d header reads at %v latency: %v without the cache, %v with it (%.1fx)",
		objects, latency, slow.Round(time.Millisecond), fast.Round(time.Millisecond),
		float64(slow)/float64(fast))
	if fast > slow {
		t.Errorf("the cache made the header walk slower: %v vs %v", fast, slow)
	}
}

// TestHighLatencyFilesystem runs the store over a real high-latency mount.
//
// The plan names the rclone-mounted Google Drive at /home/renes/GoogleDrive.
// BORGE_SLOW_FS_DIR overrides it. The test skips when the mount is absent or not
// usable - an rclone mount can be present in /proc/mounts and still fail every
// operation with EIO when the backing service is not reachable, which is what it was
// doing when this test was written.
func TestHighLatencyFilesystem(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the high-latency filesystem test in short mode")
	}
	base := os.Getenv("BORGE_SLOW_FS_DIR")
	if base == "" {
		base = "/home/renes/GoogleDrive"
	}
	if _, err := os.ReadDir(base); err != nil {
		t.Skipf("%s is not usable (%v); set BORGE_SLOW_FS_DIR to a mounted, writable path to run this", base, err)
	}

	dir := filepath.Join(base, fmt.Sprintf(".borge-store-test-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Skipf("cannot create a test directory under %s: %v", base, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	name := "packs/" + strings.Repeat("ab", 32)
	value := bytes.Repeat([]byte{0x5A}, 100000)

	// The same workload with and without the pack cache. The uncached run does fewer
	// reads because each one is a real round trip over the mount; the figure reported
	// is per read, so the two are comparable.
	const cachedReads, uncachedReads = 50, 10

	open := func(subdir string, withCache bool) (*Store, func()) {
		backend, err := NewPosixFS(filepath.Join(dir, subdir), nil)
		if err != nil {
			t.Fatal(err)
		}
		var cache Backend
		mode := CacheOff
		if withCache {
			cb, err := NewPosixFS(filepath.Join(t.TempDir(), "cache"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := cb.Create(); err != nil {
				t.Fatal(err)
			}
			cache, mode = cb, CacheWritethrough
		}
		s, err := New(backend, map[string]NamespaceConfig{
			"config/": {Levels: []int{0}},
			"packs/":  {Levels: []int{1}, Cache: mode},
		}, cache)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Create(); err != nil {
			t.Fatal(err)
		}
		if err := s.Open(); err != nil {
			t.Fatal(err)
		}
		return s, func() { _ = s.Close() }
	}

	// --- with the pack cache ---
	cached, closeCached := open("cached", true)
	defer closeCached()

	start := time.Now()
	if err := cached.Store(name, value); err != nil {
		t.Fatalf("store on %s: %v", base, err)
	}
	storeTime := time.Since(start)

	start = time.Now()
	for i := 0; i < cachedReads; i++ {
		if _, err := cached.Load(name, int64(i*49), 49, false); err != nil {
			t.Fatalf("cached range read on %s: %v", base, err)
		}
	}
	cachedPerRead := time.Since(start) / cachedReads

	got, err := cached.Load(name, 0, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, value) {
		t.Error("content differs after a round trip over the high-latency mount")
	}

	// --- without it ---
	uncached, closeUncached := open("uncached", false)
	defer closeUncached()
	if err := uncached.Store(name, value); err != nil {
		t.Fatalf("store on %s: %v", base, err)
	}

	start = time.Now()
	for i := 0; i < uncachedReads; i++ {
		if _, err := uncached.Load(name, int64(i*49), 49, false); err != nil {
			t.Fatalf("uncached range read on %s: %v", base, err)
		}
	}
	uncachedPerRead := time.Since(start) / uncachedReads

	t.Logf("%s: stored 100 kB in %v", base, storeTime.Round(time.Millisecond))
	t.Logf("%s: per object-header read: %v uncached, %v cached (%.0fx)",
		base, uncachedPerRead.Round(time.Microsecond), cachedPerRead.Round(time.Microsecond),
		float64(uncachedPerRead)/float64(cachedPerRead))
	if uncachedPerRead < cachedPerRead {
		t.Errorf("the cache made reads slower on %s: %v vs %v", base, cachedPerRead, uncachedPerRead)
	}
}
