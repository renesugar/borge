// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/msgpackx"
)

// The expected strings below are what CPython's json module actually produced for the same
// values, captured rather than reasoned about. borge's dumps are compared against borg's
// byte for byte in debug_test.go, but that comparison only covers the shapes a real
// repository happens to contain: an emoji in a filename, a control character in an xattr
// and an empty container nested inside another are all legal and all rendered by rules
// that are only exercised here.

func dumpString(t *testing.T, v any, indent int) string {
	t.Helper()
	var b strings.Builder
	if err := writeDumpJSON(&b, v, indent); err != nil {
		t.Fatalf("writeDumpJSON: %v", err)
	}
	return b.String()
}

func TestDumpJSONMatchesCPython(t *testing.T) {
	nested := newDumpObject().
		set("a", int64(1)).
		set("b", []any{int64(1), int64(2)}).
		set("c", newDumpObject())

	// Insertion order, because a Python dict has one and json.dumps writes it. The msgpack
	// dicts borge dumps are sorted before they get here; see prepareDumpMap.
	strings_ := newDumpObject().
		set("s", "plain").
		set("u", "café-日本").
		set("e", "emoji 😀").
		set("d", "del\u007fhere").
		set("q", `a"b\c`).
		set("ctl", "tab\tnl\nbell\a").
		set("sur", "bad\xff\xfe")

	scalars := []any{true, false, nil, int64(0), int64(-1), int64(1234567890123456789)}

	for _, tc := range []struct {
		name           string
		value          any
		pretty, packed string
	}{
		{
			name:   "empty object",
			value:  newDumpObject(),
			pretty: "{}",
			packed: "{}",
		},
		{
			name:   "empty array",
			value:  []any{},
			pretty: "[]",
			packed: "[]",
		},
		{
			name:   "nested containers",
			value:  nested,
			pretty: "{\n    \"a\": 1,\n    \"b\": [\n        1,\n        2\n    ],\n    \"c\": {}\n}",
			packed: `{"a": 1, "b": [1, 2], "c": {}}`,
		},
		{
			name:  "strings",
			value: strings_,
			pretty: "{\n    \"s\": \"plain\",\n    \"u\": \"caf\\u00e9-\\u65e5\\u672c\"," +
				"\n    \"e\": \"emoji \\ud83d\\ude00\",\n    \"d\": \"del\\u007fhere\"," +
				"\n    \"q\": \"a\\\"b\\\\c\",\n    \"ctl\": \"tab\\tnl\\nbell\\u0007\"," +
				"\n    \"sur\": \"bad\\udcff\\udcfe\"\n}",
			packed: "{\"s\": \"plain\", \"u\": \"caf\\u00e9-\\u65e5\\u672c\", " +
				"\"e\": \"emoji \\ud83d\\ude00\", \"d\": \"del\\u007fhere\", " +
				"\"q\": \"a\\\"b\\\\c\", \"ctl\": \"tab\\tnl\\nbell\\u0007\", " +
				"\"sur\": \"bad\\udcff\\udcfe\"}",
		},
		{
			name:   "scalars",
			value:  scalars,
			pretty: "[\n    true,\n    false,\n    null,\n    0,\n    -1,\n    1234567890123456789\n]",
			packed: `[true, false, null, 0, -1, 1234567890123456789]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dumpString(t, tc.value, 4); got != tc.pretty {
				t.Errorf("indented:\n  got:  %q\n  want: %q", got, tc.pretty)
			}
			if got := dumpString(t, tc.value, 0); got != tc.packed {
				t.Errorf("compact:\n  got:  %q\n  want: %q", got, tc.packed)
			}
		})
	}
}

// TestPrepareDumpSplitsTextFromBytes pins the one rule in these dumps that is borg's own
// invention rather than JSON's: a byte string is shown as text when it decodes and as
// U+007F followed by hex when it does not.
func TestPrepareDumpSplitsTextFromBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want string
	}{
		{"plain text", []byte("a/path.txt"), "a/path.txt"},
		{"valid UTF-8", []byte("café"), "café"},
		{"empty", []byte{}, ""},
		{"not UTF-8", []byte{0x00, 0x01, 0xff}, "0001ff"},
		// The escape hatch: text that already begins with the marker has to be hexed too,
		// or a dump could not be read back unambiguously.
		{"starts with the marker", []byte("\u007fnot hex"), "\u007f7f6e6f7420686578"},
	} {
		got, err := prepareDump(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPrepareDumpSortsMapKeys: borg decodes these dicts into a StableDict, whose items()
// sorts, so a dump's key order is the sorted order regardless of what the msgpack held.
//
// It matters because the order is what a diff between two dumps depends on: an item whose
// keys came out in storage order would differ from borg's on every line.
func TestPrepareDumpSortsMapKeys(t *testing.T) {
	m := msgpackx.NewMap()
	m.Set("zebra", int64(1))
	m.Set("alpha", int64(2))
	m.Set("mike", int64(3))

	got, err := prepareDump(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"alpha": 2, "mike": 3, "zebra": 1}`; dumpString(t, got, 0) != want {
		t.Errorf("got %s, want %s", dumpString(t, got, 0), want)
	}
}

// TestPrepareDumpRendersTimestampsAsNanoseconds. JSON has no timestamp; borg writes the
// nanoseconds since the epoch, and an item's mtime is one of these.
func TestPrepareDumpRendersTimestampsAsNanoseconds(t *testing.T) {
	m := msgpackx.NewMap()
	m.Set("mtime", msgpackx.TimestampFromUnixNano(1786985733056248153))

	got, err := prepareDump(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"mtime": 1786985733056248153}`; dumpString(t, got, 0) != want {
		t.Errorf("got %s, want %s", dumpString(t, got, 0), want)
	}
}

// TestPrepareDumpRefusesWhatItCannotRender.
//
// Python raises a TypeError rather than inventing a rendering for a value json does not
// know. borge does the same: a debug dump that quietly made something up would be worse
// than one that stops, because the reader's whole reason for running it is to find out
// what is really there.
func TestPrepareDumpRefusesWhatItCannotRender(t *testing.T) {
	m := msgpackx.NewMap()
	m.Set("odd", msgpackx.Ext{Type: 42, Data: []byte{1, 2, 3}})

	if _, err := prepareDump(m); err == nil {
		t.Error("prepareDump rendered an unknown msgpack extension instead of refusing")
	}
}

// TestPyBytesReprMatchesPython pins the other Python-shaped rendering: what borg prints
// around a search hit.
func TestPyBytesReprMatchesPython(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", `b''`},
		{"hello", `b'hello'`},
		{" world\n", `b' world\n'`},
		{"tab\there", `b'tab\there'`},
		{"back\\slash", `b'back\\slash'`},
		// A single quote inside switches the delimiter, as long as there is no double
		// quote to conflict with.
		{"it's", `b"it's"`},
		{`it's a "quote"`, `b'it\'s a "quote"'`},
		{"\x00\x01\xff", `b'\x00\x01\xff'`},
		{"\x7f", `b'\x7f'`},
	} {
		if got := pyBytesRepr([]byte(tc.in)); got != tc.want {
			t.Errorf("pyBytesRepr(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
