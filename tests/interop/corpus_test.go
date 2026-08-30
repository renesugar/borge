// SPDX-License-Identifier: Apache-2.0

//go:build linux

package interop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The corpora of plans/PORTING_PLAN.md §10.
//
// The real ones are on this machine and are skipped elsewhere, with the skip saying so
// rather than passing quietly. The synthetic one is generated and is **not optional**:
// real data does not contain the cases that break a port, and a gate that only ever sees
// ordinary files proves less than it appears to.

// corpus is one body of data to run the matrix over.
type corpus struct {
	name string
	// path is the directory to back up. It is filled in by prepare.
	path string
	// prepare returns the path, or "" with a reason when the corpus is unavailable.
	prepare func(t *testing.T) (string, string)
	// limit, when non-zero, caps how many files are used, so the slow rows stay runnable.
	limit int
	// sparse says whether the comparator should compare physical data layout.
	sparse bool
}

// realCorpora are the paths named in the plan.
var realCorpora = []struct{ name, path string }{
	{"joplin-archive", "/home/renes/Documents/Joplin Archive/JoplinExport_2026_07_18"},
	{"joplin-recipes", "/home/renes/projects/recipedb/recipe_joplin"},
	{"obsidian-vault", "/home/renes/projects/recipedb/recipe_vault"},
	{"recipedb", "/home/renes/projects/recipedb"},
	{"pathological-dir", "/home/renes/projects/recipedb/recipe_vault/www-wedesoft-de/downloads/deutsche-rezepte"},
	{"googledrive", "/home/renes/GoogleDrive"},
}

// syntheticTree builds the edge cases. Each one is here because it is a way a port goes
// wrong, not because it is common.
func syntheticTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mkdir := func(p string) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	write := func(p string, content []byte, mode os.FileMode) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
		return full
	}

	// Zero-byte and one-byte files: the boundaries of the chunker's input.
	write("empty", nil, 0o644)
	write("one-byte", []byte{0}, 0o644)
	write("just-newline", []byte("\n"), 0o644)

	// Modes, including the ones whose rendering is easy to get wrong.
	write("setuid", []byte("s"), 0o4755)
	write("setgid", []byte("s"), 0o2755)
	write("sticky-file", []byte("t"), 0o1644)
	write("read-only", []byte("r"), 0o400)
	write("all-read", []byte("r"), 0o444)
	// A mode of 0o000 is deliberately absent. It is a fine mode to *restore*, but the
	// file cannot be read to back it up in the first place (except as root), so both
	// tools would skip it and the comparator would report a difference that is not a
	// defect. The odd-mode restore path is covered by setuid, setgid and sticky above.
	if err := os.Chmod(mkdir("sticky-dir"), 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mkdir("setgid-dir"), 0o2755); err != nil {
		t.Fatal(err)
	}

	// Names: unicode normalisation pairs, invalid UTF-8, control characters, and the
	// shapes a shell would mangle. A path is bytes on Linux, and a port that treats it as
	// text loses exactly these.
	write("unicode/precomposed-é", []byte("nfc"), 0o644)      // é as one code point
	write("unicode/decomposed-é", []byte("nfd"), 0o644)      // e + combining acute
	write("unicode/emoji-\U0001F5C4", []byte("emoji"), 0o644) // outside the BMP
	write("unicode/rtl-‮name", []byte("rtl"), 0o644)          // a bidi override
	write("odd names/with space", []byte("space"), 0o644)
	write("odd names/with\ttab", []byte("tab"), 0o644)
	write("odd names/with\nnewline", []byte("newline"), 0o644)
	write("odd names/-leading-dash", []byte("dash"), 0o644)
	write("odd names/trailing.dot.", []byte("dot"), 0o644)

	// An invalid UTF-8 name. Written with a raw syscall, because it is not a Go string
	// that any encoder would accept - which is precisely the point.
	invalid := append([]byte(filepath.Join(root, "invalid-utf8-")), 0xff, 0xfe)
	if err := unix.Mknod(string(invalid), unix.S_IFREG|0o644, 0); err != nil {
		t.Logf("could not create an invalid-UTF-8 name (%v); that case is not covered", err)
	}

	// Deep nesting: a path longer than PATH_MAX in total, reached one component at a time.
	deep := "deep"
	for i := 0; i < 40; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("level%02d", i))
	}
	write(filepath.Join(deep, "bottom.txt"), []byte("the bottom"), 0o644)

	// A directory with many entries, in miniature: the pathological corpus does this for
	// real, but this keeps the case covered when that corpus is absent.
	for i := 0; i < 500; i++ {
		write(fmt.Sprintf("many/f%04d", i), []byte(fmt.Sprintf("%d", i)), 0o644)
	}

	// Sparse files, including one whose hole is at the end.
	makeSparse := func(name string, holeAt, dataLen, total int64) {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(full)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(holeAt, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(make([]byte, dataLen)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(total); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	// Big enough that a whole *chunk* falls inside the hole. Sparseness survives a backup
	// only at chunk granularity - borg's --sparse restores an all-zero chunk as a hole,
	// and a chunk that also contains data is written out in full. With the default fastcdc
	// maximum of 8 MiB, a hole has to be several times that before any chunk is entirely
	// zeros. A smaller file round-trips correctly and simply comes back fully allocated.
	makeSparse("sparse/hole-then-data", 48<<20, 100, 96<<20)
	makeSparse("sparse/trailing-hole", 16<<20, 10, 96<<20)

	// Symlinks: relative, absolute, broken, to a directory, and one whose target has odd
	// bytes in it.
	links := mkdir("links")
	for _, l := range []struct{ name, target string }{
		{"relative", "../one-byte"},
		{"absolute", "/etc/hostname"},
		{"broken", "nowhere-at-all"},
		{"to-dir", "../many"},
		{"odd-target", "../odd names/with space"},
		{"self", "self"},
	} {
		if err := os.Symlink(l.target, filepath.Join(links, l.name)); err != nil {
			t.Fatal(err)
		}
	}

	// Hard links: a group of three, a group of two, and a hard-linked symlink.
	base := write("hardlinks/original", []byte("shared contents"), 0o644)
	for _, n := range []string{"hardlinks/link-a", "hardlinks/link-b"} {
		if err := os.Link(base, filepath.Join(root, n)); err != nil {
			t.Fatal(err)
		}
	}
	second := write("hardlinks/second", []byte("another"), 0o600)
	if err := os.Link(second, filepath.Join(root, "hardlinks/second-link")); err != nil {
		t.Fatal(err)
	}
	linkedSymlink := filepath.Join(root, "hardlinks/a-symlink")
	if err := os.Symlink("original", linkedSymlink); err != nil {
		t.Fatal(err)
	}
	if err := unix.Linkat(unix.AT_FDCWD, linkedSymlink, unix.AT_FDCWD,
		filepath.Join(root, "hardlinks/a-symlink-link"), 0); err != nil {
		t.Logf("could not hard-link a symlink (%v); that case is not covered", err)
	}

	// A FIFO. Device nodes need privilege and are left to a root-run of this gate.
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Logf("could not create a fifo (%v)", err)
	}

	// Extended attributes: several on one file, an empty value, a long value, and one on
	// a directory and a symlink.
	target := filepath.Join(root, "one-byte")
	if err := unix.Lsetxattr(target, "user.simple", []byte("value"), 0); err != nil {
		t.Logf("could not set an xattr (%v); that case is not covered", err)
	} else {
		_ = unix.Lsetxattr(target, "user.empty", []byte(""), 0)
		_ = unix.Lsetxattr(target, "user.long", []byte(strings.Repeat("v", 3000)), 0)
		_ = unix.Lsetxattr(target, "user.binary", []byte{0, 1, 2, 0xff}, 0)
		_ = unix.Lsetxattr(filepath.Join(root, "many"), "user.on-dir", []byte("d"), 0)
	}

	// POSIX ACLs, access and default.
	if _, err := exec.LookPath("setfacl"); err == nil {
		for _, args := range [][]string{
			{"-m", "u:root:rwx,g:root:r-x", filepath.Join(root, "setuid")},
			{"-m", "u:0:r--", filepath.Join(root, "one-byte")},
			{"-d", "-m", "u:root:rwx,g:root:r-x", filepath.Join(root, "many")},
		} {
			if out, err := exec.Command("setfacl", args...).CombinedOutput(); err != nil {
				t.Logf("setfacl %v failed (%v: %s)", args, err, out)
			}
		}
	} else {
		t.Log("setfacl not available; ACLs are not covered")
	}

	// Distinct sub-second timestamps, so a restore that rounds is caught.
	for i, name := range []string{"empty", "one-byte", "setuid", "sticky-file"} {
		ts := []unix.Timespec{
			unix.NsecToTimespec(int64(1600000000+i)*1e9 + 123456789),
			unix.NsecToTimespec(int64(1500000000+i)*1e9 + 987654321),
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, filepath.Join(root, name), ts, 0); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// corpora returns the corpora available on this machine.
//
// The synthetic one is always present. A missing real corpus is reported, not silently
// skipped: "the gate passed" has to mean something specific about what it ran on.
func corpora(t *testing.T, includeReal bool) []corpus {
	t.Helper()
	out := []corpus{{
		name:   "synthetic",
		path:   syntheticTree(t),
		sparse: false, // borg does not restore sparsely without --sparse; see TestSparseRestore
	}}
	if !includeReal {
		return out
	}
	for _, rc := range realCorpora {
		info, err := os.Stat(rc.path)
		if err != nil || !info.IsDir() {
			t.Logf("corpus %s not available at %s; skipping it", rc.name, rc.path)
			continue
		}
		out = append(out, corpus{name: rc.name, path: rc.path})
	}
	return out
}

// subsetOf copies at most n files from a corpus into a temporary tree, preserving relative
// layout, so the slow rows can run over real data without running for hours.
//
// It is a *subset*, and the reports say so. A subset that pretends to be the whole corpus
// would be the worst of both.
func subsetOf(t *testing.T, root string, n int) (string, int) {
	t.Helper()
	dest := t.TempDir()
	copied := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable corner of a real corpus is not this test's problem
		}
		if copied >= n {
			return filepath.SkipAll
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		target := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return nil
		}
		_ = os.Chtimes(target, info.ModTime(), info.ModTime())
		copied++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dest, copied
}

// countFiles counts the regular files under a directory, for the corpora that are
// archived in place rather than copied.
func countFiles(root string) int {
	n := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}
