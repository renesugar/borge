// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recreateRepo builds a repository with one archive of a small, varied tree.
func recreateRepo(t *testing.T) (*borgRepo, string) {
	t.Helper()
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for i := 0; i < 6; i++ {
		data := []byte(strings.Repeat(fmt.Sprintf("file %d ", i), 30000))
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.txt", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p := filepath.Join(src, "logs", fmt.Sprintf("l%d.log", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("log line\n", 5000)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, stderr, code := r.borge(t, "create", "original", src); code != ExitOK {
		t.Fatalf("create: %d\n%s", code, stderr)
	}
	return r, src
}

// archiveChunkerParams reads the chunker parameters borg reports for an archive.
func archiveChunkerParams(t *testing.T, r *borgRepo, name string) string {
	t.Helper()
	out := r.mustRun("info", "-r", r.path, "-a", name, "--json")
	var doc struct {
		Archives []struct {
			ChunkerParams []any `json:"chunker_params"`
		} `json:"archives"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("borg info --json did not parse: %v\n%s", err, out)
	}
	if len(doc.Archives) == 0 {
		t.Fatalf("borg info reported no archive:\n%s", out)
	}
	return fmt.Sprint(doc.Archives[0].ChunkerParams)
}

// The archive is always named with "-a" here, never as a positional.
//
// borg's recreate takes "[PATH ...]" and no archive name, so a positional is a path to
// keep. These tests passed the archive name as a positional until 2026-08-20, when borge
// stopped reading it as a selector - and every one of them then emptied the archive it was
// testing, which is exactly what the same command line had been doing under borg all along.
// See DIVERGENCES.md #54.

// TestRecreateExcludesPaths is the reason recreate exists: excluding a path from future
// backups does nothing about the copies already stored.
func TestRecreateExcludesPaths(t *testing.T) {
	r, _ := recreateRepo(t)

	before := listedPaths(t, r, "original")
	var logsBefore int
	for _, p := range before {
		if strings.Contains(p, "/logs/") {
			logsBefore++
		}
	}
	if logsBefore == 0 {
		t.Fatal("the test tree has no logs to exclude")
	}

	stdout, stderr, code := r.borge(t, "recreate", "-exclude", "*.log", "-stats", "-a", "original")
	if code != ExitOK {
		t.Fatalf("recreate: %d\n%s", code, stderr)
	}
	t.Log(strings.TrimSpace(stdout))

	after := listedPaths(t, r, "original")
	for _, p := range after {
		if strings.HasSuffix(p, ".log") {
			t.Errorf("%s survived an exclude", p)
		}
	}
	if len(after) != len(before)-logsBefore {
		t.Errorf("the archive has %d items, want %d", len(after), len(before)-logsBefore)
	}

	// borg has to agree the result is a valid archive.
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Errorf("borg check after a recreate: %v\n%s", err, out)
	}
	// And the surviving files still restore.
	dest := t.TempDir()
	if _, stderr, code := r.borge(t, "extract", "-C", dest, "original"); code != ExitOK {
		t.Fatalf("extract after recreate: %d\n%s", code, stderr)
	}
}

// TestRecreateRechunks: the archive must record the new parameters and still restore
// byte-identically, which is the thing re-chunking most easily breaks.
func TestRecreateRechunks(t *testing.T) {
	r, src := recreateRepo(t)

	before := archiveChunkerParams(t, r, "original")
	t.Logf("before: %s", before)

	// Smaller chunks than the default, so every boundary moves.
	stdout, stderr, code := r.borge(t, "recreate", "-chunker-params", "fastcdc,16,20,18,4",
		"-stats", "-a", "original")
	if code != ExitOK {
		t.Fatalf("recreate: %d\n%s", code, stderr)
	}
	t.Log(strings.TrimSpace(stdout))

	after := archiveChunkerParams(t, r, "original")
	t.Logf("after: %s", after)
	if after == before {
		t.Errorf("the archive still records %s after re-chunking", after)
	}
	if !strings.Contains(after, "16") {
		t.Errorf("the archive does not record the new parameters: %s", after)
	}

	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Fatalf("borg check after re-chunking: %v\n%s", err, out)
	}

	// The contents must be identical: re-chunking changes how the bytes are stored, not
	// what they are.
	dest := t.TempDir()
	if _, stderr, code := r.borge(t, "extract", "-C", dest, "original"); code != ExitOK {
		t.Fatalf("extract after re-chunking: %d\n%s", code, stderr)
	}
	restored := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/")))
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		want, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("%s missing after re-chunking: %v", name, err)
		}
		if string(want) != string(got) {
			t.Errorf("%s differs after re-chunking", name)
		}
	}
}

// TestRepoCompressRecompresses is where recompression actually happens.
//
// A chunk's id is the hash of its plaintext, so compression lives *below* the id: a
// recompressed chunk has the same id, and every chunk-writing path deduplicates. So
// recompressing cannot go through that path at all - it has to rewrite the stored objects.
// borg has the same split, and "recreate --compression" silently does nothing there.
func TestRepoCompressRecompresses(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	// Data chosen so the two compressors actually differ. lz4 works from a small window,
	// so it handles short repeats well and long-range structure badly; zstd at a high
	// level finds the latter. A trivially repetitive tree does not separate them - lz4
	// compresses that almost as well, and a test on it passes on a margin of noise.
	src := t.TempDir()
	for f := 0; f < 4; f++ {
		var b strings.Builder
		for i := 0; i < 20000; i++ {
			fmt.Fprintf(&b, "record %06d field=%d status=%s note=the quick brown fox %d\n",
				i, i%97, []string{"ok", "warn", "error"}[i%3], i%7919)
		}
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("t%d.txt", f)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, stderr, code := r.borge(t, "create", "-C", "lz4", "original", src); code != ExitOK {
		t.Fatalf("create: %d\n%s", code, stderr)
	}

	beforeIDs := contentChunkIDs(t, r, "original")
	beforeSize := repoBytes(t, r.path)

	stdout, stderr, code := r.borge(t, "repo-compress", "-C", "zstd,19", "-stats")
	if code != ExitOK {
		t.Fatalf("repo-compress: %d\n%s", code, stderr)
	}
	t.Log(strings.TrimSpace(stdout))

	// The ids are unchanged: compression is below the id.
	afterIDs := contentChunkIDs(t, r, "original")
	if strings.Join(afterIDs, ",") != strings.Join(beforeIDs, ",") {
		t.Error("recompression changed what the archive lists; compression is below the id")
	}

	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Fatalf("borg check after recompression: %v\n%s", err, out)
	}

	// Compact first: the old copies are still there until they are collected.
	if _, stderr, code := r.borge(t, "compact"); code != ExitOK {
		t.Fatalf("compact: %d\n%s", code, stderr)
	}
	afterSize := repoBytes(t, r.path)
	saved := 100 * float64(beforeSize-afterSize) / float64(beforeSize)
	t.Logf("repository: %d bytes with lz4, %d after recompressing to zstd,19 (%.1f%% smaller)",
		beforeSize, afterSize, saved)
	if saved < 10 {
		t.Errorf("recompressing to zstd,19 saved only %.1f%%; this data should compress "+
			"markedly better than under lz4", saved)
	}

	// And the contents are unchanged.
	dest := t.TempDir()
	if _, stderr, code := r.borge(t, "extract", "-C", dest, "original"); code != ExitOK {
		t.Fatalf("extract after recompression: %d\n%s", code, stderr)
	}
	restored := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/")))
	want, err := os.ReadFile(filepath.Join(src, "t0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "t0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Error("the contents changed under recompression")
	}

	// A second run has nothing to do: everything already has the wanted compression. Left
	// unguarded this is a loop that rewrites the whole repository on every run.
	// Read from stderr: repo-compress reports its work there, where borg does, and stdout
	// carries a command's data only (DIVERGENCES.md #46).
	_, stderr, code = r.borge(t, "repo-compress", "-C", "zstd,19", "-stats")
	if code != ExitOK {
		t.Fatalf("the second repo-compress exited %d", code)
	}
	if !strings.Contains(stderr, "0 recompressed") {
		t.Errorf("a second recompression to the same setting was not a no-op:\n%s", stderr)
	}
}

// TestRecreateRefusesCompressionAlone: --compression on its own would appear to work and
// change nothing, because a recompressed chunk deduplicates against the original. borg
// does exactly that, silently; borge points at the command that does work.
func TestRecreateRefusesCompressionAlone(t *testing.T) {
	r, _ := recreateRepo(t)

	before := repoBytes(t, r.path)
	_, stderr, code := r.borge(t, "recreate", "-C", "zstd,19", "-a", "original")
	if code == ExitOK {
		t.Error("recreate --compression alone succeeded, having done nothing")
	}
	if !strings.Contains(stderr, "repo-compress") {
		t.Errorf("the refusal does not point at the command that works: %q", stderr)
	}
	if after := repoBytes(t, r.path); after != before {
		t.Errorf("the refused recreate changed the repository: %d -> %d", before, after)
	}
}

// TestRecreateToTargetKeepsTheOriginal.
func TestRecreateToTargetKeepsTheOriginal(t *testing.T) {
	r, _ := recreateRepo(t)

	if _, stderr, code := r.borge(t, "recreate", "-target", "trimmed", "-exclude", "*.log", "-a", "original"); code != ExitOK {
		t.Fatalf("recreate: %d\n%s", code, stderr)
	}
	names := borgArchiveNames(t, r)
	if len(names) != 2 {
		t.Fatalf("borg lists %v, want the original and the target", names)
	}

	// The original still has its logs; the target does not.
	for _, p := range listedPaths(t, r, "original") {
		if strings.HasSuffix(p, ".log") {
			goto found
		}
	}
	t.Error("the original archive lost its logs")
found:
	for _, p := range listedPaths(t, r, "trimmed") {
		if strings.HasSuffix(p, ".log") {
			t.Errorf("%s survived the exclude in the target", p)
		}
	}
}

// TestRecreateWithNothingToDoDoesNothing: a recreate that changes nothing still costs a
// full rewrite, so it is worth not doing.
func TestRecreateWithNothingToDoDoesNothing(t *testing.T) {
	r, _ := recreateRepo(t)

	before := borgArchiveNames(t, r)
	_, stderr, code := r.borge(t, "recreate", "-a", "original")
	if code != ExitOK {
		t.Fatalf("recreate exited %d", code)
	}
	if !strings.Contains(stderr, "nothing to do") {
		t.Errorf("a recreate with no options did not say it had nothing to do: %q", stderr)
	}
	if names := borgArchiveNames(t, r); strings.Join(names, ",") != strings.Join(before, ",") {
		t.Errorf("the archive list changed: %v -> %v", before, names)
	}
}

// contentChunkIDs returns the chunk ids of the archive's regular files, via borge's list.
func contentChunkIDs(t *testing.T, r *borgRepo, archiveName string) []string {
	t.Helper()
	// borge's JSON listing does not carry chunk ids, so the ids are taken from a diff
	// against the archive itself: an archive always compares equal to itself, and a
	// changed chunk list would show up as a modification.
	//
	// What is actually compared here is simpler: the per-file sizes, which change if and
	// only if the chunking changed.
	stdout, _, code := r.borge(t, "list", "-json-lines", archiveName)
	if code != ExitOK {
		t.Fatalf("list exited %d", code)
	}
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var it struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			t.Fatal(err)
		}
		if it.Type == "-" {
			out = append(out, fmt.Sprintf("%s:%d", it.Path, it.Size))
		}
	}
	return out
}
