// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/renesugar/borge/internal/item"
)

func chunks(n int) []item.ChunkListEntry {
	out := make([]item.ChunkListEntry, n)
	for i := range out {
		out[i] = item.ChunkListEntry{ID: []byte{byte(i), 1, 2, 3}, Size: int64(100 + i)}
	}
	return out
}

// old is a timestamp comfortably in the past, so the save-time exclusions do not fire.
func old(secondsAgo int64) int64 {
	return time.Now().Add(-time.Duration(secondsAgo) * time.Second).UnixNano()
}

func TestParseMode(t *testing.T) {
	m, err := ParseMode("ctime,size,inode")
	if err != nil {
		t.Fatal(err)
	}
	if !m.CTime || !m.Size || !m.Inode || m.MTime || m.Disabled {
		t.Errorf("parsed %+v", m)
	}
	if got := m.String(); got != "ctime,size,inode" {
		t.Errorf("String() is %q", got)
	}

	// ctime already changes whenever mtime does, so asking for both is a mistake worth
	// naming rather than silently accepting.
	if _, err := ParseMode("ctime,mtime,size"); err == nil {
		t.Error("ctime,mtime was accepted")
	}
	// Without a timestamp, an edited file of unchanged size would look unchanged.
	if _, err := ParseMode("size,inode"); err == nil {
		t.Error("a mode with no timestamp was accepted")
	}
	if _, err := ParseMode("nonsense"); err == nil {
		t.Error("an unknown property was accepted")
	}
	if m, err := ParseMode("disabled"); err != nil || !m.Disabled {
		t.Errorf("disabled parsed as %+v, %v", m, err)
	}
}

// TestLookupDetectsChanges is the correctness core: for each property a mode compares,
// changing it has to make the file look changed.
func TestLookupDetectsChanges(t *testing.T) {
	base := FileInfo{Size: 1000, Inode: 42, CTime: old(100), MTime: old(200)}

	cases := []struct {
		name    string
		mode    Mode
		changed FileInfo
		want    bool // want: still considered unchanged
	}{
		{"size change caught", Mode{CTime: true, Size: true},
			FileInfo{Size: 1001, Inode: 42, CTime: base.CTime, MTime: base.MTime}, false},
		{"ctime change caught", Mode{CTime: true, Size: true},
			FileInfo{Size: 1000, Inode: 42, CTime: old(50), MTime: base.MTime}, false},
		{"inode change caught", Mode{CTime: true, Size: true, Inode: true},
			FileInfo{Size: 1000, Inode: 43, CTime: base.CTime, MTime: base.MTime}, false},
		{"mtime change caught in mtime mode", Mode{MTime: true, Size: true},
			FileInfo{Size: 1000, Inode: 42, CTime: base.CTime, MTime: old(150)}, false},
		{"inode change ignored when not compared", Mode{CTime: true, Size: true},
			FileInfo{Size: 1000, Inode: 999, CTime: base.CTime, MTime: base.MTime}, true},
		{"mtime change ignored in ctime mode", Mode{CTime: true, Size: true},
			FileInfo{Size: 1000, Inode: 42, CTime: base.CTime, MTime: old(1)}, true},
		{"nothing changed", Mode{CTime: true, Size: true, Inode: true}, base, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.mode, 0)
			c.Memorize("k", base, chunks(3))

			known, got := c.Lookup("k", tc.changed)
			if !known {
				t.Fatal("the file is not known at all")
			}
			if (got != nil) != tc.want {
				t.Errorf("unchanged=%v, want %v", got != nil, tc.want)
			}
			if tc.want && len(got) != 3 {
				t.Errorf("returned %d chunks, want 3", len(got))
			}
		})
	}
}

// TestLookupRefreshesIgnoredValues: a value the mode does not compare is still updated in
// the cache, so a user who switches the comparison back on later does not have to re-read
// the whole tree.
func TestLookupRefreshesIgnoredValues(t *testing.T) {
	c := New(Mode{CTime: true, Size: true}, 0)
	base := FileInfo{Size: 1000, Inode: 42, CTime: old(100), MTime: old(200)}
	c.Memorize("k", base, chunks(1))

	moved := base
	moved.Inode = 999
	if _, got := c.Lookup("k", moved); got == nil {
		t.Fatal("an inode change was treated as a content change")
	}
	if c.entries["k"].Inode != 999 {
		t.Errorf("the ignored inode was not refreshed: %d", c.entries["k"].Inode)
	}
}

func TestDisabledAndRechunkModes(t *testing.T) {
	info := FileInfo{Size: 1, Inode: 1, CTime: old(100), MTime: old(100)}

	disabled := New(DisabledMode(), 0)
	disabled.Memorize("k", info, chunks(1))
	if known, _ := disabled.Lookup("k", info); known {
		t.Error("the disabled cache answered a lookup")
	}
	if disabled.Len() != 0 {
		t.Error("the disabled cache stored an entry")
	}

	// Rechunk still *records* what it read, so the run after it is fast again.
	rechunk := New(Mode{Rechunk: true, CTime: true, Size: true}, 0)
	rechunk.Memorize("k", info, chunks(1))
	if known, _ := rechunk.Lookup("k", info); known {
		t.Error("rechunk mode reused a cached chunk list")
	}
	if rechunk.Len() != 1 {
		t.Error("rechunk mode did not record the file")
	}
}

// TestSaveDropsTheNewestEntries is the rule that keeps the cache from losing data.
//
// A file modified twice within one timestamp tick - once before borge read it, once after -
// has identical recorded times either way, so a later run would call it unchanged and the
// second modification would never be stored. Dropping the newest entries costs a re-read
// and closes that hole.
func TestSaveDropsTheNewestEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "files")

	start := time.Now().UnixNano()
	c := New(DefaultMode(), start)

	// Comfortably old: keeps.
	c.Memorize("old", FileInfo{Size: 1, Inode: 1, CTime: old(3600), MTime: old(3600)}, chunks(1))
	// Written during the backup: must not be kept.
	c.Memorize("fresh", FileInfo{Size: 2, Inode: 2, CTime: start, MTime: start}, chunks(1))
	// Written just before the backup started, inside the three-second window borg allows
	// for clock skew and coarse timestamps: also not kept.
	c.Memorize("recent", FileInfo{Size: 3, Inode: 3, CTime: start - 1e9, MTime: start - 1e9}, chunks(1))

	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Read(path, DefaultMode(), 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := back.entries["old"]; !ok {
		t.Error("an old, safe entry was dropped")
	}
	for _, key := range []string{"fresh", "recent"} {
		if _, ok := back.entries[key]; ok {
			t.Errorf("%q was kept; a file touched around backup time is not safe to trust", key)
		}
	}
}

// TestSaveAgesEntriesOut: an entry not seen again survives a couple of runs and then goes,
// which is what makes an alternating backup pattern work without keeping every file that
// ever existed.
func TestSaveAgesEntriesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "files")

	c := New(DefaultMode(), 0)
	c.Memorize("k", FileInfo{Size: 1, Inode: 1, CTime: old(3600), MTime: old(3600)}, chunks(1))
	// A second, newer entry: with only one entry every entry is the newest, and the
	// newest-timestamp exclusion would drop it - see TestOneFileCachesNothing.
	c.Memorize("newer", FileInfo{Size: 1, Inode: 2, CTime: old(60), MTime: old(60)}, chunks(1))

	for run := 1; run <= 4; run++ {
		if err := c.Save(path); err != nil {
			t.Fatal(err)
		}
		next, err := Read(path, DefaultMode(), 0)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing re-confirms the entry, so its age grows each time it is written back:
		// it survives exactly defaultTTL saves after the run that created it.
		_, keptOld := next.entries["k"]
		if run <= defaultTTL {
			if !keptOld {
				t.Errorf("after %d save(s) the entry is gone, want it kept for %d", run, defaultTTL)
			}
		} else if keptOld {
			t.Errorf("after %d save(s) the entry is still there, want it aged out", run)
		}
		c = next
		if c.Len() == 0 {
			break
		}
	}
}

func TestRoundTripPreservesChunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "files")

	c := New(DefaultMode(), 0)
	want := chunks(5)
	c.Memorize("k", FileInfo{Size: 500, Inode: 7, CTime: old(3600), MTime: old(3600)}, want)
	// A newer entry, so "k" is not itself the newest; see TestOneFileCachesNothing.
	c.Memorize("newer", FileInfo{Size: 1, Inode: 8, CTime: old(60), MTime: old(60)}, chunks(1))
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}

	back, err := Read(path, DefaultMode(), 0)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := back.entries["k"]
	if !ok {
		t.Fatal("the entry did not survive the round trip")
	}
	if e.Size != 500 || e.Inode != 7 {
		t.Errorf("metadata changed: %+v", e)
	}
	if len(e.Chunks) != len(want) {
		t.Fatalf("chunk list is %d long, want %d", len(e.Chunks), len(want))
	}
	for i := range want {
		if string(e.Chunks[i].ID) != string(want[i].ID) || e.Chunks[i].Size != want[i].Size {
			t.Errorf("chunk %d differs: %+v vs %+v", i, e.Chunks[i], want[i])
		}
	}
}

// TestReadIgnoresAForeignFile: a cache written by something else must be ignored, not
// guessed at. Guessing wrong here means trusting a stale chunk list.
func TestReadIgnoresAForeignFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "files")
	if err := writeFileAtomic(path, []byte("not a borge cache at all")); err != nil {
		t.Fatal(err)
	}
	c, err := Read(path, DefaultMode(), 0)
	if err != nil {
		t.Fatalf("reading a foreign cache file failed instead of being ignored: %v", err)
	}
	if c.Len() != 0 {
		t.Error("a foreign cache file produced entries")
	}
}

// TestOneFileCachesNothing records a consequence of the newest-timestamp exclusion that
// looks like a bug and is not: when every entry carries the newest timestamp - which is
// always true of a single file - none of them can be kept.
//
// borg behaves identically. It is harmless in practice, where a tree has many files with
// spread-out timestamps, and the alternative is trusting exactly the entries most likely
// to be wrong.
func TestOneFileCachesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "files")

	c := New(DefaultMode(), 0)
	c.Memorize("only", FileInfo{Size: 1, Inode: 1, CTime: old(3600), MTime: old(3600)}, chunks(1))
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Read(path, DefaultMode(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if back.Len() != 0 {
		t.Errorf("a single-entry cache kept %d entries; the newest is never safe to keep", back.Len())
	}
}

func TestPathIsPerArchiveName(t *testing.T) {
	t.Setenv("BORGE_CACHE_DIR", "/tmp/borge-cache-test")
	repoID := make([]byte, 32)

	daily, err := Path(repoID, "daily")
	if err != nil {
		t.Fatal(err)
	}
	weekly, err := Path(repoID, "weekly")
	if err != nil {
		t.Fatal(err)
	}
	if daily == weekly {
		t.Error("two archive names share a cache file; they would evict each other")
	}
	again, _ := Path(repoID, "daily")
	if again != daily {
		t.Error("the same archive name gave a different cache file")
	}
}
