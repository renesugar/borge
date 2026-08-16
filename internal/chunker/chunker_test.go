// SPDX-License-Identifier: Apache-2.0

package chunker

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// Unit tests that run without the borg venv: the invariants every chunker must hold,
// parameter validation, and the CSPRNG.

func collect(t *testing.T, c Chunker) []Chunk {
	t.Helper()
	var out []Chunk
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("chunker: %v", err)
		}
		// Data must be copied: the contract says it is only valid until the next call.
		if chunk.Data != nil {
			chunk.Data = append([]byte(nil), chunk.Data...)
		}
		out = append(out, chunk)
	}
}

func unitParams(algo string) Params {
	p := Params{
		Algorithm:    algo,
		ChunkMinExp:  10,
		ChunkMaxExp:  14,
		HashMaskBits: 12,
		NCLevel:      2,
	}
	switch algo {
	case AlgoBuzhash:
		p.WindowSize, p.NCLevel = 63, 0
	case AlgoBuzhash64:
		p.WindowSize = 63
	}
	return p
}

var unitKey = bytes.Repeat([]byte{0x5A}, 32)

// TestEveryByteIsAccountedFor is the invariant that must hold for every chunker on
// every input: the chunks reassemble the input exactly, in order, with no gaps or
// overlaps. A backup tool that loses a byte here loses it silently.
func TestEveryByteIsAccountedFor(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	inputs := map[string][]byte{
		"empty":     {},
		"one":       {1},
		"under_min": make([]byte, 1023),
		"exact_min": make([]byte, 1024),
		"over_max":  make([]byte, 40000),
		"random":    func() []byte { b := make([]byte, 300000); rnd.Read(b); return b }(),
		"zeros":     make([]byte, 300000),
	}

	for _, algo := range []string{AlgoFastCDC, AlgoBuzhash64, AlgoBuzhash} {
		for name, data := range inputs {
			t.Run(algo+"/"+name, func(t *testing.T) {
				c, err := New(unitParams(algo), unitKey, 0, bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				chunks := collect(t, c)

				var total int
				var rebuilt bytes.Buffer
				for i, ch := range chunks {
					if ch.Size <= 0 {
						t.Errorf("chunk %d has size %d; a zero-length chunk is never valid", i, ch.Size)
					}
					total += ch.Size
					if ch.Allocation == AllocData {
						if len(ch.Data) != ch.Size {
							t.Errorf("chunk %d: Size %d but %d bytes of data", i, ch.Size, len(ch.Data))
						}
						rebuilt.Write(ch.Data)
					} else {
						// An all-zero chunk carries no data; it is reconstructed from Size.
						if ch.Data != nil {
							t.Errorf("chunk %d: allocation %d must carry no data", i, ch.Allocation)
						}
						rebuilt.Write(make([]byte, ch.Size))
					}
				}
				if total != len(data) {
					t.Errorf("chunks total %d bytes, input is %d", total, len(data))
				}
				if !bytes.Equal(rebuilt.Bytes(), data) {
					t.Error("reassembling the chunks did not reproduce the input")
				}
			})
		}
	}
}

// TestChunkSizeBounds: no chunk may exceed max_size, and only the last may be below
// min_size. Both bounds are load-bearing - max_size keeps objects storable, and the
// min_size floor is what makes sub-minimum cut-point skipping safe.
func TestChunkSizeBounds(t *testing.T) {
	rnd := rand.New(rand.NewSource(8))
	data := make([]byte, 2<<20)
	rnd.Read(data)

	for _, algo := range []string{AlgoFastCDC, AlgoBuzhash64, AlgoBuzhash} {
		t.Run(algo, func(t *testing.T) {
			p := unitParams(algo)
			c, err := New(p, unitKey, 0, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			chunks := collect(t, c)
			if len(chunks) < 10 {
				t.Fatalf("expected many chunks from %d bytes, got %d", len(data), len(chunks))
			}

			minSize := 1 << uint(p.ChunkMinExp)
			maxSize := 1 << uint(p.ChunkMaxExp)
			for i, ch := range chunks {
				if ch.Size > maxSize {
					t.Errorf("chunk %d is %d bytes, over the %d maximum", i, ch.Size, maxSize)
				}
				if i < len(chunks)-1 && ch.Size < minSize {
					t.Errorf("chunk %d is %d bytes, under the %d minimum (only the last may be)",
						i, ch.Size, minSize)
				}
			}
		})
	}
}

// TestAllZeroChunksAreNotStored: borg records an all-zero chunk by size alone. This is
// what makes backing up a sparse or freshly-zeroed disk image cheap.
func TestAllZeroChunksAreNotStored(t *testing.T) {
	c, err := New(unitParams(AlgoFastCDC), unitKey, 0, bytes.NewReader(make([]byte, 200000)))
	if err != nil {
		t.Fatal(err)
	}
	for i, ch := range collect(t, c) {
		if ch.Allocation != AllocAlloc {
			t.Errorf("chunk %d of all-zero input has allocation %d, want AllocAlloc", i, ch.Allocation)
		}
		if ch.Data != nil {
			t.Errorf("chunk %d carries %d bytes of data; zeros must not be stored", i, len(ch.Data))
		}
	}
}

// TestContentDefinedResynchronisation is the reason content-defined chunking exists:
// after an insertion near the front, later boundaries must realign rather than all
// shifting. A fixed-size chunker fails this, which is the point of the comparison.
func TestContentDefinedResynchronisation(t *testing.T) {
	rnd := rand.New(rand.NewSource(9))
	base := make([]byte, 1<<20)
	rnd.Read(base)
	shifted := append([]byte("inserted"), base...)

	sizes := func(algo string, data []byte, p Params) []int {
		c, err := New(p, unitKey, 0, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, ch := range collect(t, c) {
			out = append(out, ch.Size)
		}
		return out
	}

	for _, algo := range []string{AlgoFastCDC, AlgoBuzhash64} {
		t.Run(algo, func(t *testing.T) {
			p := unitParams(algo)
			a, b := sizes(algo, base, p), sizes(algo, shifted, p)
			shared := 0
			for i := 1; i < len(a) && i < len(b); i++ {
				if a[i] == b[i] {
					shared++
				}
			}
			if shared*2 < len(a) {
				t.Errorf("only %d of %d chunks realigned after an insertion", shared, len(a))
			}
		})
	}

	// The fixed chunker must *not* realign - it is the control that shows the property
	// above is real and not an artefact of the comparison.
	fp := Params{Algorithm: AlgoFixed, BlockSize: 4096}
	a, b := sizes(AlgoFixed, base, fp), sizes(AlgoFixed, shifted, fp)
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("fixed chunker produced nothing")
	}
	// Sizes are all identical for a fixed chunker, so compare content instead.
	ca, _ := New(fp, nil, 0, bytes.NewReader(base))
	cb, _ := New(fp, nil, 0, bytes.NewReader(shifted))
	first, second := collect(t, ca), collect(t, cb)
	same := 0
	for i := 1; i < len(first) && i < len(second); i++ {
		if bytes.Equal(first[i].Data, second[i].Data) {
			same++
		}
	}
	if same*2 > len(first) {
		t.Errorf("the fixed chunker realigned %d of %d chunks; it should realign none, "+
			"so the resynchronisation check above is not testing what it claims", same, len(first))
	}
}

func TestFixedChunker(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 1000)) // 10000 bytes

	t.Run("block size only", func(t *testing.T) {
		c, err := New(Params{Algorithm: AlgoFixed, BlockSize: 4096}, nil, 0, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		chunks := collect(t, c)
		want := []int{4096, 4096, 1808} // a short final block is expected, not an error
		if len(chunks) != len(want) {
			t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
		}
		for i, ch := range chunks {
			if ch.Size != want[i] {
				t.Errorf("chunk %d is %d bytes, want %d", i, ch.Size, want[i])
			}
		}
	})

	t.Run("with header", func(t *testing.T) {
		c, err := New(Params{Algorithm: AlgoFixed, BlockSize: 4096, HeaderSize: 512},
			nil, 0, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		chunks := collect(t, c)
		want := []int{512, 4096, 4096, 1296}
		if len(chunks) != len(want) {
			t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
		}
		for i, ch := range chunks {
			if ch.Size != want[i] {
				t.Errorf("chunk %d is %d bytes, want %d", i, ch.Size, want[i])
			}
		}
	})
}

// TestShortReadsDoNotChangeBoundaries guards the bug the differential test caught: a
// reader that returns data in small pieces must produce exactly the same chunks as one
// that returns it all at once. Treating a short read as EOF silently merged the last
// few chunks of every file.
func TestShortReadsDoNotChangeBoundaries(t *testing.T) {
	rnd := rand.New(rand.NewSource(10))
	data := make([]byte, 500000)
	rnd.Read(data)

	for _, algo := range []string{AlgoFastCDC, AlgoBuzhash64, AlgoBuzhash} {
		t.Run(algo, func(t *testing.T) {
			p := unitParams(algo)

			whole, err := New(p, unitKey, 0, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			want := collect(t, whole)

			for _, piece := range []int{1, 7, 1000, 65537} {
				dribbled, err := New(p, unitKey, 0, &slowReader{data: data, max: piece})
				if err != nil {
					t.Fatal(err)
				}
				got := collect(t, dribbled)
				if len(got) != len(want) {
					t.Fatalf("reads of at most %d bytes gave %d chunks, want %d",
						piece, len(got), len(want))
				}
				for i := range want {
					if got[i].Size != want[i].Size {
						t.Fatalf("reads of at most %d bytes: chunk %d is %d bytes, want %d",
							piece, i, got[i].Size, want[i].Size)
					}
				}
			}
		})
	}
}

// slowReader hands out at most max bytes per Read, like a pipe or a slow network mount.
type slowReader struct {
	data []byte
	max  int
	pos  int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.max {
		n = r.max
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func TestParameterValidation(t *testing.T) {
	tests := []struct {
		name string
		p    Params
		key  []byte
	}{
		{"unknown algorithm", Params{Algorithm: "nope"}, unitKey},
		{"min not below max", Params{Algorithm: AlgoFastCDC, ChunkMinExp: 20, ChunkMaxExp: 20, HashMaskBits: 12}, unitKey},
		{"nc_level too large for mask bits", Params{Algorithm: AlgoFastCDC, ChunkMinExp: 10, ChunkMaxExp: 14, HashMaskBits: 2, NCLevel: 2}, unitKey},
		{"buzhash64 without a window", Params{Algorithm: AlgoBuzhash64, ChunkMinExp: 10, ChunkMaxExp: 14, HashMaskBits: 12}, unitKey},
		{"short key", Params{Algorithm: AlgoFastCDC, ChunkMinExp: 10, ChunkMaxExp: 14, HashMaskBits: 12}, []byte("short")},
		{"fixed without a block size", Params{Algorithm: AlgoFixed}, nil},
		// 8192 + 8192 + 1 > 16384: a cut decision needs min_size plus a whole window
		// plus one byte to be buffered, so max_size has to hold all three.
		{"window does not fit in max", Params{Algorithm: AlgoBuzhash64, ChunkMinExp: 13, ChunkMaxExp: 14, HashMaskBits: 12, WindowSize: 8192}, unitKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.p, tc.key, 0, bytes.NewReader(nil)); err == nil {
				t.Error("accepted invalid parameters")
			}
		})
	}

	// buzhash must refuse nc_level: it has to stay bit-compatible with borg 1.x.
	p := unitParams(AlgoBuzhash)
	p.NCLevel = 2
	if _, err := New(p, nil, 0, bytes.NewReader(nil)); err == nil {
		t.Error("buzhash accepted an nc_level; it has no such parameter")
	}

	// The AES chunkers must fail with an explanation rather than silently doing
	// something else.
	for _, algo := range []string{AlgoRabinAES, AlgoGoldilocksAES, AlgoToeplitzAES} {
		q := unitParams(AlgoFastCDC)
		q.Algorithm = algo
		_, err := New(q, unitKey, 0, bytes.NewReader(nil))
		if err == nil {
			t.Errorf("%s reported success but is not implemented", algo)
		} else if !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("%s: unhelpful error %v", algo, err)
		}
	}
}

func TestMaskBits(t *testing.T) {
	// Where the one-bits sit is not cosmetic: Gear-style hashes accumulate in the high
	// bits and would otherwise decide cuts from a handful of recent bytes.
	if got := maskBits(4, false); got != 0x0f {
		t.Errorf("maskBits(4, low) = %#x, want 0xf", got)
	}
	if got := maskBits(4, true); got != 0xf000000000000000 {
		t.Errorf("maskBits(4, high) = %#x, want 0xf000000000000000", got)
	}
	if got := maskBits(0, false); got != 0 {
		t.Errorf("maskBits(0) = %#x, want 0", got)
	}
	if got := maskBits(64, true); got != ^uint64(0) {
		t.Errorf("maskBits(64) = %#x, want all ones", got)
	}
}

func TestNormalSizeDefault(t *testing.T) {
	// The default normal_size is the nominal target minus the expected loose-phase
	// tail, which lands the mean chunk size near the target instead of overshooting.
	cfg, err := newConfig(AlgoFastCDC, Params{
		ChunkMinExp: 19, ChunkMaxExp: 23, HashMaskBits: 21, NCLevel: 2,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := (1 << 21) - (1 << 19)
	if cfg.normalSize != want {
		t.Errorf("normalSize = %d, want %d", cfg.normalSize, want)
	}

	// nc_level 0 must reduce exactly to the single-mask chunker.
	cfg, err = newConfig(AlgoFastCDC, Params{
		ChunkMinExp: 19, ChunkMaxExp: 23, HashMaskBits: 21, NCLevel: 0,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.maskS != cfg.chunkMask || cfg.maskL != cfg.chunkMask || cfg.normalSize != 0 {
		t.Error("nc_level 0 must give a single mask and no normal size")
	}
}

// TestCSPRNGIsAESCTR pins the generator to a value that can be checked independently:
// AES-256 with an all-zero key and an all-zero block is a widely published vector.
func TestCSPRNGIsAESCTR(t *testing.T) {
	r, err := NewCSPRNG(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	const want = "dc95c078a2408989ad48a21492842087530f8afbc74536b9a963b4f1c4cb738b"
	if got := hex.EncodeToString(r.RandomBytes(32)); got != want {
		t.Errorf("CSPRNG stream\n  got:  %s\n  want: %s", got, want)
	}

	if _, err := NewCSPRNG(make([]byte, 16)); err == nil {
		t.Error("accepted a 16-byte seed; borg requires 32")
	}
}

// TestCSPRNGChunkedReadsMatch: the generator refills an internal buffer, and a request
// that straddles the refill boundary must produce the same bytes as one that does not.
func TestCSPRNGChunkedReadsMatch(t *testing.T) {
	whole, _ := NewCSPRNG(unitKey)
	want := whole.RandomBytes(10000)

	piecewise, _ := NewCSPRNG(unitKey)
	var got []byte
	for _, n := range []int{1, 4095, 1, 4096, 1000, 807} {
		got = append(got, piecewise.RandomBytes(n)...)
	}
	if !bytes.Equal(got, want) {
		t.Error("piecewise reads gave a different stream than one large read")
	}
}

func TestCSPRNGShuffleIsDeterministic(t *testing.T) {
	shuffleOnce := func() []int {
		r, err := NewCSPRNG(unitKey)
		if err != nil {
			t.Fatal(err)
		}
		items := make([]int, 256)
		for i := range items {
			items[i] = i
		}
		if err := r.Shuffle(items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	a, b := shuffleOnce(), shuffleOnce()
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("shuffle is not deterministic for the same seed")
		}
	}

	// It must actually permute, and it must be a permutation.
	seen := make(map[int]bool, 256)
	identity := true
	for i, v := range a {
		if seen[v] {
			t.Fatalf("value %d appears twice; not a permutation", v)
		}
		seen[v] = true
		if v != i {
			identity = false
		}
	}
	if identity {
		t.Error("shuffle left the slice in order")
	}
}

func TestBuzhashTableBaseIsIntact(t *testing.T) {
	// This constant is part of the on-disk format: borg 1.x cut chunks with it. Spot
	// checks at both ends catch a transcription slip.
	if buzhashTableBase[0] != 0xe7f831ec {
		t.Errorf("table_base[0] = %#08x, want 0xe7f831ec", buzhashTableBase[0])
	}
	if buzhashTableBase[255] != 0x71893f7b {
		t.Errorf("table_base[255] = %#08x, want 0x71893f7b", buzhashTableBase[255])
	}
	// It should have no duplicate entries; a repeated value would mean a copy/paste slip.
	seen := make(map[uint32]bool, 256)
	for i, v := range buzhashTableBase {
		if seen[v] {
			t.Errorf("table_base[%d] = %#08x is a duplicate", i, v)
		}
		seen[v] = true
	}
}

func BenchmarkFastCDC(b *testing.B) {
	data := make([]byte, 8<<20)
	rand.New(rand.NewSource(1)).Read(data)
	p := DefaultParams()

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := New(p, unitKey, 0, bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := c.Next(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkBuzhash64(b *testing.B) {
	data := make([]byte, 8<<20)
	rand.New(rand.NewSource(1)).Read(data)
	p := Params{
		Algorithm: AlgoBuzhash64, ChunkMinExp: ChunkMinExp, ChunkMaxExp: ChunkMaxExp,
		HashMaskBits: HashMaskBits, WindowSize: HashWindowSize, NCLevel: NCLevel,
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := New(p, unitKey, 0, bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := c.Next(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}
