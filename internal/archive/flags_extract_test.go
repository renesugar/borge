// SPDX-License-Identifier: Apache-2.0

//go:build linux

package archive

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/renesugar/borge/internal/item"
)

// TestFlagsSyscallRoundTrip: setFlags writes what GetFlags reads back.
//
// nodump is the only flag an unprivileged process can set - immutable and append-only need
// CAP_LINUX_IMMUTABLE - so it is the only one these tests can exercise. The mapping for
// the other two is covered by TestExcludedByAttr's use of the constants and by the table
// in flags_linux.go, not by a syscall.
func TestFlagsSyscallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}

	if got := GetFlags(path, st.Mode); got != 0 {
		t.Fatalf("a fresh file already has flags %#x; the test cannot tell a change apart", got)
	}
	if err := setFlags(path, bsdNoDump, st.Mode); err != nil {
		t.Fatalf("setFlags: %v", err)
	}
	if got := GetFlags(path, st.Mode); got != bsdNoDump {
		if got == 0 {
			t.Skipf("the filesystem under %s does not keep file flags", dir)
		}
		t.Fatalf("GetFlags = %#x after setting nodump, want %#x", got, bsdNoDump)
	}

	// And clearing works, which is the case that would break a restore onto an existing
	// tree: the flags of the archived item, not the flags already on disk, are what the
	// restored file must end up with.
	if err := setFlags(path, 0, st.Mode); err != nil {
		t.Fatalf("setFlags(0): %v", err)
	}
	if got := GetFlags(path, st.Mode); got != 0 {
		t.Errorf("GetFlags = %#x after clearing, want 0", got)
	}
}

// TestFlagsSurviveExtraction: a flag stored in an archive is applied on restore.
//
// The item is put into the archive directly rather than by walking a source tree, because
// no unprivileged "create" can produce such an archive: the only settable flag is nodump,
// and a nodump file is excluded from the backup entirely (DIVERGENCES.md #39). An archive
// carrying flags comes from a privileged backup or another machine, and this builds that
// situation rather than skipping the code path that handles it.
func TestFlagsSurviveExtraction(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	repo, b := createBuilder(t, r)
	mode := int64(unix.S_IFREG | 0o644)
	flags := bsdNoDump
	it := &item.Item{
		Path:      "flagged.txt",
		Mode:      &mode,
		BSDFlags:  &flags,
		Chunks:    nil,
		ChunksSet: true,
	}
	mtime := int64(1_700_000_000_000_000_000)
	it.MTime = &mtime
	if err := b.AddItem(it); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Save(SaveOptions{Name: "flagged"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	m := r.open(t)
	a, err := OpenByName(m, "flagged")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Extract(ExtractOptions{Dest: dest}); err != nil {
		t.Fatalf("extract: %v", err)
	}

	restored := filepath.Join(dest, "flagged.txt")
	var st unix.Stat_t
	if err := unix.Lstat(restored, &st); err != nil {
		t.Fatalf("the item was not extracted: %v", err)
	}
	got := GetFlags(restored, st.Mode)
	if got == 0 {
		// Distinguish "the restore did not apply it" from "this filesystem has no flags",
		// which would otherwise look identical and make the test vacuous.
		probe := filepath.Join(dest, "probe")
		if err := os.WriteFile(probe, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		var pst unix.Stat_t
		if err := unix.Lstat(probe, &pst); err != nil {
			t.Fatal(err)
		}
		if err := setFlags(probe, bsdNoDump, pst.Mode); err != nil || GetFlags(probe, pst.Mode) == 0 {
			t.Skipf("the filesystem under %s does not keep file flags", dest)
		}
		t.Fatal("the restored file has no flags; the archived nodump flag was not applied")
	}
	if got != bsdNoDump {
		t.Errorf("restored flags = %#x, want %#x", got, bsdNoDump)
	}
}
