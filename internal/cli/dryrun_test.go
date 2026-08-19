// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// create --dry-run: walk and decide, store nothing.
//
// It is the option a user reaches for before trusting a new exclude pattern, so the thing
// it has to get right is not "no archive appears" but "the report is what a real run would
// have done".
func TestCreateDryRun(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for _, rel := range []string{"f.txt", "sub/g.txt", "sub/h.txt", "other/i.txt"} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// relToSrc normalises either spelling of a path to one relative to the source root.
	//
	// The two sides of this test speak different forms and that is correct: a listing
	// names the path borge *read* ("/tmp/x/f.txt"), while the archive holds the path it
	// *stored* ("tmp/x/f.txt") - see DIVERGENCES.md #40. This test is about which items a
	// dry run promises, not how they are spelled, so both are reduced to the same thing.
	// The spelling has its own test, TestCreateListPathMatchesBorg.
	abs := filepath.ToSlash(src)
	rel := strings.TrimPrefix(abs, "/")
	relToSrc := func(p string) string {
		if p == abs || p == rel {
			return "."
		}
		p = strings.TrimPrefix(p, abs+"/")
		return strings.TrimPrefix(p, rel+"/")
	}

	// statuses splits a --list run into the paths it said it would keep and drop.
	statuses := func(out string) (kept, dropped []string) {
		for _, line := range strings.Split(out, "\n") {
			if len(line) < 3 {
				continue
			}
			path := relToSrc(line[2:])
			switch line[0] {
			case '+':
				kept = append(kept, path)
			case '-':
				dropped = append(dropped, path)
			}
		}
		sort.Strings(kept)
		sort.Strings(dropped)
		return kept, dropped
	}

	// A dry run with an exclusion says what it would keep and what it would drop.
	// The listing is read from stderr because that is where borg puts it, and where
	// borge put it from 2026-08-18: "borg create --list" writes nothing to stdout.
	stdout, stderr, code := r.borge(t, "create", "-n", "--list", "--exclude", "sh:**/sub", "a", src)
	if code != ExitOK {
		t.Fatalf("borge create -n exited %d\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("create wrote to stdout, which --json needs clean:\n%s", stdout)
	}
	kept, dropped := statuses(stderr)
	if len(kept) == 0 || len(dropped) == 0 {
		t.Fatalf("a dry run that reported %d kept and %d dropped proves nothing about "+
			"either\n%s", len(kept), len(dropped), stderr)
	}
	if strings.Join(dropped, ",") != "sub" && !strings.HasSuffix(dropped[0], "sub") {
		t.Errorf("the excluded directory is not what was reported dropped: %v", dropped)
	}

	// Nothing was written.
	if names := borgArchiveNames(t, r); len(names) != 0 {
		t.Fatalf("a dry run created an archive: %v", names)
	}

	// And the report matches what a real run stores. This is the assertion that makes the
	// option worth anything: a dry run whose list did not match reality would be worse
	// than no dry run, because it would be believed.
	if _, stderr, code := r.borge(t, "create", "--exclude", "sh:**/sub", "real", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}
	var stored []string
	for _, p := range sortedItemPaths(t, r.mustRun("list", "-r", r.path, "real", "--json-lines")) {
		stored = append(stored, relToSrc(p))
	}
	sort.Strings(stored)
	if strings.Join(kept, "\n") != strings.Join(stored, "\n") {
		t.Errorf("the dry run said it would store\n  %s\nand the real run stored\n  %s",
			strings.Join(kept, "\n  "), strings.Join(stored, "\n  "))
	}
}

// TestDryRunDoesNotPoisonTheFilesCache is the failure that would be silent and expensive.
//
// The files cache spares a file from being read when it looks unchanged. If a dry run
// updated it, the *next* real backup would skip files the dry run never stored, and the
// archive would be missing them - with no error anywhere.
func TestDryRunDoesNotPoisonTheFilesCache(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := r.borge(t, "create", "-n", "same-name", src); code != ExitOK {
		t.Fatalf("dry run exited %d\n%s", code, stderr)
	}
	// The archive name is what the files cache is keyed by, so the real run below is the
	// one that would be affected.
	_, stderr, code := r.borge(t, "create", "--list", "same-name", src)
	if code != ExitOK {
		t.Fatalf("real run exited %d\n%s", code, stderr)
	}
	if strings.Contains(stderr, "U ") {
		t.Errorf("the real run treated a file as unchanged after a dry run:\n%s", stderr)
	}
	// And the file really is in the archive.
	paths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "same-name", "--json-lines"))
	found := false
	for _, p := range paths {
		if strings.HasSuffix(p, "f.txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("the file is missing from the archive the real run wrote: %v", paths)
	}
}

// TestCreateListReportsExclusions: borg prints a "-" line for every excluded path, and
// borge printed nothing at all - so "--list --exclude" showed only what was kept and could
// not confirm the exclusion had happened. That is the same silent-about-absence shape as
// PORTING_PLAN.md §2.3 collects.
func TestCreateListReportsExclusions(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for _, rel := range []string{"keep.txt", "skip/x.txt"} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, stderr, code := r.borge(t, "create", "--list", "--exclude", "sh:**/skip", "a", src)
	if code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "- ") {
		t.Errorf("a real create with an exclusion reported nothing excluded:\n%s", stderr)
	}
	if !strings.Contains(stderr, "skip") {
		t.Errorf("the excluded path is not named:\n%s", stderr)
	}
}
