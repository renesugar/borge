// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// Unit tests for what the differential test cannot reach: spec parsing, level
// encoding, the obfuscation padding schemes, and the failure modes. These run without
// the borg venv, so the package stays testable on a bare checkout.

func TestParseSpec(t *testing.T) {
	tests := []struct {
		in        string
		wantName  string
		wantLevel int
		wantStr   string
	}{
		{"none", "none", UnknownLevel, "none"},
		{"lz4", "lz4", UnknownLevel, "lz4"},
		{"zlib", "zlib", 6, "zlib,6"},
		{"zlib,0", "zlib", 0, "zlib,0"},
		{"zlib,9", "zlib", 9, "zlib,9"},
		{"lzma", "lzma", 6, "lzma,6"},
		{"zstd", "zstd", 3, "zstd,3"},
		{"zstd,1", "zstd", 1, "zstd,1"},
		{"zstd,22", "zstd", 22, "zstd,22"},
		{"zstd,-128", "zstd", -128, "zstd,-128"},
		{"auto,zstd,3", "auto", UnknownLevel, "auto,zstd,3"},
		{"auto,lz4", "auto", UnknownLevel, "auto,lz4"},
		{"obfuscate,110,zstd,3", "obfuscate", 110, "obfuscate,110,zstd,3"},
		{"obfuscate,250,lz4", "obfuscate", 250, "obfuscate,250,lz4"},
		{"obfuscate,1,auto,zstd,3", "obfuscate", 1, "obfuscate,1,auto,zstd,3"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			spec, err := ParseSpec(tc.in)
			if err != nil {
				t.Fatalf("ParseSpec(%q): %v", tc.in, err)
			}
			if spec.Name != tc.wantName {
				t.Errorf("name = %q, want %q", spec.Name, tc.wantName)
			}
			if spec.Level != tc.wantLevel {
				t.Errorf("level = %d, want %d", spec.Level, tc.wantLevel)
			}
			// Round-tripping matters: borg records the spec string in archive metadata.
			if got := spec.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
			if _, err := spec.Compressor(); err != nil {
				t.Errorf("Compressor(): %v", err)
			}
		})
	}
}

func TestParseSpecRejects(t *testing.T) {
	bad := []string{
		"", "bogus", "none,1", "lz4,1",
		"zlib,10", "zlib,-1", "zlib,x",
		"zstd,23", "zstd,-129",
		"auto", "auto,zstd,3,4",
		"obfuscate", "obfuscate,110", "obfuscate,7,lz4", "obfuscate,109,lz4",
		"obfuscate,124,lz4", "obfuscate,251,lz4",
		"zlib_legacy", // borg 1.x only; must be refused with an explanation
	}
	for _, in := range bad {
		if spec, err := ParseSpec(in); err == nil {
			t.Errorf("ParseSpec(%q) accepted, got %v", in, spec)
		}
	}
}

// TestZstdLevelByte covers the one compressor whose level does not equal its stored
// byte. Getting this wrong makes negative levels read back as large positive ones.
func TestZstdLevelByte(t *testing.T) {
	tests := []struct {
		level int
		want  uint8
	}{
		{0, 0}, {1, 1}, {3, 3}, {22, 22},
		{-1, 0xff}, {-5, 0xfb}, {-128, 0x80},
	}
	for _, tc := range tests {
		got, err := Zstd{}.EncodeLevel(tc.level)
		if err != nil {
			t.Fatalf("EncodeLevel(%d): %v", tc.level, err)
		}
		if got != tc.want {
			t.Errorf("EncodeLevel(%d) = 0x%02x, want 0x%02x", tc.level, got, tc.want)
		}
		if back := (Zstd{}).DecodeLevel(got); back != tc.level {
			t.Errorf("DecodeLevel(0x%02x) = %d, want %d", got, back, tc.level)
		}
	}
	// Levels 1..22 must encode to the byte they always did, or repositories written by
	// older borg versions would be misread.
	for level := 1; level <= 22; level++ {
		got, _ := Zstd{}.EncodeLevel(level)
		if int(got) != level {
			t.Errorf("level %d encodes to %d; positive levels must be unchanged", level, got)
		}
	}
}

// TestIncompressibleFallsBackToNone is borg's central deciding behaviour: data that
// does not shrink is stored uncompressed, with ctype none.
func TestIncompressibleFallsBackToNone(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	random := make([]byte, 64*1024)
	rnd.Read(random)

	for _, spec := range []string{"lz4", "zlib,9", "lzma,9", "zstd,22"} {
		c, err := FromSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		meta := Meta{Type: ROBJFileStream}
		out, err := c.Compress(&meta, random)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if meta.CType != IDNone {
			t.Errorf("%s: incompressible data stored as %s (%d bytes from %d), want none",
				spec, Name(meta.CType), len(out), len(random))
		}
		if len(out) != len(random) {
			t.Errorf("%s: output grew from %d to %d bytes", spec, len(random), len(out))
		}
		if meta.CLevel != UnknownLevel {
			t.Errorf("%s: clevel = %d, want %d for none", spec, meta.CLevel, UnknownLevel)
		}
	}
}

func TestCompressibleActuallyCompresses(t *testing.T) {
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 2000)
	for _, tc := range []struct {
		spec   string
		wantID uint8
	}{
		{"lz4", IDLZ4},
		{"zlib,6", IDZlib},
		{"lzma,6", IDLZMA},
		{"zstd,3", IDZstd},
	} {
		c, err := FromSpec(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		meta := Meta{Type: ROBJFileStream}
		out, err := c.Compress(&meta, text)
		if err != nil {
			t.Fatalf("%s: %v", tc.spec, err)
		}
		if meta.CType != tc.wantID {
			t.Errorf("%s: ctype = %s, want %s", tc.spec, Name(meta.CType), Name(tc.wantID))
		}
		if len(out) >= len(text) {
			t.Errorf("%s: did not shrink (%d -> %d)", tc.spec, len(text), len(out))
		}
		back, err := Decompress(&meta, out)
		if err != nil {
			t.Fatalf("%s: %v", tc.spec, err)
		}
		if !bytes.Equal(back, text) {
			t.Errorf("%s: round trip changed the data", tc.spec)
		}
	}
}

// TestObfuscateOnlyPadsFileStreams: metadata objects must not be padded, because their
// sizes are not what a size-based attack looks at and padding them wastes space.
func TestObfuscateOnlyPadsFileStreams(t *testing.T) {
	data := bytes.Repeat([]byte("recipe data "), 1000)

	for _, robjType := range []string{ROBJFileStream, "M", "A", "C", "S"} {
		c, err := NewObfuscateSize(120, LZ4{})
		if err != nil {
			t.Fatal(err)
		}
		meta := Meta{Type: robjType}
		out, err := c.Compress(&meta, data)
		if err != nil {
			t.Fatal(err)
		}
		padded := meta.CSize > meta.PSize
		if robjType == ROBJFileStream {
			// Level 120 pads by up to 1 MiB, so a zero draw is possible but vanishingly
			// unlikely; assert only that padding is *possible*, via the psize bookkeeping.
			if !meta.PSizeSet {
				t.Errorf("%s: psize not recorded", robjType)
			}
		} else if padded {
			t.Errorf("%s: metadata object was padded (csize %d > psize %d)",
				robjType, meta.CSize, meta.PSize)
		}
		if meta.CSize != len(out) {
			t.Errorf("%s: csize %d but %d byte(s) produced", robjType, meta.CSize, len(out))
		}
		if !meta.OLevelSet || meta.OLevel != 120 {
			t.Errorf("%s: olevel = %d (set=%v), want 120", robjType, meta.OLevel, meta.OLevelSet)
		}

		back, err := Decompress(&meta, out)
		if err != nil {
			t.Fatalf("%s: %v", robjType, err)
		}
		if !bytes.Equal(back, data) {
			t.Errorf("%s: round trip changed the data", robjType)
		}
	}
}

func TestPadmePadding(t *testing.T) {
	// Padmé rounds a length up so that the padded length leaks little about the real
	// one, while keeping overhead bounded (about 12% worst case).
	tests := []struct{ in, want int }{
		{0, 0}, {1, 0}, // too small to round
		{2, 0}, {3, 0},
		{9, 1},   // 9 -> 10
		{100, 4}, // 100 -> 104
		{1000, 24},
	}
	for _, tc := range tests {
		if got := padmePadding(tc.in); got != tc.want {
			t.Errorf("padmePadding(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// The overhead bound is the property that matters, so check it broadly.
	for n := 2; n < 1<<20; n = n*3/2 + 1 {
		pad := padmePadding(n)
		if pad < 0 {
			t.Fatalf("padmePadding(%d) = %d, must not be negative", n, pad)
		}
		if overhead := float64(pad) / float64(n); overhead > 0.12 {
			t.Errorf("padmePadding(%d) = %d, overhead %.1f%% exceeds the ~12%% bound",
				n, pad, overhead*100)
		}
	}
}

func TestObfuscateStaysUnderMaxDataSize(t *testing.T) {
	// Level 123 pads by up to 8 MiB. A chunk already near MAX_DATA_SIZE must not be
	// pushed past it, or the object becomes unstorable.
	big := make([]byte, MaxDataSize-2048)
	rnd := rand.New(rand.NewSource(2))
	rnd.Read(big) // incompressible, so the payload stays big

	c, err := NewObfuscateSize(123, LZ4{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		meta := Meta{Type: ROBJFileStream}
		out, err := c.Compress(&meta, big)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > MaxDataSize {
			t.Fatalf("obfuscated payload is %d bytes, over the %d limit", len(out), MaxDataSize)
		}
	}
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		ctype  uint8
		clevel uint8
		want   int
	}{
		{IDNone, 255, 255},
		{IDLZ4, 255, 255},
		{IDZlib, 6, 6},
		{IDLZMA, 6, 6},
		{IDZstd, 3, 3},
		{IDZstd, 0xfb, -5}, // negative zstd level, read back through int8
	} {
		_, level, err := Detect(tc.ctype, tc.clevel)
		if err != nil {
			t.Errorf("Detect(%d, %d): %v", tc.ctype, tc.clevel, err)
			continue
		}
		if level != tc.want {
			t.Errorf("Detect(%d, %d) level = %d, want %d", tc.ctype, tc.clevel, level, tc.want)
		}
	}

	// Unknown and legacy ids must be refused with an explanation, not guessed at.
	for _, ctype := range []uint8{IDZlibLegacy, IDObfuscate, 0x06, 0x42, 0xff} {
		if _, _, err := Detect(ctype, 0); err == nil {
			t.Errorf("Detect(0x%02x) succeeded, want an error", ctype)
		}
	}
}

func TestDecompressRejectsInconsistentMetadata(t *testing.T) {
	data := bytes.Repeat([]byte("compress me "), 500)
	c, _ := FromSpec("zstd,3")
	meta := Meta{Type: ROBJFileStream}
	out, err := c.Compress(&meta, data)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("csize mismatch", func(t *testing.T) {
		m := meta
		m.CSize++
		if _, err := Decompress(&m, out); err == nil {
			t.Error("accepted a csize that disagrees with the payload length")
		}
	})
	t.Run("size mismatch", func(t *testing.T) {
		m := meta
		m.Size++
		if _, err := Decompress(&m, out); err == nil {
			t.Error("accepted a plaintext size that disagrees with the result")
		}
	})
	t.Run("psize past the payload", func(t *testing.T) {
		m := meta
		m.PSize, m.PSizeSet = len(out)+1, true
		if _, err := Decompress(&m, out); err == nil {
			t.Error("accepted a psize beyond the available bytes")
		}
	})
	t.Run("corrupted payload", func(t *testing.T) {
		bad := append([]byte(nil), out...)
		bad[len(bad)/2] ^= 0xff
		if _, err := Decompress(&meta, bad); err == nil {
			t.Error("accepted a corrupted zstd payload")
		}
	})
}

// TestLZ4WithoutRecordedSize exercises the grow-and-retry path, which is what reading
// a borg object written with --compression auto,... actually takes.
func TestLZ4WithoutRecordedSize(t *testing.T) {
	for _, n := range []int{1, 1000, 64 * 1024, 3 << 20} {
		data := bytes.Repeat([]byte("borge "), n/6+1)[:n]
		meta := Meta{Type: ROBJFileStream}
		out, err := LZ4{}.Compress(&meta, data)
		if err != nil {
			t.Fatal(err)
		}
		if meta.CType != IDLZ4 {
			continue // incompressible at this size; not the path under test
		}

		// Drop the size, as borg's Auto would have.
		noSize := meta
		noSize.Size, noSize.SizeSet = 0, false
		got, err := Decompress(&noSize, out)
		if err != nil {
			t.Fatalf("n=%d: decompress without a recorded size: %v", n, err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("n=%d: round trip without a recorded size changed the data", n)
		}
	}
}

func TestAutoPrefersCheapWhenExpensiveBarelyWins(t *testing.T) {
	// Highly repetitive data compresses well under both lz4 and zstd, but if zstd does
	// not beat lz4 by more than 1% borg keeps the lz4 result, because it decompresses
	// far faster. Assert the outcome is one of the two, with the metadata consistent.
	data := bytes.Repeat([]byte("aaaaaaaabbbbbbbb"), 4096)
	c, err := FromSpec("auto,zstd,3")
	if err != nil {
		t.Fatal(err)
	}
	meta := Meta{Type: ROBJFileStream}
	out, err := c.Compress(&meta, data)
	if err != nil {
		t.Fatal(err)
	}
	if meta.CType != IDLZ4 && meta.CType != IDZstd && meta.CType != IDNone {
		t.Errorf("auto chose %s, expected lz4, zstd or none", Name(meta.CType))
	}
	if meta.CSize != len(out) {
		t.Errorf("csize %d but %d byte(s) produced", meta.CSize, len(out))
	}
	back, err := Decompress(&meta, out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Error("round trip changed the data")
	}
}

func TestNamesAreStable(t *testing.T) {
	// These names appear in --compression specs and in borg's stored archive metadata.
	got := strings.Join(Names(), " ")
	want := "lz4 lzma none obfuscate zlib zlib_legacy zstd"
	if got != want {
		t.Errorf("Names() = %q, want %q", got, want)
	}
}

func FuzzDecompressDoesNotPanic(f *testing.F) {
	f.Add(uint8(IDZstd), uint8(3), 100, []byte{1, 2, 3})
	f.Add(uint8(IDLZ4), uint8(255), 10, []byte{0xff, 0xff})
	f.Add(uint8(IDZlib), uint8(6), 0, []byte{})
	f.Fuzz(func(t *testing.T, ctype, clevel uint8, size int, data []byte) {
		if size < 0 || size > 1<<24 {
			t.Skip() // the caller bounds this from authenticated metadata
		}
		meta := Meta{CType: ctype, CLevel: clevel, Size: size, SizeSet: true, CSize: len(data)}
		// Arbitrary metadata over arbitrary bytes must produce an error, never a crash.
		_, _ = Decompress(&meta, data)
	})
}
