// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// findFixture builds a repository whose archives differ, so a path can be present in some
// and absent from others - which is the case the path cache exists to make cheap, and the
// case where a wrong cache would silently drop a match.
func findFixture(t *testing.T) (*borgRepo, string) {
	t.Helper()
	r := newBorgRepo(t, "none-sha256")
	src := t.TempDir()
	for _, n := range []string{"keep-a.txt", "keep-b.txt", "transient.txt"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.mustRun("create", "-r", r.path, "arc1", src)
	// transient.txt exists only in arc1.
	if err := os.Remove(filepath.Join(src, "transient.txt")); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "arc2", src)
	// later.txt exists only in arc3.
	if err := os.WriteFile(filepath.Join(src, "later.txt"), []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "arc3", src)
	return r, src
}

func mustEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil || len(es) == 0 {
		t.Fatalf("expected a populated cache at %s: %v", dir, err)
	}
	return es
}

// substitutePayload rewrites one cache entry so that it stays structurally valid - same
// magic, same path count, a healthy zstd frame - while holding paths that are not the
// archive's. Only the checksum stands between that and find believing it.
func substitutePayload(t *testing.T, name string) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil || len(b) < 48 {
		t.Fatalf("reading %s: %v", name, err)
	}
	count := binary.LittleEndian.Uint64(b[8:16])
	var plain bytes.Buffer
	for i := uint64(0); i < count; i++ {
		fmt.Fprintf(&plain, "not/the/real/path-%d\n", i)
	}
	var out bytes.Buffer
	out.Write(b[:48]) // magic, count and the ORIGINAL checksum
	enc, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(plain.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// findOut runs *borge* find - r.mustRun runs borg, which is what the fixture uses to write
// the archives and is emphatically not what this test is about.
func findOut(t *testing.T, r *borgRepo, pat string) string {
	t.Helper()
	stdout, stderr, code := r.borge(t, "find", "-r", r.path, "-a", "sh:arc*",
		"--format", "{archivename} {path}{NL}", pat)
	if code != 0 {
		t.Fatalf("borge find %q exited %d: %s", pat, code, stderr)
	}
	return stdout
}

func findVerbose(t *testing.T, r *borgRepo, pat string) string {
	t.Helper()
	stdout, stderr, code := r.borge(t, "find", "-r", r.path, "-a", "sh:arc*", "-v",
		"--format", "{path}{NL}", pat)
	if code != 0 {
		t.Fatalf("borge find -v %q exited %d: %s", pat, code, stderr)
	}
	return stdout + stderr
}

// TestFindIsIdenticalWhateverTheCacheHolds is the path cache's whole contract: it may make
// find faster and must never make it different.
//
// Each pattern is run against a cold cache, then a warm one, then a deliberately corrupted
// one, then no cache at all - and every run must produce the same bytes. The corruption
// arm matters most: a cache that is trusted when it should not be would drop archives from
// the answer, and a search that quietly omits an archive says "no" when it means "unknown".
func TestFindIsIdenticalWhateverTheCacheHolds(t *testing.T) {
	r, _ := findFixture(t)
	cacheDir := t.TempDir()
	t.Setenv("BORGE_CACHE_DIR", cacheDir)

	patterns := []string{
		"sh:**/transient.txt", // in one archive only
		"sh:**/later.txt",     // in one archive only, the newest
		"sh:**/keep-a.txt",    // in every archive
		"sh:**/absent.txt",    // in none
		"re:keep-[ab]\\.txt",  // several per archive
	}

	// cache.Dir nests per repository id, so the entries live under <cacheDir>/<repoID>/paths.
	pathsDirFor := func(t *testing.T) string {
		t.Helper()
		matches, err := filepath.Glob(filepath.Join(cacheDir, "*", "paths"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("expected one path-cache directory under %s, found %v (err %v)", cacheDir, matches, err)
		}
		return matches[0]
	}

	for _, pat := range patterns {
		t.Run(pat, func(t *testing.T) {
			// Cold: nothing cached yet.
			os.RemoveAll(cacheDir)
			cold := findOut(t, r, pat)

			// Warm: the cold run populated it.
			warm := findOut(t, r, pat)
			if warm != cold {
				t.Errorf("warm cache changed the answer:\ncold: %q\nwarm: %q", cold, warm)
			}

			// Corrupted: every entry filled with rubbish. It must be rejected, not believed.
			pathsDir := pathsDirFor(t)
			entries, err := os.ReadDir(pathsDir)
			if err != nil {
				t.Fatalf("expected a populated cache at %s: %v", pathsDir, err)
			}
			if len(entries) == 0 {
				t.Fatal("the cache is empty, so the warm arm proved nothing")
			}
			for _, e := range entries {
				if err := os.WriteFile(filepath.Join(pathsDir, e.Name()), []byte("not a path cache"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			corrupt := findOut(t, r, pat)
			if corrupt != cold {
				t.Errorf("corrupt cache changed the answer:\ncold:    %q\ncorrupt: %q", cold, corrupt)
			}

			// Truncated: a plausible prefix of a real entry, which is what a crash leaves.
			for _, e := range entries {
				name := filepath.Join(pathsDir, e.Name())
				b, err := os.ReadFile(name)
				if err != nil || len(b) < 4 {
					continue
				}
				if err := os.WriteFile(name, b[:len(b)/2], 0o600); err != nil {
					t.Fatal(err)
				}
			}
			truncated := findOut(t, r, pat)
			if truncated != cold {
				t.Errorf("truncated cache changed the answer:\ncold:      %q\ntruncated: %q", cold, truncated)
			}

			// Substituted: a well-formed entry whose payload is a *different* list of the
			// same length. Magic, length and the zstd frame all pass, so this is the only
			// arm the checksum can catch - and without it the archive would be skipped on
			// the strength of paths it does not contain. Corrupting and truncating, above,
			// are both rejected earlier than that and prove nothing about the checksum.
			os.RemoveAll(pathsDir)
			_ = findOut(t, r, pat) // repopulate
			for _, e := range mustEntries(t, pathsDirFor(t)) {
				name := filepath.Join(pathsDirFor(t), e.Name())
				substitutePayload(t, name)
			}
			swapped := findOut(t, r, pat)
			if swapped != cold {
				t.Errorf("an entry whose payload was swapped changed the answer:\ncold:    %q\nswapped: %q", cold, swapped)
			}

			// Removed mid-life.
			os.RemoveAll(pathsDir)
			gone := findOut(t, r, pat)
			if gone != cold {
				t.Errorf("removing the cache changed the answer:\ncold: %q\ngone: %q", cold, gone)
			}
		})
	}
}

// TestFindPathCacheActuallySkips guards against the tests above passing because the cache
// never did anything. Everything else here asserts that find is unchanged, which a cache
// that silently never engaged would also satisfy.
func TestFindPathCacheActuallySkips(t *testing.T) {
	r, _ := findFixture(t)
	cacheDir := t.TempDir()
	t.Setenv("BORGE_CACHE_DIR", cacheDir)

	// Populate.
	findOut(t, r, "sh:**/transient.txt")

	matches, _ := filepath.Glob(filepath.Join(cacheDir, "*", "paths"))
	if len(matches) != 1 {
		t.Fatalf("expected one path-cache directory under %s, found %v", cacheDir, matches)
	}
	entries, err := os.ReadDir(matches[0])
	if err != nil || len(entries) != 3 {
		t.Fatalf("expected 3 cached archives, got %v (err %v)", len(entries), err)
	}

	// transient.txt is in arc1 only, so the two later archives must be skipped.
	out := findVerbose(t, r, "sh:**/transient.txt")
	if !strings.Contains(out, "2 skipped") {
		t.Errorf("expected 2 archives skipped, got:\n%s", out)
	}

	// A pattern present everywhere must skip nothing: the cache proves negatives only.
	out = findVerbose(t, r, "sh:**/keep-a.txt")
	if !strings.Contains(out, "0 skipped") {
		t.Errorf("expected 0 archives skipped for a path in every archive, got:\n%s", out)
	}
}

// TestFindServedFromCacheMatchesTheItemStream checks the second thing the cache does. Where
// the earlier test proves an archive with no match may be skipped, this proves that output
// *served from the cache* is byte-identical to output read from the item stream - and that
// a template needing anything but the path falls back rather than inventing values.
func TestFindServedFromCacheMatchesTheItemStream(t *testing.T) {
	r, _ := findFixture(t)
	cacheDir := t.TempDir()
	t.Setenv("BORGE_CACHE_DIR", cacheDir)

	// Two runners on purpose. The comparison must not pass -v, because the verbose summary
	// reports how many archives the cache served and so differs between a cold run and a
	// warm one by design - comparing it would be comparing the instrument to itself.
	run := func(tmpl, pat string) string {
		t.Helper()
		stdout, stderr, code := r.borge(t, "find", "-r", r.path, "-a", "sh:arc*",
			"--format", tmpl, pat)
		if code != 0 {
			t.Fatalf("borge find exited %d: %s", code, stderr)
		}
		return stdout
	}
	runVerbose := func(tmpl, pat string) string {
		t.Helper()
		stdout, _, code := r.borge(t, "find", "-r", r.path, "-a", "sh:arc*", "-v",
			"--format", tmpl, pat)
		if code != 0 {
			t.Fatalf("borge find -v exited %d", code)
		}
		return stdout
	}

	for _, tc := range []struct {
		tmpl       string
		wantServed bool
	}{
		{"{path}{NL}", true}, // static fields need no item
		{"{archivename} {archiveid} {path}{NL}", true},
		{"{path} {size}{NL}", false}, // size comes from the item
		{"{path} {mode}{NL}", false},
	} {
		t.Run(tc.tmpl, func(t *testing.T) {
			os.RemoveAll(cacheDir)
			cold := run(tc.tmpl, "sh:**/keep-a.txt")
			warm := run(tc.tmpl, "sh:**/keep-a.txt")
			if warm != cold {
				t.Errorf("served output differs from the item stream:\ncold: %q\nwarm: %q", cold, warm)
			}
			summary := runVerbose(tc.tmpl, "sh:**/keep-a.txt")
			served := !strings.Contains(summary, "0 served by the path cache")
			if served != tc.wantServed {
				t.Errorf("template %q served=%v, want %v\nsummary: %s", tc.tmpl, served, tc.wantServed, summary)
			}
		})
	}
}
