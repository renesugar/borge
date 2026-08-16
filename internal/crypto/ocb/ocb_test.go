// SPDX-License-Identifier: Apache-2.0

package ocb

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"errors"
	"testing"
)

// The test vectors from RFC 7253 section 4, verbatim.
//
// These are the primary correctness check for this package. Every one exercises a
// different combination of plaintext and associated-data lengths across the block
// boundary, which is where an OCB implementation goes wrong: the partial-block padding,
// the L_* offset for the final block, and the checksum's 1-bit terminator.

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// Section 4.2: K = 000102...0F, N = BBAA99887766554433221100 with the low byte varying,
// tag length 128 bits.
var rfc7253Vectors = []struct {
	nonce  string
	ad     string
	plain  string
	cipher string // ciphertext || tag
}{
	{"BBAA99887766554433221100", "", "", "785407BFFFC8AD9EDCC5520AC9111EE6"},
	{"BBAA99887766554433221101", "0001020304050607", "0001020304050607",
		"6820B3657B6F615A5725BDA0D3B4EB3A257C9AF1F8F03009"},
	{"BBAA99887766554433221102", "0001020304050607", "", "81017F8203F081277152FADE694A0A00"},
	{"BBAA99887766554433221103", "", "0001020304050607", "45DD69F8F5AAE72414054CD1F35D82760B2CD00D2F99BFA9"},
	{"BBAA99887766554433221104", "000102030405060708090A0B0C0D0E0F", "000102030405060708090A0B0C0D0E0F",
		"571D535B60B277188BE5147170A9A22C3AD7A4FF3835B8C5701C1CCEC8FC3358"},
	{"BBAA99887766554433221105", "000102030405060708090A0B0C0D0E0F", "", "8CF761B6902EF764462AD86498CA6B97"},
	{"BBAA99887766554433221106", "", "000102030405060708090A0B0C0D0E0F",
		"5CE88EC2E0692706A915C00AEB8B2396F40E1C743F52436BDF06D8FA1ECA343D"},
	{"BBAA99887766554433221107", "000102030405060708090A0B0C0D0E0F1011121314151617",
		"000102030405060708090A0B0C0D0E0F1011121314151617",
		"1CA2207308C87C010756104D8840CE1952F09673A448A122C92C62241051F57356D7F3C90BB0E07F"},
	{"BBAA99887766554433221108", "000102030405060708090A0B0C0D0E0F1011121314151617", "",
		"6DC225A071FC1B9F7C69F93B0F1E10DE"},
	{"BBAA99887766554433221109", "", "000102030405060708090A0B0C0D0E0F1011121314151617",
		"221BD0DE7FA6FE993ECCD769460A0AF2D6CDED0C395B1C3CE725F32494B9F914D85C0B1EB38357FF"},
	{"BBAA9988776655443322110A", "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F",
		"000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F",
		"BD6F6C496201C69296C11EFD138A467ABD3C707924B964DEAFFC40319AF5A48540FBBA186C5553C68AD9F592A79A4240"},
	{"BBAA9988776655443322110B", "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F", "",
		"FE80690BEE8A485D11F32965BC9D2A32"},
	{"BBAA9988776655443322110C", "", "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F",
		"2942BFC773BDA23CABC6ACFD9BFD5835BD300F0973792EF46040C53F1432BCDFB5E1DDE3BC18A5F840B52E653444D5DF"},
	{"BBAA9988776655443322110D", "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F2021222324252627",
		"000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F2021222324252627",
		"D5CA91748410C1751FF8A2F618255B68A0A12E093FF454606E59F9C1D0DDC54B65E8628E568BAD7AED07BA06A4A69483A7035490C5769E60"},
	{"BBAA9988776655443322110E", "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F2021222324252627", "",
		"C5CD9D1850C141E358649994EE701B68"},
	{"BBAA9988776655443322110F", "", "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F2021222324252627",
		"4412923493C57D5DE0D700F753CCE0D1D2D95060122E9F15A5DDBFC5787E50B5CC55EE507BCB084E479AD363AC366B95A98CA5F3000B1479"},
}

const rfc7253Key = "000102030405060708090A0B0C0D0E0F"

func TestRFC7253Vectors(t *testing.T) {
	block, err := aes.NewCipher(unhex(t, rfc7253Key))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := New(block)
	if err != nil {
		t.Fatal(err)
	}

	for i, v := range rfc7253Vectors {
		nonce := unhex(t, v.nonce)
		ad := unhex(t, v.ad)
		plain := unhex(t, v.plain)
		want := unhex(t, v.cipher)

		got := aead.Seal(nil, nonce, plain, ad)
		if !bytes.Equal(got, want) {
			t.Errorf("vector %d: Seal\n  got:  %X\n  want: %X", i, got, want)
			continue
		}

		back, err := aead.Open(nil, nonce, want, ad)
		if err != nil {
			t.Errorf("vector %d: Open: %v", i, err)
			continue
		}
		if !bytes.Equal(back, plain) {
			t.Errorf("vector %d: Open\n  got:  %X\n  want: %X", i, back, plain)
		}
	}
}

// TestRFC7253IterativeVectors runs the generator algorithm from RFC 7253 appendix A.
//
// It chains 384 encryptions covering every plaintext and associated-data length from 0
// to 127 bytes, then authenticates the whole accumulated output - so a single 16-byte
// comparison pins down every partial-block case at once. It is by far the strongest
// check in this file, and it covers all three AES key sizes and three tag lengths.
//
// AES-256 with a 128-bit tag is borg's configuration.
func TestRFC7253IterativeVectors(t *testing.T) {
	tests := []struct {
		name   string
		keyLen int
		tagLen int
		want   string
	}{
		{"AEAD_AES_128_OCB_TAGLEN128", 16, 16, "67E944D23256C5E0B6C61FA22FDF1EA2"},
		{"AEAD_AES_192_OCB_TAGLEN128", 24, 16, "F673F2C3E7174AAE7BAE986CA9F29E17"},
		{"AEAD_AES_256_OCB_TAGLEN128", 32, 16, "D90EB8E9C977C88B79DD793D7FFA161C"}, // borg's mode
		// Tag lengths other than 128 bits change the first byte of the nonce block and
		// therefore Offset_0. An implementation that hard-codes that byte to zero passes
		// every 128-bit vector above and fails all of these.
		{"AEAD_AES_128_OCB_TAGLEN96", 16, 12, "77A3D8E73589158D25D01209"},
		{"AEAD_AES_192_OCB_TAGLEN96", 24, 12, "05D56EAD2752C86BE6932C5E"},
		{"AEAD_AES_256_OCB_TAGLEN96", 32, 12, "5458359AC23B0CBA9E6330DD"},
		{"AEAD_AES_128_OCB_TAGLEN64", 16, 8, "192C9B7BD90BA06A"},
		{"AEAD_AES_192_OCB_TAGLEN64", 24, 8, "0066BC6E0EF34E24"},
		{"AEAD_AES_256_OCB_TAGLEN64", 32, 8, "7D4EA5D445501CBE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rfc7253IterativeTest(t, tc.keyLen, tc.tagLen)
			want := unhex(t, tc.want)
			if !bytes.Equal(got, want) {
				t.Errorf("\n  got:  %X\n  want: %X", got, want)
			}
		})
	}
}

// rfc7253IterativeTest implements the algorithm from RFC 7253 appendix A verbatim:
//
//	K = zeros(KEYLEN-8) || num2str(TAGLEN,8)
//	C = <empty string>
//	for i = 0 to 127 do
//	   S = zeros(8i)
//	   N = num2str(3i+1,96); C = C || OCB-ENCRYPT(K,N,S,S)
//	   N = num2str(3i+2,96); C = C || OCB-ENCRYPT(K,N,<empty>,S)
//	   N = num2str(3i+3,96); C = C || OCB-ENCRYPT(K,N,S,<empty>)
//	end for
//	N = num2str(385,96)
//	Output : OCB-ENCRYPT(K,N,C,<empty string>)
//
// Two details are easy to get wrong and both silently produce a plausible-looking
// wrong answer:
//
//   - The key is *not* all zeros. Its last byte is the tag length in bits, so the same
//     algorithm run at a different tag length also uses a different key.
//   - The RFC's argument order is (K, N, A, P) - associated data before plaintext -
//     the reverse of Go's Seal(dst, nonce, plaintext, additionalData).
func rfc7253IterativeTest(t *testing.T, keyLen, tagLen int) []byte {
	t.Helper()
	key := make([]byte, keyLen)
	key[keyLen-1] = byte(tagLen * 8) // num2str(TAGLEN, 8), TAGLEN in bits
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := NewWithSizes(block, 12, tagLen)
	if err != nil {
		t.Fatal(err)
	}

	// num2str(n, 96): a 96-bit big-endian encoding of n.
	nonce := func(n uint32) []byte {
		return []byte{0, 0, 0, 0, 0, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}

	var c []byte
	for i := uint32(0); i < 128; i++ {
		s := make([]byte, i) // zeros(8i) bits = i bytes
		c = append(c, aead.Seal(nil, nonce(3*i+1), s, s)...)
		c = append(c, aead.Seal(nil, nonce(3*i+2), s, nil)...)
		c = append(c, aead.Seal(nil, nonce(3*i+3), nil, s)...)
	}
	return aead.Seal(nil, nonce(385), nil, c)
}

// TestAES256 checks the key size borg actually uses. The RFC's vectors are AES-128, so
// without this the 256-bit path would rest entirely on the differential test.
func TestAES256RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := New(block)
	if err != nil {
		t.Fatal(err)
	}

	// Lengths chosen around the block boundary, where partial-block handling lives.
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 1000, 1024, 65536} {
		plain := make([]byte, n)
		for i := range plain {
			plain[i] = byte(i * 7)
		}
		ad := make([]byte, n%37)
		for i := range ad {
			ad[i] = byte(i * 11)
		}
		nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, byte(n)}

		sealed := aead.Seal(nil, nonce, plain, ad)
		if len(sealed) != n+16 {
			t.Errorf("n=%d: sealed length %d, want %d", n, len(sealed), n+16)
		}
		got, err := aead.Open(nil, nonce, sealed, ad)
		if err != nil {
			t.Fatalf("n=%d: Open: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("n=%d: round trip changed the plaintext", n)
		}
	}
}

// TestTamperingIsDetected is the property that matters most in a backup tool: a
// modified repository object must be rejected, not silently decrypted to garbage.
func TestTamperingIsDetected(t *testing.T) {
	key := make([]byte, 32)
	block, _ := aes.NewCipher(key)
	aead, err := New(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	plain := []byte("the quick brown fox jumps over the lazy dog, twice over")
	ad := []byte("associated data")
	sealed := aead.Seal(nil, nonce, plain, ad)

	// Every single-bit change anywhere in the envelope must fail.
	for i := 0; i < len(sealed); i++ {
		for _, bit := range []byte{0x01, 0x80} {
			bad := append([]byte(nil), sealed...)
			bad[i] ^= bit
			if _, err := aead.Open(nil, nonce, bad, ad); !errors.Is(err, ErrOpen) {
				t.Errorf("flipping bit %#x of byte %d was not detected (err=%v)", bit, i, err)
			}
		}
	}

	// So must a changed nonce, changed associated data, and truncation.
	t.Run("wrong nonce", func(t *testing.T) {
		other := append([]byte(nil), nonce...)
		other[11] ^= 1
		if _, err := aead.Open(nil, other, sealed, ad); !errors.Is(err, ErrOpen) {
			t.Errorf("wrong nonce accepted (err=%v)", err)
		}
	})
	t.Run("wrong ad", func(t *testing.T) {
		if _, err := aead.Open(nil, nonce, sealed, []byte("different")); !errors.Is(err, ErrOpen) {
			t.Errorf("wrong associated data accepted (err=%v)", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for n := 0; n < len(sealed); n++ {
			if _, err := aead.Open(nil, nonce, sealed[:n], ad); err == nil {
				t.Errorf("truncation to %d of %d bytes accepted", n, len(sealed))
			}
		}
	})
}

// TestOpenReleasesNoPlaintextOnFailure: an AEAD that hands back unauthenticated
// plaintext is a decryption oracle, which is the standard way OCB implementations get
// broken in practice.
func TestOpenReleasesNoPlaintextOnFailure(t *testing.T) {
	key := make([]byte, 32)
	block, _ := aes.NewCipher(key)
	aead, _ := New(block)
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	plain := bytes.Repeat([]byte("secret!!"), 8)
	sealed := aead.Seal(nil, nonce, plain, nil)

	bad := append([]byte(nil), sealed...)
	bad[len(bad)-1] ^= 0xff // corrupt the tag only, so decryption itself succeeds

	dst := make([]byte, 0, len(plain))
	got, err := aead.Open(dst, nonce, bad, nil)
	if err == nil {
		t.Fatal("corrupted tag accepted")
	}
	if got != nil {
		t.Errorf("Open returned %d bytes alongside an error", len(got))
	}
	// The scratch buffer must not still hold the plaintext.
	scratch := dst[:cap(dst)]
	if bytes.Contains(scratch, []byte("secret!!")) {
		t.Error("plaintext left in the caller's buffer after an authentication failure")
	}
}

func TestDoubleMatchesGF128(t *testing.T) {
	// Doubling is the one piece of arithmetic here, and the 0x87 reduction only fires
	// when the top bit is set - so a broken reduction passes most random tests.
	tests := []struct{ in, want string }{
		{"00000000000000000000000000000000", "00000000000000000000000000000000"},
		{"00000000000000000000000000000001", "00000000000000000000000000000002"},
		{"40000000000000000000000000000000", "80000000000000000000000000000000"},
		// Top bit set: shift left, then XOR 0x87 into the last byte.
		{"80000000000000000000000000000000", "00000000000000000000000000000087"},
		{"ffffffffffffffffffffffffffffffff", "ffffffffffffffffffffffffffffff79"},
	}
	for _, tc := range tests {
		var in [BlockSize]byte
		copy(in[:], unhex(t, tc.in))
		got := double(in)
		if !bytes.Equal(got[:], unhex(t, tc.want)) {
			t.Errorf("double(%s) = %X, want %s", tc.in, got, tc.want)
		}
	}
}

func TestRejectsBadParameters(t *testing.T) {
	block, _ := aes.NewCipher(make([]byte, 32))

	if _, err := NewWithSizes(block, 0, 16); err == nil {
		t.Error("accepted a zero-length nonce")
	}
	if _, err := NewWithSizes(block, 16, 16); err == nil {
		t.Error("accepted a 16-byte nonce (the maximum is 15)")
	}
	if _, err := NewWithSizes(block, 12, 17); err == nil {
		t.Error("accepted a 17-byte tag")
	}
	if _, err := NewWithSizes(block, 12, 0); err == nil {
		t.Error("accepted a zero-length tag")
	}

	aead, _ := New(block)
	defer func() {
		if recover() == nil {
			t.Error("Seal with a wrong-size nonce should panic, as the standard library's AEADs do")
		}
	}()
	aead.Seal(nil, []byte{1, 2, 3}, nil, nil)
}

func TestNonceSizesRoundTrip(t *testing.T) {
	block, _ := aes.NewCipher(make([]byte, 32))
	// The nonce length changes where the marker 1 bit sits, and `bottom` changes the
	// bit-level window into Stretch. Cover every legal length.
	for n := 1; n <= MaxNonceSize; n++ {
		aead, err := NewWithSizes(block, n, 16)
		if err != nil {
			t.Fatalf("nonce size %d: %v", n, err)
		}
		nonce := make([]byte, n)
		for i := range nonce {
			nonce[i] = byte(0xB0 + i)
		}
		plain := []byte("nonce length coverage")
		sealed := aead.Seal(nil, nonce, plain, []byte("ad"))
		got, err := aead.Open(nil, nonce, sealed, []byte("ad"))
		if err != nil {
			t.Fatalf("nonce size %d: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("nonce size %d: round trip changed the plaintext", n)
		}
	}
}

// TestBottomWindowCoverage forces every value of `bottom` (0..63), which selects the
// bit offset of the Offset_0 window into Stretch. This is the only non-byte-aligned
// step in OCB and the likeliest place for an off-by-one.
func TestBottomWindowCoverage(t *testing.T) {
	block, _ := aes.NewCipher(make([]byte, 32))
	aead, _ := New(block)
	seen := make(map[byte]bool)
	plain := []byte("bottom coverage")

	for i := 0; i < 256; i++ {
		nonce := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i)}
		seen[byte(i)&0x3f] = true
		sealed := aead.Seal(nil, nonce, plain, nil)
		got, err := aead.Open(nil, nonce, sealed, nil)
		if err != nil {
			t.Fatalf("bottom=%d: %v", i&0x3f, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("bottom=%d: round trip changed the plaintext", i&0x3f)
		}
	}
	if len(seen) != 64 {
		t.Errorf("covered %d of 64 bottom values", len(seen))
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("plaintext"), []byte("ad"), byte(0))
	f.Add([]byte(""), []byte(""), byte(17))
	f.Fuzz(func(t *testing.T, plain, ad []byte, nonceLast byte) {
		if len(plain) > 1<<20 {
			t.Skip()
		}
		block, _ := aes.NewCipher(make([]byte, 32))
		aead, _ := New(block)
		nonce := []byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 0, nonceLast}

		sealed := aead.Seal(nil, nonce, plain, ad)
		got, err := aead.Open(nil, nonce, sealed, ad)
		if err != nil {
			t.Fatalf("Open of own output failed: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("round trip changed the plaintext")
		}
	})
}

func BenchmarkSeal64K(b *testing.B) {
	block, _ := aes.NewCipher(make([]byte, 32))
	aead, _ := New(block)
	nonce := make([]byte, 12)
	plain := make([]byte, 64*1024)
	dst := make([]byte, 0, len(plain)+16)

	b.SetBytes(int64(len(plain)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = aead.Seal(dst[:0], nonce, plain, nil)
	}
}
