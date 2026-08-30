// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// poolTree builds a tree with the shapes the pool has to get right: enough files to fill
// the workers several times, nested directories whose attributes are applied on the way
// out, a hard link to a file written earlier, and a symlink.
func poolTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, d := range []string{"a", "a/b", "a/b/c", "z"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		dir := []string{"", "a", "a/b", "a/b/c", "z"}[i%5]
		name := filepath.Join(src, dir, fmt.Sprintf("f%03d.txt", i))
		body := strings.Repeat(fmt.Sprintf("%d-", i), i%40+1)
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(src, "f000.txt"), filepath.Join(src, "z", "linked.txt")); err != nil {
		t.Fatal(err)
	}
	// A hard link that is hard to get right: the target is big enough to still be being
	// written when the link is attempted, and the two names are adjacent in the sort order
	// the item stream uses, so the link follows its target immediately. Linking f000.txt
	// above proves nothing about the barrier - by the time the pool reaches it, a 200-item
	// queue has long since drained it.
	big := make([]byte, 12<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(src, "big-0.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "big-0.bin"), filepath.Join(src, "big-1.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../f000.txt", filepath.Join(src, "a", "sym")); err != nil {
		t.Fatal(err)
	}
	// A directory mtime worth checking: distinctive, in the past, and not what writing a
	// file into it would leave behind.
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, d := range []string{"a/b/c", "a/b", "a", "z"} {
		if err := os.Chtimes(filepath.Join(src, d), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// snapshot records every path with the properties an extraction must reproduce.
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			out = append(out, fmt.Sprintf("%s symlink -> %s", rel, tgt))
		case fi.IsDir():
			out = append(out, fmt.Sprintf("%s dir mode=%o mtime=%d", rel, fi.Mode().Perm(), fi.ModTime().Unix()))
		default:
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out = append(out, fmt.Sprintf("%s file mode=%o size=%d mtime=%d sum=%x",
				rel, fi.Mode().Perm(), fi.Size(), fi.ModTime().Unix(), simpleSum(b)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func simpleSum(b []byte) uint64 {
	var h uint64 = 1469598103934665603
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// TestExtractPoolMatchesSerial is the pool's correctness gate: whatever the worker count,
// the tree on disk must be the one the serial path produces, down to directory mtimes.
//
// Directory mtimes are the part that would break silently. Every file written into a
// directory restamps it, so a directory whose attributes were applied while its files were
// still in flight would come out with the time of the extraction rather than the time in
// the archive - and nothing else in the tree would look wrong.
func TestExtractPoolMatchesSerial(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	src := poolTree(t)
	r.mustRun("create", "-r", r.path, "tree", src)

	m := r.open(t)
	a, err := OpenByName(m, "tree")
	if err != nil {
		t.Fatal(err)
	}

	// Compare only the archived subtree. An extraction also recreates the parent
	// directories of the archived path, and those are not in the archive - makeParent
	// stamps them with the time of the extraction, so two runs a second apart differ on
	// them for a reason that has nothing to do with the pool. Comparing the whole
	// destination made this test pass or fail on how fast the machine was.
	rel := strings.TrimPrefix(filepath.ToSlash(src), "/")
	extract := func(t *testing.T, workers string) []string {
		t.Helper()
		t.Setenv("BORGE_EXTRACT_WORKERS", workers)
		dest := t.TempDir()
		if _, err := a.Extract(ExtractOptions{Dest: dest}); err != nil {
			t.Fatal(err)
		}
		return snapshot(t, filepath.Join(dest, filepath.FromSlash(rel)))
	}

	want := extract(t, "1")
	if len(want) < 200 {
		t.Fatalf("fixture is too small to exercise a pool: %d entries", len(want))
	}
	for _, workers := range []string{"2", "3", "8"} {
		t.Run("workers="+workers, func(t *testing.T) {
			got := extract(t, workers)
			if len(got) != len(want) {
				t.Fatalf("workers=%s produced %d entries, serial produced %d", workers, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("workers=%s differs from serial:\n got %s\nwant %s", workers, got[i], want[i])
				}
			}
		})
	}
}

// TestExtractPoolIsActuallyUsed guards against the whole suite passing because the pool
// never ran. A test that cannot tell the two paths apart would report success for a pool
// that silently fell back to serial, which is exactly how a concurrency change gets
// believed without being exercised.
func TestExtractPoolIsActuallyUsed(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", true}, // NumCPU >= 2 on anything this is developed on
		{"0", false},
		{"1", false},
		{"2", true},
		{"9", true},
	} {
		t.Run("BORGE_EXTRACT_WORKERS="+tc.env, func(t *testing.T) {
			if tc.env == "" {
				os.Unsetenv("BORGE_EXTRACT_WORKERS")
			} else {
				t.Setenv("BORGE_EXTRACT_WORKERS", tc.env)
			}
			if got := extractWorkers() > 1; got != tc.want {
				t.Errorf("extractWorkers()>1 = %v with %q, want %v", got, tc.env, tc.want)
			}
		})
	}
}
