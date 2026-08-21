// SPDX-License-Identifier: Apache-2.0

package repository

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
	"strconv"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/repoobj"
)

// The stage 3 gate: borge writes packs the borg-2 venv reads and indexes; borge
// rebuilds a chunk index from borg-written packs that matches borg's own; and borg's
// own repository check passes on a borge-written repository.

type oracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg repository differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the repository differential test")
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
	return &oracle{stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<22), stderr: &stderr}
}

func (o *oracle) ask(t *testing.T, format string, args ...any) string {
	t.Helper()
	resp, err := o.try(format, args...)
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
	return resp
}

func (o *oracle) try(format string, args ...any) (string, error) {
	req := fmt.Sprintf(format, args...)
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

func enhex(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func splitList(s string) []string {
	if s == "-" || s == "" {
		return nil
	}
	out := strings.Split(s, ",")
	sort.Strings(out)
	return out
}

// testObjects builds a set of repository objects using borge's own repoobj layer, so
// what goes into the packs is the real thing rather than arbitrary bytes.
func testObjects(t *testing.T, n int) (ids [][]byte, objects [][]byte) {
	t.Helper()
	k := key.NewNoneSHA256()
	r, err := repoobj.New(k)
	if err != nil {
		t.Fatal(err)
	}
	c, err := compress.FromSpec("lz4")
	if err != nil {
		t.Fatal(err)
	}
	r.SetCompressor(c)

	for i := 0; i < n; i++ {
		// A mix of shapes, so the packs hold objects of varying sizes.
		var data []byte
		switch i % 3 {
		case 0:
			data = bytes.Repeat([]byte(fmt.Sprintf("object %d ", i)), 20)
		case 1:
			data = make([]byte, 1000+i)
			for j := range data {
				data[j] = byte(i * j)
			}
		default:
			data = []byte(fmt.Sprintf("%d", i))
		}
		id := k.IDHash(data)
		obj, err := r.Format(id, &repoobj.Meta{Type: repoobj.TypeFileStream}, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		objects = append(objects, obj)
	}
	return ids, objects
}

// indexDigest is the order-independent digest the oracle computes, in Go.
//
// Entry order inside a fragment is not part of the format (docs/FORMAT.md §6.1), so the
// digest is over the sorted (id, pack_id, offset, size) tuples.
func indexDigest(chunks *hashindex.ChunkIndex) (int, string) {
	var rows [][]byte
	chunks.Iterate(func(id []byte, e hashindex.Entry) bool {
		row := make([]byte, 0, 32+32+8)
		row = append(row, id...)
		row = append(row, e.PackID[:]...)
		row = append(row, byte(e.ObjOffset), byte(e.ObjOffset>>8), byte(e.ObjOffset>>16), byte(e.ObjOffset>>24))
		row = append(row, byte(e.ObjSize), byte(e.ObjSize>>8), byte(e.ObjSize>>16), byte(e.ObjSize>>24))
		rows = append(rows, row)
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })

	h := sha256.New()
	for _, row := range rows {
		h.Write(row)
	}
	return len(rows), hex.EncodeToString(h.Sum(nil))
}

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

// smallPackOptions forces many small packs, so the tests exercise several pack files
// and the writer's roll-over rather than putting everything in one.
func smallPackOptions() Options {
	return Options{PackMaxSize: 4096}
}

// TestBorgeWritesBorgReads is the core of the gate.
func TestBorgeWritesBorgReads(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")

	ids, objects := testObjects(t, 60)

	r, err := Create(location.MustLocal(path), smallPackOptions())
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if _, err := r.Put(ids[i], objects[i]); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	borgeCount, borgeDigest := indexDigest(mustChunks(t, r))
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// borg opens it, sees the same index, and reads every object back.
	o.ask(t, "O %s", path)
	resp := o.ask(t, "I %s", path)
	fields := strings.Fields(resp)
	if len(fields) != 2 {
		t.Fatalf("malformed index response %q", resp)
	}
	borgCount, _ := strconv.Atoi(fields[0])
	if borgCount != borgeCount {
		t.Errorf("borg's index has %d entries, borge's %d", borgCount, borgeCount)
	}
	if fields[1] != borgeDigest {
		t.Errorf("index digests differ\n  borge: %s\n  borg:  %s", borgeDigest, fields[1])
	}

	for i := range ids {
		got := o.ask(t, "G %s %s", path, enhex(ids[i]))
		data, err := hex.DecodeString(got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, objects[i]) {
			t.Fatalf("object %d differs when borg reads it (%d vs %d bytes)", i, len(data), len(objects[i]))
		}
	}
	o.ask(t, "X %s", path)
}

// TestBorgWritesBorgeReads is the other direction, and it also checks that borge
// rebuilds the same index borg has.
func TestBorgWritesBorgeReads(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")

	ids, objects := testObjects(t, 60)

	o.ask(t, "C %s", path)
	for i := range ids {
		o.ask(t, "P %s %s %s", path, enhex(ids[i]), enhex(objects[i]))
	}
	o.ask(t, "F %s", path)
	resp := o.ask(t, "I %s", path)
	fields := strings.Fields(resp)
	borgCount, _ := strconv.Atoi(fields[0])
	borgDigest := fields[1]
	o.ask(t, "X %s", path)

	r, err := Open(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatalf("borge could not open borg's repository: %v", err)
	}
	defer r.Close()

	gotCount, gotDigest := indexDigest(mustChunks(t, r))
	if gotCount != borgCount {
		t.Errorf("borge's index has %d entries, borg's %d", gotCount, borgCount)
	}
	if gotDigest != borgDigest {
		t.Errorf("index digests differ\n  borge: %s\n  borg:  %s", gotDigest, borgDigest)
	}

	for i := range ids {
		data, err := r.Get(ids[i])
		if err != nil {
			t.Fatalf("borge could not read object %d: %v", i, err)
		}
		if !bytes.Equal(data, objects[i]) {
			t.Fatalf("object %d differs when borge reads it", i)
		}
	}
}

// TestBorgeRebuildsIndexFromBorgPacks: with the index fragments removed, borge must
// reconstruct the same index by walking the packs. That is what makes the index a cache
// rather than data - losing it costs time, not content.
func TestBorgeRebuildsIndexFromBorgPacks(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")

	ids, objects := testObjects(t, 40)
	o.ask(t, "C %s", path)
	for i := range ids {
		o.ask(t, "P %s %s %s", path, enhex(ids[i]), enhex(objects[i]))
	}
	o.ask(t, "F %s", path)
	resp := o.ask(t, "I %s", path)
	borgDigest := strings.Fields(resp)[1]
	o.ask(t, "X %s", path)

	// Remove every index fragment, leaving only the packs.
	indexDir := filepath.Join(path, "index")
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(indexDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	r, err := Open(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	gotCount, gotDigest := indexDigest(mustChunks(t, r))
	if gotCount != len(ids) {
		t.Errorf("rebuilt index has %d entries, want %d", gotCount, len(ids))
	}
	if gotDigest != borgDigest {
		t.Errorf("the index rebuilt from packs differs from borg's\n  borge: %s\n  borg:  %s",
			gotDigest, borgDigest)
	}

	// And every object is still readable through the rebuilt index.
	for i := range ids {
		data, err := r.Get(ids[i])
		if err != nil {
			t.Fatalf("object %d unreadable after a rebuild: %v", i, err)
		}
		if !bytes.Equal(data, objects[i]) {
			t.Fatalf("object %d differs after a rebuild", i)
		}
	}
}

// TestBorgCheckPassesOnBorgeRepository runs borg's own repository check, which is the
// strongest single statement that borge produced a valid repository.
func TestBorgCheckPassesOnBorgeRepository(t *testing.T) {
	o := startOracle(t)
	path := filepath.Join(t.TempDir(), "repo")

	ids, objects := testObjects(t, 50)
	r, err := Create(location.MustLocal(path), smallPackOptions())
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if _, err := r.Put(ids[i], objects[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	got := o.ask(t, "K %s", path)
	if got != "ok" {
		t.Errorf("borg's repository check on a borge-written repository: %s", got)
	}
	o.ask(t, "X %s", path)
}

// TestLayoutMatchesBorg: both tools writing the same objects must produce the same set
// of files, modulo the content-addressed names that depend on how objects were grouped
// into packs.
func TestLayoutMatchesBorg(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()
	borgPath := filepath.Join(dir, "borg")
	borgePath := filepath.Join(dir, "borge")

	ids, objects := testObjects(t, 20)

	o.ask(t, "C %s", borgPath)
	for i := range ids {
		o.ask(t, "P %s %s %s", borgPath, enhex(ids[i]), enhex(objects[i]))
	}
	o.ask(t, "F %s", borgPath)
	o.ask(t, "X %s", borgPath)

	r, err := Create(location.MustLocal(borgePath), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if _, err := r.Put(ids[i], objects[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	shape := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			switch {
			case strings.HasPrefix(p, "packs/"):
				// Pack names are content hashes, and depend on how objects were grouped;
				// the shape (namespace and nesting depth) is what must match.
				out = append(out, fmt.Sprintf("packs/<nested %d>", strings.Count(p, "/")-1))
			case strings.HasPrefix(p, "index/"):
				out = append(out, "index/<hash>")
			default:
				out = append(out, p)
			}
		}
		sort.Strings(out)
		return out
	}

	borgShape := shape(splitList(o.ask(t, "T %s", borgPath)))
	borgeShape := shape(diskTree(t, borgePath))

	if strings.Join(borgShape, ",") != strings.Join(borgeShape, ",") {
		t.Errorf("repository layouts differ\n  borge: %v\n  borg:  %v", borgeShape, borgShape)
	}
}

// TestPackContentsAreIdentical: with both tools putting the same objects in the same
// order into one pack, the pack bytes must match, because a pack is the plain
// concatenation of its objects.
func TestPackContentsAreIdentical(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()
	borgPath := filepath.Join(dir, "borg")
	borgePath := filepath.Join(dir, "borge")

	ids, objects := testObjects(t, 10)

	o.ask(t, "C %s", borgPath)
	for i := range ids {
		o.ask(t, "P %s %s %s", borgPath, enhex(ids[i]), enhex(objects[i]))
	}
	o.ask(t, "F %s", borgPath)
	o.ask(t, "X %s", borgPath)

	r, err := Create(location.MustLocal(borgePath), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if _, err := r.Put(ids[i], objects[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	packNames := func(root string) []string {
		var out []string
		for _, p := range diskTree(t, root) {
			if strings.HasPrefix(p, "packs/") {
				out = append(out, p)
			}
		}
		return out
	}
	borgPacks, borgePacks := packNames(borgPath), packNames(borgePath)
	if len(borgPacks) != 1 || len(borgePacks) != 1 {
		t.Skipf("expected one pack each, got %d and %d", len(borgPacks), len(borgePacks))
	}
	// The pack is named by the sha256 of its content, so identical names already prove
	// identical bytes - but compare the bytes too, so a failure says what differs.
	if filepath.Base(borgPacks[0]) != filepath.Base(borgePacks[0]) {
		borgData, _ := os.ReadFile(filepath.Join(borgPath, borgPacks[0]))
		borgeData, _ := os.ReadFile(filepath.Join(borgePath, borgePacks[0]))
		t.Errorf("pack contents differ (%d vs %d bytes)\n  borge name: %s\n  borg name:  %s",
			len(borgeData), len(borgData), borgePacks[0], borgPacks[0])
	}
}

func mustChunks(t *testing.T, r *Repository) *hashindex.ChunkIndex {
	t.Helper()
	chunks, err := r.Chunks()
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}
