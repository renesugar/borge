// SPDX-License-Identifier: Apache-2.0

package hashindex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
)

// The stage 1.6 gate: borge reads an index written by borg, and borg reads one written
// by borge.
//
// Byte-identical output is deliberately *not* required. Entries are serialised in the
// table's internal bucket order, which depends on capacity and insertion history, so
// matching borg's bytes would mean reproducing its whole resize history for no benefit
// - the reader inserts each entry by key, so any order round-trips to the same index.
// The comparison is therefore an order-independent digest of the entry set.

type oracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg chunk index differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the chunk index differential test")
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
	return strings.TrimPrefix(line, "OK "), nil
}

// makeEntries generates the same entries the oracle does for a given seed, so the two
// sides can be compared without shipping the data across the pipe.
func makeEntries(n, seed int) ([][ChunkIDSize]byte, []Entry) {
	ids := make([][ChunkIDSize]byte, n)
	entries := make([]Entry, n)
	for i := 0; i < n; i++ {
		ids[i] = sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, i)))
		packID := sha256.Sum256([]byte(fmt.Sprintf("pack:%d:%d", seed, i/100)))
		entries[i] = Entry{
			Flags:     uint32(i%4) | 1,
			Size:      uint32((i*7 + 1) % 100000),
			PackID:    packID,
			ObjOffset: uint32((i * 13) % 4000000),
			ObjSize:   uint32((i*3 + 5) % 100000),
		}
	}
	return ids, entries
}

// entryDigest is the order-independent digest the oracle computes, reproduced in Go.
func entryDigest(c *ChunkIndex) string {
	var rows [][]byte
	c.Iterate(func(id []byte, e Entry) bool {
		row := make([]byte, 0, ChunkIDSize+EntrySize)
		row = append(row, id...)
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], e.Flags)
		row = append(row, buf[:]...)
		binary.LittleEndian.PutUint32(buf[:], e.Size)
		row = append(row, buf[:]...)
		row = append(row, e.PackID[:]...)
		binary.LittleEndian.PutUint32(buf[:], e.ObjOffset)
		row = append(row, buf[:]...)
		binary.LittleEndian.PutUint32(buf[:], e.ObjSize)
		row = append(row, buf[:]...)
		rows = append(rows, row)
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })

	h := sha256.New()
	for _, row := range rows {
		h.Write(row)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// entryCounts spans the resize thresholds: the table starts at capacity 1000 and grows
// when used+tombstones passes half of it, so these cross several rebuilds.
var entryCounts = []int{0, 1, 2, 100, 499, 500, 501, 1000, 5000, 50000}

// TestBorgWritesBorgeReads is direction one of the gate.
func TestBorgWritesBorgeReads(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()

	for _, n := range entryCounts {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("borg-%d.idx", n))
			wantDigest, err := o.ask(fmt.Sprintf("W %s %d %d", path, n, 42))
			if err != nil {
				t.Fatalf("borg write: %v", err)
			}

			c, err := ReadChunkIndexFile(path)
			if err != nil {
				t.Fatalf("borge could not read borg's index: %v", err)
			}
			if c.Len() != n {
				t.Fatalf("borge read %d entries, borg wrote %d", c.Len(), n)
			}
			if got := entryDigest(c); got != wantDigest {
				t.Errorf("entry sets differ\n  borge: %s\n  borg:  %s", got, wantDigest)
			}

			// And the entries must be individually correct, not merely digest-equal.
			ids, entries := makeEntries(n, 42)
			for i := range ids {
				got, ok := c.Get(ids[i][:])
				if !ok {
					t.Fatalf("entry %d (%x) is missing", i, ids[i])
				}
				if got != entries[i] {
					t.Fatalf("entry %d differs:\n  got:  %+v\n  want: %+v", i, got, entries[i])
				}
			}
		})
	}
}

// TestBorgeWritesBorgReads is direction two of the gate.
func TestBorgeWritesBorgReads(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()

	for _, n := range entryCounts {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			c, err := NewChunkIndex(n)
			if err != nil {
				t.Fatal(err)
			}
			ids, entries := makeEntries(n, 7)
			for i := range ids {
				if err := c.Set(ids[i][:], entries[i]); err != nil {
					t.Fatal(err)
				}
			}

			path := filepath.Join(dir, fmt.Sprintf("borge-%d.idx", n))
			if err := c.WriteFile(path); err != nil {
				t.Fatal(err)
			}

			resp, err := o.ask("R " + path)
			if err != nil {
				t.Fatalf("borg could not read borge's index: %v", err)
			}
			fields := strings.Fields(resp)
			if len(fields) != 2 {
				t.Fatalf("malformed oracle response %q", resp)
			}
			gotLen, err := strconv.Atoi(fields[0])
			if err != nil {
				t.Fatal(err)
			}
			if gotLen != n {
				t.Errorf("borg read %d entries, borge wrote %d", gotLen, n)
			}
			if want := entryDigest(c); fields[1] != want {
				t.Errorf("entry sets differ\n  borge: %s\n  borg:  %s", want, fields[1])
			}
		})
	}
}

// TestHeaderMatchesBorg compares the serialised header field by field. The entry order
// legitimately differs, but nothing in the header should - and a header difference is
// what would make borg reject the file outright.
func TestHeaderMatchesBorg(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()

	borgPath := filepath.Join(dir, "borg.idx")
	if _, err := o.ask(fmt.Sprintf("W %s %d %d", borgPath, 1000, 1)); err != nil {
		t.Fatal(err)
	}
	borgBytes, err := os.ReadFile(borgPath)
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewChunkIndex(1000)
	if err != nil {
		t.Fatal(err)
	}
	ids, entries := makeEntries(1000, 1)
	for i := range ids {
		if err := c.Set(ids[i][:], entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	var borgeBuf bytes.Buffer
	if err := c.Write(&borgeBuf); err != nil {
		t.Fatal(err)
	}
	borgeBytes := borgeBuf.Bytes()

	// Magic and version must match byte for byte.
	if !bytes.Equal(borgBytes[:12], borgeBytes[:12]) {
		t.Errorf("header prefix differs\n  borg:  %x\n  borge: %x", borgBytes[:12], borgeBytes[:12])
	}

	parseMeta := func(b []byte) map[string]any {
		size := binary.LittleEndian.Uint32(b[12:16])
		var m map[string]any
		if err := json.Unmarshal(b[16:16+size], &m); err != nil {
			t.Fatalf("cannot parse metadata: %v", err)
		}
		return m
	}
	borgMeta, borgeMeta := parseMeta(borgBytes), parseMeta(borgeBytes)

	// Every field except capacity must agree. Capacity reflects resize history, which
	// is an implementation detail the reader only uses as a sizing hint.
	for key, want := range borgMeta {
		if key == "capacity" {
			continue
		}
		got, ok := borgeMeta[key]
		if !ok {
			t.Errorf("borge's metadata is missing %q", key)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("metadata %q differs\n  borge: %v\n  borg:  %v", key, got, want)
		}
	}
	for key := range borgeMeta {
		if _, ok := borgMeta[key]; !ok {
			t.Errorf("borge's metadata has an extra field %q", key)
		}
	}

	// The body must be the same length: used x (key + value).
	borgBody := len(borgBytes) - 16 - int(binary.LittleEndian.Uint32(borgBytes[12:16]))
	borgeBody := len(borgeBytes) - 16 - int(binary.LittleEndian.Uint32(borgeBytes[12:16]))
	if borgBody != borgeBody {
		t.Errorf("body length differs: borg %d, borge %d", borgBody, borgeBody)
	}
	if want := 1000 * (ChunkIDSize + EntrySize); borgeBody != want {
		t.Errorf("body is %d bytes, want %d", borgeBody, want)
	}
}

// TestRoundTripThroughBorg sends an index out to borg and back, which catches an error
// that cancels itself out in a borge-only round trip.
func TestRoundTripThroughBorg(t *testing.T) {
	o := startOracle(t)
	dir := t.TempDir()

	// borg writes, borge reads and rewrites, borg reads again: the digest must survive
	// the whole loop.
	path := filepath.Join(dir, "a.idx")
	wantDigest, err := o.ask(fmt.Sprintf("W %s %d %d", path, 5000, 99))
	if err != nil {
		t.Fatal(err)
	}

	c, err := ReadChunkIndexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := filepath.Join(dir, "b.idx")
	if err := c.WriteFile(rewritten); err != nil {
		t.Fatal(err)
	}

	resp, err := o.ask("R " + rewritten)
	if err != nil {
		t.Fatalf("borg could not read borge's rewrite: %v", err)
	}
	fields := strings.Fields(resp)
	if len(fields) != 2 || fields[1] != wantDigest {
		t.Errorf("digest changed across the loop\n  after: %v\n  before: %s", fields, wantDigest)
	}
}
