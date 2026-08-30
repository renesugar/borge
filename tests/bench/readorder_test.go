// SPDX-License-Identifier: Apache-2.0

package bench_test

import (
	"encoding/hex"
	"os"
	"sort"
	"testing"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// placement is where one chunk read lives in the repository.
type placement struct {
	pack   [32]byte
	offset uint32
	size   uint32
}

// readOrderStats describes a sequence of chunk reads as a storage device sees it.
//
// The question R0 item 1 asks is whether extracting in *item* order reads the packs badly
// enough to be worth reordering. That is answerable without extracting anything: the chunk
// index already knows where every chunk lives, so the sequence can be scored directly.
type readOrderStats struct {
	reads         int
	distinctPacks int
	// packSwitches counts consecutive reads that land in different packs. With a pack
	// cache of one it is the number of pack opens; it is the headline "is this sequential"
	// number, and its floor is distinctPacks.
	packSwitches int
	// misses counts reads whose pack is not in an LRU of the given size - the opens that
	// actually happen, given that borge caches open pack handles.
	misses int
	// backward counts consecutive reads in the same pack whose offset decreases: a seek
	// against the grain of a sequential read.
	backward int
	// bytesForward and bytesBackward total the absolute offset movement in each direction,
	// within a pack. Together they say whether the head is walking or thrashing.
	bytesForward   int64
	bytesBackward  int64
	distinctChunks int
}

func scoreReadOrder(seq []placement, lru int) readOrderStats {
	st := readOrderStats{reads: len(seq)}
	packs := map[[32]byte]bool{}
	for _, p := range seq {
		packs[p.pack] = true
	}
	st.distinctPacks = len(packs)

	// A tiny LRU, modelled the way the pack cache behaves: most-recent at the front.
	var cache [][32]byte
	touch := func(id [32]byte) bool {
		for i, c := range cache {
			if c == id {
				cache = append(cache[:i], cache[i+1:]...)
				cache = append([][32]byte{id}, cache...)
				return true
			}
		}
		cache = append([][32]byte{id}, cache...)
		if len(cache) > lru {
			cache = cache[:lru]
		}
		return false
	}

	for i, p := range seq {
		if !touch(p.pack) {
			st.misses++
		}
		if i == 0 {
			continue
		}
		prev := seq[i-1]
		if prev.pack != p.pack {
			st.packSwitches++
			continue
		}
		if p.offset < prev.offset {
			st.backward++
			st.bytesBackward += int64(prev.offset) - int64(p.offset)
			continue
		}
		st.bytesForward += int64(p.offset) - int64(prev.offset)
	}
	return st
}

// TestExtractionReadOrder is R0 T1's measurement, and it deliberately measures before
// anything is built.
//
// It reports how an extraction in item order reads the packs, and what sorting by
// (pack, offset) would change. Reordering the extraction is a real change with a real
// cost - chunks must be buffered until the file that wants them is being written - and it
// is only worth paying if the current order is actually bad. This says whether it is.
//
// Point it at a repository with BORGE_READORDER_REPO and name the archive with
// BORGE_READORDER_ARCHIVE.
func TestExtractionReadOrder(t *testing.T) {
	repoPath := os.Getenv("BORGE_READORDER_REPO")
	if repoPath == "" {
		t.Skip("set BORGE_READORDER_REPO (and BORGE_READORDER_ARCHIVE) to score a real archive")
	}
	name := os.Getenv("BORGE_READORDER_ARCHIVE")
	if name == "" {
		name = "bench"
	}

	loc, err := location.Parse(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.Open(loc, repository.Options{NoLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	key, _, err := repo.Unlock(os.Getenv("BORGE_PASSPHRASE"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(repo, key, manifest.OpRead)
	if err != nil {
		t.Fatal(err)
	}
	a, err := archive.OpenByName(m, name)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := repo.Chunks()
	if err != nil {
		t.Fatal(err)
	}

	// The extraction sequence: every content chunk of every item, in the order Extract
	// would ask for them.
	var seq []placement
	seen := map[string]bool{}
	missing := 0
	err = a.Items(func(it *item.Item) error {
		for _, c := range it.Chunks {
			e, ok := chunks.Get(c.ID)
			if !ok {
				missing++
				continue
			}
			seq = append(seq, placement{pack: e.PackID, offset: e.ObjOffset, size: e.ObjSize})
			seen[hex.EncodeToString(c.ID)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seq) == 0 {
		t.Fatalf("archive %q has no content chunks", name)
	}
	if missing > 0 {
		t.Errorf("%d chunk(s) are in the archive but not in the chunk index", missing)
	}

	sorted := append([]placement(nil), seq...)
	sort.Slice(sorted, func(i, j int) bool {
		if c := comparePack(sorted[i].pack, sorted[j].pack); c != 0 {
			return c < 0
		}
		return sorted[i].offset < sorted[j].offset
	})

	// Two caches sit under an extraction, and they are different sizes, so both are
	// reported. repository.packCache holds decoded PackReaders and is newPackCache(3) -
	// but Extract goes through repo.Get, which does not populate it. What actually absorbs
	// a pack switch is the posixfs descriptor cache, maxOpenHandles = 16, which is what
	// §12.1e added. A single number here would have hidden which layer is doing the work.
	const lru = 16
	itemOrder := scoreReadOrder(seq, lru)
	best := scoreReadOrder(sorted, lru)
	itemSmall := scoreReadOrder(seq, 3)
	bestSmall := scoreReadOrder(sorted, 3)
	itemOrder.distinctChunks = len(seen)
	best.distinctChunks = len(seen)

	t.Logf("archive %q: %d chunk reads, %d distinct chunks, %d distinct packs",
		name, itemOrder.reads, itemOrder.distinctChunks, itemOrder.distinctPacks)
	t.Logf("%-22s %14s %14s", "", "item order", "sorted")
	t.Logf("%-22s %14d %14d", "pack switches", itemOrder.packSwitches, best.packSwitches)
	t.Logf("%-22s %14d %14d", "misses, lru 16 (fds)", itemOrder.misses, best.misses)
	t.Logf("%-22s %14d %14d", "misses, lru 3 (readers)", itemSmall.misses, bestSmall.misses)
	t.Logf("%-22s %14d %14d", "backward seeks", itemOrder.backward, best.backward)
	t.Logf("%-22s %14d %14d", "bytes seeked back", itemOrder.bytesBackward, best.bytesBackward)
	t.Logf("%-22s %14d %14d", "bytes seeked fwd", itemOrder.bytesForward, best.bytesForward)

	// The floor: one switch per pack after the first.
	floor := itemOrder.distinctPacks - 1
	t.Logf("pack-switch floor is %d; item order is %.2fx it, sorted order is %.2fx",
		floor, ratio(itemOrder.packSwitches, floor), ratio(best.packSwitches, floor))
	// A control, because every number above says "optimal" and a metric that cannot say
	// anything else is not measuring. Scored against a cache deliberately smaller than the
	// working set, the same sequence must show thrashing - misses far above the floor. If
	// this line ever reports the floor too, the scorer is broken rather than the read
	// order being good.
	tooSmall := scoreReadOrder(seq, 2)
	t.Logf("control: at a cache of 2, item order misses %d against a floor of %d (%.1fx) - the metric can register a failure",
		tooSmall.misses, itemOrder.distinctPacks, ratio(tooSmall.misses, itemOrder.distinctPacks))
	if tooSmall.misses <= itemOrder.distinctPacks && itemOrder.packSwitches > itemOrder.distinctPacks {
		t.Errorf("control failed: a cache of 2 should thrash on %d pack switches, but missed only %d times",
			itemOrder.packSwitches, tooSmall.misses)
	}

	t.Logf("smallest cache that opens each pack once: item order %d, sorted %d (borge caches 16 descriptors)",
		minLRUForOptimal(seq, itemOrder.distinctPacks),
		minLRUForOptimal(sorted, itemOrder.distinctPacks))
	t.Logf("misses: item order %.2fx the %d-pack floor, sorted %.2fx",
		ratio(itemOrder.misses, itemOrder.distinctPacks), itemOrder.distinctPacks,
		ratio(best.misses, itemOrder.distinctPacks))
}

// minLRUForOptimal is the smallest pack cache at which a sequence opens each pack exactly
// once - the number that actually decides whether sorting by (pack, offset) is worth
// anything.
//
// Pack *switches* are the wrong headline. A switch between two packs that are both already
// open costs nothing; only a switch that evicts something costs an open. So the question
// is not "how often does the read move between packs" but "how many packs does it need
// open at the same time", and that is what this returns.
func minLRUForOptimal(seq []placement, distinctPacks int) int {
	for n := 1; n <= distinctPacks; n++ {
		if scoreReadOrder(seq, n).misses == distinctPacks {
			return n
		}
	}
	return distinctPacks
}

func ratio(n, floor int) float64 {
	if floor <= 0 {
		return 0
	}
	return float64(n) / float64(floor)
}

func comparePack(a, b [32]byte) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
