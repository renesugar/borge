// SPDX-License-Identifier: Apache-2.0

package msgpackx

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// These cover what the borg-generated fixtures cannot: container sizes too large to
// check into testdata, the Go-side API, and the failure modes that matter for a tool
// reading potentially corrupt bytes.

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T): %v", v, err)
	}
	return b
}

// TestLargeContainerHeaders reaches the array32/map32/str32/bin32 formats. The
// fixtures stop below these because a 65536-element fixture would add megabytes of hex
// to the repository, but the format boundary still has to be right.
func TestLargeContainerHeaders(t *testing.T) {
	tests := []struct {
		name       string
		value      any
		wantPrefix string
	}{
		{"array16_at_16", make([]any, 16), "dc0010"},
		{"array16_max", make([]any, 65535), "dcffff"},
		{"array32_at_65536", make([]any, 65536), "dd00010000"},
		{"str16_at_65536_minus_1", strings.Repeat("a", 65535), "daffff"},
		{"str32_at_65536", strings.Repeat("a", 65536), "db00010000"},
		{"bin16_max", make([]byte, 65535), "c5ffff"},
		{"bin32_at_65536", make([]byte, 65536), "c600010000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustMarshal(t, tc.value)
			want, err := hex.DecodeString(tc.wantPrefix)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(got, want) {
				t.Errorf("header = %x..., want prefix %s", got[:min(len(got), 8)], tc.wantPrefix)
			}
			// And it must survive the round trip at that size.
			if _, err := Unmarshal(got); err != nil {
				t.Errorf("round trip failed: %v", err)
			}
		})
	}
}

func TestLargeMapHeaders(t *testing.T) {
	for _, n := range []int{16, 65535, 65536} {
		m := NewMap()
		for i := 0; i < n; i++ {
			m.entries = append(m.entries, MapEntry{Key: int64(i), Value: nil})
		}
		got := mustMarshal(t, m)
		var wantPrefix string
		switch {
		case n <= 65535:
			wantPrefix = "de"
		default:
			wantPrefix = "df"
		}
		if hex.EncodeToString(got[:1]) != wantPrefix {
			t.Errorf("map of %d: header %x, want prefix %s", n, got[:1], wantPrefix)
		}
		v, err := Unmarshal(got)
		if err != nil {
			t.Fatalf("map of %d: %v", n, err)
		}
		if v.(*Map).Len() != n {
			t.Errorf("map of %d: decoded %d entries", n, v.(*Map).Len())
		}
	}
}

// TestStableSortsOnEncode checks the StableDict behaviour from the Go side. The
// fixtures prove borge agrees with borg on already-sorted input; this proves borge
// does the sorting itself rather than relying on the caller's order.
func TestStableSortsOnEncode(t *testing.T) {
	unsorted := []MapEntry{{Key: "z", Value: int64(1)}, {Key: "a", Value: int64(2)}, {Key: "m", Value: int64(3)}}

	stable := mustMarshal(t, NewStableMap(unsorted...))
	if got, want := hex.EncodeToString(stable), "83a16102a16d03a17a01"; got != want {
		t.Errorf("stable map = %s, want %s (borg's StableDict order)", got, want)
	}

	ordered := mustMarshal(t, NewMap(unsorted...))
	if got, want := hex.EncodeToString(ordered), "83a17a01a16102a16d03"; got != want {
		t.Errorf("ordered map = %s, want %s (insertion order, like a plain dict)", got, want)
	}
}

// TestStableSortMatchesPythonCodePointOrder is the case where sorting by raw bytes and
// sorting by Python code points disagree. Byte order would put the astral character
// before the invalid byte; Python puts it after, because U+DCFF < U+10FFFF.
func TestStableSortMatchesPythonCodePointOrder(t *testing.T) {
	m := NewStableMap(
		MapEntry{Key: "\U0010FFFF", Value: "astral"},
		MapEntry{Key: "\xff", Value: "invalid-byte"},
		MapEntry{Key: "a", Value: "ascii"},
	)
	got := mustMarshal(t, m)

	// This is the exact byte string borg produced for the same dict; see
	// testdata/fixtures.txt, fixture "stabledict_surrogate_vs_astral".
	want := "83a161a56173636969a1ffac696e76616c69642d62797465a4f48fbfbfa661737472616c"
	if hex.EncodeToString(got) != want {
		t.Errorf("order differs from borg\n  got:  %x\n  want: %s", got, want)
	}

	if sortedByBytes := comparePyStr("\xff", "\U0010FFFF") < 0; !sortedByBytes {
		t.Error(`comparePyStr("\xff", "\U0010FFFF") should be negative: ` +
			"Python sees U+DCFF < U+10FFFF even though byte 0xff > byte 0xf4")
	}
	if bytes.Compare([]byte("\xff"), []byte("\U0010FFFF")) < 0 {
		t.Error("precondition failed: raw byte order was expected to disagree here")
	}
}

func TestStableSortRejectsMixedKeyTypes(t *testing.T) {
	m := NewStableMap(MapEntry{Key: "a", Value: 1}, MapEntry{Key: int64(1), Value: 2})
	_, err := Marshal(m)
	if err == nil {
		t.Fatal("expected an error for a map mixing str and int keys, as Python would raise TypeError")
	}
	if !strings.Contains(err.Error(), "different types") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestTimestampConversion(t *testing.T) {
	tests := []struct {
		ns       int64
		sec      int64
		nsec     uint32
		wantForm string // the msgpack format actually chosen
	}{
		{0, 0, 0, "d6"},
		{1, 0, 1, "d7"},
		{1_000_000_000, 1, 0, "d6"},
		{1755000000123456789, 1755000000, 123456789, "d7"},
		// Python floors, so a negative nanosecond count borrows from the seconds.
		{-1, -1, 999999999, "c7"},
		{-1000000005, -2, 999999995, "c7"},
	}
	for _, tc := range tests {
		ts := TimestampFromUnixNano(tc.ns)
		if ts.Seconds != tc.sec || ts.Nanoseconds != tc.nsec {
			t.Errorf("TimestampFromUnixNano(%d) = {%d, %d}, want {%d, %d}",
				tc.ns, ts.Seconds, ts.Nanoseconds, tc.sec, tc.nsec)
		}
		if back := ts.UnixNano(); back != tc.ns {
			t.Errorf("round trip of %d ns gave %d", tc.ns, back)
		}
		b := mustMarshal(t, ts)
		if hex.EncodeToString(b[:1]) != tc.wantForm {
			t.Errorf("%d ns encoded with format %x, want %s", tc.ns, b[:1], tc.wantForm)
		}
	}
}

func TestTimestampRejectsBadNanoseconds(t *testing.T) {
	if _, err := Marshal(Timestamp{Seconds: 0, Nanoseconds: 1_000_000_000}); err == nil {
		t.Error("expected an error for nanoseconds >= 1e9")
	}
	// A timestamp64 whose nanosecond field is out of range is corruption, not a value.
	bad, _ := hex.DecodeString("d7ff" + "fffffffc00000000")
	if _, err := Unmarshal(bad); err == nil {
		t.Error("expected an error decoding a timestamp64 with out-of-range nanoseconds")
	}
}

func TestStringAndBytesStayApart(t *testing.T) {
	// use_bin_type=True exists precisely so these do not collide. If a port ever maps
	// both onto one Go type, this is the test that notices.
	s := mustMarshal(t, "ab")
	b := mustMarshal(t, []byte("ab"))
	if bytes.Equal(s, b) {
		t.Fatal("str and bytes encoded identically")
	}
	if hex.EncodeToString(s) != "a26162" {
		t.Errorf("str  = %x, want a26162 (fixstr)", s)
	}
	if hex.EncodeToString(b) != "c4026162" {
		t.Errorf("bytes = %x, want c4026162 (bin8)", b)
	}

	vs, err := Unmarshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vs.(string); !ok {
		t.Errorf("str decoded as %T, want string", vs)
	}
	vb, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vb.([]byte); !ok {
		t.Errorf("bin decoded as %T, want []byte", vb)
	}
}

// TestArbitraryBytePathsRoundTrip is the property the plan calls out as the one that
// silently corrupts filenames if a port gets it wrong. A Go string is an arbitrary
// byte sequence, so this must hold for every input, not just valid UTF-8.
func TestArbitraryBytePathsRoundTrip(t *testing.T) {
	paths := []string{
		"", "a", "plain/path.txt",
		"caf\xe9.txt", "\xff\xfe\xfd", "\x80\x81\x82",
		"file\xc3", "\xf0\x9d\x94\x98\xff",
		"h\xc3\xa9llo", "\x00embedded\x00nul",
		strings.Repeat("\xff", 300),
	}
	for _, p := range paths {
		b := mustMarshal(t, p)
		v, err := Unmarshal(b)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		got, ok := v.(string)
		if !ok {
			t.Fatalf("%q decoded as %T", p, v)
		}
		if got != p {
			t.Errorf("round trip changed the bytes:\n  in:  %x\n  out: %x", p, got)
		}
	}
}

func FuzzArbitraryStringRoundTrip(f *testing.F) {
	for _, s := range []string{"", "a", "caf\xe9", "\xff", "\xf0\x9d\x94\x98\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := Marshal(s)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", s, err)
		}
		v, err := Unmarshal(b)
		if err != nil {
			t.Fatalf("Unmarshal of %x: %v", b, err)
		}
		if got := v.(string); got != s {
			t.Errorf("round trip changed the bytes:\n  in:  %x\n  out: %x", s, got)
		}
	})
}

func FuzzDecodeDoesNotPanic(f *testing.F) {
	f.Add([]byte{0xc0})
	f.Add([]byte{0x83, 0xa1, 0x61, 0x02})
	f.Add([]byte{0xdd, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		// The only requirement is that arbitrary bytes never panic and never hang.
		// borge reads bytes that may be corrupt or hostile; an error is a fine answer,
		// a crash is not.
		_, _ = Unmarshal(b)
	})
}

// TestHugeLengthIsRejectedCheaply guards the allocation path: a container header may
// claim four billion elements, and the decoder must reject it against the bytes
// actually available instead of trying to allocate for it.
func TestHugeLengthIsRejectedCheaply(t *testing.T) {
	cases := map[string]string{
		"array32 claiming 4Gi elements": "ddffffffff",
		"map32 claiming 4Gi entries":    "dfffffffff",
		"str32 claiming 4GiB":           "dbffffffff",
		"bin32 claiming 4GiB":           "c6ffffffff",
	}
	for name, h := range cases {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Unmarshal(b)
		if err == nil {
			t.Errorf("%s: decoded without error", name)
			continue
		}
		if !errors.Is(err, ErrShortBuffer) {
			t.Errorf("%s: got %v, want a short-buffer error", name, err)
		}
	}
}

func TestNestingLimit(t *testing.T) {
	// A deeply nested array is cheap to construct and would otherwise recurse until
	// the stack gives out.
	var deep []byte
	for i := 0; i < DefaultMaxDepth+10; i++ {
		deep = append(deep, 0x91) // fixarray of 1
	}
	deep = append(deep, 0xc0) // nil
	if _, err := Unmarshal(deep); err == nil {
		t.Error("expected an error for nesting beyond the limit")
	}

	var v any = nil
	for i := 0; i < DefaultMaxDepth+10; i++ {
		v = []any{v}
	}
	if _, err := Marshal(v); err == nil {
		t.Error("expected an encode error for nesting beyond the limit")
	}
}

func TestTrailingBytesRejected(t *testing.T) {
	b := append(mustMarshal(t, int64(1)), 0xc0)
	if _, err := Unmarshal(b); err == nil {
		t.Error("Unmarshal should reject trailing bytes; use a Decoder for a stream")
	}
}

// TestDecoderStream covers reading concatenated values, which is how the item
// metadata stream is laid out.
func TestDecoderStream(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	want := []any{int64(1), "two", []byte{3}, nil, true}
	for _, v := range want {
		if err := e.Encode(v); err != nil {
			t.Fatal(err)
		}
	}

	d := NewDecoder(buf.Bytes())
	var got []any
	for d.More() {
		v, err := d.Decode()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d values, want %d", len(got), len(want))
	}
	if got[0] != want[0] || got[1] != want[1] || got[3] != nil || got[4] != true {
		t.Errorf("stream mismatch: %v", got)
	}
	if _, err := d.Decode(); !errors.Is(err, io.EOF) {
		t.Errorf("a fully consumed decoder should report io.EOF, got %v", err)
	}
}

func TestMapAccessors(t *testing.T) {
	m := NewMap()
	m.Set("s", "value")
	m.Set("b", []byte{1, 2})
	m.Set("i", int64(42))
	m.Set("s", "replaced") // must replace in place, not append

	if m.Len() != 3 {
		t.Errorf("Len = %d, want 3 (Set must replace an existing key)", m.Len())
	}
	if v, ok := m.GetString("s"); !ok || v != "replaced" {
		t.Errorf("GetString(s) = %q, %v", v, ok)
	}
	if v, ok := m.GetBytes("b"); !ok || !bytes.Equal(v, []byte{1, 2}) {
		t.Errorf("GetBytes(b) = %v, %v", v, ok)
	}
	if v, ok := m.GetInt("i"); !ok || v != 42 {
		t.Errorf("GetInt(i) = %d, %v", v, ok)
	}
	if _, ok := m.Get("missing"); ok {
		t.Error("Get(missing) reported present")
	}
	if !m.Delete("i") || m.Len() != 2 {
		t.Error("Delete did not remove the entry")
	}

	// Order must survive: it is what makes re-encoding reproduce borg's bytes.
	if got := m.Entries()[0].Key; got != "s" {
		t.Errorf("first key = %v, want s (insertion order must be preserved)", got)
	}
}

func TestNilMapIsUsable(t *testing.T) {
	var m *Map
	if m.Len() != 0 {
		t.Error("nil map should report zero length")
	}
	if _, ok := m.Get("x"); ok {
		t.Error("nil map should report nothing present")
	}
}

func TestUnencodableType(t *testing.T) {
	if _, err := Marshal(struct{ A int }{1}); err == nil {
		t.Error("expected an error: encoding a struct by reflection is not supported on purpose")
	}
}
