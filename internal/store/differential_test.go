// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The stage 2 gate: borge's store lists, reads and range-reads a repository created by
// borg, and borg reads a store directory borge wrote, with nesting and naming
// byte-identical.
//
// Layout is the part that has to match exactly. Where an object lands on disk is the
// format: an object borge writes to packs/<key> instead of packs/<xx>/<key> is one borg
// will not find, and the failure surfaces much later as a missing chunk.

// borgNSConfig is borg's namespace configuration (src/borg/repository.py:684-692),
// mirrored in the oracle.
func borgNSConfig() map[string]NamespaceConfig {
	return map[string]NamespaceConfig{
		"archives/": {Levels: []int{0}},
		"cache/":    {Levels: []int{0}},
		"config/":   {Levels: []int{0}},
		"index/":    {Levels: []int{0}},
		"keys/":     {Levels: []int{0}},
		"locks/":    {Levels: []int{0}},
		"packs/":    {Levels: []int{1}},
	}
}

type oracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borgstore differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the store differential test")
	}

	cmd := exec.Command(py, "testdata/oracle.py")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return &oracle{stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20), stderr: &stderr}
}

func (o *oracle) ask(req string) (string, error) {
	if _, err := io.WriteString(o.stdin, req+"\n"); err != nil {
		return "", fmt.Errorf("oracle write: %w (stderr: %s)", err, o.stderr)
	}
	line, err := o.stdout.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("oracle read: %w (stderr: %s)", err, o.stderr)
	}
	line = strings.TrimRight(line, "\n")
	if strings.HasPrefix(line, "ERR ") {
		return "", errors.New("borg said: " + strings.TrimPrefix(line, "ERR "))
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "OK"), " "), nil
}

func (o *oracle) mustAsk(t *testing.T, format string, args ...any) string {
	t.Helper()
	resp, err := o.ask(fmt.Sprintf(format, args...))
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
	return resp
}

func enhex(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func unhexOrDash(s string) []byte {
	if s == "-" || s == "" {
		return nil
	}
	b, _ := hex.DecodeString(s)
	return b
}

func splitList(s string) []string {
	if s == "-" || s == "" {
		return nil
	}
	out := strings.Split(s, ",")
	sort.Strings(out)
	return out
}

// borgeStore opens a borge Store over a path, creating it when asked.
func borgeStore(t *testing.T, path string, create bool) *Store {
	t.Helper()
	backend, err := NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(backend, borgNSConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if create {
		if err := s.Create(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// diskTree lists every regular file below path, as sorted slash-separated relative
// paths - the same thing the oracle's "T" command reports.
func diskTree(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// testObjects is a set of names spanning every namespace, with enough packs/ entries to
// exercise several distinct nesting directories.
func testObjects() []struct {
	Name  string
	Value []byte
} {
	var out []struct {
		Name  string
		Value []byte
	}
	add := func(name string, value []byte) {
		out = append(out, struct {
			Name  string
			Value []byte
		}{name, value})
	}

	add("config/readme", []byte("This is a Borg Backup repository.\nSee https://borgbackup.readthedocs.io/\n"))
	add("config/version", []byte("4"))
	add("config/id", []byte(strings.Repeat("ab", 32)))
	add("config/manifest", bytes.Repeat([]byte{0x42}, 436))

	// packs/ is the nested namespace: these keys start with different byte pairs, so
	// they must land in different subdirectories.
	for i := 0; i < 8; i++ {
		id := sha256.Sum256([]byte(fmt.Sprintf("pack-%d", i)))
		add("packs/"+hex.EncodeToString(id[:]), bytes.Repeat([]byte{byte(i)}, 1000+i))
	}
	for i := 0; i < 4; i++ {
		id := sha256.Sum256([]byte(fmt.Sprintf("archive-%d", i)))
		add("archives/"+hex.EncodeToString(id[:]), nil) // archive entries are empty files
	}
	for i := 0; i < 3; i++ {
		id := sha256.Sum256([]byte(fmt.Sprintf("index-%d", i)))
		add("index/"+hex.EncodeToString(id[:]), bytes.Repeat([]byte{0xEE}, 100))
	}
	add("keys/"+strings.Repeat("cd", 32), []byte("key blob"))
	add("cache/checked-packs", []byte("cache data"))
	return out
}

// TestLayoutMatchesBorg is the core of the gate: given the same writes, the two
// implementations must produce the same tree on disk.
func TestLayoutMatchesBorg(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()
	borgPath := filepath.Join(dir, "borg")
	borgePath := filepath.Join(dir, "borge")

	o.mustAsk(t, "C %s", borgPath)
	s := borgeStore(t, borgePath, true)

	for _, obj := range testObjects() {
		o.mustAsk(t, "S %s %s %s", borgPath, obj.Name, enhex(obj.Value))
		if err := s.Store(obj.Name, obj.Value); err != nil {
			t.Fatalf("borge store %s: %v", obj.Name, err)
		}
	}

	borgTree := splitList(o.mustAsk(t, "T %s", borgPath))
	borgeTree := diskTree(t, borgePath)

	if len(borgTree) != len(borgeTree) {
		t.Errorf("tree sizes differ: borg %d files, borge %d", len(borgTree), len(borgeTree))
	}
	for i := 0; i < len(borgTree) && i < len(borgeTree); i++ {
		if borgTree[i] != borgeTree[i] {
			t.Fatalf("first layout difference at entry %d:\n  borg:  %s\n  borge: %s",
				i, borgTree[i], borgeTree[i])
		}
	}
	t.Logf("%d objects laid out identically", len(borgTree))

	// Spot-check that the nesting actually happened, so an all-flat layout on both
	// sides could not pass.
	nested := 0
	for _, p := range borgeTree {
		if strings.HasPrefix(p, "packs/") && strings.Count(p, "/") == 2 {
			nested++
		}
	}
	if nested == 0 {
		t.Error("no packs/ object was nested; the nesting level is not being applied")
	}
}

// TestBorgWritesBorgeReads: borge must read what borg wrote, including range reads.
func TestBorgWritesBorgeReads(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")
	o.mustAsk(t, "C %s", path)

	objects := testObjects()
	for _, obj := range objects {
		o.mustAsk(t, "S %s %s %s", path, obj.Name, enhex(obj.Value))
	}

	s := borgeStore(t, path, false)
	for _, obj := range objects {
		got, err := s.Load(obj.Name, 0, -1, false)
		if err != nil {
			t.Fatalf("borge could not load %s written by borg: %v", obj.Name, err)
		}
		if !bytes.Equal(got, obj.Value) {
			t.Errorf("%s: content differs (%d bytes vs %d)", obj.Name, len(got), len(obj.Value))
		}
	}

	// Range reads are what the pack reader depends on: it walks 49-byte object headers
	// rather than downloading whole packs.
	big := objects[4] // a packs/ object, 1000+ bytes
	for _, r := range []struct{ offset, size int64 }{
		{0, 49}, {49, 49}, {100, 1}, {0, 1}, {500, 200}, {0, -1},
	} {
		got, err := s.Load(big.Name, r.offset, r.size, false)
		if err != nil {
			t.Fatalf("range read (%d, %d): %v", r.offset, r.size, err)
		}
		want := sliceRange(big.Value, r.offset, r.size)
		if !bytes.Equal(got, want) {
			t.Errorf("range read (%d, %d) gave %d bytes, want %d", r.offset, r.size, len(got), len(want))
		}
	}

	// A read past the end must come back short rather than failing: that is how the
	// pack reader detects a clean end of pack.
	got, err := s.Load(big.Name, int64(len(big.Value))-10, 49, false)
	if err != nil {
		t.Fatalf("short read at the end of an object: %v", err)
	}
	if len(got) != 10 {
		t.Errorf("read past the end returned %d bytes, want 10", len(got))
	}
}

// TestBorgeWritesBorgReads is the other direction.
func TestBorgeWritesBorgReads(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")

	s := borgeStore(t, path, true)
	objects := testObjects()
	for _, obj := range objects {
		if err := s.Store(obj.Name, obj.Value); err != nil {
			t.Fatal(err)
		}
	}

	for _, obj := range objects {
		resp := o.mustAsk(t, "L %s %s 0 -1", path, obj.Name)
		if got := unhexOrDash(resp); !bytes.Equal(got, obj.Value) {
			t.Errorf("borg read %s as %d bytes, borge wrote %d", obj.Name, len(got), len(obj.Value))
		}
	}

	// And borg's own find must agree about where each object lives.
	for _, obj := range objects {
		borgNested := o.mustAsk(t, "F %s %s", path, obj.Name)
		borgeNested, err := s.Find(obj.Name, false)
		if err != nil {
			t.Fatal(err)
		}
		if borgNested != borgeNested {
			t.Errorf("%s: nested name differs\n  borg:  %s\n  borge: %s", obj.Name, borgNested, borgeNested)
		}
	}
}

// TestListingMatchesBorg covers the namespace walk, including the recursion into
// nesting directories and the un-nesting of reported names.
func TestListingMatchesBorg(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")
	o.mustAsk(t, "C %s", path)
	for _, obj := range testObjects() {
		o.mustAsk(t, "S %s %s %s", path, obj.Name, enhex(obj.Value))
	}

	s := borgeStore(t, path, false)
	for _, ns := range []string{"archives", "packs", "index", "config", "keys", "cache"} {
		borgNames := splitList(o.mustAsk(t, "N %s %s 0", path, ns))
		borgeNames, err := s.ListNames(ns, false)
		if err != nil {
			t.Fatalf("borge list %s: %v", ns, err)
		}
		sort.Strings(borgeNames)

		if len(borgNames) != len(borgeNames) {
			t.Errorf("%s: borg lists %d names, borge %d\n  borg:  %v\n  borge: %v",
				ns, len(borgNames), len(borgeNames), borgNames, borgeNames)
			continue
		}
		for i := range borgNames {
			if borgNames[i] != borgeNames[i] {
				t.Errorf("%s: name %d differs\n  borg:  %s\n  borge: %s", ns, i, borgNames[i], borgeNames[i])
			}
		}
	}
}

// TestSoftDeleteMatchesBorg: soft deletion is a rename, and it is what borg's undelete
// depends on. A listing has to report the two halves separately.
func TestSoftDeleteMatchesBorg(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()
	borgPath := filepath.Join(dir, "borg")
	borgePath := filepath.Join(dir, "borge")

	o.mustAsk(t, "C %s", borgPath)
	s := borgeStore(t, borgePath, true)

	objects := testObjects()
	for _, obj := range objects {
		o.mustAsk(t, "S %s %s %s", borgPath, obj.Name, enhex(obj.Value))
		if err := s.Store(obj.Name, obj.Value); err != nil {
			t.Fatal(err)
		}
	}

	// Soft-delete two archives on both sides.
	var victims []string
	for _, obj := range objects {
		if strings.HasPrefix(obj.Name, "archives/") && len(victims) < 2 {
			victims = append(victims, obj.Name)
		}
	}
	for _, name := range victims {
		o.mustAsk(t, "D %s %s", borgPath, name)
		if err := s.SoftDelete(name); err != nil {
			t.Fatalf("borge soft delete %s: %v", name, err)
		}
	}

	// The on-disk trees must still match: the suffix and its placement are format.
	borgTree := splitList(o.mustAsk(t, "T %s", borgPath))
	borgeTree := diskTree(t, borgePath)
	for i := 0; i < len(borgTree) && i < len(borgeTree); i++ {
		if borgTree[i] != borgeTree[i] {
			t.Fatalf("layout differs after a soft delete at entry %d:\n  borg:  %s\n  borge: %s",
				i, borgTree[i], borgeTree[i])
		}
	}
	if len(borgTree) != len(borgeTree) {
		t.Fatalf("tree sizes differ after a soft delete: borg %d, borge %d", len(borgTree), len(borgeTree))
	}

	// Live and deleted listings must agree, and be disjoint.
	for _, deleted := range []bool{false, true} {
		flag := "0"
		if deleted {
			flag = "1"
		}
		borgNames := splitList(o.mustAsk(t, "N %s archives %s", borgPath, flag))
		borgeNames, err := s.ListNames("archives", deleted)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(borgeNames)
		if strings.Join(borgNames, ",") != strings.Join(borgeNames, ",") {
			t.Errorf("archives listing (deleted=%v) differs\n  borg:  %v\n  borge: %v",
				deleted, borgNames, borgeNames)
		}
		if deleted && len(borgeNames) != 2 {
			t.Errorf("expected 2 soft-deleted archives, got %d", len(borgeNames))
		}
	}

	// Undelete restores them.
	for _, name := range victims {
		o.mustAsk(t, "U %s %s", borgPath, name)
		if err := s.Undelete(name); err != nil {
			t.Fatalf("borge undelete %s: %v", name, err)
		}
	}
	borgTree = splitList(o.mustAsk(t, "T %s", borgPath))
	borgeTree = diskTree(t, borgePath)
	if strings.Join(borgTree, ",") != strings.Join(borgeTree, ",") {
		t.Error("layout differs after undelete")
	}
	deletedNames, err := s.ListNames("archives", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletedNames) != 0 {
		t.Errorf("%d archives are still soft-deleted after undelete", len(deletedNames))
	}
}

// TestBorgReadsAfterBorgeSoftDelete checks the interoperable case that actually
// matters: borge soft-deletes, and borg can still find and undelete the object.
func TestBorgReadsAfterBorgeSoftDelete(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")

	s := borgeStore(t, path, true)
	name := "archives/" + strings.Repeat("aa", 32)
	if err := s.Store(name, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDelete(name); err != nil {
		t.Fatal(err)
	}

	live := splitList(o.mustAsk(t, "N %s archives 0", path))
	if len(live) != 0 {
		t.Errorf("borg still lists %v as live after borge soft-deleted it", live)
	}
	// List reports the bare key, without the namespace - see Store.List.
	key := strings.TrimPrefix(name, "archives/")
	deleted := splitList(o.mustAsk(t, "N %s archives 1", path))
	if len(deleted) != 1 || deleted[0] != key {
		t.Errorf("borg's deleted listing = %v, want [%s]", deleted, key)
	}

	// borg can undelete it, and borge sees it live again.
	o.mustAsk(t, "U %s %s", path, name)
	names, err := s.ListNames("archives", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != key {
		t.Errorf("after borg undeleted it, borge lists %v, want [%s]", names, key)
	}
}
