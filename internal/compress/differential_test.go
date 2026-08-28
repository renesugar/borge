// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"bufio"
	"bytes"
	"encoding/hex"
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

// The stage 1.2 gate: for every algorithm and level, borge decompresses borg's output
// and borg decompresses borge's output.
//
// Note what is *not* asserted: that the compressed bytes are identical. They need not
// be. Chunk ids are computed over plaintext and pack names over the pack's own
// contents, so nothing downstream depends on borge and borg producing identical
// compressed output. Requiring it would mean reimplementing zlib and libzstd bit for
// bit, for no gain. What is asserted is that both sides can read what the other wrote,
// and that the recorded metadata agrees.

// oracle is a running borg compression oracle (testdata/oracle.py).
type oracle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
	t      *testing.T
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		// The full corpus takes minutes, and under -race several more. The evidence
		// bundle runs the race pass with -short for that reason; the normal (non-race)
		// pass still runs everything, which is where these tests actually earn their
		// keep - there is no concurrency in this package for -race to find.
		t.Skip("skipping the borg differential corpus in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the compression differential test")
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

	o := &oracle{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20), stderr: &stderr, t: t}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return o
}

func (o *oracle) ask(request string) (string, error) {
	if _, err := io.WriteString(o.stdin, request+"\n"); err != nil {
		return "", fmt.Errorf("oracle write: %w (stderr: %s)", err, o.stderr.String())
	}
	line, err := o.stdout.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("oracle read: %w (stderr: %s)", err, o.stderr.String())
	}
	line = strings.TrimRight(line, "\n")
	if strings.HasPrefix(line, "ERR ") {
		return "", fmt.Errorf("borg said: %s", strings.TrimPrefix(line, "ERR "))
	}
	return strings.TrimPrefix(line, "OK "), nil
}

// borgCompress asks borg to compress data with the given --compression spec.
func (o *oracle) borgCompress(spec, robjType string, data []byte) (Meta, []byte, error) {
	resp, err := o.ask(fmt.Sprintf("C %s %s %s", spec, robjType, hexOrDash(data)))
	if err != nil {
		return Meta{}, nil, err
	}
	f := strings.Fields(resp)
	if len(f) != 6 {
		return Meta{}, nil, fmt.Errorf("malformed compress response: %q", resp)
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	meta := Meta{
		Type:   robjType,
		CType:  uint8(atoi(f[0])),
		CLevel: uint8(atoi(f[1])),
		CSize:  atoi(f[3]),
	}
	// f[2] is "-" when borg recorded no plaintext size, which is what the Auto
	// meta-compressor does. See Meta.Size.
	if f[2] != "-" {
		meta.Size, meta.SizeSet = atoi(f[2]), true
	}
	if f[4] != "-" {
		meta.PSize, meta.PSizeSet = atoi(f[4]), true
	}
	cdata, err := unhexOrDash(f[5])
	if err != nil {
		return Meta{}, nil, err
	}
	return meta, cdata, nil
}

// hexOrDash encodes a payload, using "-" for empty so the space-separated protocol
// keeps a constant field count.
func hexOrDash(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func unhexOrDash(s string) ([]byte, error) {
	if s == "-" {
		return nil, nil
	}
	return hex.DecodeString(s)
}

// borgDecompress asks borg to decompress what borge produced.
func (o *oracle) borgDecompress(meta Meta, cdata []byte) ([]byte, error) {
	psize := "-"
	if meta.PSizeSet {
		psize = strconv.Itoa(meta.PSize)
	}
	size := "-"
	if meta.SizeSet {
		size = strconv.Itoa(meta.Size)
	}
	resp, err := o.ask(fmt.Sprintf("D %d %d %s %s %s",
		meta.CType, meta.CLevel, size, psize, hexOrDash(cdata)))
	if err != nil {
		return nil, err
	}
	return unhexOrDash(resp)
}

// corpus returns the data the gate runs over: shapes chosen so each compressor's
// decision path is exercised, plus real files from the recipedb test data when it is
// present on this machine.
func corpus(t *testing.T) map[string][]byte {
	t.Helper()
	rnd := rand.New(rand.NewSource(20260816))

	random := func(n int) []byte {
		b := make([]byte, n)
		rnd.Read(b)
		return b
	}
	text := func(n int) []byte {
		// Markdown-ish, like the recipedb corpus: compresses well.
		var b bytes.Buffer
		for b.Len() < n {
			fmt.Fprintf(&b, "## Recipe %d\n\n- 200 g flour\n- 3 eggs\n- a pinch of salt\n\n"+
				"Mix everything, then bake at 180 C for 25 minutes.\n\n", b.Len())
		}
		return b.Bytes()[:n]
	}

	c := map[string][]byte{
		// Boundaries and degenerate inputs.
		"empty":     {},
		"one_byte":  {0x42},
		"tiny_text": []byte("hello"),
		"zeros_1k":  make([]byte, 1024),
		"zeros_1m":  make([]byte, 1<<20),

		// Incompressible: every DecidingCompressor must fall back to none here.
		"random_1k":  random(1024),
		"random_64k": random(64 * 1024),
		"random_1m":  random(1 << 20),

		// Compressible, at the sizes the chunker actually produces.
		"text_1k":  text(1024),
		"text_64k": text(64 * 1024),
		"text_2m":  text(2 << 20), // the default chunker aims at ~2 MiB chunks

		// Mixed: a compressible head and an incompressible tail, which is roughly what
		// a document with an embedded image looks like.
		"mixed_1m": append(text(512*1024), random(512*1024)...),

		// Just-barely-compressible, to land near Auto's 0.97 probe threshold.
		"barely_compressible": func() []byte {
			b := random(64 * 1024)
			for i := 0; i < len(b); i += 64 {
				b[i] = 0 // a sprinkle of structure, not enough to matter
			}
			return b
		}(),
	}

	// The plan's gate calls for a corpus drawn from the recipedb test data. Use it when
	// it is on this machine; the synthetic cases above keep the test meaningful when it
	// is not, so the gate is reproducible on a bare checkout.
	realFiles := []string{
		"/home/renes/projects/recipedb/recipe_vault/www-wedesoft-de/downloads/deutsche-rezepte",
		"/home/renes/projects/recipedb/recipe_joplin",
	}
	added := 0
	for _, dir := range realFiles {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if added >= 8 {
				break
			}
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil || len(b) == 0 || len(b) > 4<<20 {
				continue
			}
			c["real_"+filepath.Base(dir)+"_"+strconv.Itoa(added)] = b
			added++
		}
	}
	if added > 0 {
		t.Logf("corpus includes %d real file(s) from the recipedb test data", added)
	} else {
		t.Log("recipedb test data not present; running on the synthetic corpus only")
	}
	return c
}

// specs covers every algorithm at its boundary levels, plus the meta-compressors.
func specs() []string {
	return []string{
		"none",
		"lz4",
		"zlib,0", "zlib,1", "zlib,6", "zlib,9",
		"lzma,0", "lzma,6", "lzma,9",
		"zstd,-128", "zstd,-5", "zstd,1", "zstd,3", "zstd,10", "zstd,22",
		"auto,zstd,3", "auto,lzma,6", "auto,zlib,6", "auto,lz4",
		"obfuscate,1,zstd,3", "obfuscate,110,lz4", "obfuscate,250,zstd,3",
	}
}

// TestBorgWritesBorgeReads is direction one of the gate.
func TestBorgWritesBorgeReads(t *testing.T) {
	o := startOracle(t)
	data := corpus(t)

	for _, spec := range specs() {
		for name, plain := range data {
			t.Run(spec+"/"+name, func(t *testing.T) {
				if len(plain) == 0 && strings.HasPrefix(spec, "auto,") {
					t.Skip("borg raises ZeroDivisionError compressing empty data with auto; " +
						"see TestBorgeHandlesEmptyInputWhereBorgCrashes")
				}
				meta, cdata, err := o.borgCompress(spec, ROBJFileStream, plain)
				if err != nil {
					t.Fatalf("borg compress: %v", err)
				}
				// borg's Auto records no size at all; every other compressor must.
				if strings.HasPrefix(spec, "auto,") {
					if meta.SizeSet {
						t.Errorf("borg unexpectedly recorded a size for %s; "+
							"if upstream fixed this, Meta.Size's comment needs updating", spec)
					}
				} else if !meta.SizeSet || meta.Size != len(plain) {
					t.Errorf("borg recorded size %d (set=%v) for %d byte(s) of input",
						meta.Size, meta.SizeSet, len(plain))
				}
				if meta.CSize != len(cdata) {
					t.Errorf("borg recorded csize %d but produced %d byte(s)", meta.CSize, len(cdata))
				}

				got, err := Decompress(&meta, cdata)
				if err != nil {
					t.Fatalf("borge could not decompress borg's %s output "+
						"(ctype=%d clevel=%d size=%d csize=%d psize=%d/%v): %v",
						spec, meta.CType, meta.CLevel, meta.Size, meta.CSize, meta.PSize, meta.PSizeSet, err)
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("round trip changed the data (%d in, %d out)", len(plain), len(got))
				}
			})
		}
	}
}

// TestBorgeWritesBorgReads is direction two of the gate.
func TestBorgeWritesBorgReads(t *testing.T) {
	o := startOracle(t)
	data := corpus(t)

	for _, spec := range specs() {
		for name, plain := range data {
			t.Run(spec+"/"+name, func(t *testing.T) {
				if len(plain) == 0 && strings.HasPrefix(spec, "auto,") {
					t.Skip("borg cannot decompress what it cannot compress; " +
						"see TestBorgeHandlesEmptyInputWhereBorgCrashes")
				}
				c, err := FromSpec(spec)
				if err != nil {
					t.Fatalf("FromSpec(%q): %v", spec, err)
				}
				meta := Meta{Type: ROBJFileStream}
				cdata, err := c.Compress(&meta, plain)
				if err != nil {
					t.Fatalf("borge compress: %v", err)
				}
				// borge always records the size, including for auto - a deliberate
				// divergence that borg accepts. See the note in meta.go.
				if !meta.SizeSet || meta.Size != len(plain) {
					t.Errorf("borge recorded size %d (set=%v) for %d byte(s) of input",
						meta.Size, meta.SizeSet, len(plain))
				}
				if meta.CSize != len(cdata) {
					t.Errorf("borge recorded csize %d but produced %d byte(s)", meta.CSize, len(cdata))
				}

				got, err := o.borgDecompress(meta, cdata)
				if err != nil {
					t.Fatalf("borg could not decompress borge's %s output "+
						"(ctype=%d clevel=%d size=%d csize=%d psize=%d/%v): %v",
						spec, meta.CType, meta.CLevel, meta.Size, meta.CSize, meta.PSize, meta.PSizeSet, err)
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("round trip changed the data (%d in, %d out)", len(plain), len(got))
				}
			})
		}
	}
}

// TestDecisionsMatchBorg checks the part that *is* format-visible beyond the bytes:
// which compressor each side settles on, and therefore what ctype gets stored.
//
// The decision is not required to match for correctness - either ctype decompresses
// correctly - but a divergence would mean borge and borg disagree about when
// compression is worthwhile, which shows up as unexplained repository size
// differences. Where they legitimately differ (zstd's level mapping is approximate,
// see zstdLevel) the case is reported rather than failed.
//
//borge:checks compression/incompressible-stored-plain
func TestDecisionsMatchBorg(t *testing.T) {
	o := startOracle(t)
	data := corpus(t)

	// lz4 and none have no library-dependent behaviour, so their decisions must agree
	// exactly. zlib/lzma/zstd depend on the underlying implementation's ratio.
	exact := map[string]bool{"none": true, "lz4": true}

	var mismatches int
	for _, spec := range []string{"none", "lz4", "zlib,6", "lzma,6", "zstd,3", "auto,zstd,3", "auto,lzma,6"} {
		for name, plain := range data {
			if len(plain) == 0 && strings.HasPrefix(spec, "auto,") {
				continue // borg crashes here; see TestBorgeHandlesEmptyInputWhereBorgCrashes
			}
			borgMeta, _, err := o.borgCompress(spec, ROBJFileStream, plain)
			if err != nil {
				t.Fatalf("%s/%s: borg compress: %v", spec, name, err)
			}
			c, err := FromSpec(spec)
			if err != nil {
				t.Fatal(err)
			}
			borgeMeta := Meta{Type: ROBJFileStream}
			if _, err := c.Compress(&borgeMeta, plain); err != nil {
				t.Fatalf("%s/%s: borge compress: %v", spec, name, err)
			}

			if borgMeta.CType == borgeMeta.CType {
				continue
			}
			if exact[spec] {
				t.Errorf("%s/%s: ctype differs: borg chose %s, borge chose %s",
					spec, name, Name(borgMeta.CType), Name(borgeMeta.CType))
				continue
			}
			mismatches++
			t.Logf("%s/%s: ctype differs (borg %s, borge %s) - allowed, the underlying "+
				"library's ratio decides", spec, name, Name(borgMeta.CType), Name(borgeMeta.CType))
		}
	}
	if mismatches > 0 {
		t.Logf("%d decision(s) differed on library-dependent compressors", mismatches)
	}
}

// TestBorgeHandlesEmptyInputWhereBorgCrashes pins down an upstream bug found by the
// differential test above.
//
// borg's Auto.compress raises ZeroDivisionError on empty input, for every inner
// compressor. The chain: LZ4._decide short-circuits an empty input to a zero-length
// result, so Auto._decide computes 0/(0+2) = 0.0, which passes the 0.97 probe
// threshold and selects the expensive compressor; Auto.compress then divides
// len(expensive) by len(cheap), and both are zero.
//
// Reachability through "borg create" is doubtful, since the chunker yields no chunks
// for a zero-byte file - which is presumably why it has gone unnoticed. borge guards
// the division rather than reproducing the crash: refusing to compress an empty buffer
// is not behaviour worth being bug-compatible about, and the divergence is invisible
// to borg because it only shows up where borg would have raised.
//
// If upstream fixes this, the two skips above can be removed.
func TestBorgeHandlesEmptyInputWhereBorgCrashes(t *testing.T) {
	for _, spec := range specs() {
		c, err := FromSpec(spec)
		if err != nil {
			t.Fatalf("FromSpec(%q): %v", spec, err)
		}
		meta := Meta{Type: ROBJFileStream}
		cdata, err := c.Compress(&meta, nil)
		if err != nil {
			t.Errorf("%s: compressing empty data failed: %v", spec, err)
			continue
		}
		if meta.CType != IDNone {
			t.Errorf("%s: empty input stored as %s, want none", spec, Name(meta.CType))
		}
		if strings.HasPrefix(spec, "obfuscate,") {
			// Padding an empty chunk is the point of obfuscation - a zero-length
			// payload would advertise that the chunk was empty. The real payload is
			// still empty, which is what psize records.
			if !meta.PSizeSet || meta.PSize != 0 {
				t.Errorf("%s: psize = %d (set=%v), want 0", spec, meta.PSize, meta.PSizeSet)
			}
		} else if len(cdata) != 0 {
			t.Errorf("%s: empty input produced %d byte(s)", spec, len(cdata))
		}
		got, err := Decompress(&meta, cdata)
		if err != nil {
			t.Errorf("%s: decompressing empty data failed: %v", spec, err)
			continue
		}
		if len(got) != 0 {
			t.Errorf("%s: round trip of empty data produced %d byte(s)", spec, len(got))
		}
	}
}
