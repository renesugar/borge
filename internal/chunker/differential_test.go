// SPDX-License-Identifier: Apache-2.0

package chunker

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The stage 1.4 gate: for a fixed key and parameters, borge's chunk boundaries are
// *identical* to borg's - every offset, not a sample.
//
// The plan is deliberately strict here because a boundary disagreement is silent.
// Both implementations keep working; they simply deduplicate nothing against each
// other, and nobody finds out until a repository written by one and appended to by the
// other has stored everything twice. There is no round-trip test that catches it,
// which is why this compares cut offsets directly.

type oracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg chunker differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the chunker differential test")
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

// borgChunkSizes returns the sizes borg cuts the file at, in order. Sizes rather than
// absolute offsets because that is what borg reports, and the running sum is the
// boundary set.
func (o *oracle) borgChunkSizes(algo, keyArg string, p Params, path string) ([]int, error) {
	resp, err := o.ask(fmt.Sprintf("C %s %s %d %d %d %d %d %s",
		algo, keyArg, p.ChunkMinExp, p.ChunkMaxExp, p.HashMaskBits, p.WindowSize, p.NCLevel, path))
	if err != nil {
		return nil, err
	}
	return parseSizes(resp)
}

func parseSizes(s string) ([]int, error) {
	if s == "-" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, len(parts))
	for i, f := range parts {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("bad size %q: %w", f, err)
		}
		out[i] = n
	}
	return out, nil
}

// borgeChunkSizes runs borge's chunker over the same file.
func borgeChunkSizes(t *testing.T, p Params, key []byte, seed uint32, path string) []int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	c, err := New(p, key, seed, f)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("borge chunker: %v", err)
		}
		out = append(out, chunk.Size)
	}
	return out
}

// compareBoundaries reports the first divergence with enough context to debug it. The
// running offsets matter more than the sizes: a single differing size shifts every
// boundary after it, so the first mismatch is the only informative one.
func compareBoundaries(t *testing.T, name string, borgSizes, borgeSizes []int) {
	t.Helper()
	if len(borgSizes) == len(borgeSizes) {
		same := true
		for i := range borgSizes {
			if borgSizes[i] != borgeSizes[i] {
				same = false
				break
			}
		}
		if same {
			return
		}
	}

	offset := 0
	for i := 0; i < len(borgSizes) && i < len(borgeSizes); i++ {
		if borgSizes[i] != borgeSizes[i] {
			t.Errorf("%s: first boundary divergence at chunk %d (byte offset %d):\n"+
				"  borg:  %d\n  borge: %d\n"+
				"  borg has %d chunks, borge %d",
				name, i, offset, borgSizes[i], borgeSizes[i], len(borgSizes), len(borgeSizes))
			return
		}
		offset += borgSizes[i]
	}
	t.Errorf("%s: chunk counts differ after %d identical chunks (offset %d): borg %d, borge %d",
		name, min(len(borgSizes), len(borgeSizes)), offset, len(borgSizes), len(borgeSizes))
}

// corpusFile writes a temp file and returns its path. borg reads the data from disk, so
// megabytes of hex do not have to cross the pipe.
func corpusFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildCorpus produces the inputs the gate runs over. Sizes are chosen relative to the
// *test* chunker parameters (min 2^10, max 2^14) rather than borg's defaults, so a
// 4 MiB corpus exercises hundreds of cuts instead of two.
func buildCorpus(t *testing.T) map[string][]byte {
	t.Helper()
	rnd := rand.New(rand.NewSource(20260816))
	random := func(n int) []byte {
		b := make([]byte, n)
		rnd.Read(b)
		return b
	}
	text := func(n int) []byte {
		var b bytes.Buffer
		for b.Len() < n {
			fmt.Fprintf(&b, "## Rezept %d\n\n- 200 g Mehl\n- 3 Eier\n- eine Prise Salz\n\n"+
				"Alles verruehren, dann bei 180 C 25 Minuten backen.\n\n", b.Len())
		}
		return b.Bytes()[:n]
	}

	c := map[string][]byte{
		// Degenerate and boundary sizes. A file shorter than min_size has no cut at all;
		// exactly min_size and min_size+1 sit on either side of the driver's first test.
		"empty":        {},
		"one_byte":     {0x42},
		"under_min":    random(1023),
		"exactly_min":  random(1024),
		"min_plus_one": random(1025),
		"over_max":     random(16385), // forces a cut at max_size

		// The main content: random data cuts on hash luck alone, text has structure.
		"random_4m": random(4 << 20),
		"text_4m":   text(4 << 20),

		// All zeros: every window hashes identically, so this is where a chunker with a
		// broken cut condition either never cuts or cuts every byte.
		"zeros_4m": make([]byte, 4<<20),

		// A shifted copy of the same data. This is the property content-defined chunking
		// exists for: after an insertion, boundaries must resynchronise rather than all
		// shifting. If borge and borg resynchronise at different points, dedup is lost.
		"shifted": append(append([]byte("PREFIX"), text(2<<20)...), random(1<<20)...),
	}

	// Real data from the recipedb corpus when it is present, concatenated up to a few
	// MiB so there are plenty of cuts.
	dir := "/home/renes/projects/recipedb/recipe_vault/www-wedesoft-de/downloads/deutsche-rezepte"
	if entries, err := os.ReadDir(dir); err == nil {
		var buf bytes.Buffer
		for _, e := range entries {
			if buf.Len() > 4<<20 {
				break
			}
			if e.IsDir() {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				buf.Write(b)
			}
		}
		if buf.Len() > 0 {
			c["real_recipedb"] = buf.Bytes()
			t.Logf("corpus includes %d bytes of real recipedb data", buf.Len())
		}
	}
	return c
}

// testParams uses much smaller chunks than borg's defaults so a few MiB of input
// produces hundreds of boundaries. The algorithm does not care about the scale, and a
// corpus big enough to exercise borg's 2 MiB target would make the test intolerably slow.
func testParams(algo string, ncLevel int) Params {
	p := Params{
		Algorithm:    algo,
		ChunkMinExp:  10, // 1 KiB
		ChunkMaxExp:  14, // 16 KiB
		HashMaskBits: 12, // ~4 KiB chunks
		NCLevel:      ncLevel,
	}
	if algo == AlgoBuzhash || algo == AlgoBuzhash64 {
		p.WindowSize = 63
	}
	return p
}

var testKey = bytes.Repeat([]byte{0x5A}, 32)

// TestGearTableMatchesBorg checks the derived table before any chunking. If the table
// is wrong, every boundary is wrong, and comparing boundaries first would only say
// "everything differs".
func TestGearTableMatchesBorg(t *testing.T) {
	o := startOracle(t)

	for _, keyHex := range []string{
		strings.Repeat("00", 32),
		strings.Repeat("5a", 32),
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	} {
		key, err := hex.DecodeString(keyHex)
		if err != nil {
			t.Fatal(err)
		}

		t.Run("fastcdc/"+keyHex[:8], func(t *testing.T) {
			resp, err := o.ask("T fastcdc " + keyHex)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fastCDCGearTable(key)
			if err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			for _, v := range got {
				fmt.Fprintf(&sb, "%016x", v)
			}
			if sb.String() != resp {
				t.Errorf("gear table differs from borg's\n  borge: %s...\n  borg:  %s...",
					sb.String()[:64], resp[:64])
			}
		})

		t.Run("buzhash64/"+keyHex[:8], func(t *testing.T) {
			resp, err := o.ask("T buzhash64 " + keyHex)
			if err != nil {
				t.Fatal(err)
			}
			got, err := buzhash64Table(key)
			if err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			for _, v := range got {
				fmt.Fprintf(&sb, "%016x", v)
			}
			if sb.String() != resp {
				t.Errorf("buzhash64 table differs from borg's\n  borge: %s...\n  borg:  %s...",
					sb.String()[:64], resp[:64])
			}
		})
	}
}

// TestBoundariesMatchBorg is the gate.
func TestBoundariesMatchBorg(t *testing.T) {
	o := startOracle(t)
	corpus := buildCorpus(t)
	keyHex := hex.EncodeToString(testKey)

	type variant struct {
		algo    string
		keyArg  string
		ncLevel int
		seed    uint32
	}
	variants := []variant{
		{AlgoFastCDC, keyHex, 2, 0},   // borg's default configuration
		{AlgoFastCDC, keyHex, 0, 0},   // normalized chunking disabled
		{AlgoFastCDC, keyHex, 4, 0},   // a stronger normalization level
		{AlgoBuzhash64, keyHex, 2, 0}, //
		{AlgoBuzhash64, keyHex, 0, 0}, //
		{AlgoBuzhash, "0", 0, 0},      // borg 1.x compatible, integer seed, no nc_level
		{AlgoBuzhash, "305419896", 0, 305419896},
	}

	for _, v := range variants {
		p := testParams(v.algo, v.ncLevel)
		for name, data := range corpus {
			caseName := fmt.Sprintf("%s/nc%d/%s", v.algo, v.ncLevel, name)
			t.Run(caseName, func(t *testing.T) {
				path := corpusFile(t, "data.bin", data)
				borgSizes, err := o.borgChunkSizes(v.algo, v.keyArg, p, path)
				if err != nil {
					t.Fatalf("borg chunker: %v", err)
				}
				borgeSizes := borgeChunkSizes(t, p, testKey, v.seed, path)

				// Both must account for every byte, or one of them lost data.
				sum := func(xs []int) int {
					total := 0
					for _, x := range xs {
						total += x
					}
					return total
				}
				if got := sum(borgSizes); got != len(data) {
					t.Fatalf("borg's chunks total %d bytes, input is %d", got, len(data))
				}
				if got := sum(borgeSizes); got != len(data) {
					t.Fatalf("borge's chunks total %d bytes, input is %d", got, len(data))
				}

				compareBoundaries(t, caseName, borgSizes, borgeSizes)
			})
		}
	}
}

// TestResynchronisationMatchesBorg is the property content-defined chunking exists for:
// after bytes are inserted near the front, most later boundaries must be unchanged.
// Checking it explicitly guards against a chunker that agrees with borg on one input
// but drifts on a shifted one.
func TestResynchronisationMatchesBorg(t *testing.T) {
	o := startOracle(t)
	keyHex := hex.EncodeToString(testKey)
	p := testParams(AlgoFastCDC, 2)

	rnd := rand.New(rand.NewSource(99))
	base := make([]byte, 2<<20)
	rnd.Read(base)
	shifted := append([]byte("an inserted prefix of some length"), base...)

	basePath := corpusFile(t, "base.bin", base)
	shiftedPath := corpusFile(t, "shifted.bin", shifted)

	for _, tc := range []struct{ name, path string }{{"base", basePath}, {"shifted", shiftedPath}} {
		borgSizes, err := o.borgChunkSizes(AlgoFastCDC, keyHex, p, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		borgeSizes := borgeChunkSizes(t, p, testKey, 0, tc.path)
		compareBoundaries(t, tc.name, borgSizes, borgeSizes)
	}

	// And the property itself: the two runs must share most of their chunk sizes,
	// otherwise the corpus is not actually exercising resynchronisation.
	baseSizes := borgeChunkSizes(t, p, testKey, 0, basePath)
	shiftedSizes := borgeChunkSizes(t, p, testKey, 0, shiftedPath)
	shared := 0
	for i := 1; i < len(baseSizes) && i < len(shiftedSizes); i++ {
		if baseSizes[i] == shiftedSizes[i] {
			shared++
		}
	}
	if shared*2 < len(baseSizes) {
		t.Errorf("only %d of %d chunks resynchronised after an insertion; "+
			"content-defined chunking is not working", shared, len(baseSizes))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
