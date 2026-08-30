// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/msgpackx"
	"github.com/renesugar/borge/internal/repository"
)

// TestItemRoundTripLosesNothing is R0 T8's measurement.
//
// T8 was written on the premise that "unknown msgpack keys are dropped at the Item struct
// boundary", and asks for the round trip to be *established* rather than assumed: decode
// every item of a real archive, re-encode it, and report which keys fall off. This does
// that, on whatever repository BORGE_ROUNDTRIP_REPO names - including one borg wrote, which
// is the case that matters, because borg is the thing that might write a key borge has
// never heard of.
//
// It compares key sets and values rather than bytes. A re-encoding may legitimately order
// keys differently or choose a narrower integer width for the same number; neither loses
// anything, and requiring byte equality would report those as data loss.
func TestItemRoundTripLosesNothing(t *testing.T) {
	repoPath := os.Getenv("BORGE_ROUNDTRIP_REPO")
	if repoPath == "" {
		t.Skip("set BORGE_ROUNDTRIP_REPO (and BORGE_ROUNDTRIP_ARCHIVE) to audit a real archive")
	}
	name := os.Getenv("BORGE_ROUNDTRIP_ARCHIVE")
	if name == "" {
		name = "arc"
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
	a, err := OpenByName(m, name)
	if err != nil {
		t.Fatal(err)
	}

	var items, differed int
	lostKeys := map[string]int{}
	changedKeys := map[string]int{}
	unknownKeys := map[string]int{}

	err = a.RawItems(func(v any) error {
		orig, ok := v.(*msgpackx.Map)
		if !ok {
			return fmt.Errorf("item %d is %T, not a map", items, v)
		}
		items++

		before := map[string]any{}
		for _, e := range orig.Entries() {
			if k, ok := e.Key.(string); ok {
				before[k] = e.Value
			}
		}

		decoded, err := item.DecodeItem(orig)
		if err != nil {
			return fmt.Errorf("decoding item %d: %w", items, err)
		}
		for _, e := range decoded.Unknown {
			if k, ok := e.Key.(string); ok {
				unknownKeys[k]++
			}
		}
		reencoded, err := decoded.Encode()
		if err != nil {
			return fmt.Errorf("re-encoding item %d: %w", items, err)
		}
		after := map[string]any{}
		for _, e := range reencoded.Entries() {
			if k, ok := e.Key.(string); ok {
				after[k] = e.Value
			}
		}

		lost := false
		for k, bv := range before {
			av, present := after[k]
			if !present {
				lostKeys[k]++
				lost = true
				continue
			}
			if fmt.Sprintf("%v", av) != fmt.Sprintf("%v", bv) {
				changedKeys[k]++
				lost = true
			}
		}
		if lost {
			differed++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if items == 0 {
		t.Fatalf("archive %q has no items", name)
	}

	t.Logf("archive %q: %d items round-tripped, %d differed", name, items, differed)
	report := func(label string, m map[string]int) {
		if len(m) == 0 {
			t.Logf("  %s: none", label)
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("  %s: %q on %d item(s)", label, k, m[k])
		}
	}
	report("key not borge's, carried in Unknown", unknownKeys)
	report("KEY LOST", lostKeys)
	report("VALUE CHANGED", changedKeys)

	if len(lostKeys) > 0 || len(changedKeys) > 0 {
		t.Errorf("the item round trip is lossy: %d of %d items changed", differed, items)
	}

	// What a clean run here does and does not prove, stated so nobody reads more into it.
	//
	// If the archive contains no key borge does not already know, then dropping unknown
	// keys entirely would *also* produce a clean run - and it does: removing the loop that
	// re-emits Unknown leaves this test passing on both a borge-written and a borg-written
	// archive, because neither contains such a key. Verified by mutation on 2026-08-30
	// rather than assumed.
	//
	// So this audit is a diagnostic for archives written by something newer, not a
	// guarantee. The guarantee is item.TestUnknownKeysArePreserved, which builds an item
	// carrying keys borge has never heard of and requires the re-encoding to be
	// byte-identical; that test does fail when the loop is removed.
	if len(unknownKeys) == 0 {
		t.Logf("NOTE: this archive contains no key borge does not know, so a clean result " +
			"here would also be produced by a borge that discarded unknown keys. " +
			"The real guarantee is item.TestUnknownKeysArePreserved.")
	}
}
