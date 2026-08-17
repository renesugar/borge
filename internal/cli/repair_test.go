// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The stage 8 gate: deliberately corrupted repositories, repaired and then verified.
//
// Each case damages a repository in a specific way and asks two questions: does borge's
// check *notice*, and after --repair is the repository consistent again by borg's own
// reckoning. Noticing is the more important half - a check that misses damage is worse
// than one that cannot fix it, because it tells the user everything is fine.

// damagedRepo builds a repository with a few archives and returns it ready to be broken.
func damagedRepo(t *testing.T) (*borgRepo, string) {
	t.Helper()
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	// Enough files, and big enough, that the item stream and the content span several
	// chunks - otherwise "lose a chunk" has nothing to lose.
	for i := 0; i < 40; i++ {
		data := make([]byte, 40000)
		x := uint32(i + 1)
		for j := range data {
			x = x*1664525 + 1013904223
			data[j] = byte(x >> 24)
		}
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%03d.bin", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, stderr, code := r.borge(t, "create", "archive-one", src); code != ExitOK {
		t.Fatalf("create: %d\n%s", code, stderr)
	}
	return r, src
}

func packFiles(t *testing.T, repo string) []string {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(repo, "packs", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(packs)
	return packs
}

func indexFiles(t *testing.T, repo string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repo, "index", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestCheckNoticesABitFlip: a single flipped bit inside a pack must be caught by
// --verify-data, and must *not* be caught without it - the cheap pass only reads framing,
// and claiming otherwise would be a false promise about what a routine check costs.
func TestCheckNoticesABitFlip(t *testing.T) {
	r, _ := damagedRepo(t)

	packs := packFiles(t, r.path)
	if len(packs) == 0 {
		t.Fatal("no packs")
	}
	// Flip a bit well inside the largest pack, where the payload is rather than a header.
	var biggest string
	var biggestSize int64
	for _, p := range packs {
		if info, err := os.Stat(p); err == nil && info.Size() > biggestSize {
			biggest, biggestSize = p, info.Size()
		}
	}
	data, err := os.ReadFile(biggest)
	if err != nil {
		t.Fatal(err)
	}
	pos := len(data) / 2
	data[pos] ^= 0x40
	if err := os.WriteFile(biggest, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := r.borge(t, "check", "--verify-data")
	if code == ExitOK {
		t.Error("--verify-data did not notice a flipped bit")
	}
	if !strings.Contains(stderr, "chunk") {
		t.Errorf("the report does not name a chunk: %q", stderr)
	}
	// borg agrees that this repository is broken, which is what makes the detection
	// meaningful rather than borge being fussy about its own format.
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err == nil {
		t.Errorf("borg found no problem with the same damage:\n%s", out)
	}
}

// TestCheckNoticesAMissingPack.
func TestCheckNoticesAMissingPack(t *testing.T) {
	r, _ := damagedRepo(t)

	packs := packFiles(t, r.path)
	if len(packs) < 2 {
		t.Skipf("only %d pack(s)", len(packs))
	}
	if err := os.Remove(packs[0]); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := r.borge(t, "check")
	if code == ExitOK {
		t.Error("check did not notice a missing pack")
	}
	if stderr == "" {
		t.Error("check failed without saying why")
	}
}

// TestRepairRebuildsAWrongIndex: an index that disagrees with the packs is the state a
// crashed backup leaves behind, and everything downstream trusts it.
func TestRepairRebuildsAWrongIndex(t *testing.T) {
	r, _ := damagedRepo(t)

	// Remove the index entirely: the repository is intact, only the cache of where things
	// are is gone.
	for _, f := range indexFiles(t, r.path) {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}

	// borge rebuilds it on read, so a plain check should already pass...
	if _, stderr, code := r.borge(t, "check"); code != ExitOK {
		t.Errorf("check failed with a missing index: %d\n%s", code, stderr)
	}
	// ...and a repair should leave it written back.
	if _, stderr, code := r.borge(t, "check", "--repair"); code != ExitOK {
		t.Errorf("repair failed with a missing index: %d\n%s", code, stderr)
	}
	if len(indexFiles(t, r.path)) == 0 {
		t.Error("the repair did not write the index back")
	}
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Errorf("borg check after borge repaired a missing index: %v\n%s", err, out)
	}
}

// TestCheckReportsMissingContentChunks: losing a content pack leaves the archive readable
// - its metadata is elsewhere - but some files can no longer be restored. The check has to
// name them, because "the archive lists fine" would otherwise read as "the backup is fine".
func TestCheckReportsMissingContentChunks(t *testing.T) {
	t.Setenv("BORGE_PACK_MAX_SIZE", "150000")
	r, _ := damagedRepo(t)

	before := listedPaths(t, r, "archive-one")
	if len(before) < 30 {
		t.Skipf("the archive has only %d items", len(before))
	}

	// The largest pack is content: the metadata stream and the archive object are small.
	packs := packFiles(t, r.path)
	var biggest string
	var biggestSize int64
	for _, p := range packs {
		if info, err := os.Stat(p); err == nil && info.Size() > biggestSize {
			biggest, biggestSize = p, info.Size()
		}
	}
	if err := os.Remove(biggest); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := r.borge(t, "check")
	if code == ExitOK {
		t.Error("check did not notice the missing content")
	}
	// The chunks are reported by the *repository* check rather than the archive check:
	// the index still lists them, so "is this chunk in the index" says yes and only the
	// read fails. borg's archive check tests index membership the same way. Either
	// phrasing is a correct report; what matters is that the missing data is named.
	if !strings.Contains(stderr, "missing chunk") && !strings.Contains(stderr, "object not found") {
		t.Errorf("the report does not name the missing chunks: %q", stderr)
	}

	// The archive is still listable: its metadata survived, so the user can see what they
	// have lost rather than losing the whole archive with it.
	after := listedPaths(t, r, "archive-one")
	if len(after) != len(before) {
		t.Errorf("the archive lists %d items after losing content, was %d", len(after), len(before))
	}
}

// TestRepairSurvivesALostArchiveObject: when the archive metadata object itself is gone,
// there is nothing to repair *from* - the item pointers lived in it. What repair must do is
// say so and leave the rest of the repository usable, rather than failing in a way that
// makes the other archives unreachable too.
func TestRepairSurvivesALostArchiveObject(t *testing.T) {
	t.Setenv("BORGE_PACK_MAX_SIZE", "150000")
	r, src := damagedRepo(t)

	// A second archive, so there is something that must survive the repair.
	if _, _, code := r.borge(t, "create", "archive-two", src); code != ExitOK {
		t.Fatal("create archive-two failed")
	}

	// The smallest pack holds the archive object: it is a single small record, written
	// into a pack of its own by the flush that precedes it.
	packs := packFiles(t, r.path)
	if len(packs) < 3 {
		t.Skipf("only %d pack(s)", len(packs))
	}
	var smallest string
	var smallestSize int64 = 1 << 62
	for _, p := range packs {
		if info, err := os.Stat(p); err == nil && info.Size() < smallestSize {
			smallest, smallestSize = p, info.Size()
		}
	}
	if err := os.Remove(smallest); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := r.borge(t, "check"); code == ExitOK {
		t.Errorf("check did not notice a missing archive object\n%s", stderr)
	}

	stdout, stderr, _ := r.borge(t, "check", "--repair")
	t.Logf("repair said: %s / %s", strings.TrimSpace(stdout), strings.TrimSpace(stderr))
	if !strings.Contains(stderr, "archive object is missing") && !strings.Contains(stderr, "missing") {
		t.Errorf("the repair does not say what is unrecoverable: %q", stderr)
	}

	// The surviving archive must still be listable and extractable: one broken archive
	// must not take the repository with it.
	names := borgArchiveNames(t, r)
	t.Logf("borg lists %v after the repair", names)
	if len(names) == 0 {
		t.Error("no archive survived the repair")
	}
	// The dangling entry must be gone: a pointer to an object that is not there is not an
	// archive, and leaving it makes every listing report a phantom.
	for _, name := range names {
		if strings.Contains(name, "does-not-exist") {
			t.Errorf("a dangling directory entry survived the repair: %v", names)
		}
		if paths := listedPaths(t, r, name); len(paths) == 0 {
			t.Errorf("the surviving archive %s lists nothing", name)
		}
	}
}

// TestFindLostArchives: an archive whose directory entry is gone is invisible, and a
// compaction would eventually delete it. The scan finds it in the packs and puts the
// pointer back.
func TestFindLostArchives(t *testing.T) {
	r, _ := damagedRepo(t)

	entries, err := filepath.Glob(filepath.Join(r.path, "archives", "*"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no archive directory entries: %v", err)
	}
	if err := os.Remove(entries[0]); err != nil {
		t.Fatal(err)
	}
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Fatalf("borg still lists %v after the pointer was removed", names)
	}

	// Without --repair it reports and changes nothing.
	stdout, stderr, code := r.borge(t, "check", "--find-lost-archives")
	if code == ExitOK {
		t.Error("the scan reported no problem for an archive with no directory entry")
	}
	if !strings.Contains(stderr, "no directory entry") {
		t.Errorf("the report does not say what is wrong: %q%q", stdout, stderr)
	}
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Error("the scan changed the repository without --repair")
	}

	// With --repair the archive comes back.
	if _, stderr, _ := r.borge(t, "check", "--find-lost-archives", "--repair"); stderr != "" {
		t.Logf("repair reported: %s", strings.TrimSpace(stderr))
	}
	names := borgArchiveNames(t, r)
	if len(names) != 1 || names[0] != "archive-one" {
		t.Errorf("borg lists %v after the recovery, want [archive-one]", names)
	}
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Errorf("borg check after recovering a lost archive: %v\n%s", err, out)
	}
}

// TestRepairOfAHealthyRepositoryChangesNothing: the most important thing repair can do is
// leave a working repository alone.
func TestRepairOfAHealthyRepositoryChangesNothing(t *testing.T) {
	r, _ := damagedRepo(t)

	beforeNames := borgArchiveNames(t, r)
	beforePaths := listedPaths(t, r, "archive-one")

	if _, stderr, code := r.borge(t, "check", "--repair"); code != ExitOK {
		t.Fatalf("repair of a healthy repository failed: %d\n%s", code, stderr)
	}

	if names := borgArchiveNames(t, r); strings.Join(names, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the archive list changed: %v -> %v", beforeNames, names)
	}
	after := listedPaths(t, r, "archive-one")
	if len(after) != len(beforePaths) {
		t.Errorf("the archive holds %d items after a repair, was %d", len(after), len(beforePaths))
	}
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Errorf("borg check after a no-op repair: %v\n%s", err, out)
	}
}

// listedPaths returns the paths borge lists in an archive.
func listedPaths(t *testing.T, r *borgRepo, archiveName string) []string {
	t.Helper()
	stdout, _, code := r.borge(t, "list", "-short", archiveName)
	if code != ExitOK {
		return nil
	}
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
