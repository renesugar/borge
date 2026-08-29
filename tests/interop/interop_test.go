// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package interop is the stage 7 gate: the matrix in docs/PORTING_PLAN.md §10.
//
// # What this is for
//
// Every other test in this repository checks one layer against borg. This one checks the
// *product*: real corpora, every key mode, both tools writing into one repository and
// each reading what the other wrote. Rows 5 to 8 are the point - they exercise a shared
// chunk index, shared packs and a shared archive directory, which is where a format
// misunderstanding that the per-layer tests miss will actually bite.
//
// It drives both tools through their command lines, because that is what a user has.
package interop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------- harness

type tools struct {
	t          *testing.T
	borg       string
	borge      string
	repo       string
	keysDir    string
	configDir  string
	cacheDir   string
	passphrase string
}

// requireCurrentBinary fails when bin/borge is older than the source it is built from.
//
// This gate runs the binary rather than compiling it, so nothing ties the result to the
// tree. On 2026-08-28 a full suite reported "ok tests/interop (cached)" against a binary
// seven days old - the cache was right, its input was stale, and the compatibility gate
// therefore passed without having tested the change in front of it. Missing was already
// handled; being out of date is the case that reports success.
//
// It fails rather than skips. A skip here would be the same silence in a different
// costume: the gate that protects borg-2 format compatibility must not be quietly absent.
func requireCurrentBinary(t *testing.T, root, borge string, built time.Time) {
	t.Helper()
	var newest time.Time
	var newestPath string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not this check's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "vendor", "testdata", ".venv-borg2", "evidence":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && d.Name() != "go.mod" && d.Name() != "go.sum" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest, newestPath = info.ModTime(), path
		}
		return nil
	})
	if err != nil || newest.IsZero() {
		return // cannot tell; do not invent a failure
	}
	if newest.After(built) {
		rel, relErr := filepath.Rel(root, newestPath)
		if relErr != nil {
			rel = newestPath
		}
		t.Fatalf("bin/borge was built %s but %s changed %s: this gate runs the binary "+
			"rather than compiling it, so it would be testing code that is not in the "+
			"tree. Run 'make build' (or 'make test', which now depends on it).",
			built.Format(time.RFC3339), rel, newest.Format(time.RFC3339))
	}
}

func newTools(t *testing.T, encryption string) *tools {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the interop gate in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	borg := filepath.Join(root, ".venv-borg2", "bin", "borg")
	if _, err := os.Stat(borg); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the interop gate")
	}
	borge := filepath.Join(root, "bin", "borge")
	info, err := os.Stat(borge)
	if err != nil {
		t.Skipf("borge binary not built at %s; run 'make build'", borge)
	}
	requireCurrentBinary(t, root, borge, info.ModTime())

	base := t.TempDir()
	tl := &tools{
		t: t, borg: borg, borge: borge,
		repo:       filepath.Join(base, "repo"),
		keysDir:    filepath.Join(base, "keys"),
		configDir:  filepath.Join(base, "config"),
		cacheDir:   filepath.Join(base, "cache"),
		passphrase: "interop gate",
	}
	for _, d := range []string{tl.keysDir, tl.configDir, tl.cacheDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tl.mustBorg("repo-create", "-r", tl.repo, "-e", encryption)
	return tl
}

// env gives both tools the same key directory and the same weakened KDF, and keeps their
// caches apart - borge's cache is its own (docs/DIVERGENCES.md §4).
func (tl *tools) env() []string {
	return append(os.Environ(),
		"BORG_KEYS_DIR="+tl.keysDir,
		"BORG_CONFIG_DIR="+tl.configDir,
		"BORG_CACHE_DIR="+filepath.Join(tl.cacheDir, "borg"),
		"BORG_PASSPHRASE="+tl.passphrase,
		// transfer reads the source repository's passphrase from its own variable, which
		// falls back to nothing in either tool (docs/DIVERGENCES.md #55).
		"BORG_OTHER_PASSPHRASE="+tl.passphrase,
		"BORG_TESTONLY_WEAKEN_KDF=1",
		"BORG_KEY_FILE=",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",

		"BORGE_REPO="+tl.repo,
		"BORGE_KEYS_DIR="+tl.keysDir,
		"BORGE_CACHE_DIR="+filepath.Join(tl.cacheDir, "borge"),
		"BORGE_PASSPHRASE="+tl.passphrase,
		"BORGE_OTHER_PASSPHRASE="+tl.passphrase,
		"BORGE_TESTONLY_WEAKEN_KDF=1",
		"BORGE_KEY_FILE=",
	)
}

func (tl *tools) run(bin, dir string, args ...string) (string, error) {
	tl.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = tl.env()
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func (tl *tools) mustBorg(args ...string) string {
	tl.t.Helper()
	out, err := tl.run(tl.borg, "", args...)
	if err != nil {
		tl.t.Fatal(err)
	}
	return out
}

func (tl *tools) mustBorge(args ...string) string {
	tl.t.Helper()
	out, err := tl.run(tl.borge, "", args...)
	if err != nil {
		tl.t.Fatal(err)
	}
	return out
}

// extractWith restores an archive into a fresh directory and returns the path of the
// archived tree inside it.
func (tl *tools) extractWith(bin, archive, sourcePath string) string {
	tl.t.Helper()
	dest := tl.t.TempDir()
	var err error
	if bin == tl.borg {
		_, err = tl.run(bin, dest, "extract", "-r", tl.repo, archive)
	} else {
		_, err = tl.run(bin, "", "extract", "-C", dest, archive)
	}
	if err != nil {
		tl.t.Fatal(err)
	}
	return filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")))
}

// ---------------------------------------------------------------- comparator

// entry is everything the comparator knows about one path. The fields are the ones a
// restore is supposed to reproduce; anything not here is deliberately not part of the
// contract (see the notes on atime and ctime below).
type entry struct {
	Mode      uint32
	UID, GID  uint32
	Size      int64
	MTimeNsec int64
	Digest    string
	Link      string
	Inode     uint64
	NLink     uint64
	RDev      uint64
	XAttrs    map[string]string
	ACLAccess string
	ACLDflt   string
	DataMap   string
}

func scan(root string) (map[string]*entry, error) {
	out := map[string]*entry{}
	root = filepath.Clean(root)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		e, err := describe(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	return out, err
}

func describe(path string) (*entry, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, err
	}
	e := &entry{
		Mode: st.Mode, UID: st.Uid, GID: st.Gid, Size: st.Size,
		MTimeNsec: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		Inode:     st.Ino, NLink: st.Nlink, RDev: st.Rdev,
		XAttrs: map[string]string{},
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		d, err := digest(path)
		if err != nil {
			return nil, err
		}
		e.Digest = d
		e.DataMap, _ = dataMap(path, st.Size)
	case unix.S_IFLNK:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		e.Link = target
	}
	names, _ := listXattrs(path)
	for _, n := range names {
		if strings.HasPrefix(n, "system.posix_acl_") {
			continue // compared separately, decoded
		}
		v, err := getXattr(path, n)
		if err != nil {
			continue
		}
		e.XAttrs[n] = string(v)
	}
	e.ACLAccess = aclText(path, "system.posix_acl_access")
	e.ACLDflt = aclText(path, "system.posix_acl_default")
	return e, nil
}

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dataMap reports where a file's data physically is, so a restore that fills holes with
// zeros is distinguishable from one that keeps them.
func dataMap(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var parts []string
	var off int64
	for off < size {
		start, err := f.Seek(off, unix.SEEK_DATA)
		if err != nil {
			break
		}
		end, err := f.Seek(start, unix.SEEK_HOLE)
		if err != nil {
			end = size
		}
		parts = append(parts, fmt.Sprintf("%d-%d", start, end))
		off = end
	}
	return strings.Join(parts, ","), nil
}

func listXattrs(path string) ([]string, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil || size == 0 {
		return nil, err
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, part := range bytes.Split(buf[:size], []byte{0}) {
		if len(part) > 0 {
			out = append(out, string(part))
		}
	}
	return out, nil
}

func getXattr(path, name string) ([]byte, error) {
	size, err := unix.Lgetxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if size > 0 {
		if _, err := unix.Lgetxattr(path, name, buf); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// aclText renders an ACL attribute as sorted "tag:id:perm" lines, so two byte encodings
// of the same ACL compare equal.
func aclText(path, attr string) string {
	raw, err := getXattr(path, attr)
	if err != nil || len(raw) < 4 {
		return ""
	}
	var lines []string
	for off := 4; off+8 <= len(raw); off += 8 {
		tag := uint16(raw[off]) | uint16(raw[off+1])<<8
		perm := uint16(raw[off+2]) | uint16(raw[off+3])<<8
		id := uint32(raw[off+4]) | uint32(raw[off+5])<<8 | uint32(raw[off+6])<<16 | uint32(raw[off+7])<<24
		lines = append(lines, fmt.Sprintf("%d:%d:%d", tag, id, perm))
	}
	sort.Strings(lines)
	return strings.Join(lines, ",")
}

// compare returns every difference between two trees.
//
// Differences are enumerated rather than reported one at a time: "three files have the
// wrong mode" and "everything has the wrong mode" are different problems, and a
// first-failure comparator cannot tell them apart.
func compare(want, got map[string]*entry, checkSparse bool) []string {
	var diffs []string
	var paths []string
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		if _, ok := got[p]; !ok {
			diffs = append(diffs, "missing: "+p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, "unexpected: "+p)
		}
	}

	for _, p := range paths {
		w, g := want[p], got[p]
		if g == nil {
			continue
		}
		add := func(what string, a, b any) {
			diffs = append(diffs, fmt.Sprintf("%s: %s (%v vs %v)", p, what, a, b))
		}
		if w.Mode != g.Mode {
			add("mode", fmt.Sprintf("0o%o", w.Mode), fmt.Sprintf("0o%o", g.Mode))
		}
		if w.UID != g.UID {
			add("uid", w.UID, g.UID)
		}
		if w.GID != g.GID {
			add("gid", w.GID, g.GID)
		}
		if w.Digest != g.Digest {
			add("content", w.Digest, g.Digest)
		}
		if w.Link != g.Link {
			add("symlink target", w.Link, g.Link)
		}
		if w.MTimeNsec != g.MTimeNsec {
			add("mtime", w.MTimeNsec, g.MTimeNsec)
		}
		if w.RDev != g.RDev {
			add("device", w.RDev, g.RDev)
		}
		if !mapsEqual(w.XAttrs, g.XAttrs) {
			add("xattrs", w.XAttrs, g.XAttrs)
		}
		if w.ACLAccess != g.ACLAccess {
			add("access ACL", w.ACLAccess, g.ACLAccess)
		}
		if w.ACLDflt != g.ACLDflt {
			add("default ACL", w.ACLDflt, g.ACLDflt)
		}
		if checkSparse && w.DataMap != g.DataMap {
			add("data layout", w.DataMap, g.DataMap)
		}
	}

	if a, b := hardlinkGroups(want), hardlinkGroups(got); a != b {
		diffs = append(diffs, fmt.Sprintf("hard link groups: %q vs %q", a, b))
	}
	return diffs
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// hardlinkGroups renders the hard link partitioning. Inode numbers differ between two
// separately extracted trees by construction, so the *grouping* is what is compared.
func hardlinkGroups(tree map[string]*entry) string {
	byInode := map[uint64][]string{}
	for p, e := range tree {
		if e.NLink > 1 && e.Mode&unix.S_IFMT != unix.S_IFDIR {
			byInode[e.Inode] = append(byInode[e.Inode], p)
		}
	}
	var groups []string
	for _, paths := range byInode {
		sort.Strings(paths)
		groups = append(groups, strings.Join(paths, "+"))
	}
	sort.Strings(groups)
	return strings.Join(groups, " ")
}

// statSize is the size of a file, for the deduplication sanity check.
func statSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
