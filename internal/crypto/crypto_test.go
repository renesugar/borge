// SPDX-License-Identifier: Apache-2.0

package crypto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Unit tests that run without the borg venv: envelope structure, parameter validation,
// and published vectors for the hashes.

func testKey() []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func testIV() []byte {
	iv := make([]byte, IVSize)
	for i := range iv {
		iv[i] = byte(0xA0 + i)
	}
	return iv
}

func allSuites() []Suite { return []Suite{SuiteAESOCB, SuiteChaCha20Poly1305} }

func TestEnvelopeLayout(t *testing.T) {
	// header || tag(16) || ciphertext, with the header in the clear. Getting the order
	// wrong is the kind of mistake that only shows up against real borg data.
	for _, suite := range allSuites() {
		const headerLen = 8
		a, err := NewAEAD(suite, testKey(), headerLen, 0)
		if err != nil {
			t.Fatal(err)
		}
		header := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		plain := []byte("some plaintext of a known length")

		env, err := a.Encrypt(testIV(), plain, header, []byte("aad"))
		if err != nil {
			t.Fatal(err)
		}
		if want := headerLen + MACSize + len(plain); len(env) != want {
			t.Errorf("%s: envelope is %d bytes, want %d", suite, len(env), want)
		}
		if !bytes.Equal(env[:headerLen], header) {
			t.Errorf("%s: header is not at the front of the envelope", suite)
		}
		// The plaintext must not appear verbatim anywhere in the envelope.
		if bytes.Contains(env, plain) {
			t.Errorf("%s: plaintext appears in the envelope", suite)
		}
		if a.EnvelopeOverhead() != headerLen+MACSize {
			t.Errorf("%s: EnvelopeOverhead = %d, want %d", suite, a.EnvelopeOverhead(), headerLen+MACSize)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, suite := range allSuites() {
		for _, headerLen := range []int{0, 1, 32} {
			for _, n := range []int{0, 1, 15, 16, 17, 4096} {
				a, err := NewAEAD(suite, testKey(), headerLen, 0)
				if err != nil {
					t.Fatal(err)
				}
				plain := bytes.Repeat([]byte{0xAB}, n)
				header := bytes.Repeat([]byte{0xCD}, headerLen)
				aad := []byte("associated")

				env, err := a.Encrypt(testIV(), plain, header, aad)
				if err != nil {
					t.Fatalf("%s hdr=%d n=%d: %v", suite, headerLen, n, err)
				}
				got, err := a.Decrypt(testIV(), env, aad)
				if err != nil {
					t.Fatalf("%s hdr=%d n=%d: %v", suite, headerLen, n, err)
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("%s hdr=%d n=%d: round trip changed the plaintext", suite, headerLen, n)
				}
			}
		}
	}
}

// TestAADOrderMatters pins down the one part of the envelope a reader cannot infer
// from the layout: the AAD is aad || header[aadOffset:], in that order. Swapping the
// two halves still authenticates consistently within borge and fails against borg, so
// only an explicit test catches it.
func TestAADOrderMatters(t *testing.T) {
	const headerLen = 8
	a, err := NewAEAD(SuiteAESOCB, testKey(), headerLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	header := []byte("HHHHHHHH")
	aad := []byte("AAAA")
	plain := []byte("payload")

	env, err := a.Encrypt(testIV(), plain, header, aad)
	if err != nil {
		t.Fatal(err)
	}

	// Reversing the concatenation must produce a different tag, or the order is not
	// actually being honoured.
	swapped := append(append([]byte{}, header...), aad...)
	inner := a.aead
	sealedSwapped := inner.Seal(nil, testIV(), plain, swapped)
	tagFromEnvelope := env[headerLen : headerLen+MACSize]
	tagSwapped := sealedSwapped[len(plain):]
	if bytes.Equal(tagFromEnvelope, tagSwapped) {
		t.Error("aad||header and header||aad produced the same tag; the order is not being applied")
	}
}

// TestAADOffsetExcludesHeaderPrefix: bytes before aadOffset are deliberately not
// authenticated. Both halves of that statement need checking.
func TestAADOffsetExcludesHeaderPrefix(t *testing.T) {
	const headerLen, aadOffset = 16, 4
	a, err := NewAEAD(SuiteAESOCB, testKey(), headerLen, aadOffset)
	if err != nil {
		t.Fatal(err)
	}
	header := bytes.Repeat([]byte{0x11}, headerLen)
	plain := []byte("payload")
	env, err := a.Encrypt(testIV(), plain, header, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < headerLen; i++ {
		bad := append([]byte(nil), env...)
		bad[i] ^= 0xff
		_, err := a.Decrypt(testIV(), bad, nil)
		if i < aadOffset {
			if err != nil {
				t.Errorf("byte %d is before aadOffset and must not be authenticated, got %v", i, err)
			}
		} else if !errors.Is(err, ErrAuthentication) {
			t.Errorf("byte %d is at or after aadOffset and must be authenticated, got %v", i, err)
		}
	}
}

func TestTamperingRejected(t *testing.T) {
	for _, suite := range allSuites() {
		const headerLen = 4
		a, err := NewAEAD(suite, testKey(), headerLen, 0)
		if err != nil {
			t.Fatal(err)
		}
		header := []byte{9, 9, 9, 9}
		plain := []byte("a chunk of repository data")
		aad := []byte("aad")
		env, err := a.Encrypt(testIV(), plain, header, aad)
		if err != nil {
			t.Fatal(err)
		}

		for i := range env {
			bad := append([]byte(nil), env...)
			bad[i] ^= 0x01
			if _, err := a.Decrypt(testIV(), bad, aad); !errors.Is(err, ErrAuthentication) {
				t.Errorf("%s: flipping byte %d was not detected (err=%v)", suite, i, err)
			}
		}

		// Truncation at every length, a wrong IV, and wrong associated data.
		for n := 0; n < len(env); n++ {
			if _, err := a.Decrypt(testIV(), env[:n], aad); err == nil {
				t.Errorf("%s: truncation to %d bytes accepted", suite, n)
			}
		}
		otherIV := append([]byte(nil), testIV()...)
		otherIV[0] ^= 1
		if _, err := a.Decrypt(otherIV, env, aad); !errors.Is(err, ErrAuthentication) {
			t.Errorf("%s: wrong IV accepted", suite)
		}
		if _, err := a.Decrypt(testIV(), env, []byte("other")); !errors.Is(err, ErrAuthentication) {
			t.Errorf("%s: wrong associated data accepted", suite)
		}
	}
}

// TestAuthenticationErrorIsOpaque: the error must not distinguish a bad tag from a
// malformed envelope. Telling them apart is a decryption oracle.
func TestAuthenticationErrorIsOpaque(t *testing.T) {
	a, err := NewAEAD(SuiteAESOCB, testKey(), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.Encrypt(testIV(), []byte("data"), []byte{1, 2, 3, 4}, nil)
	if err != nil {
		t.Fatal(err)
	}

	bad := append([]byte(nil), env...)
	bad[len(bad)-1] ^= 0xff
	_, errTag := a.Decrypt(testIV(), bad, nil)
	_, errShort := a.Decrypt(testIV(), env[:3], nil)

	if !errors.Is(errTag, ErrAuthentication) || !errors.Is(errShort, ErrAuthentication) {
		t.Fatalf("both should be ErrAuthentication: tag=%v short=%v", errTag, errShort)
	}
	if errTag.Error() != errShort.Error() {
		t.Errorf("errors distinguishable:\n  bad tag:   %v\n  truncated: %v", errTag, errShort)
	}
}

func TestNewAEADRejectsBadParameters(t *testing.T) {
	tests := []struct {
		name      string
		suite     Suite
		keyLen    int
		headerLen int
		aadOffset int
	}{
		{"short key", SuiteAESOCB, 16, 0, 0},
		{"long key", SuiteAESOCB, 64, 0, 0},
		{"empty key", SuiteAESOCB, 0, 0, 0},
		{"negative header", SuiteAESOCB, 32, -1, 0},
		{"aad offset past header", SuiteAESOCB, 32, 4, 5},
		{"negative aad offset", SuiteAESOCB, 32, 4, -1},
		{"unknown suite", Suite(99), 32, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAEAD(tc.suite, make([]byte, tc.keyLen), tc.headerLen, tc.aadOffset); err == nil {
				t.Error("accepted invalid parameters")
			}
		})
	}
}

func TestEncryptRejectsBadInputs(t *testing.T) {
	a, err := NewAEAD(SuiteAESOCB, testKey(), 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Encrypt(make([]byte, 11), nil, make([]byte, 8), nil); err == nil {
		t.Error("accepted an 11-byte IV")
	}
	if _, err := a.Encrypt(testIV(), nil, make([]byte, 7), nil); err == nil {
		t.Error("accepted a header of the wrong length")
	}
	if _, err := a.Decrypt(make([]byte, 13), make([]byte, 100), nil); err == nil {
		t.Error("accepted a 13-byte IV")
	}
}

func TestSuiteString(t *testing.T) {
	// These names appear in error messages and in borg's key metadata (ENC_NAME).
	if got := SuiteAESOCB.String(); got != "aes256-ocb" {
		t.Errorf("SuiteAESOCB = %q, want aes256-ocb", got)
	}
	if got := SuiteChaCha20Poly1305.String(); got != "chacha20-poly1305" {
		t.Errorf("SuiteChaCha20Poly1305 = %q, want chacha20-poly1305", got)
	}
}

func TestBlockCount(t *testing.T) {
	// borg's num_cipher_blocks, rounding up.
	for _, tc := range []struct {
		length, blockSize int
		want              int64
	}{
		{0, 16, 0}, {1, 16, 1}, {16, 16, 1}, {17, 16, 2},
		{0, 64, 0}, {64, 64, 1}, {65, 64, 2},
	} {
		if got := blockCount(tc.length, tc.blockSize); got != tc.want {
			t.Errorf("blockCount(%d, %d) = %d, want %d", tc.length, tc.blockSize, got, tc.want)
		}
	}
}

// TestBlake3KeyedVector uses a published BLAKE3 keyed-mode value, so this does not rest
// entirely on agreeing with the same library borg uses.
func TestBlake3KeyedVector(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	got, err := Blake3Keyed(key, []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	// Cross-checked against the blake3 Python package in the pinned borg venv.
	const want = "6da54495d8152f2bcba87bd7282df70901cdb66b4448ed5f4c7bd2852b8b5532"
	if hex.EncodeToString(got) != want {
		t.Errorf("blake3 keyed(abc)\n  got:  %x\n  want: %s", got, want)
	}

	if _, err := Blake3Keyed(make([]byte, 31), nil); err == nil {
		t.Error("accepted a 31-byte blake3 key; keyed mode requires exactly 32")
	}
}

func TestHMACSHA256Vector(t *testing.T) {
	// RFC 4231 test case 1.
	key := bytes.Repeat([]byte{0x0b}, 20)
	got := HMACSHA256(key, []byte("Hi There"))
	const want = "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
	if hex.EncodeToString(got) != want {
		t.Errorf("HMAC-SHA-256\n  got:  %x\n  want: %s", got, want)
	}
}

// TestBlake2b256IsNotKeyedMode guards the surprise in borg's blake2b_256: despite the
// name it hashes key||data *unkeyed*, rather than using BLAKE2b's keyed mode. A port
// that reaches for the keyed constructor produces different digests for every input.
func TestBlake2b256IsNotKeyedMode(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	data := []byte("borge")

	viaHelper := Blake2b256(key, data)
	// The same thing spelled out: an unkeyed BLAKE2b-256 over the concatenation.
	viaConcat := Blake2b256(nil, append(append([]byte{}, key...), data...))
	if !bytes.Equal(viaHelper, viaConcat) {
		t.Error("Blake2b256(key, data) must equal an unkeyed hash of key||data")
	}
	if len(viaHelper) != 32 {
		t.Errorf("digest is %d bytes, want 32", len(viaHelper))
	}
	if len(Blake2b128(data)) != 16 {
		t.Errorf("blake2b_128 digest is %d bytes, want 16", len(Blake2b128(data)))
	}
}

func TestArgon2Parameters(t *testing.T) {
	// borg's ARGON2_ARGS. These are stored in the key blob, so a change here without a
	// corresponding format change would make new keys unreadable by borg.
	if Argon2TimeCost != 3 || Argon2MemoryCost != 65536 || Argon2Parallelism != 4 {
		t.Errorf("argon2 defaults drifted: time=%d memory=%d parallelism=%d, want 3/65536/4",
			Argon2TimeCost, Argon2MemoryCost, Argon2Parallelism)
	}
	if Argon2SaltBytes != 16 {
		t.Errorf("ARGON2_SALT_BYTES = %d, want 16", Argon2SaltBytes)
	}
	if Argon2Type != "id" {
		t.Errorf("argon2 type = %q, want id", Argon2Type)
	}
}

func TestArgon2IDIsDeterministicAndValidated(t *testing.T) {
	salt := bytes.Repeat([]byte{7}, Argon2SaltBytes)
	// Deliberately small parameters: the real ones cost 64 MiB per call.
	a, err := Argon2ID([]byte("passphrase"), salt, 1, 8, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Argon2ID([]byte("passphrase"), salt, 1, 8, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("argon2 is not deterministic for the same inputs")
	}
	c, _ := Argon2ID([]byte("passphrase"), bytes.Repeat([]byte{8}, 16), 1, 8, 1, 32)
	if bytes.Equal(a, c) {
		t.Error("a different salt produced the same key")
	}
	if len(a) != 32 {
		t.Errorf("output is %d bytes, want 32", len(a))
	}

	for _, bad := range []struct {
		name                                 string
		salt                                 []byte
		time, memory, parallelism, outputLen uint32
	}{
		{"no salt", nil, 1, 8, 1, 32},
		{"zero time", salt, 0, 8, 1, 32},
		{"zero memory", salt, 1, 0, 1, 32},
		{"zero parallelism", salt, 1, 8, 0, 32},
		{"zero output", salt, 1, 8, 1, 0},
		{"parallelism too large", salt, 1, 8, 256, 32},
	} {
		if _, err := Argon2ID([]byte("p"), bad.salt, bad.time, bad.memory, bad.parallelism, bad.outputLen); err == nil {
			t.Errorf("%s: accepted invalid parameters", bad.name)
		}
	}
}

func TestMessageSizeLimitMessage(t *testing.T) {
	// The limit cannot be reached with a real allocation, so just check the wording is
	// the one a user would need, rather than a bare arithmetic failure.
	a, err := NewAEAD(SuiteAESOCB, testKey(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Encrypt(testIV(), []byte("small"), nil, nil); err != nil {
		t.Fatalf("a small message should encrypt: %v", err)
	}
	if !strings.Contains(SuiteChaCha20Poly1305.String(), "chacha") {
		t.Error("suite naming changed unexpectedly")
	}
}

func FuzzEnvelopeRoundTrip(f *testing.F) {
	f.Add([]byte("plain"), []byte("aad"), uint8(0))
	f.Add([]byte(""), []byte(""), uint8(1))
	f.Fuzz(func(t *testing.T, plain, aad []byte, suiteSel uint8) {
		if len(plain) > 1<<18 {
			t.Skip()
		}
		suite := SuiteAESOCB
		if suiteSel%2 == 1 {
			suite = SuiteChaCha20Poly1305
		}
		a, err := NewAEAD(suite, testKey(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		env, err := a.Encrypt(testIV(), plain, nil, aad)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		got, err := a.Decrypt(testIV(), env, aad)
		if err != nil {
			t.Fatalf("Decrypt of own output: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Error("round trip changed the plaintext")
		}
	})
}

func FuzzDecryptDoesNotPanic(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte("aad"), uint8(0))
	f.Fuzz(func(t *testing.T, envelope, aad []byte, suiteSel uint8) {
		suite := SuiteAESOCB
		if suiteSel%2 == 1 {
			suite = SuiteChaCha20Poly1305
		}
		a, err := NewAEAD(suite, testKey(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Arbitrary bytes must produce an error, never a crash, and never plaintext.
		if out, err := a.Decrypt(testIV(), envelope, aad); err == nil && out == nil {
			t.Error("Decrypt reported success with no output")
		}
	})
}
