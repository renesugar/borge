// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// The stage 5b gate: for an archive borg created, borge walks the item stream and gets
// the same items borg's own listing reports - path, mode, owner, times, sizes, symlink
// targets and hard link groups.
//
// The stream is the part most likely to go subtly wrong: items are msgpack maps
// concatenated with no framing across chunk boundaries, behind two levels of chunk id
// indirection. A bug there does not fail loudly, it drops files.

type borgRepo struct {
	t          *testing.T
	binary     string
	path       string
	keysDir    string
	configDir  string
	passphrase string
}

func newBorgRepo(t *testing.T, encryption string) *borgRepo {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg archive gate in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, ".venv-borg2", "bin", "borg")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the archive gate")
	}

	base := t.TempDir()
	r := &borgRepo{
		t:          t,
		binary:     binary,
		path:       filepath.Join(base, "repo"),
		keysDir:    filepath.Join(base, "keys"),
		configDir:  filepath.Join(base, "config"),
		passphrase: "archive gate",
	}
	for _, d := range []string{r.keysDir, r.configDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BORGE_KEYS_DIR", r.keysDir)
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	t.Setenv("BORGE_KEY_FILE", "")
	t.Setenv("BORG_KEY_FILE", "")

	r.mustRun("repo-create", "-r", r.path, "-e", encryption)
	return r
}

func (r *borgRepo) env() []string {
	return append(os.Environ(),
		"BORG_KEYS_DIR="+r.keysDir,
		"BORG_CONFIG_DIR="+r.configDir,
		"BORG_CACHE_DIR="+filepath.Join(r.configDir, "cache"),
		"BORG_TESTONLY_WEAKEN_KDF=1",
		"BORG_PASSPHRASE="+r.passphrase,
		"BORG_KEY_FILE=",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
	)
}

func (r *borgRepo) run(args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("borg %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func (r *borgRepo) mustRun(args ...string) string {
	r.t.Helper()
	out, err := r.run(args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *borgRepo) open(t *testing.T) *manifest.Manifest {
	t.Helper()
	repo, err := repository.Open(r.path, repository.Options{})
	if err != nil {
		t.Fatalf("borge could not open borg's repository: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	k, _, err := repo.Unlock(r.passphrase)
	if err != nil {
		t.Fatalf("borge could not unlock: %v", err)
	}
	m, err := manifest.Load(repo, k, manifest.OpRead)
	if err != nil {
		t.Fatalf("borge could not load the manifest: %v", err)
	}
	return m
}

// borgItem is one line of "borg list --json-lines".
type borgItem struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	MTime  string `json:"mtime"`
	Size   int64  `json:"size"`
	UID    int64  `json:"uid"`
	GID    int64  `json:"gid"`
	User   string `json:"user"`
	Group  string `json:"group"`
	Target string `json:"target"`
	Type   string `json:"type"`
	HLID   string `json:"hlid"`
}

func (r *borgRepo) list(archive string) []borgItem {
	r.t.Helper()
	out := r.mustRun("list", "-r", r.path, archive, "--json-lines")
	var items []borgItem
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var it borgItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			r.t.Fatalf("could not parse borg's list JSON: %v\n%s", err, line)
		}
		items = append(items, it)
	}
	if err := sc.Err(); err != nil {
		r.t.Fatal(err)
	}
	return items
}

// buildTree writes a source tree covering the item shapes the stream has to carry:
// directories, regular files of several sizes, an empty file, a symlink, a hard link
// pair, a FIFO, and names needing escaping.
func buildTree(t *testing.T, fileCount int) string {
	t.Helper()
	src := t.TempDir()

	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(src, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, content []byte, mode os.FileMode) {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, mode); err != nil {
			t.Fatal(err)
		}
	}

	mkdir("dir")
	mkdir("dir/nested")
	mkdir("empty-dir")

	write("empty.txt", nil, 0o644)
	write("small.txt", []byte("hello"), 0o644)
	write("exec.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	// Big enough to be split into several content chunks, so the chunk list is not
	// trivially one entry.
	write("large.bin", []byte(strings.Repeat("borge stream test ", 400000)), 0o644)
	write("dir/nested/deep.txt", []byte("deep"), 0o600)
	write("name with spaces.txt", []byte("spaces"), 0o644)
	write("ünïcodé.txt", []byte("unicode"), 0o644)

	// Enough files to push the item stream past one chunk.
	for i := 0; i < fileCount; i++ {
		write(fmt.Sprintf("many/file-%05d.txt", i), []byte(fmt.Sprintf("file %d contents", i)), 0o644)
	}

	if err := os.Symlink("small.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "small.txt"), filepath.Join(src, "hardlink")); err != nil {
		t.Fatal(err)
	}
	return src
}

// TestBorgeWalksBorgItemStream is the gate.
func TestBorgeWalksBorgItemStream(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := buildTree(t, 2000)
	r.mustRun("create", "-r", r.path, "tree", src)

	want := r.list("tree")
	if len(want) < 2000 {
		t.Fatalf("borg listed only %d items", len(want))
	}

	m := r.open(t)
	a, err := OpenByName(m, "tree")
	if err != nil {
		t.Fatal(err)
	}

	var got []*item.Item
	if err := a.Items(func(it *item.Item) error {
		got = append(got, it)
		return nil
	}); err != nil {
		t.Fatalf("borge could not walk the item stream: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("borge walked %d items, borg listed %d", len(got), len(want))
	}

	// The stream is ordered, so item i must be item i - not merely present.
	for i := range want {
		w, g := want[i], got[i]
		if g.Path != w.Path {
			t.Fatalf("item %d: borge says path %q, borg %q", i, g.Path, w.Path)
		}
		if mode := item.FormatMode(g.ModeOr(0)); mode != w.Mode {
			t.Errorf("%s: borge says mode %s, borg %s", w.Path, mode, w.Mode)
		}
		if item.TypeChar(g.ModeOr(0)) != w.Type {
			t.Errorf("%s: borge says type %s, borg %s", w.Path, item.TypeChar(g.ModeOr(0)), w.Type)
		}
		if g.UID == nil || *g.UID != w.UID {
			t.Errorf("%s: borge says uid %v, borg %d", w.Path, g.UID, w.UID)
		}
		if g.GID == nil || *g.GID != w.GID {
			t.Errorf("%s: borge says gid %v, borg %d", w.Path, g.GID, w.GID)
		}
		if g.User == nil || *g.User != w.User {
			t.Errorf("%s: borge says user %v, borg %q", w.Path, g.User, w.User)
		}
		if g.Group == nil || *g.Group != w.Group {
			t.Errorf("%s: borge says group %v, borg %q", w.Path, g.Group, w.Group)
		}
		if g.MTime == nil {
			t.Errorf("%s: borge has no mtime", w.Path)
		} else {
			wantTime, err := time.Parse("2006-01-02T15:04:05.999999-07:00", w.MTime)
			if err != nil {
				t.Fatalf("could not parse borg's mtime %q: %v", w.MTime, err)
			}
			// borg's JSON truncates to microseconds; compare at that resolution.
			gotTime := time.Unix(0, *g.MTime).UTC().Truncate(time.Microsecond)
			if !gotTime.Equal(wantTime.UTC().Truncate(time.Microsecond)) {
				t.Errorf("%s: borge says mtime %s, borg %s", w.Path, gotTime, wantTime.UTC())
			}
		}

		// Sizes: borg reports the content size for a file, the target length for a
		// symlink and zero for everything else.
		var gotSize int64
		switch {
		case g.IsSymlink():
			if g.Target != nil {
				gotSize = int64(len(*g.Target))
			}
		case g.IsRegular():
			gotSize = g.ContentSize()
		}
		if gotSize != w.Size {
			t.Errorf("%s: borge says size %d, borg %d", w.Path, gotSize, w.Size)
		}

		gotTarget := ""
		if g.Target != nil {
			gotTarget = *g.Target
		}
		if gotTarget != w.Target {
			t.Errorf("%s: borge says target %q, borg %q", w.Path, gotTarget, w.Target)
		}
		if hlid := hex.EncodeToString(g.HLID); hlid != w.HLID {
			t.Errorf("%s: borge says hlid %q, borg %q", w.Path, hlid, w.HLID)
		}
	}
}

// TestItemStreamSpansChunks: with enough items the stream is several chunks long, and an
// item straddles a boundary. That is the case a naive "decode each chunk" reader gets
// wrong, so assert the stream really is multi-chunk rather than hoping.
func TestItemStreamSpansChunks(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	src := buildTree(t, 20000)
	r.mustRun("create", "-r", r.path, "big", src)

	m := r.open(t)
	a, err := OpenByName(m, "big")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := a.ItemStreamIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) < 2 {
		t.Fatalf("the item stream is %d chunk(s); this test needs at least 2", len(ids))
	}
	t.Logf("item stream is %d chunks behind %d pointer block(s)", len(ids), len(a.Meta.ItemPtrs))

	var count int
	seen := map[string]bool{}
	if err := a.Items(func(it *item.Item) error {
		if seen[it.Path] {
			return fmt.Errorf("path %q appeared twice", it.Path)
		}
		seen[it.Path] = true
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	want := len(r.list("big"))
	if count != want {
		t.Errorf("borge walked %d items across %d chunks, borg listed %d", count, len(ids), want)
	}
}

// TestStopIteration: a caller that only wants the first few items should not have to
// read the whole stream.
func TestStopIteration(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	src := buildTree(t, 100)
	r.mustRun("create", "-r", r.path, "tree", src)

	m := r.open(t)
	a, err := OpenByName(m, "tree")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.Items(func(it *item.Item) error {
		count++
		if count == 5 {
			return ErrStopIteration
		}
		return nil
	}); err != nil {
		t.Fatalf("stopping early reported an error: %v", err)
	}
	if count != 5 {
		t.Errorf("walked %d items after asking to stop at 5", count)
	}
}

// TestArchiveMetadataMatchesBorg checks the archive object itself.
func TestArchiveMetadataMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	src := buildTree(t, 10)
	r.mustRun("create", "-r", r.path, "--comment", "a comment", "meta", src)

	m := r.open(t)
	a, err := OpenByName(m, "meta")
	if err != nil {
		t.Fatal(err)
	}

	if a.Meta.Version != 2 {
		t.Errorf("archive version is %d, want 2", a.Meta.Version)
	}
	if !a.Meta.ItemPtrsSet || len(a.Meta.ItemPtrs) == 0 {
		t.Error("the archive has no item_ptrs")
	}
	if a.Meta.ItemsSet {
		t.Error("a borg 2 archive must not carry the legacy items key")
	}
	if a.Info.Comment != "a comment" {
		t.Errorf("comment is %q, want %q", a.Info.Comment, "a comment")
	}
	if a.Meta.CommandLine == nil || !strings.Contains(*a.Meta.CommandLine, "create") {
		t.Errorf("command_line is %v", a.Meta.CommandLine)
	}
	// nfiles and size are borg's own accounting; check they are present and sane rather
	// than recomputing them, which is stage 6's job.
	if a.Meta.NFiles == nil || *a.Meta.NFiles <= 0 {
		t.Errorf("nfiles is %v", a.Meta.NFiles)
	}
	if a.Meta.ChunkerParamsSet && len(a.Meta.ChunkerParams) == 0 {
		t.Error("chunker_params is set but empty")
	}
}
