// SPDX-License-Identifier: Apache-2.0

package item

import (
	"bytes"
	"testing"

	"github.com/renesugar/borge/internal/msgpackx"
)

// Helpers shared with path_test.go for building raw maps by hand.
func newRawMap() *msgpackx.Map                   { return msgpackx.NewStableMap() }
func marshalRaw(m *msgpackx.Map) ([]byte, error) { return msgpackx.Marshal(m) }
func timestampFor(ns int64) msgpackx.Timestamp   { return msgpackx.TimestampFromUnixNano(ns) }

func TestRequiredKeys(t *testing.T) {
	// REQUIRED_ITEM_KEYS is {path, mtime}. Decoding without either must fail rather
	// than yield an item with a zero path or a 1970 timestamp.
	t.Run("missing mtime", func(t *testing.T) {
		m := newRawMap()
		m.Set("path", "a")
		b, _ := marshalRaw(m)
		if _, err := UnmarshalItem(b); err == nil {
			t.Error("decoded an item with no mtime")
		}
	})
	t.Run("missing path", func(t *testing.T) {
		m := newRawMap()
		m.Set("mtime", timestampFor(0))
		b, _ := marshalRaw(m)
		if _, err := UnmarshalItem(b); err == nil {
			t.Error("decoded an item with no path")
		}
	})
}

// TestAbsentIsNotZero is the reason every optional field is a pointer: writing mode 0
// and not writing mode at all are different on the wire, and conflating them would
// give every item a mode of 0000 on the first rewrite.
func TestAbsentIsNotZero(t *testing.T) {
	withMode := &Item{Path: "a", MTime: OptInt(0), Mode: OptInt(0)}
	withoutMode := &Item{Path: "a", MTime: OptInt(0)}

	a, err := withMode.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := withoutMode.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("an item with mode 0 encoded the same as one with no mode at all")
	}

	back, err := UnmarshalItem(a)
	if err != nil {
		t.Fatal(err)
	}
	if back.Mode == nil || *back.Mode != 0 {
		t.Errorf("mode 0 did not survive the round trip: %v", back.Mode)
	}
	back, err = UnmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Mode != nil {
		t.Errorf("an absent mode came back as %d", *back.Mode)
	}
}

// TestEmptyChunkListIsNotAbsent: an empty file has a chunk list of length zero, which
// is not the same as a symlink having no chunk list at all.
func TestEmptyChunkListIsNotAbsent(t *testing.T) {
	empty := &Item{Path: "empty", MTime: OptInt(0), ChunksSet: true, Chunks: nil}
	none := &Item{Path: "empty", MTime: OptInt(0)}

	a, err := empty.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := none.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("an empty chunk list encoded the same as no chunk list")
	}

	back, err := UnmarshalItem(a)
	if err != nil {
		t.Fatal(err)
	}
	if !back.ChunksSet || len(back.Chunks) != 0 {
		t.Errorf("empty chunk list did not survive: set=%v len=%d", back.ChunksSet, len(back.Chunks))
	}
}

// TestUnknownKeysArePreserved: dropping a key written by a newer borg would silently
// strip metadata on any rewrite. Losing data while reporting success is the worst
// available failure mode.
func TestUnknownKeysArePreserved(t *testing.T) {
	m := newRawMap()
	m.Set("path", "a")
	m.Set("mtime", timestampFor(42))
	m.Set("some_future_key", "a value borge does not know")
	m.Set("another_one", int64(7))
	original, err := marshalRaw(m)
	if err != nil {
		t.Fatal(err)
	}

	it, err := UnmarshalItem(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Unknown) != 2 {
		t.Fatalf("kept %d unknown keys, want 2", len(it.Unknown))
	}

	again, err := it.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, original) {
		t.Errorf("unknown keys did not survive the round trip\n  in:  %x\n  out: %x", original, again)
	}
}

// TestChunkListDropsLegacyCompressedSize: borg 1.x wrote (id, size, csize); borg reads
// only the first two, so a re-encode must produce two-element entries.
func TestChunkListDropsLegacyCompressedSize(t *testing.T) {
	m := newRawMap()
	m.Set("path", "a")
	m.Set("mtime", timestampFor(0))
	m.Set("chunks", []any{
		[]any{bytes.Repeat([]byte{1}, 32), int64(100), int64(50)},
	})
	b, err := marshalRaw(m)
	if err != nil {
		t.Fatal(err)
	}

	it, err := UnmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Chunks) != 1 || it.Chunks[0].Size != 100 {
		t.Fatalf("chunk list decoded wrong: %+v", it.Chunks)
	}

	out, err := it.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := UnmarshalItem(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Chunks) != 1 || again.Chunks[0].Size != 100 {
		t.Errorf("chunk list did not survive: %+v", again.Chunks)
	}
	if bytes.Equal(out, b) {
		t.Error("the legacy compressed size was kept; borg drops it on read")
	}
}

// TestNoneUserGroupIsDropped: borg 1 stored "not known" as None; borg 2's policy is to
// omit the key entirely rather than store an empty string.
func TestNoneUserGroupIsDropped(t *testing.T) {
	m := newRawMap()
	m.Set("path", "a")
	m.Set("mtime", timestampFor(0))
	m.Set("user", nil)
	m.Set("group", nil)
	b, err := marshalRaw(m)
	if err != nil {
		t.Fatal(err)
	}

	it, err := UnmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	if it.User != nil {
		t.Errorf("a None user became %q; it should be dropped", *it.User)
	}
	if it.Group != nil {
		t.Errorf("a None group became %q; it should be dropped", *it.Group)
	}
}

// TestLegacyTimestampFormsAreAccepted covers borg 1.x's two older spellings of a
// timestamp. The bigint form is little-endian signed, the opposite byte order from
// everything else in the format.
func TestLegacyTimestampForms(t *testing.T) {
	t.Run("plain integer nanoseconds", func(t *testing.T) {
		m := newRawMap()
		m.Set("path", "a")
		m.Set("mtime", int64(1755000000123456789))
		b, _ := marshalRaw(m)
		it, err := UnmarshalItem(b)
		if err != nil {
			t.Fatal(err)
		}
		if *it.MTime != 1755000000123456789 {
			t.Errorf("mtime = %d", *it.MTime)
		}
	})

	t.Run("little-endian signed bigint", func(t *testing.T) {
		for _, tc := range []struct {
			raw  []byte
			want int64
		}{
			{[]byte{0x01, 0x00, 0x00, 0x00}, 1},
			{[]byte{0xff, 0xff, 0xff, 0xff}, -1},
			{[]byte{0x00, 0x00, 0x00, 0x80}, -2147483648},
			// Cross-checked with int.from_bytes(raw, "little", signed=True).
			{[]byte{0xd5, 0x94, 0x9c, 0x30, 0x9b, 0x2c, 0x59, 0x18}, 1754482574884639957},
		} {
			m := newRawMap()
			m.Set("path", "a")
			m.Set("mtime", tc.raw)
			b, _ := marshalRaw(m)
			it, err := UnmarshalItem(b)
			if err != nil {
				t.Fatalf("%x: %v", tc.raw, err)
			}
			if *it.MTime != tc.want {
				t.Errorf("bigint %x decoded to %d, want %d", tc.raw, *it.MTime, tc.want)
			}
		}
	})
}

// TestBytesSpelledAsStringIsAccepted: borg < 1.3 packed with use_bin_type=False, so a
// value that should be bytes can arrive as a str. borg's want_bytes accepts both.
func TestBytesSpelledAsStringIsAccepted(t *testing.T) {
	m := newRawMap()
	m.Set("path", "a")
	m.Set("mtime", timestampFor(0))
	m.Set("hlid", "not-really-a-string") // str where bytes is expected
	b, _ := marshalRaw(m)

	it, err := UnmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(it.HLID) != "not-really-a-string" {
		t.Errorf("hlid = %q", it.HLID)
	}
}

func TestXAttrsSorted(t *testing.T) {
	it := &Item{
		Path:      "x",
		MTime:     OptInt(0),
		XAttrsSet: true,
		XAttrs: map[string][]byte{
			"user.zzz":         []byte("last"),
			"user.aaa":         []byte("first"),
			"security.selinux": []byte("ctx"),
		},
	}
	// Encoding twice must give the same bytes: a Go map iterates in random order, so
	// without an explicit sort this would be non-deterministic and the resulting item
	// would hash to a different chunk id on every run.
	first, err := it.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := it.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("encoding is not deterministic; xattrs are not being sorted")
		}
	}

	back, err := UnmarshalItem(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.XAttrs) != 3 || string(back.XAttrs["user.aaa"]) != "first" {
		t.Errorf("xattrs did not survive: %v", back.XAttrs)
	}
}

func TestXAttrsNoneValueBecomesEmpty(t *testing.T) {
	// Old borg stored None instead of b"" for an empty attribute value.
	inner := msgpackx.NewMap()
	inner.Set([]byte("user.empty"), nil)
	m := newRawMap()
	m.Set("path", "a")
	m.Set("mtime", timestampFor(0))
	m.Set("xattrs", inner)
	b, _ := marshalRaw(m)

	it, err := UnmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := it.XAttrs["user.empty"]
	if !ok {
		t.Fatal("the attribute was dropped")
	}
	if v == nil || len(v) != 0 {
		t.Errorf("a None value became %v, want an empty byte string", v)
	}
}

func TestArchiveItemRequiredKeys(t *testing.T) {
	for _, missing := range []string{"version", "name", "item_ptrs", "command_line"} {
		m := newRawMap()
		m.Set("version", int64(2))
		m.Set("name", "a")
		m.Set("item_ptrs", []any{[]byte{1}})
		m.Set("command_line", "borg create")
		m.Delete(missing)
		b, _ := marshalRaw(m)
		if _, err := DecodeArchiveItemBytes(b); err == nil {
			t.Errorf("decoded an archive with no %q", missing)
		}
	}
}

// DecodeArchiveItemBytes is a thin alias so the test reads clearly.
func DecodeArchiveItemBytes(b []byte) (*ArchiveItem, error) { return UnmarshalArchiveItem(b) }

func TestKeyJoinsLegacyEncKeys(t *testing.T) {
	// borg 1.x split what borg 2 calls crypt_key into enc_key and enc_hmac_key.
	m := newRawMap()
	m.Set("version", int64(1))
	m.Set("repository_id", bytes.Repeat([]byte{1}, 32))
	m.Set("enc_key", bytes.Repeat([]byte{2}, 32))
	m.Set("enc_hmac_key", bytes.Repeat([]byte{3}, 32))
	m.Set("id_key", bytes.Repeat([]byte{4}, 32))
	b, _ := marshalRaw(m)

	k, err := UnmarshalKey(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.CryptKey) != 64 {
		t.Fatalf("crypt_key is %d bytes, want 64", len(k.CryptKey))
	}
	if !bytes.Equal(k.CryptKey[:32], bytes.Repeat([]byte{2}, 32)) ||
		!bytes.Equal(k.CryptKey[32:], bytes.Repeat([]byte{3}, 32)) {
		t.Error("crypt_key is not enc_key followed by enc_hmac_key")
	}

	// A wrong total length is corruption, not a legacy spelling.
	m.Set("enc_hmac_key", bytes.Repeat([]byte{3}, 7))
	b, _ = marshalRaw(m)
	if _, err := UnmarshalKey(b); err == nil {
		t.Error("accepted a legacy key of the wrong length")
	}
}

func FuzzItemRoundTrip(f *testing.F) {
	base := &Item{Path: "a", MTime: OptInt(0)}
	if b, err := base.Marshal(); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		it, err := UnmarshalItem(b)
		if err != nil {
			return // rejected, which is a fine answer for arbitrary bytes
		}
		// Anything that decodes must re-encode, and the result must decode again to the
		// same thing. A decode path that produces a value its own encoder refuses would
		// make borge unable to rewrite archives it can read.
		out, err := it.Marshal()
		if err != nil {
			t.Fatalf("decoded %x but could not re-encode: %v", b, err)
		}
		again, err := UnmarshalItem(out)
		if err != nil {
			t.Fatalf("re-encoded output does not decode: %v", err)
		}
		third, err := again.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, third) {
			t.Errorf("encoding is not stable:\n  first:  %x\n  second: %x", out, third)
		}
	})
}
