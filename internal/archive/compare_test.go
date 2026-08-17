// SPDX-License-Identifier: Apache-2.0

//go:build linux

package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// The strict tree comparator the stage 5 gate calls for.
//
// It compares what a restore is actually supposed to reproduce, and it enumerates
// differences rather than reporting the first: a restore that gets the contents right and
// the ownership wrong is a specific, fixable defect, and a comparator that stops at the
// first mismatch hides how many there are.

// treeEntry is everything the comparator knows about one path.
type treeEntry struct {
	Path string

	Mode     uint32 // the full st_mode: type and permission bits
	UID, GID uint32
	Size     int64

	MTimeNsec int64
	// ATime is compared only when asked: reading a file updates it, so a comparison run
	// can change what it is comparing.
	ATimeNsec int64

	// Digest is the sha256 of a regular file's contents.
	Digest string
	// LinkTarget is a symlink's target.
	LinkTarget string
	// HardlinkGroup identifies files sharing an inode: entries with the same value are
	// the same inode. The value itself is arbitrary and differs between the two trees,
	// which is why the comparison is over the *grouping*, not the numbers.
	HardlinkGroup uint64
	NLink         uint64

	// RDev is a device node's major/minor.
	RDev uint64

	XAttrs     map[string][]byte
	ACLAccess  string
	ACLDefault string

	// Sparse records where a file's data actually lives, so a restore that fills a hole
	// with zeros is distinguishable from one that keeps it.
	DataRanges string
}

// scanTree walks a directory and describes every entry below it.
func scanTree(root string) (map[string]*treeEntry, error) {
	out := map[string]*treeEntry{}
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
		e, err := describe(path, rel)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	return out, err
}

func describe(path, rel string) (*treeEntry, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, fmt.Errorf("lstat %s: %w", path, err)
	}

	e := &treeEntry{
		Path:          filepath.ToSlash(rel),
		Mode:          st.Mode,
		UID:           st.Uid,
		GID:           st.Gid,
		Size:          st.Size,
		MTimeNsec:     st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		ATimeNsec:     st.Atim.Sec*1e9 + st.Atim.Nsec,
		HardlinkGroup: st.Ino,
		NLink:         st.Nlink,
		RDev:          st.Rdev,
	}

	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		digest, err := fileDigest(path)
		if err != nil {
			return nil, err
		}
		e.Digest = digest
		ranges, err := dataRanges(path, st.Size)
		if err != nil {
			return nil, err
		}
		e.DataRanges = ranges
	case unix.S_IFLNK:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		e.LinkTarget = target
	}

	attrs, err := GetXAttrs(path)
	if err != nil {
		return nil, err
	}
	// The kernel exposes ACLs as extended attributes too. They are compared separately, in
	// their decoded form, so an identical ACL written by a different library version does
	// not read as a difference.
	e.XAttrs = map[string][]byte{}
	for k, v := range attrs {
		if strings.HasPrefix(k, "system.posix_acl_") {
			continue
		}
		e.XAttrs[k] = v
	}
	if e.ACLAccess, err = GetACLText(path, xattrACLAccess, true); err != nil {
		return nil, err
	}
	if e.ACLDefault, err = GetACLText(path, xattrACLDefault, true); err != nil {
		return nil, err
	}
	return e, nil
}

func fileDigest(path string) (string, error) {
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

// dataRanges reports where a file's data is, using SEEK_DATA and SEEK_HOLE.
//
// This is how a sparse restore is checked: two files can have identical contents and
// digests while one occupies its full size on disk and the other is mostly holes.
func dataRanges(path string, size int64) (string, error) {
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
			// ENXIO means no more data: the rest is a hole. A filesystem without
			// SEEK_DATA reports the whole file as data, which is the safe answer.
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

// compareOptions say which properties have to match.
type compareOptions struct {
	// CheckOwner compares uid and gid. An unprivileged restore cannot set them, so a test
	// running as a normal user compares them only when the source was created by that same
	// user - which it is here.
	CheckOwner bool
	// CheckATime compares access times. Off by default: reading a file to compare it
	// updates its atime, so the comparison would change its own subject.
	CheckATime bool
	// CheckSparse compares where the data physically is.
	CheckSparse bool
	// CheckACLs compares POSIX ACLs.
	CheckACLs bool
}

// compareTrees returns a list of differences, empty when the trees match.
func compareTrees(want, got map[string]*treeEntry, opts compareOptions) []string {
	var diffs []string

	var wantPaths, gotPaths []string
	for p := range want {
		wantPaths = append(wantPaths, p)
	}
	for p := range got {
		gotPaths = append(gotPaths, p)
	}
	sort.Strings(wantPaths)
	sort.Strings(gotPaths)

	for _, p := range wantPaths {
		if _, ok := got[p]; !ok {
			diffs = append(diffs, "missing: "+p)
		}
	}
	for _, p := range gotPaths {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, "unexpected: "+p)
		}
	}

	for _, p := range wantPaths {
		w, g := want[p], got[p]
		if g == nil {
			continue
		}
		add := func(what string, a, b any) {
			diffs = append(diffs, fmt.Sprintf("%s: %s differs (borg %v, borge %v)", p, what, a, b))
		}
		if w.Mode != g.Mode {
			add("mode", fmt.Sprintf("0o%o", w.Mode), fmt.Sprintf("0o%o", g.Mode))
		}
		if opts.CheckOwner {
			if w.UID != g.UID {
				add("uid", w.UID, g.UID)
			}
			if w.GID != g.GID {
				add("gid", w.GID, g.GID)
			}
		}
		if w.Digest != g.Digest {
			add("content", w.Digest, g.Digest)
		}
		if w.LinkTarget != g.LinkTarget {
			add("symlink target", w.LinkTarget, g.LinkTarget)
		}
		if w.MTimeNsec != g.MTimeNsec {
			add("mtime", w.MTimeNsec, g.MTimeNsec)
		}
		if opts.CheckATime && w.ATimeNsec != g.ATimeNsec {
			add("atime", w.ATimeNsec, g.ATimeNsec)
		}
		if w.RDev != g.RDev {
			add("device", w.RDev, g.RDev)
		}
		if !xattrsEqual(w.XAttrs, g.XAttrs) {
			add("xattrs", formatXAttrs(w.XAttrs), formatXAttrs(g.XAttrs))
		}
		if opts.CheckACLs {
			if w.ACLAccess != g.ACLAccess {
				add("access ACL", w.ACLAccess, g.ACLAccess)
			}
			if w.ACLDefault != g.ACLDefault {
				add("default ACL", w.ACLDefault, g.ACLDefault)
			}
		}
		if opts.CheckSparse && w.DataRanges != g.DataRanges {
			add("data layout", w.DataRanges, g.DataRanges)
		}
	}

	// Hard link groups: compare the partitioning, not the inode numbers, which differ
	// between two separately extracted trees by construction.
	if g := groupSignature(want); g != groupSignature(got) {
		diffs = append(diffs, fmt.Sprintf("hard link groups differ\n  borg:  %s\n  borge: %s",
			groupSignature(want), groupSignature(got)))
	}
	return diffs
}

// groupSignature renders the hard link partitioning as a canonical string.
func groupSignature(tree map[string]*treeEntry) string {
	byInode := map[uint64][]string{}
	for p, e := range tree {
		if e.NLink > 1 {
			byInode[e.HardlinkGroup] = append(byInode[e.HardlinkGroup], p)
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

func xattrsEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || string(av) != string(bv) {
			return false
		}
	}
	return true
}

func formatXAttrs(m map[string][]byte) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}
