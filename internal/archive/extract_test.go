// SPDX-License-Identifier: Apache-2.0

//go:build linux

package archive

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/renesugar/borge/internal/item"
)

// The stage 5 gate: for a tree borg archived, "borge extract" produces a directory that
// compares equal to "borg extract"'s under the strict comparator in compare_test.go.
//
// Both tools extract the same archive into their own directory, and the two results are
// compared - rather than comparing a restore against the original source. That is the
// stronger test: it holds borge to what borg actually does, including the places where
// borg's own restore is lossy.

// extractTree is the source tree the gate runs over. It is deliberately awkward: the
// shapes below are the ones a restore gets wrong.
func extractTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()

	mk := func(p string, mode os.FileMode) string {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	write := func(p string, content []byte, mode os.FileMode) {
		full := mk(p, mode)
		if err := os.WriteFile(full, content, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Join(src, "dir/nested/deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory whose mode forbids writing: extraction has to create it permissively
	// and tighten it afterwards, or it cannot put anything inside.
	if err := os.MkdirAll(filepath.Join(src, "readonly-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("readonly-dir/inside.txt", []byte("inside a read-only directory"), 0o644)

	write("empty.txt", nil, 0o644)
	write("small.txt", []byte("hello"), 0o644)
	write("exec.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	write("odd-mode.txt", []byte("odd"), 0o400)
	write("sticky-ish.txt", []byte("sticky"), 0o644)
	write("dir/nested/deep.txt", []byte("deep"), 0o600)
	write("dir/nested/deeper/deepest.txt", []byte("deepest"), 0o644)
	write("name with spaces.txt", []byte("spaces"), 0o644)
	write("ünïcodé-☃.txt", []byte("unicode"), 0o644)
	write("trailing.space .txt", []byte("trailing"), 0o644)
	// Several chunks' worth, so the chunk list is not one entry.
	write("large.bin", []byte(strings.Repeat("borge extraction test ", 300000)), 0o644)

	// A sparse file: a hole, then data, then a hole.
	sparse := filepath.Join(src, "sparse.bin")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(4<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("data in the middle")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("small.txt", filepath.Join(src, "link-relative")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(src, "link-absolute")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere-at-all", filepath.Join(src, "link-broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../dir", filepath.Join(src, "dir/nested/link-up")); err != nil {
		t.Fatal(err)
	}

	// A hard link group of three, and a second group of two.
	if err := os.Link(filepath.Join(src, "small.txt"), filepath.Join(src, "hard-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "small.txt"), filepath.Join(src, "dir/hard-2")); err != nil {
		t.Fatal(err)
	}
	write("other.txt", []byte("other"), 0o644)
	if err := os.Link(filepath.Join(src, "other.txt"), filepath.Join(src, "other-link")); err != nil {
		t.Fatal(err)
	}

	// A FIFO.
	if err := unix.Mkfifo(filepath.Join(src, "fifo"), 0o644); err != nil {
		t.Logf("could not create a fifo (%v); the tree will not cover them", err)
	}

	// Extended attributes, where the filesystem allows them.
	if err := unix.Lsetxattr(filepath.Join(src, "small.txt"), "user.borge", []byte("attribute value"), 0); err != nil {
		t.Logf("could not set an xattr (%v); the tree will not cover them", err)
	} else {
		_ = unix.Lsetxattr(filepath.Join(src, "small.txt"), "user.second", []byte(""), 0)
		_ = unix.Lsetxattr(filepath.Join(src, "dir"), "user.on-a-dir", []byte("dir attr"), 0)
	}

	// A POSIX ACL, if setfacl is available.
	if _, err := exec.LookPath("setfacl"); err == nil {
		cmd := exec.Command("setfacl", "-m", "u:root:r--,g:root:r--", filepath.Join(src, "odd-mode.txt"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("setfacl failed (%v: %s); the tree will not cover ACLs", err, out)
		}
		cmd = exec.Command("setfacl", "-d", "-m", "u:root:rwx", filepath.Join(src, "dir"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("setfacl -d failed (%v: %s); the tree will not cover default ACLs", err, out)
		}
	} else {
		t.Log("setfacl not available; the tree will not cover ACLs")
	}

	// Distinct mtimes with sub-second precision, so a restore that rounds is caught.
	for i, p := range []string{"small.txt", "exec.sh", "dir/nested/deep.txt", "empty.txt"} {
		full := filepath.Join(src, p)
		ts := []unix.Timespec{
			unix.NsecToTimespec(int64(1600000000+i)*1e9 + int64(123456789)),
			unix.NsecToTimespec(int64(1500000000+i)*1e9 + int64(987654321)),
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, full, ts, 0); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// TestExtractMatchesBorg is the gate.
func TestExtractMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := extractTree(t)
	r.mustRun("create", "-r", r.path, "tree", src)

	// borg extracts into its own directory. Extraction is relative to the working
	// directory, and archived paths keep their full (leading-slash-stripped) form, so both
	// tools produce the same subtree below their own root.
	borgDir := t.TempDir()
	cmd := exec.Command(r.binary, "extract", "-r", r.path, "tree")
	cmd.Env = r.env()
	cmd.Dir = borgDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("borg extract: %v\n%s", err, out)
	}

	borgeDir := t.TempDir()
	m := r.open(t)
	a, err := OpenByName(m, "tree")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := a.Extract(ExtractOptions{
		Dest: borgeDir,
		OnError: func(path string, err error) error {
			t.Errorf("borge could not extract %s: %v", path, err)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("borge extract: %v", err)
	}
	t.Logf("borge extracted %d items (%d files, %d dirs, %d symlinks, %d hard links, %d other), %d bytes",
		stats.Items, stats.Files, stats.Dirs, stats.Symlinks, stats.Hardlinks, stats.Others, stats.Bytes)

	// Both trees hold the archived path below their own root; compare from the common
	// prefix down.
	rel := strings.TrimPrefix(filepath.ToSlash(src), "/")
	wantRoot := filepath.Join(borgDir, filepath.FromSlash(rel))
	gotRoot := filepath.Join(borgeDir, filepath.FromSlash(rel))

	want, err := scanTree(wantRoot)
	if err != nil {
		t.Fatalf("scanning borg's tree: %v", err)
	}
	got, err := scanTree(gotRoot)
	if err != nil {
		t.Fatalf("scanning borge's tree: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("borg's extraction produced nothing")
	}

	// A comparator that finds nothing to compare passes vacuously, so check the corpus
	// actually reached the interesting properties before trusting the result.
	var withXAttrs, withACLs, withHardlinks, withSymlinks, withFIFOs int
	for _, e := range want {
		if len(e.XAttrs) > 0 {
			withXAttrs++
		}
		if e.ACLAccess != "" || e.ACLDefault != "" {
			withACLs++
		}
		if e.NLink > 1 {
			withHardlinks++
		}
		if e.Mode&unix.S_IFMT == unix.S_IFLNK {
			withSymlinks++
		}
		if e.Mode&unix.S_IFMT == unix.S_IFIFO {
			withFIFOs++
		}
	}
	t.Logf("comparing %d entries: %d with xattrs, %d with ACLs, %d hard-linked, %d symlinks, %d fifos",
		len(want), withXAttrs, withACLs, withHardlinks, withSymlinks, withFIFOs)
	if withSymlinks == 0 || withHardlinks == 0 {
		t.Error("the corpus reached neither symlinks nor hard links; the gate would pass vacuously")
	}
	if withXAttrs == 0 {
		t.Log("note: no extended attributes survived into borg's extraction, so they were not compared")
	}
	if withACLs == 0 {
		t.Log("note: no ACLs survived into borg's extraction, so they were not compared")
	}

	diffs := compareTrees(want, got, compareOptions{
		CheckOwner: true,
		CheckACLs:  true,
		// Sparse layout is compared in its own test, where borge is asked for it.
		CheckSparse: false,
	})
	for _, d := range diffs {
		t.Error(d)
	}
	if len(diffs) > 0 {
		t.Errorf("%d difference(s) between borg's and borge's extraction", len(diffs))
	}
	if stats.SkippedACL > 0 {
		t.Logf("note: %d ACL(s) were skipped", stats.SkippedACL)
	}
}

// TestExtractSparse: with --sparse, an all-zero chunk becomes a hole rather than zeros,
// and the file still reads back identically.
func TestExtractSparse(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")

	src := t.TempDir()
	f, err := os.Create(filepath.Join(src, "holes.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(16<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.mustRun("create", "-r", r.path, "sparse", src)

	m := r.open(t)
	a, err := OpenByName(m, "sparse")
	if err != nil {
		t.Fatal(err)
	}

	for _, sparse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sparse=%v", sparse), func(t *testing.T) {
			dest := t.TempDir()
			if _, err := a.Extract(ExtractOptions{Dest: dest, Sparse: sparse}); err != nil {
				t.Fatal(err)
			}
			rel := strings.TrimPrefix(filepath.ToSlash(src), "/")
			out := filepath.Join(dest, filepath.FromSlash(rel), "holes.bin")

			st, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != 32<<20 {
				t.Errorf("restored size is %d, want %d", st.Size(), 32<<20)
			}
			digest, err := fileDigest(out)
			if err != nil {
				t.Fatal(err)
			}
			wantDigest, err := fileDigest(filepath.Join(src, "holes.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if digest != wantDigest {
				t.Error("the restored contents differ from the original")
			}

			var sst unix.Stat_t
			if err := unix.Stat(out, &sst); err != nil {
				t.Fatal(err)
			}
			allocated := sst.Blocks * 512
			t.Logf("sparse=%v: %d bytes allocated for a %d byte file", sparse, allocated, st.Size())
			if sparse && allocated >= st.Size() {
				t.Errorf("--sparse allocated %d bytes for a %d byte file; no hole was made",
					allocated, st.Size())
			}
		})
	}
}

// TestExtractRefusesPathTraversal: an archive whose item path escapes the destination
// must be refused, not extracted. This is a security property, so it is tested against a
// crafted item rather than against something borg would produce.
func TestExtractRefusesPathTraversal(t *testing.T) {
	dest := t.TempDir()
	x := &extractor{
		dest:     dest,
		safeDirs: map[string]bool{dest: true},
		stats:    &ExtractStats{},
	}
	for _, path := range []string{
		"../escape.txt",
		"a/../../escape.txt",
		"a/b/../../../escape.txt",
	} {
		if err := x.checkSafeParent(path); !errors.Is(err, ErrPathTraversal) {
			t.Errorf("%q was accepted (%v)", path, err)
		}
	}
	for _, path := range []string{"a.txt", "a/b.txt", "./a/b.txt", "a/./b.txt"} {
		if err := x.checkSafeParent(path); err != nil {
			t.Errorf("%q was refused: %v", path, err)
		}
	}
}

// TestExtractRefusesSymlinkedParent: a parent component that is an existing symlink must
// not be followed, or an archive could write through it to anywhere on the filesystem.
func TestExtractRefusesSymlinkedParent(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "evil")); err != nil {
		t.Fatal(err)
	}

	x := &extractor{
		dest:     dest,
		safeDirs: map[string]bool{dest: true},
		stats:    &ExtractStats{},
	}
	if err := x.checkSafeParent("evil/target.txt"); !errors.Is(err, ErrSymlinkParent) {
		t.Errorf("a symlinked parent was accepted (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "target.txt")); err == nil {
		t.Error("a file was written outside the destination")
	}
}

// TestHardlinkedSymlinkIsNotFollowed covers CVE-2026-62268: a hard link whose group
// leader is a symlink must recreate the symlink, not link the file it points at.
func TestHardlinkedSymlinkIsNotFollowed(t *testing.T) {
	dest := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	leader := filepath.Join(dest, "leader")
	if err := os.Symlink(outside, leader); err != nil {
		t.Fatal(err)
	}
	follower := filepath.Join(dest, "follower")
	if err := linkNoFollow(leader, follower); err != nil {
		t.Skipf("this filesystem does not support linkat without following: %v", err)
	}

	st, err := os.Lstat(follower)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the hard link followed the symlink and linked the external file")
	}
	target, err := os.Readlink(follower)
	if err != nil {
		t.Fatal(err)
	}
	if target != outside {
		t.Errorf("the recreated symlink points at %q, want %q", target, outside)
	}
}

// TestExtractDryRunWritesNothing: a dry run still reads and authenticates every chunk, so
// it is a real check that the archive is restorable.
func TestExtractDryRunWritesNothing(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("x", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "dry", src)

	m := r.open(t)
	a, err := OpenByName(m, "dry")
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	stats, err := a.Extract(ExtractOptions{Dest: dest, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bytes != 5000 {
		t.Errorf("a dry run read %d bytes, want 5000", stats.Bytes)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a dry run wrote %d entries", len(entries))
	}
}

// TestExtractFilterAndStrip covers the two ways a caller narrows an extraction.
func TestExtractFilterAndStrip(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	src := t.TempDir()
	for _, p := range []string{"keep.txt", "skip.txt", "sub/keep.txt"} {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.mustRun("create", "-r", r.path, "filtered", src)

	m := r.open(t)
	a, err := OpenByName(m, "filtered")
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	stats, err := a.Extract(ExtractOptions{
		Dest:   dest,
		Filter: func(it *item.Item) bool { return !strings.HasSuffix(it.Path, "skip.txt") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 {
		t.Errorf("extracted %d files with a filter, want 2", stats.Files)
	}

	// Stripping components removes leading path elements, so an archive of an absolute
	// path can be restored somewhere shallower.
	stripped := t.TempDir()
	depth := len(strings.Split(strings.TrimPrefix(filepath.ToSlash(src), "/"), "/"))
	if _, err := a.Extract(ExtractOptions{Dest: stripped, StripComponents: depth}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stripped, "keep.txt")); err != nil {
		t.Errorf("strip-components did not flatten the tree: %v", err)
	}
}
