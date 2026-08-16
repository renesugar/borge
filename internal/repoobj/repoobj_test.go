// SPDX-License-Identifier: Apache-2.0

package repoobj

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
)

// Unit tests that run without the borg venv: the header, the AAD's slot binding, the
// BORG_ASSERT_ID policy, and the failure modes.

func testRepoObj(t *testing.T, mode string) (*RepoObj, key.Key) {
	t.Helper()
	k, err := key.ByName(mode, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	return r, k
}

func TestHeaderLayout(t *testing.T) {
	r, k := testRepoObj(t, "none-sha256")
	data := []byte("payload")
	id := k.IDHash(data)

	obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}

	if len(obj) < HeaderSize {
		t.Fatalf("object is %d bytes, shorter than the %d byte header", len(obj), HeaderSize)
	}
	if string(obj[:8]) != Magic {
		t.Errorf("magic = %q, want %q", obj[:8], Magic)
	}
	if obj[8] != VersionHeaderAAD {
		t.Errorf("version = %d, want %d (borge must never write version 1)", obj[8], VersionHeaderAAD)
	}
	if !bytes.Equal(obj[9:41], id) {
		t.Error("the chunk id is not at offset 9")
	}
	metaSize := binary.LittleEndian.Uint32(obj[41:45])
	dataSize := binary.LittleEndian.Uint32(obj[45:49])
	if want := HeaderSize + int(metaSize) + int(dataSize); want != len(obj) {
		t.Errorf("header sizes say %d bytes, object is %d", want, len(obj))
	}

	// ParseHeader must report the same thing without decrypting anything.
	gotID, gotSize, err := ParseHeader(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotID, id) || gotSize != len(obj) {
		t.Errorf("ParseHeader = (%x, %d), want (%x, %d)", gotID, gotSize, id, len(obj))
	}
	// It must work from a header-sized prefix alone, which is what a pack walk has.
	if _, _, err := ParseHeader(obj[:HeaderSize]); err != nil {
		t.Errorf("ParseHeader on a header-only prefix: %v", err)
	}
}

// TestSlotTagsBindCiphertextToSlot is the property the "M"/"D" tags exist for. With a
// mode whose two slots happen to be the same length, swapping them must fail - without
// the slot tag in the AAD the two ciphertexts would be interchangeable.
func TestSlotTagsBindCiphertextToSlot(t *testing.T) {
	r, k := testRepoObj(t, "authenticated-sha256")

	// Choose a payload whose encrypted length equals the encrypted metadata length, so
	// a swap produces a structurally valid object.
	var obj []byte
	var id []byte
	for n := 1; n < 200; n++ {
		data := bytes.Repeat([]byte{0x41}, n)
		id = k.IDHash(data)
		candidate, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
		if err != nil {
			t.Fatal(err)
		}
		metaSize := binary.LittleEndian.Uint32(candidate[41:45])
		dataSize := binary.LittleEndian.Uint32(candidate[45:49])
		if metaSize == dataSize {
			obj = candidate
			break
		}
	}
	if obj == nil {
		t.Skip("could not construct an object whose two slots are the same length")
	}

	metaSize := int(binary.LittleEndian.Uint32(obj[41:45]))
	swapped := append([]byte(nil), obj...)
	copy(swapped[HeaderSize:HeaderSize+metaSize], obj[HeaderSize+metaSize:])
	copy(swapped[HeaderSize+metaSize:], obj[HeaderSize:HeaderSize+metaSize])

	if _, _, err := r.Parse(id, swapped, TypeFileStream, ParseOptions{}); err == nil {
		t.Error("swapping the metadata and data slots was accepted; the slot tags are not being applied")
	}
}

// TestAADBindsObjectToItsID: moving a slot to another object must fail, because the
// chunk id is in the AAD.
func TestAADBindsObjectToItsID(t *testing.T) {
	r, k := testRepoObj(t, "authenticated-sha256")

	dataA := []byte("first object")
	idA := k.IDHash(dataA)
	objA, err := r.Format(idA, &Meta{Type: TypeFileStream}, dataA)
	if err != nil {
		t.Fatal(err)
	}

	// Parsing object A under a different id must fail even though the bytes are intact.
	idB := k.IDHash([]byte("second object"))
	if _, _, err := r.Parse(idB, objA, TypeFileStream, ParseOptions{}); err == nil {
		t.Error("an object parsed successfully under the wrong chunk id")
	}
}

func TestTamperingIsDetected(t *testing.T) {
	for _, mode := range []string{"none-sha256", "authenticated-sha256"} {
		r, k := testRepoObj(t, mode)
		data := bytes.Repeat([]byte("tamper me "), 20)
		id := k.IDHash(data)
		obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
		if err != nil {
			t.Fatal(err)
		}

		for i := range obj {
			bad := append([]byte(nil), obj...)
			bad[i] ^= 0x01
			if _, _, err := r.Parse(id, bad, TypeFileStream, ParseOptions{}); err == nil {
				t.Errorf("%s: flipping byte %d was not detected", mode, i)
			}
		}
		for n := 0; n < len(obj); n++ {
			if _, _, err := r.Parse(id, obj[:n], TypeFileStream, ParseOptions{}); err == nil {
				t.Errorf("%s: truncation to %d bytes was accepted", mode, n)
			}
		}
	}
}

// TestUnkeyedModeAlwaysVerifiesID: for a mode whose envelope checksum anyone can
// recompute, the id check *is* the read-path integrity check and must never be skipped.
func TestUnkeyedModeAlwaysVerifiesID(t *testing.T) {
	r, k := testRepoObj(t, "none-sha256")
	if !k.IDCheckIsAuthentication() {
		t.Fatal("none-sha256 should report that its id check is the authentication")
	}
	// Whatever place a read is attributed to, verification happens.
	for _, place := range []string{PlaceRead, PlaceRepair, PlaceTransfer, PlaceRechunk, PlaceVerifyData} {
		if !r.shouldAssertID(place) {
			t.Errorf("an unkeyed mode skipped the id check at place %q", place)
		}
	}
}

// TestKeyedModeFollowsAssertIDPolicy: for a keyed mode the envelope already
// authenticates the payload for that chunk id, so the id check is an optional extra
// hash pass - and skipping it on the hot read path is a deliberate performance choice.
func TestKeyedModeFollowsAssertIDPolicy(t *testing.T) {
	r, k := testRepoObj(t, "authenticated-sha256")
	if k.IDCheckIsAuthentication() {
		t.Fatal("a keyed mode should not report its id check as the authentication")
	}

	if r.shouldAssertID(PlaceRead) {
		t.Error("the default policy should not verify on the general read path")
	}
	for _, place := range []string{PlaceRepair, PlaceTransfer, PlaceRechunk} {
		if !r.shouldAssertID(place) {
			t.Errorf("the default policy should verify at %q", place)
		}
	}
	if !r.shouldAssertID(PlaceVerifyData) {
		t.Error("verify_data must always verify; it is the audit the other choices rest on")
	}
}

func TestAssertIDPlaceFromEnv(t *testing.T) {
	t.Setenv("BORG_ASSERT_ID", "read,repair")
	places, err := assertIDPlacesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !places[PlaceRead] || !places[PlaceRepair] {
		t.Errorf("configured places not honoured: %v", places)
	}
	if places[PlaceTransfer] {
		t.Error("an unconfigured place was enabled")
	}

	// borge's own variable wins over borg's (docs/PORTING_PLAN.md §0.5).
	t.Setenv("BORGE_ASSERT_ID", "transfer")
	places, err = assertIDPlacesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if places[PlaceRead] || !places[PlaceTransfer] {
		t.Errorf("BORGE_ASSERT_ID did not take precedence: %v", places)
	}

	// verify_data cannot be configured, and an unknown place is an error rather than
	// being silently ignored.
	t.Setenv("BORGE_ASSERT_ID", "verify_data")
	if _, err := assertIDPlacesFromEnv(); err == nil {
		t.Error("verify_data was accepted as a configurable place")
	}
	t.Setenv("BORGE_ASSERT_ID", "nonsense")
	if _, err := assertIDPlacesFromEnv(); err == nil {
		t.Error("an unknown place was accepted")
	}
}

// TestWrongIDIsDetected: a chunk whose plaintext does not hash to its id is a chunk an
// evil or broken writer produced, and the audit path must catch it.
func TestWrongIDIsDetected(t *testing.T) {
	r, k := testRepoObj(t, "authenticated-sha256")
	data := []byte("content")
	wrongID := k.IDHash([]byte("something else"))

	obj, err := r.Format(wrongID, &Meta{Type: TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}
	// The envelope itself is valid, so the read path accepts it by default.
	if _, _, err := r.Parse(wrongID, obj, TypeFileStream, ParseOptions{}); err != nil {
		t.Fatalf("the envelope should verify: %v", err)
	}
	// verify_data must not.
	_, _, err = r.Parse(wrongID, obj, TypeFileStream, ParseOptions{AssertIDPlace: PlaceVerifyData})
	if err == nil {
		t.Error("verify_data accepted a chunk whose plaintext does not match its id")
	} else if !errors.Is(err, key.ErrIntegrity) {
		t.Errorf("unexpected error type: %v", err)
	}
}

func TestManifestIDIsNotVerified(t *testing.T) {
	// The manifest's id is 32 zero bytes, not a hash of anything, so verifying it would
	// fail every time.
	r, k := testRepoObj(t, "authenticated-sha256")
	data := []byte("manifest contents")
	obj, err := r.Format(key.ManifestID, &Meta{Type: TypeManifest}, data)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := r.Parse(key.ManifestID, obj, TypeManifest,
		ParseOptions{AssertIDPlace: PlaceVerifyData})
	if err != nil {
		t.Fatalf("the manifest failed its id check; it must be exempt: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("manifest payload differs")
	}
	_ = k
}

func TestRoundTripAllModesAndCompressors(t *testing.T) {
	payloads := map[string][]byte{
		"empty": {},
		"small": []byte("x"),
		"text":  bytes.Repeat([]byte("compress me "), 500),
		"random": func() []byte {
			b := make([]byte, 4096)
			for i := range b {
				b[i] = byte(i * 7)
			}
			return b
		}(),
	}
	for _, mode := range []string{"none-sha256", "none-blake3", "authenticated-sha256", "authenticated-blake3"} {
		for _, spec := range []string{"none", "lz4", "zstd,3", "zlib,6", "lzma,6", "obfuscate,110,lz4", "auto,zstd,3"} {
			r, k := testRepoObj(t, mode)
			c, err := compress.FromSpec(spec)
			if err != nil {
				t.Fatal(err)
			}
			r.SetCompressor(c)

			for name, data := range payloads {
				// borg's Auto crashes on empty input (docs/DIVERGENCES.md §2); borge
				// handles it, but there is nothing to compare against.
				id := k.IDHash(data)
				obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
				if err != nil {
					t.Fatalf("%s/%s/%s: format: %v", mode, spec, name, err)
				}
				_, got, err := r.Parse(id, obj, TypeFileStream, ParseOptions{})
				if err != nil {
					t.Fatalf("%s/%s/%s: parse: %v", mode, spec, name, err)
				}
				if !bytes.Equal(got, data) {
					t.Errorf("%s/%s/%s: round trip changed the payload", mode, spec, name)
				}
			}
		}
	}
}

func TestFormatRejectsBadInput(t *testing.T) {
	r, k := testRepoObj(t, "none-sha256")
	data := []byte("x")
	id := k.IDHash(data)

	if _, err := r.Format(make([]byte, 31), &Meta{Type: TypeFileStream}, data); err == nil {
		t.Error("a 31-byte chunk id was accepted")
	}
	if _, err := r.Format(id, &Meta{}, data); err == nil {
		t.Error("an object with no type was accepted")
	}
	if _, err := r.Format(id, &Meta{Type: TypeDontCare}, data); err == nil {
		t.Error("the wildcard type was accepted for writing")
	}
	if _, err := r.Format(id, &Meta{Type: TypeFileStream}, make([]byte, MaxDataSize+1)); err == nil {
		t.Errorf("a payload over the %d byte maximum was accepted", MaxDataSize)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	r, k := testRepoObj(t, "none-sha256")
	data := []byte("payload")
	id := k.IDHash(data)
	obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), obj...)
		bad[0] = 'X'
		if _, _, err := r.Parse(id, bad, TypeFileStream, ParseOptions{}); !errors.Is(err, ErrIntegrity) {
			t.Errorf("got %v, want an integrity error", err)
		}
	})
	t.Run("unsupported version", func(t *testing.T) {
		bad := append([]byte(nil), obj...)
		bad[8] = 0x03
		if _, _, err := r.Parse(id, bad, TypeFileStream, ParseOptions{}); !errors.Is(err, ErrIntegrity) {
			t.Errorf("got %v, want an integrity error", err)
		}
	})
	t.Run("size fields disagree with the object", func(t *testing.T) {
		bad := append([]byte(nil), obj...)
		binary.LittleEndian.PutUint32(bad[45:49], 0xFFFF)
		if _, _, err := r.Parse(id, bad, TypeFileStream, ParseOptions{}); !errors.Is(err, ErrIntegrity) {
			t.Errorf("got %v, want an integrity error", err)
		}
	})
	t.Run("trailing bytes", func(t *testing.T) {
		bad := append(append([]byte(nil), obj...), 0x00)
		if _, _, err := r.Parse(id, bad, TypeFileStream, ParseOptions{}); !errors.Is(err, ErrIntegrity) {
			t.Errorf("got %v, want an integrity error", err)
		}
	})
	t.Run("invalid options", func(t *testing.T) {
		if _, _, err := r.Parse(id, obj, TypeFileStream, ParseOptions{SkipDecompress: true}); err == nil {
			t.Error("SkipDecompress without WantCompressed should be refused")
		}
	})
}

func TestParseMetaFromPrefix(t *testing.T) {
	// The metadata can be read from a prefix of the object, which is what makes a
	// short range read enough to learn its type and size.
	r, k := testRepoObj(t, "none-sha256")
	data := bytes.Repeat([]byte{0x5A}, 10000)
	id := k.IDHash(data)
	obj, err := r.Format(id, &Meta{Type: TypeArchiveStream}, data)
	if err != nil {
		t.Fatal(err)
	}

	metaSize := int(binary.LittleEndian.Uint32(obj[41:45]))
	prefix := obj[:HeaderSize+metaSize]

	meta, err := r.ParseMeta(id, prefix, TypeArchiveStream)
	if err != nil {
		t.Fatalf("ParseMeta on a prefix: %v", err)
	}
	if meta.Type != TypeArchiveStream {
		t.Errorf("type = %q", meta.Type)
	}
	if !meta.SizeSet || meta.Size != len(data) {
		t.Errorf("size = %d (set=%v), want %d", meta.Size, meta.SizeSet, len(data))
	}

	// One byte short must fail rather than decrypt something partial.
	if _, err := r.ParseMeta(id, obj[:HeaderSize+metaSize-1], TypeArchiveStream); err == nil {
		t.Error("ParseMeta accepted a prefix that does not cover the metadata slot")
	}
}

func TestExtractCryptedData(t *testing.T) {
	r, k := testRepoObj(t, "none-sha256")
	data := []byte("payload")
	id := k.IDHash(data)
	obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}
	crypted, err := ExtractCryptedData(obj)
	if err != nil {
		t.Fatal(err)
	}
	// The first byte is the key type, which is how borg detects a repository's mode
	// before it has a key.
	if len(crypted) == 0 || crypted[0] != k.Type() {
		t.Errorf("crypted data starts with %v, want the key type byte %#x", crypted[:1], k.Type())
	}
}

func TestWantCompressed(t *testing.T) {
	// A caller that means to re-store a chunk unchanged asks for the compressed payload
	// back, so it does not decompress and recompress it.
	r, k := testRepoObj(t, "none-sha256")
	c, _ := compress.FromSpec("zstd,3")
	r.SetCompressor(c)

	data := bytes.Repeat([]byte("compressible "), 500)
	id := k.IDHash(data)
	obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}

	meta, compressed, err := r.Parse(id, obj, TypeFileStream, ParseOptions{WantCompressed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) >= len(data) {
		t.Errorf("the payload does not look compressed: %d bytes from %d", len(compressed), len(data))
	}
	if meta.CSize != len(compressed) {
		t.Errorf("csize = %d, payload is %d bytes", meta.CSize, len(compressed))
	}

	// And with decompression skipped entirely, which also skips the id check.
	_, quick, err := r.Parse(id, obj, TypeFileStream,
		ParseOptions{WantCompressed: true, SkipDecompress: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(quick, compressed) {
		t.Error("the quick path returned different bytes")
	}
}

func TestKeyTypeNames(t *testing.T) {
	// Error messages have to be able to name what was found, including the modes borge
	// does not implement.
	for typeByte, want := range map[byte]string{
		key.TypeSHA256None:                 "none-sha256",
		key.TypeBlake3Authenticated:        "authenticated-blake3",
		key.TypeAESOCB:                     "aes256-ocb",
		key.TypeDroppedBlake3Authenticated: "a dropped borg 2 beta format (reserved, never valid)",
	} {
		if got := key.TypeName(typeByte); got != want {
			t.Errorf("TypeName(%#x) = %q, want %q", typeByte, got, want)
		}
	}
	if got := key.TypeName(key.TypeKeyfile); got == "" {
		t.Error("a borg 1.x type should still be named")
	}
}

func TestAEADModesAreRefusedWithAnExplanation(t *testing.T) {
	for _, mode := range []string{"aes256-ocb", "chacha20-poly1305"} {
		_, err := key.ByName(mode, make([]byte, 32), make([]byte, 32))
		if err == nil {
			t.Errorf("%s reported success but is not implemented yet", mode)
		}
	}
}
