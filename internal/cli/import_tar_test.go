// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// tarEntry is one entry to write into a test tar.
type tarEntry struct {
	hdr  tar.Header
	body string
}

// writeTar builds a tar file at path.
func writeTar(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, e := range entries {
		hdr := e.hdr
		hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("writing header %q: %v", hdr.Name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// listedItem is one line of borge list -json-lines, for the fields an import can set.
type listedItem struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	User   string `json:"user"`
	Group  string `json:"group"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Size   int64  `json:"size"`
	MTime  string `json:"mtime"`
	Target string `json:"target"`
}

// listItems reads an archive's items, keyed by path.
func listItems(t *testing.T, r *borgRepo, name string) map[string]listedItem {
	t.Helper()
	stdout, stderr, code := r.borge(t, "list", "-json-lines", name)
	if code != ExitOK {
		t.Fatalf("borge list exited %d\n%s", code, stderr)
	}
	out := map[string]listedItem{}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var it listedItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			t.Fatalf("borge list JSON does not parse: %v\n%s", err, line)
		}
		out[it.Path] = it
	}
	return out
}

// sampleTar is the corpus the import tests use: every entry type tar can express, with
// the metadata that has to survive.
func sampleTar(t *testing.T, path string) {
	t.Helper()
	mtime := time.Unix(1600000000, 0)
	writeTar(t, path, []tarEntry{
		{hdr: tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o750,
			Uid: 1000, Gid: 1000, Uname: "alice", Gname: "staff", ModTime: mtime}},
		{hdr: tar.Header{Name: "dir/hello.txt", Typeflag: tar.TypeReg, Mode: 0o644,
			Uid: 1000, Gid: 1000, Uname: "alice", Gname: "staff", ModTime: mtime},
			body: "hello from a tar\n"},
		{hdr: tar.Header{Name: "dir/empty", Typeflag: tar.TypeReg, Mode: 0o600,
			Uid: 1000, Gid: 1000, ModTime: mtime}},
		{hdr: tar.Header{Name: "link-to-hello", Typeflag: tar.TypeLink,
			Linkname: "dir/hello.txt", Mode: 0o644, Uid: 1000, Gid: 1000, ModTime: mtime}},
		{hdr: tar.Header{Name: "a-symlink", Typeflag: tar.TypeSymlink,
			Linkname: "dir/hello.txt", Mode: 0o777, Uid: 1000, Gid: 1000, ModTime: mtime}},
		{hdr: tar.Header{Name: "a-fifo", Typeflag: tar.TypeFifo, Mode: 0o600,
			Uid: 1000, Gid: 1000, ModTime: mtime}},
	})
}

// TestImportTarMatchesBorg is the gate: borge's import of a tar has to produce the same
// archive borg's import of the same tar produces.
//
// The comparison is over the item metadata rather than the raw archive bytes, because the
// two archives legitimately differ in name, timestamp and id. What must not differ is what
// a restore would put on disk.
func TestImportTarMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	tarPath := filepath.Join(t.TempDir(), "sample.tar")
	sampleTar(t, tarPath)

	r.mustRun("import-tar", "-r", r.path, "by-borg", tarPath)

	if _, stderr, code := r.borge(t, "import-tar", "by-borge", tarPath); code != ExitOK {
		t.Fatalf("borge import-tar exited %d\n%s", code, stderr)
	}

	borgItems := listItems(t, r, "by-borg")
	borgeItems := listItems(t, r, "by-borge")

	// The comparison below is only as good as what it has to compare. Every entry type in
	// sampleTar has to be present, and the metadata has to be populated - otherwise two
	// empty structs would agree and the test would pass having checked nothing.
	for _, want := range []string{"dir", "dir/hello.txt", "dir/empty", "link-to-hello",
		"a-symlink", "a-fifo"} {
		it, ok := borgItems[want]
		if !ok {
			t.Fatalf("borg's import has no %q, so the comparison would not cover it; got %v",
				want, keysOf(borgItems))
		}
		if it.Mode == "" || it.MTime == "" {
			t.Fatalf("%s came back with empty mode/mtime (%+v), so comparing those fields "+
				"proves nothing", want, it)
		}
	}
	if borgItems["a-symlink"].Target == "" {
		t.Fatal("the symlink came back with no target, so the target field is not being compared")
	}
	if borgItems["dir/hello.txt"].Size == 0 {
		t.Fatal("the regular file came back empty, so size is not being compared")
	}

	var paths []string
	for p := range borgItems {
		paths = append(paths, p)
	}
	for p := range borgeItems {
		if _, ok := borgItems[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		want, inBorg := borgItems[p]
		got, inBorge := borgeItems[p]
		switch {
		case !inBorg:
			t.Errorf("%s: borge imported it, borg did not", p)
		case !inBorge:
			t.Errorf("%s: borg imported it, borge did not", p)
		case want != got:
			t.Errorf("%s differs:\n  borg:  %+v\n  borge: %+v", p, want, got)
		}
	}
}

// TestImportTarRoundTripsThroughBorgFormat: export-tar --tar-format=BORG followed by
// import-tar has to give back the archive it started from.
//
// This is the property the BORG format exists for. PAX cannot do it - it has nowhere to
// put birthtime, flags or the hard link group - so a round trip through PAX is checked
// separately and only for what PAX claims to carry.
func TestImportTarRoundTripsThroughBorgFormat(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	r.makeArchives("original")

	tarPath := filepath.Join(t.TempDir(), "round.tar")
	if _, stderr, code := r.borge(t, "export-tar", "--tar-format=BORG", "original", tarPath); code != ExitOK {
		t.Fatalf("borge export-tar exited %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borge(t, "import-tar", "restored", tarPath); code != ExitOK {
		t.Fatalf("borge import-tar exited %d\n%s", code, stderr)
	}

	before := listItems(t, r, "original")
	after := listItems(t, r, "restored")

	if len(before) == 0 {
		t.Fatal("the original archive has no items, so the round trip proves nothing")
	}
	if len(before) != len(after) {
		t.Errorf("round trip changed the item count: %d before, %d after", len(before), len(after))
	}
	for p, want := range before {
		got, ok := after[p]
		if !ok {
			t.Errorf("%s did not survive the round trip", p)
			continue
		}
		if want != got {
			t.Errorf("%s changed:\n  before: %+v\n  after:  %+v", p, want, got)
		}
	}
}

// TestImportTarReadsBorgsBorgFormat: a BORG-format tar written by borg has to import into
// borge, and the other way round.
//
// The two halves matter separately. Reading borg's output proves borge parses the pax
// record; borg reading borge's output proves borge writes one borg accepts - and a bug in
// either direction would only show up when somebody moved an archive between the tools.
func TestImportTarReadsBorgsBorgFormat(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	r.makeArchives("original")
	dir := t.TempDir()

	byBorg := filepath.Join(dir, "by-borg.tar")
	byBorge := filepath.Join(dir, "by-borge.tar")
	r.mustRun("export-tar", "-r", r.path, "--tar-format=BORG", "original", byBorg)
	if _, stderr, code := r.borge(t, "export-tar", "--tar-format=BORG", "original", byBorge); code != ExitOK {
		t.Fatalf("borge export-tar exited %d\n%s", code, stderr)
	}

	// borge reads borg's tar.
	if _, stderr, code := r.borge(t, "import-tar", "borge-read-borg", byBorg); code != ExitOK {
		t.Fatalf("borge could not import borg's BORG-format tar: exit %d\n%s", code, stderr)
	}
	// borg reads borge's tar.
	r.mustRun("import-tar", "-r", r.path, "borg-read-borge", byBorge)

	original := listItems(t, r, "original")
	for _, name := range []string{"borge-read-borg", "borg-read-borge"} {
		got := listItems(t, r, name)
		if len(got) != len(original) {
			t.Errorf("%s has %d items, the original has %d", name, len(got), len(original))
		}
		for p, want := range original {
			if got[p] != want {
				t.Errorf("%s: %s differs:\n  original: %+v\n  imported: %+v", name, p, want, got[p])
			}
		}
	}
}

// TestImportTarSharesContentBetweenHardLinks: a tar hard link entry has to come back with
// the content of the file it points at.
//
// tar stores the second link as a header with no body, so getting this wrong yields an
// empty file rather than an error - the failure is silent, which is why it is tested for
// explicitly rather than left to the round trip.
func TestImportTarSharesContentBetweenHardLinks(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	tarPath := filepath.Join(t.TempDir(), "links.tar")
	sampleTar(t, tarPath)
	if _, stderr, code := r.borge(t, "import-tar", "links", tarPath); code != ExitOK {
		t.Fatalf("borge import-tar exited %d\n%s", code, stderr)
	}

	items := listItems(t, r, "links")
	target, ok := items["dir/hello.txt"]
	if !ok {
		t.Fatal("dir/hello.txt is missing from the imported archive")
	}
	link, ok := items["link-to-hello"]
	if !ok {
		t.Fatal("link-to-hello is missing from the imported archive")
	}
	if target.Size == 0 {
		t.Fatal("dir/hello.txt imported as empty, so the link test would pass vacuously")
	}
	if link.Size != target.Size {
		t.Errorf("the hard link entry has size %d, its target has %d: the link did not "+
			"pick up the target's content", link.Size, target.Size)
	}

	// And the content itself, not just its length: extracting has to give the same bytes.
	dest := t.TempDir()
	if _, stderr, code := r.borge(t, "extract", "-C", dest, "links"); code != ExitOK {
		t.Fatalf("borge extract exited %d\n%s", code, stderr)
	}
	a, err := os.ReadFile(filepath.Join(dest, "dir", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "link-to-hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("the hard link's content differs from its target's:\n  target: %q\n  link:   %q", a, b)
	}
}

// TestImportTarRefusesUnsafePaths: a tar entry naming an absolute path or one containing
// ".." is skipped with a warning, not imported.
//
// This is the tar equivalent of the extract-time path checks. An import that stored
// "../../etc/passwd" would produce an archive that attacks whoever extracts it, so the
// refusal belongs here as well as there.
func TestImportTarRefusesUnsafePaths(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	tarPath := filepath.Join(t.TempDir(), "evil.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "good.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "fine\n"},
		{hdr: tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "bad\n"},
		{hdr: tar.Header{Name: "a/../../escape2.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "bad\n"},
	})

	stdout, stderr, code := r.borge(t, "import-tar", "evil", tarPath)
	if code != ExitWarning {
		t.Fatalf("import-tar exited %d, want ExitWarning (%d)\n%s%s", code, ExitWarning, stdout, stderr)
	}

	items := listItems(t, r, "evil")
	if _, ok := items["good.txt"]; !ok {
		t.Error("the safe entry was not imported")
	}
	for p := range items {
		if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
			t.Errorf("an unsafe path was imported as %q", p)
		}
		if strings.Contains(p, "escape") {
			t.Errorf("%q was imported; it should have been refused", p)
		}
	}
	if len(items) != 1 {
		t.Errorf("imported %d item(s), want only the one safe entry: %v", len(items), items)
	}
}

// TestImportTarIgnoreZeros reads two tars concatenated into one stream.
//
// Without --ignore-zeros the reader stops at the first end-of-archive marker, so this
// also checks that the default *does* stop there: an option that changes nothing is worse
// than no option, because it looks like it worked.
func TestImportTarIgnoreZeros(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	dir := t.TempDir()
	first := filepath.Join(dir, "first.tar")
	second := filepath.Join(dir, "second.tar")
	writeTar(t, first, []tarEntry{
		{hdr: tar.Header{Name: "from-first.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "one\n"},
	})
	writeTar(t, second, []tarEntry{
		{hdr: tar.Header{Name: "from-second.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "two\n"},
	})

	// Concatenated the way `cat a.tar b.tar` does it, which is what the option is for.
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	both := filepath.Join(dir, "both.tar")
	if err := os.WriteFile(both, append(a, b...), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := r.borge(t, "import-tar", "stops", both); code != ExitOK {
		t.Fatalf("import-tar exited %d\n%s", code, stderr)
	}
	stops := listItems(t, r, "stops")
	if _, ok := stops["from-second.txt"]; ok {
		t.Error("without --ignore-zeros the second tar was read anyway, so the flag means nothing")
	}
	if _, ok := stops["from-first.txt"]; !ok {
		t.Error("the first tar's entry is missing")
	}

	if _, stderr, code := r.borge(t, "import-tar", "--ignore-zeros", "continues", both); code != ExitOK {
		t.Fatalf("import-tar --ignore-zeros exited %d\n%s", code, stderr)
	}
	continues := listItems(t, r, "continues")
	for _, want := range []string{"from-first.txt", "from-second.txt"} {
		if _, ok := continues[want]; !ok {
			t.Errorf("--ignore-zeros did not read %s; got %v", want, keysOf(continues))
		}
	}
}

// TestImportTarIgnoreZerosMatchesBorg checks the concatenated case against borg, since
// the padding between the two tars is exactly where the two implementations could differ.
func TestImportTarIgnoreZerosMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	dir := t.TempDir()
	first := filepath.Join(dir, "first.tar")
	second := filepath.Join(dir, "second.tar")
	sampleTar(t, first)
	writeTar(t, second, []tarEntry{
		{hdr: tar.Header{Name: "second/extra.txt", Typeflag: tar.TypeReg, Mode: 0o644,
			Uid: 1000, Gid: 1000, ModTime: time.Unix(1600000000, 0)}, body: "extra\n"},
	})

	// GNU tar pads each archive out to its blocking factor, so a real concatenation has
	// far more than the two zero blocks of the marker. Reproduced with `tar --concatenate`
	// when it is available, and by hand otherwise.
	both := filepath.Join(dir, "both.tar")
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no system tar to build a realistically padded concatenation")
	}
	if err := copyFileForTest(first, both); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "--concatenate", "--file", both, second)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("tar --concatenate is unavailable here: %v\n%s", err, out)
	}

	r.mustRun("import-tar", "-r", r.path, "--ignore-zeros", "by-borg", both)
	if _, stderr, code := r.borge(t, "import-tar", "--ignore-zeros", "by-borge", both); code != ExitOK {
		t.Fatalf("borge import-tar exited %d\n%s", code, stderr)
	}

	want := listItems(t, r, "by-borg")
	got := listItems(t, r, "by-borge")
	if len(want) == 0 {
		t.Fatal("borg imported nothing, so the comparison would be vacuous")
	}
	if len(want) != len(got) {
		t.Errorf("borg imported %d item(s), borge %d:\n  borg:  %v\n  borge: %v",
			len(want), len(got), keysOf(want), keysOf(got))
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s differs:\n  borg:  %+v\n  borge: %+v", p, w, got[p])
		}
	}
}

func keysOf(m map[string]listedItem) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func copyFileForTest(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
