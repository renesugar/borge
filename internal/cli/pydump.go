// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of prepare_dump_dict in borg's
// src/borg/helpers/parseformat.py, plus enough of CPython's json encoder to match the
// output byte for byte.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/renesugar/borge/internal/msgpackx"
)

// The debug dumps are JSON renderings of msgpack that was never meant to be JSON, and
// this file is what makes the rendering reproduce borg's exactly.
//
// # Why byte-for-byte
//
// These dumps exist to be *diffed*. The realistic use is a repository that one tool reads
// and the other does not, where the question is which field differs - so the answer has
// to survive `diff borg.json borge.json` without drowning in noise from key order,
// indentation or escaping. Producing semantically equal but textually different JSON
// would make the one job these commands have harder, not easier.
//
// It also makes the port testable: an exact comparison against borg's output is a real
// assertion, where "both parse to equal objects" would pass even if borge silently lost a
// field's type.
//
// Three CPython behaviours have to be reproduced, none of which Go's encoding/json does:
//
//   - ensure_ascii: every character outside printable ASCII is escaped as \uXXXX, with
//     surrogate pairs above the BMP. Go escapes nothing above 0x7e (and escapes <, > and &
//     below it, which Python does not).
//   - separators: ", " and ": " in compact mode. Go writes "," and ":".
//   - the bytes/str split: msgpack bin is not text, and borg marks it with a leading
//     U+007F before the hex - see dumpBytes.

// dumpObject is a JSON object with a fixed key order.
//
// A Go map would sort the keys, which is right for the msgpack dicts borg dumps (they
// come out of a StableDict, whose items() sorts) but wrong for the ones borg builds
// itself in a deliberate order, such as an archive's manifest entry.
type dumpObject struct {
	keys []string
	vals []any
}

func newDumpObject() *dumpObject { return &dumpObject{} }

func (o *dumpObject) set(key string, value any) *dumpObject {
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, value)
	return o
}

// prepareDump converts a decoded msgpack value into something writeDumpJSON can write.
//
// This is borg's prepare_dump_dict. The interesting half is what happens to byte strings;
// see dumpBytes.
func prepareDump(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, string, int64, uint64, float32, float64:
		return t, nil
	case int:
		return int64(t), nil
	case []byte:
		return dumpBytes(t), nil
	case msgpackx.Timestamp:
		// borg turns a msgpack timestamp into an integer of nanoseconds since the epoch,
		// because JSON has no timestamp and the item metadata is full of them.
		return t.UnixNano(), nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			c, err := prepareDump(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case *msgpackx.Map:
		return prepareDumpMap(t)
	default:
		// msgpack ext types other than the timestamp, mainly. Python raises a TypeError
		// here rather than inventing a rendering, and so does borge: a debug dump that
		// quietly makes up structure is worse than one that stops and says what it found.
		return nil, fmt.Errorf("cannot render a %T as JSON", v)
	}
}

func prepareDumpMap(m *msgpackx.Map) (*dumpObject, error) {
	entries := m.Entries()
	// borg decodes these dicts with object_hook=StableDict, whose items() is sorted, and
	// json.dumps then writes them in that order. The sort is on the key as stored, before
	// any decoding, which is why it happens here rather than on the rendered strings.
	sorted := make([]msgpackx.MapEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return dumpKeyBytes(sorted[i].Key) < dumpKeyBytes(sorted[j].Key)
	})

	out := newDumpObject()
	for _, e := range sorted {
		var key string
		switch k := e.Key.(type) {
		case string:
			key = k
		case []byte:
			// borg calls key.decode() with strict errors here, so a key that is not UTF-8
			// makes it raise. borge marks it the same way a value would be marked instead:
			// a dump that fails on one odd xattr name shows nothing at all, which is the
			// opposite of what a debugging command is for.
			key = dumpBytes(k)
		default:
			return nil, fmt.Errorf("cannot render a %T as a JSON object key", e.Key)
		}
		val, err := prepareDump(e.Value)
		if err != nil {
			return nil, err
		}
		out.set(key, val)
	}
	return out, nil
}

// dumpMarker is borg's in-band signal that what follows is hex rather than text: U+007F,
// ASCII DEL, which cannot occur in a path or an xattr name anyone would write. It is
// spelled as an escape because the character itself is invisible in a source file.
const dumpMarker = "\u007f"

// dumpBytes renders a msgpack byte string.
//
// A chunk id and a filename are both `bytes` on the wire, and a dump that showed the id as
// mojibake would be useless while one that showed every path as hex would be unreadable.
// borg's rule: show it as text if it decodes as UTF-8, otherwise mark it with U+007F and
// write it in hex - and always take the hex branch if the text *starts* with U+007F, so
// the two cases stay distinguishable and the rendering stays reversible.
func dumpBytes(b []byte) string {
	if !bytes.HasPrefix(b, []byte(dumpMarker)) && utf8.Valid(b) {
		return string(b)
	}
	return dumpMarker + hex.EncodeToString(b)
}

// dumpKeyBytes is the sort key for a msgpack map key: the bytes as stored.
func dumpKeyBytes(k any) string {
	switch t := k.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

// writeDumpJSON writes v the way CPython's json.dump does.
//
// indent is the number of spaces per level; zero means compact output, which Python still
// spells with a space after each separator.
func writeDumpJSON(w io.Writer, v any, indent int) error {
	var b strings.Builder
	if err := appendDumpJSON(&b, v, indent, 0); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func appendDumpJSON(b *strings.Builder, v any, indent, depth int) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(t, 10))
	case float64:
		b.WriteString(pyFloat(t))
	case float32:
		b.WriteString(pyFloat(float64(t)))
	case string:
		writeDumpString(b, t)
	case []any:
		writeDumpArray(b, t, indent, depth)
	case *dumpObject:
		return writeDumpObject(b, t, indent, depth)
	default:
		return fmt.Errorf("cannot render a %T as JSON", v)
	}
	return nil
}

func writeDumpArray(b *strings.Builder, a []any, indent, depth int) error {
	// Python writes an empty container with no whitespace inside, whatever the indent.
	if len(a) == 0 {
		b.WriteString("[]")
		return nil
	}
	b.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
			if indent == 0 {
				b.WriteByte(' ')
			}
		}
		writeDumpNewline(b, indent, depth+1)
		if err := appendDumpJSON(b, e, indent, depth+1); err != nil {
			return err
		}
	}
	writeDumpNewline(b, indent, depth)
	b.WriteByte(']')
	return nil
}

func writeDumpObject(b *strings.Builder, o *dumpObject, indent, depth int) error {
	if len(o.keys) == 0 {
		b.WriteString("{}")
		return nil
	}
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
			if indent == 0 {
				b.WriteByte(' ')
			}
		}
		writeDumpNewline(b, indent, depth+1)
		writeDumpString(b, k)
		b.WriteString(": ")
		if err := appendDumpJSON(b, o.vals[i], indent, depth+1); err != nil {
			return err
		}
	}
	writeDumpNewline(b, indent, depth)
	b.WriteByte('}')
	return nil
}

func writeDumpNewline(b *strings.Builder, indent, depth int) {
	if indent == 0 {
		return
	}
	b.WriteByte('\n')
	for i := 0; i < indent*depth; i++ {
		b.WriteByte(' ')
	}
}

// pyFloat renders a float as repr() does. borg's dumps have no floats today; this is here
// so a future field that has one does not silently render differently.
func pyFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// writeDumpString escapes a string the way json.dumps(ensure_ascii=True) does.
//
// Everything outside printable ASCII becomes \uXXXX. Bytes that are not valid UTF-8
// become \udcXX, which is Python's surrogateescape: msgpack hands borg a str decoded that
// way, so a filename that is not UTF-8 - which Linux allows - dumps identically from both
// tools instead of being replaced or dropped.
func writeDumpString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 || c == 0x7f {
					writeDumpEscape(b, rune(c))
				} else {
					b.WriteByte(c)
				}
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// An invalid byte. Python's surrogateescape maps it to U+DC80..U+DCFF, and
			// json then escapes that lone surrogate literally.
			writeDumpEscape(b, rune(0xdc00+int(s[i])))
			i++
			continue
		}
		i += size
		if r > 0xffff {
			r -= 0x10000
			writeDumpEscape(b, 0xd800+(r>>10))
			writeDumpEscape(b, 0xdc00+(r&0x3ff))
			continue
		}
		writeDumpEscape(b, r)
	}
	b.WriteByte('"')
}

func writeDumpEscape(b *strings.Builder, r rune) {
	const hexDigits = "0123456789abcdef"
	b.WriteString(`\u`)
	b.WriteByte(hexDigits[(r>>12)&0xf])
	b.WriteByte(hexDigits[(r>>8)&0xf])
	b.WriteByte(hexDigits[(r>>4)&0xf])
	b.WriteByte(hexDigits[r&0xf])
}
