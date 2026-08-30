// SPDX-License-Identifier: Apache-2.0

package compress

import "testing"

// TestDefaultIsWhatItSaysItIs pins the default compression to a codec and a level.
//
// It exists because changing the default broke nothing. R0 T6 switched borge from lz4 to
// zstd,1 on 2026-08-30 and the whole suite stayed green, which means nothing anywhere
// asserted what borge compresses with when nobody asks - the default could have been set to
// `lzma,9`, or reverted to lz4 by a careless merge, and every test would still have passed.
//
// A default that no test can see is a default nothing protects. This is deliberately
// specific: it names the codec *and* the level, because DIVERGENCES #62 chose level 1 over
// level 3 on a measured trade and a silent drift to 3 would cost 14 points of wall time for
// 1.7 points of ratio.
func TestDefaultIsWhatItSaysItIs(t *testing.T) {
	spec, err := ParseSpec(Default)
	if err != nil {
		t.Fatalf("the default compression %q does not parse: %v", Default, err)
	}
	if spec.Name != "zstd" {
		t.Errorf("default compression is %q, want zstd — see DIVERGENCES #62", spec.Name)
	}
	if spec.Level != 1 {
		t.Errorf("default zstd level is %d, want 1: level 3 buys 1.7 points of ratio for 14 points of wall time (DIVERGENCES #62)", spec.Level)
	}

	// And that the spec actually yields the codec it names, rather than parsing into
	// something that happens to be called zstd.
	c, err := spec.Compressor()
	if err != nil {
		t.Fatalf("default compressor: %v", err)
	}
	if c.ID() != IDZstd {
		t.Errorf("default compressor reports id %d, want IDZstd (%d)", c.ID(), IDZstd)
	}
}

// TestDefaultActuallyCompresses guards the property the default was chosen for. zstd,1 was
// picked because it stores a quarter less than lz4 on compressible data; a "default" that
// did not compress would satisfy the test above and still be wrong.
func TestDefaultActuallyCompresses(t *testing.T) {
	spec, err := ParseSpec(Default)
	if err != nil {
		t.Fatal(err)
	}
	c, err := spec.Compressor()
	if err != nil {
		t.Fatal(err)
	}
	// Compressible, and larger than the point at which framing overhead dominates.
	plain := make([]byte, 256<<10)
	for i := range plain {
		plain[i] = byte(i % 7)
	}
	var meta Meta
	out, err := c.Compress(&meta, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) >= len(plain)/2 {
		t.Errorf("default compressed %d bytes to %d; expected well under half", len(plain), len(out))
	}

	lz4, err := ParseSpec("lz4")
	if err != nil {
		t.Fatal(err)
	}
	lc, err := lz4.Compressor()
	if err != nil {
		t.Fatal(err)
	}
	var lmeta Meta
	lout, err := lc.Compress(&lmeta, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > len(lout) {
		t.Errorf("default produced %d bytes where lz4 produced %d: the default was chosen for ratio (DIVERGENCES #62)",
			len(out), len(lout))
	}
	t.Logf("on %d bytes of compressible data: default %d, lz4 %d", len(plain), len(out), len(lout))
}
