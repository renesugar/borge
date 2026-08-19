// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoBytes is the total size of a repository's pack files.
func repoBytes(t *testing.T, repo string) int64 {
	t.Helper()
	var total int64
	packs, err := filepath.Glob(filepath.Join(repo, "packs", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packs {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

func packCount(t *testing.T, repo string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(repo, "packs", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

// writeRandomTree writes n files of the given size with distinct, incompressible content.
func writeRandomTree(t *testing.T, dir string, n, size int, seed byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		data := make([]byte, size)
		// Deterministic but incompressible enough that chunks do not deduplicate between
		// trees with different seeds.
		x := uint32(seed)<<16 | uint32(i)
		for j := range data {
			x = x*1664525 + 1013904223
			data[j] = byte(x >> 24)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.bin", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCompactReclaimsSpace: deleting an archive with unique content and compacting must
// actually return the space, and leave a repository borg still accepts.
func TestCompactReclaimsSpace(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	shared := filepath.Join(t.TempDir(), "shared")
	writeRandomTree(t, shared, 4, 300000, 1)
	unique := filepath.Join(t.TempDir(), "unique")
	writeRandomTree(t, unique, 4, 300000, 2)

	if _, stderr, code := r.borge(t, "create", "keep", shared); code != ExitOK {
		t.Fatalf("create keep: %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borge(t, "create", "drop", unique); code != ExitOK {
		t.Fatalf("create drop: %d\n%s", code, stderr)
	}
	before := repoBytes(t, r.path)

	if _, stderr, code := r.borge(t, "delete", "drop"); code != ExitOK {
		t.Fatalf("delete: %d\n%s", code, stderr)
	}
	// A delete on its own must reclaim nothing: it removes a pointer, not data.
	if after := repoBytes(t, r.path); after != before {
		t.Errorf("delete alone changed the repository size: %d -> %d", before, after)
	}

	stdout, stderr, code := r.borge(t, "compact", "-stats")
	if code != ExitOK {
		t.Fatalf("compact: %d\n%s", code, stderr)
	}
	t.Log(strings.TrimSpace(stdout))

	after := repoBytes(t, r.path)
	if after >= before {
		t.Errorf("compact reclaimed nothing: %d -> %d", before, after)
	}
	// The unique tree is 1.2 MB; most of it should be gone.
	if reclaimed := before - after; reclaimed < 1_000_000 {
		t.Errorf("compact reclaimed only %d bytes of about 1.2 MB", reclaimed)
	}

	// borg must still be happy, and the surviving archive must still be there.
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Fatalf("borg check after borge compacted: %v\n%s", err, out)
	}
	if names := borgArchiveNames(t, r); strings.Join(names, ",") != "keep" {
		t.Errorf("borg lists %v after the compaction, want [keep]", names)
	}

	// And the surviving archive still restores.
	dest := t.TempDir()
	if _, stderr, code := r.borge(t, "extract", "-C", dest, "keep"); code != ExitOK {
		t.Fatalf("extract after compact: %d\n%s", code, stderr)
	}
	restored := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(shared), "/")))
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("f%02d.bin", i)
		want, err := os.ReadFile(filepath.Join(shared, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("%s missing after compaction: %v", name, err)
		}
		if string(want) != string(got) {
			t.Errorf("%s differs after compaction", name)
		}
	}
}

// TestCompactRewritesMixedPacks exercises the path the previous test does not: a pack
// holding both live and dead chunks, which cannot be dropped and has to be rewritten.
//
// Small packs are forced with BORGE_PACK_MAX_SIZE so the two archives' chunks interleave;
// with the default 50 MB pack size a test tree fits in one pack and the case never arises.
func TestCompactRewritesMixedPacks(t *testing.T) {
	t.Setenv("BORGE_PACK_MAX_SIZE", "400000")
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	// The mixed pack is produced deliberately. One archive backs up the whole tree, so its
	// packs hold every file's chunks; a second backs up only half of it, which deduplicates
	// entirely and writes no content chunks of its own. Deleting the *first* archive then
	// leaves packs where half the chunks are still referenced and half are not - which is
	// the only shape that cannot be dropped and has to be rewritten.
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	writeRandomTree(t, tree, 8, 120000, 1)
	if _, _, code := r.borge(t, "create", "drop", tree); code != ExitOK {
		t.Fatal("create drop failed")
	}

	half := filepath.Join(base, "half")
	if err := os.MkdirAll(half, 0o755); err != nil {
		t.Fatal(err)
	}
	// Every *other* file, not the first half. Packs are filled in walk order, so a
	// contiguous subset can line up exactly with a pack boundary and produce packs that
	// are wholly live or wholly dead - which is the case this test is not about. An
	// interleaved subset guarantees every content pack holds some of each.
	kept := []int{}
	for i := 0; i < 8; i += 2 {
		kept = append(kept, i)
		name := fmt.Sprintf("f%02d.bin", i)
		data, err := os.ReadFile(filepath.Join(tree, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(half, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, code := r.borge(t, "create", "keep", half); code != ExitOK {
		t.Fatal("create keep failed")
	}

	packsBefore := packCount(t, r.path)
	if packsBefore < 3 {
		t.Skipf("only %d pack(s); this test needs several to produce a mixed one", packsBefore)
	}

	if _, _, code := r.borge(t, "delete", "drop"); code != ExitOK {
		t.Fatal("delete failed")
	}
	stdout, stderr, code := r.borge(t, "compact", "-stats", "-threshold", "1")
	if code != ExitOK {
		t.Fatalf("compact: %d\n%s", code, stderr)
	}
	t.Logf("%d packs before\n%s", packsBefore, strings.TrimSpace(stdout))
	if strings.Contains(stdout, "0 rewritten") {
		t.Error("no pack was rewritten; this test exists to exercise that path, " +
			"so a repository shaped differently than intended makes it prove nothing")
	}

	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Fatalf("borg check after a pack rewrite: %v\n%s", err, out)
	}
	if _, stderr, code := r.borge(t, "check", "--verify-data"); code != ExitOK {
		t.Fatalf("borge check after a pack rewrite: %d\n%s", code, stderr)
	}

	// The surviving archive must still restore: a rewrite that lost or misplaced an
	// object is exactly what this is guarding against.
	dest := t.TempDir()
	if _, stderr, code := r.borge(t, "extract", "-C", dest, "keep"); code != ExitOK {
		t.Fatalf("extract after a pack rewrite: %d\n%s", code, stderr)
	}
	restored := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(half), "/")))
	for _, i := range kept {
		name := fmt.Sprintf("f%02d.bin", i)
		want, err := os.ReadFile(filepath.Join(half, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("%s missing after a pack rewrite: %v", name, err)
		}
		if string(want) != string(got) {
			t.Errorf("%s differs after a pack rewrite", name)
		}
	}
}

// TestCompactRefusesOnMissingChunks is the safety property: a compaction that proceeded
// from an incomplete view of what is referenced would delete live data, silently.
func TestCompactRefusesOnMissingChunks(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	tree := filepath.Join(t.TempDir(), "tree")
	writeRandomTree(t, tree, 3, 200000, 1)
	if _, _, code := r.borge(t, "create", "one", tree); code != ExitOK {
		t.Fatal("create failed")
	}

	// Remove a pack behind borge's back, which is what a damaged repository looks like.
	packs, err := filepath.Glob(filepath.Join(r.path, "packs", "*", "*"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("no packs to damage: %v", err)
	}
	if err := os.Remove(packs[0]); err != nil {
		t.Fatal(err)
	}
	packsBefore := packCount(t, r.path)

	_, stderr, code := r.borge(t, "compact")
	if code == ExitOK {
		t.Error("compact succeeded on a repository with a missing pack")
	}
	// The message has to name what went wrong. Which layer notices first depends on where
	// the damage is - a missing pack can surface as an unreadable archive before the
	// reference scan completes - so the assertion is that it says something specific, not
	// that it says one particular sentence.
	if !strings.Contains(stderr, "archive") && !strings.Contains(stderr, "refusing") {
		t.Errorf("the refusal does not say why: %q", stderr)
	}
	// And the refusal changed nothing. Listing is *not* the check here: the pack that was
	// removed held archive metadata, so the repository really is damaged and repo-list is
	// right to say so. What matters is that the refused compaction did not delete anything
	// further on its way out.
	if after := packCount(t, r.path); after != packsBefore {
		t.Errorf("a refused compaction changed the pack count: %d -> %d", packsBefore, after)
	}
}

// TestCompactDryRunChangesNothing.
func TestCompactDryRunChangesNothing(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	tree := filepath.Join(t.TempDir(), "tree")
	writeRandomTree(t, tree, 3, 200000, 1)
	if _, _, code := r.borge(t, "create", "one", tree); code != ExitOK {
		t.Fatal("create failed")
	}
	if _, _, code := r.borge(t, "create", "two", tree); code != ExitOK {
		t.Fatal("create failed")
	}
	if _, _, code := r.borge(t, "delete", "two"); code != ExitOK {
		t.Fatal("delete failed")
	}

	before := repoBytes(t, r.path)
	stdout, stderr, code := r.borge(t, "compact", "-dry-run")
	if code != ExitOK {
		t.Fatalf("compact --dry-run: %d\n%s", code, stderr)
	}
	if after := repoBytes(t, r.path); after != before {
		t.Errorf("a dry run changed the repository: %d -> %d", before, after)
	}
	// The soft-deleted archive must still be recoverable after a dry run.
	if _, stderr, code := r.borge(t, "undelete", "two"); code != ExitOK {
		t.Errorf("undelete after a dry run failed: %d\n%s", code, stderr)
	}
	t.Log(strings.TrimSpace(stdout))
}

// TestPruneMatchesBorg: the set of archives borge's retention rules keep has to be the set
// borg's keep. A prune that disagrees deletes backups the user meant to have.
//
// Both tools are asked what they *would* do, with --dry-run, over a repository whose
// archives are spread across days, weeks and months. Comparing dry runs rather than
// outcomes means a disagreement is reported rather than acted on.
func TestPruneMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Archives dated across four months, several per day in places, so the daily, weekly
	// and monthly rules all have something to choose between.
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	var made []string
	for i, offset := range []time.Duration{
		0, 2 * time.Hour, 5 * time.Hour,
		24 * time.Hour, 26 * time.Hour,
		48 * time.Hour, 72 * time.Hour, 96 * time.Hour, 120 * time.Hour,
		7 * 24 * time.Hour, 14 * 24 * time.Hour, 21 * 24 * time.Hour,
		35 * 24 * time.Hour, 70 * 24 * time.Hour, 100 * 24 * time.Hour,
	} {
		name := fmt.Sprintf("ar%02d", i)
		ts := base.Add(-offset).Format("2006-01-02T15:04:05")
		r.mustRun("create", "-r", r.path, "--timestamp", ts, name, src)
		made = append(made, name)
	}
	t.Logf("created %d archives", len(made))

	policies := []struct {
		name      string
		borgArgs  []string
		borgeArgs []string
	}{
		{"daily=3", []string{"--keep-daily=3"}, []string{"-keep-daily", "3"}},
		{"daily=7 monthly=3", []string{"--keep-daily=7", "--keep-monthly=3"},
			[]string{"-keep-daily", "7", "-keep-monthly", "3"}},
		{"weekly=4", []string{"--keep-weekly=4"}, []string{"-keep-weekly", "4"}},
		// borg spells this one "--keep"; borge spells it "--keep-last", because "keep"
		// alone reads like the whole policy rather than one rule of it.
		{"last=5", []string{"--keep=5"}, []string{"-keep-last", "5"}},
		{"hourly=4 daily=2", []string{"--keep-hourly=4", "--keep-daily=2"},
			[]string{"-keep-hourly", "4", "-keep-daily", "2"}},
	}

	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			// borg's dry run names what it would keep and what it would prune.
			args := append([]string{"prune", "-r", r.path, "--dry-run", "--list"}, p.borgArgs...)
			borgOut := r.mustRun(args...)
			borgKeep := keptArchiveNames(borgOut)
			if len(borgKeep) == 0 {
				t.Skipf("could not read borg's decisions from:\n%s", borgOut)
			}

			borgeArgs := append([]string{"prune", "-dry-run", "-list"}, p.borgeArgs...)
			stdout, stderr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge prune exited %d\n%s", code, stderr)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Errorf("prune wrote to stdout, which is for a command's data:\n%s", stdout)
			}
			// The same parser as borg's above, because the two listings are now the same
			// format. It used to be a second parser for borge's own layout, and when that
			// layout changed the parser quietly matched nothing and every archive borg
			// kept was reported as pruned by borge - six failures describing a decision
			// bug that did not exist.
			borgeKeep := keptArchiveNames(stderr)
			if len(borgeKeep) == 0 {
				t.Fatalf("could not read borge's decisions from:\n%s", stderr)
			}

			for name := range borgKeep {
				if !borgeKeep[name] {
					t.Errorf("borg keeps %s, borge prunes it", name)
				}
			}
			for name := range borgeKeep {
				if !borgKeep[name] {
					t.Errorf("borge keeps %s, borg prunes it", name)
				}
			}
			t.Logf("both keep %d archive(s)", len(borgKeep))
		})
	}
}

// TestPruneRefusesAnEmptyPolicy: with no rules every archive would be pruned, which is
// never what somebody meant to type.
func TestPruneRefusesAnEmptyPolicy(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := r.borge(t, "create", "one", src); code != ExitOK {
		t.Fatal("create failed")
	}

	_, stderr, code := r.borge(t, "prune")
	if code == ExitOK {
		t.Error("prune with no rules succeeded")
	}
	if !strings.Contains(stderr, "keep") {
		t.Errorf("the refusal does not say what is missing: %q", stderr)
	}
	if names := borgArchiveNames(t, r); len(names) != 1 {
		t.Errorf("the refused prune changed the archive list: %v", names)
	}
}

// TestPruneIsSoftAndRecoverable: a pruned archive is undeletable until a compaction runs.
func TestPruneIsSoftAndRecoverable(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	for i := 0; i < 4; i++ {
		ts := base.AddDate(0, 0, -i).Format("2006-01-02T15:04:05")
		r.mustRun("create", "-r", r.path, "--timestamp", ts, fmt.Sprintf("d%d", i), src)
	}

	if _, stderr, code := r.borge(t, "prune", "-keep-daily", "2"); code != ExitOK {
		t.Fatalf("prune exited %d\n%s", code, stderr)
	}
	names := borgArchiveNames(t, r)
	if len(names) != 2 {
		t.Fatalf("after pruning to 2 daily, borg lists %v", names)
	}

	// The pruned ones are still there, soft-deleted.
	if _, stderr, code := r.borge(t, "undelete", "-a", "d3"); code != ExitOK {
		t.Errorf("a pruned archive could not be undeleted: %d\n%s", code, stderr)
	}
	if names := borgArchiveNames(t, r); len(names) != 3 {
		t.Errorf("after the undelete borg lists %v, want 3", names)
	}
	if out, err := r.runErr("check", "-r", r.path); err != nil {
		t.Errorf("borg check after prune and undelete: %v\n%s", err, out)
	}
}

// keptArchiveNames reads the archive names out of a prune listing.
//
// One parser for both tools, because borge's listing is borg's now: "Keeping archive
// (rule: daily #1):    NAME    <date> [id]", so the name is the first field after the
// colon. Two parsers is how the borge side came to match nothing when its layout changed.
func keptArchiveNames(out string) map[string]bool {
	kept := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "Keeping archive") {
			continue
		}
		_, rest, found := strings.Cut(line, "):")
		if !found {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			kept[fields[0]] = true
		}
	}
	return kept
}
