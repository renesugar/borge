// SPDX-License-Identifier: Apache-2.0

//go:build linux

package archive

import (
	"os/user"
	"strconv"
	"testing"

	"github.com/renesugar/borge/internal/item"
)

// Caching owner lookups is a performance fix that must not change who owns a restored
// file. The cache is per extraction and keyed by name, so the two things to prove are that
// it answers the same as the system does, and that it answers a *failure* the same way -
// restoring somebody else's backup is the case where most names do not resolve, and it is
// exactly the case where an uncached lookup costs the most.

func newTestExtractor() *extractor {
	return &extractor{
		uids: map[string]int{},
		gids: map[string]int{},
	}
}

// TestOwnerCacheAgreesWithTheSystem. The cache is only worth having if it is invisible.
func TestOwnerCacheAgreesWithTheSystem(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user; cannot compare against the system")
	}
	wantUID, err := strconv.Atoi(me.Uid)
	if err != nil {
		t.Skipf("uid %q is not a number on this platform", me.Uid)
	}

	x := newTestExtractor()
	for range 3 { // repeated, because the second answer comes from the cache
		if got := x.lookupUID(me.Username); got != wantUID {
			t.Fatalf("lookupUID(%q) = %d, want %d", me.Username, got, wantUID)
		}
	}

	g, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Skip("primary group does not resolve; nothing to compare")
	}
	wantGID, err := strconv.Atoi(g.Gid)
	if err != nil {
		t.Skipf("gid %q is not a number", g.Gid)
	}
	for range 3 {
		if got := x.lookupGID(g.Name); got != wantGID {
			t.Fatalf("lookupGID(%q) = %d, want %d", g.Name, got, wantGID)
		}
	}
}

// TestOwnerCacheRemembersFailures is the half that is easy to leave out.
//
// A name that does not exist here is the common case when restoring another machine's
// archive. Without caching the miss, every one of an archive's files pays a full cgo
// lookup to be told the same thing.
func TestOwnerCacheRemembersFailures(t *testing.T) {
	const absent = "borge-no-such-user-9d2f1c"
	x := newTestExtractor()
	if got := x.lookupUID(absent); got != -1 {
		t.Fatalf("lookupUID of an absent name = %d, want -1", got)
	}
	if _, cached := x.uids[absent]; !cached {
		t.Error("a failed user lookup was not cached, so every item would repeat it")
	}
	if got := x.lookupGID(absent); got != -1 {
		t.Fatalf("lookupGID of an absent name = %d, want -1", got)
	}
	if _, cached := x.gids[absent]; !cached {
		t.Error("a failed group lookup was not cached, so every item would repeat it")
	}
}

// TestResolveOwnerFallsBackToStoredNumbers pins the behaviour the cache sits inside:
// a name that does not resolve leaves the stored numeric id in place rather than
// clearing it, and NumericIDs skips the names entirely.
func TestResolveOwnerFallsBackToStoredNumbers(t *testing.T) {
	uid, gid := int64(4321), int64(8765)
	absent := "borge-no-such-user-9d2f1c"
	it := &item.Item{UID: &uid, GID: &gid, User: &absent, Group: &absent}

	x := newTestExtractor()
	gotUID, gotGID := x.resolveOwner(it)
	if gotUID != int(uid) || gotGID != int(gid) {
		t.Errorf("resolveOwner with unresolvable names = %d/%d, want the stored %d/%d",
			gotUID, gotGID, uid, gid)
	}

	// With NumericIDs the names are not consulted at all, so the cache stays empty.
	x = newTestExtractor()
	x.opts.NumericIDs = true
	gotUID, gotGID = x.resolveOwner(it)
	if gotUID != int(uid) || gotGID != int(gid) {
		t.Errorf("resolveOwner --numeric-ids = %d/%d, want %d/%d", gotUID, gotGID, uid, gid)
	}
	if len(x.uids) != 0 || len(x.gids) != 0 {
		t.Error("--numeric-ids looked a name up anyway")
	}
}
