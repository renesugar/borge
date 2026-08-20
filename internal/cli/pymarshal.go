// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of do_debug_convert_profile in borg's
// src/borg/archiver/debug_cmd.py, plus enough of CPython's marshal writer
// (Python/marshal.c, w_object) to produce a file marshal.load and pstats can read.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"

	"github.com/renesugar/borge/internal/msgpackx"
)

// A borg profile is msgpack; a Python profile is marshal. This file is the second half.
//
// # What the two formats are for
//
// cProfile's own on-disk format is marshal, which is CPython's internal serialisation and
// has no reader outside CPython. borg therefore writes msgpack when BORG_DEBUG_PROFILE
// names a file, and states the reason in a comment: a profile may be mailed to a
// developer, and "a format that is impossible to interpret outside an insecure
// implementation" is a poor thing to send over the internet. convert-profile is the step
// back, run by whoever has pstats or pyprof2calltree and wants to open the file.
//
// So the input is a borg profile and the output is for CPython's tooling. Neither end is
// borge's own: borge has no profiler that writes this, and pstats cannot read Go's pprof.
// The command is a file converter, and it is here so that a repository whose backups moved
// from borg to borge does not leave its old profiles unreadable by the tool that replaced
// borg. See DIVERGENCES.md #14.
//
// # Which marshal dialect is written
//
// marshal has versions. Version 3 added back-references, so an object that appears twice
// is written once and referred to afterwards; version 4 added a short form for ASCII
// strings. CPython chooses between them by *reference count* - w_ref returns early when
// Py_REFCNT(v) == 1 - so borg's output depends on which objects the interpreter happened to
// share, and reproducing it byte for byte would mean emulating CPython's refcounting.
//
// borge writes the plain forms instead: no references, no short ASCII. Every version of
// marshal.load reads them, and the loaded object is equal to the one borg's file loads to,
// which is the whole of what a profile reader needs. The two files are therefore *not*
// byte-identical, and the test compares them as loaded objects rather than as bytes -
// unlike the debug dumps in pydump.go, which are compared byte for byte because their job
// is to be diffed.
//
// Type codes, from Python/marshal.c. Only the ones a msgpack value can turn into are here:
// there is no code object, complex, set or frozenset in a profile, and no way to get one
// out of msgpack.
const (
	pyTypeNull        = '0' // ends a dict; also "no object", which is how r_object signals it
	pyTypeNone        = 'N'
	pyTypeFalse       = 'F'
	pyTypeTrue        = 'T'
	pyTypeInt         = 'i' // 32-bit, little-endian, signed
	pyTypeBinaryFloat = 'g' // IEEE 754 double, little-endian
	pyTypeLong        = 'l' // arbitrary precision, base 2^15
	pyTypeString      = 's' // bytes
	pyTypeUnicode     = 'u' // str, UTF-8 with surrogatepass
	pyTypeTuple       = '('
	pyTypeDict        = '{'
)

// pyMarshalMaxDepth is MAX_MARSHAL_STACK_DEPTH from Python/marshal.c. A file that nests
// deeper than this cannot be read back, so refusing to write it is better than producing
// one whose reader raises.
const pyMarshalMaxDepth = 2000

// pyMarshalDigitShift is PyLong_MARSHAL_SHIFT: a marshalled int is a sequence of 15-bit
// digits, least significant first, with the digit count carrying the sign.
const pyMarshalDigitShift = 15

// pyMarshal writes v to w in CPython's marshal format.
func pyMarshal(w io.Writer, v any) error {
	m := &pyMarshalWriter{w: bufio.NewWriter(w)}
	if err := m.value(v, 0); err != nil {
		return err
	}
	return m.w.Flush()
}

type pyMarshalWriter struct{ w *bufio.Writer }

// value writes one object, dispatching on the Go types msgpackx.Decoder produces.
//
// Two of them are decisions rather than translations. A msgpack array becomes a *tuple*,
// not a list, because borg unpacks with use_list=False - and it matters here: a profile's
// keys are (filename, lineno, funcname) triples, and a list is unhashable, so lists would
// make the dict unbuildable rather than merely different. A msgpack map becomes a dict in
// wire order, which is the order borg's unpacker inserts in.
func (m *pyMarshalWriter) value(v any, depth int) error {
	if depth > pyMarshalMaxDepth {
		return fmt.Errorf("value nests deeper than %d, which marshal.load cannot read back",
			pyMarshalMaxDepth)
	}
	switch x := v.(type) {
	case nil:
		return m.w.WriteByte(pyTypeNone)
	case bool:
		if x {
			return m.w.WriteByte(pyTypeTrue)
		}
		return m.w.WriteByte(pyTypeFalse)
	case int64:
		return m.int(x)
	case uint64:
		// Only reachable for values above MaxInt64, which is why there is no int32 case:
		// the decoder produces int64 for everything that fits.
		return m.long(x, false)
	case float64:
		if err := m.w.WriteByte(pyTypeBinaryFloat); err != nil {
			return err
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(x))
		_, err := m.w.Write(buf[:])
		return err
	case string:
		return m.str(x)
	case []byte:
		if err := m.w.WriteByte(pyTypeString); err != nil {
			return err
		}
		if err := m.count(len(x), "bytes"); err != nil {
			return err
		}
		_, err := m.w.Write(x)
		return err
	case []any:
		if err := m.w.WriteByte(pyTypeTuple); err != nil {
			return err
		}
		if err := m.count(len(x), "tuple"); err != nil {
			return err
		}
		for _, elem := range x {
			if err := m.value(elem, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *msgpackx.Map:
		if err := m.w.WriteByte(pyTypeDict); err != nil {
			return err
		}
		for _, e := range x.Entries() {
			if err := m.value(e.Key, depth+1); err != nil {
				return err
			}
			if err := m.value(e.Value, depth+1); err != nil {
				return err
			}
		}
		// A dict is written as pairs and terminated, not counted.
		return m.w.WriteByte(pyTypeNull)
	default:
		// Timestamp and Ext land here. CPython's marshal refuses them too - it has no
		// code for an arbitrary object - so borg's converter fails on such a file as
		// well, and saying which type stopped it is more use than a traceback.
		return fmt.Errorf("this profile holds a %T, which has no marshal representation", v)
	}
}

// int writes a Python int, using the 32-bit form where it fits.
//
// CPython makes the same choice for the same reason (w_object: PyLong_AsLongAndOverflow,
// then a range check), so ordinary profile counts come out in four bytes.
func (m *pyMarshalWriter) int(n int64) error {
	if n >= math.MinInt32 && n <= math.MaxInt32 {
		if err := m.w.WriteByte(pyTypeInt); err != nil {
			return err
		}
		return m.writeLong(int32(n))
	}
	// Negating in uint64 rather than int64: -math.MinInt64 overflows, and the unsigned
	// negation gives exactly the magnitude.
	mag := uint64(n)
	if n < 0 {
		mag = -uint64(n)
	}
	return m.long(mag, n < 0)
}

// long writes the arbitrary-precision form: a signed count of 15-bit digits, then the
// digits, least significant first. The top digit must be non-zero or marshal.load rejects
// the file as "unnormalized long data", which minimal digits give for free.
func (m *pyMarshalWriter) long(mag uint64, negative bool) error {
	if err := m.w.WriteByte(pyTypeLong); err != nil {
		return err
	}
	var digits []uint16
	for v := mag; v > 0; v >>= pyMarshalDigitShift {
		digits = append(digits, uint16(v&(1<<pyMarshalDigitShift-1)))
	}
	n := int32(len(digits))
	if negative {
		n = -n
	}
	if err := m.writeLong(n); err != nil {
		return err
	}
	var buf [2]byte
	for _, d := range digits {
		binary.LittleEndian.PutUint16(buf[:], d)
		if _, err := m.w.Write(buf[:]); err != nil {
			return err
		}
	}
	return nil
}

// str writes a Python str.
//
// The bytes are UTF-8, and TYPE_UNICODE is read back with the surrogatepass error handler
// - so a byte that is not valid UTF-8 has to be written the way Python would write it after
// surrogateescape decoding: as the three-byte encoding of U+DC00+byte, which utf8.EncodeRune
// refuses to produce because it is not a legal scalar value. This is the same convention
// pydump.go renders as \udcXX; here it goes out as bytes rather than as an escape.
//
// A filename in a profile is ASCII in practice, so the fast path is the whole story most of
// the time - but "most of the time" is not a property a converter should depend on.
func (m *pyMarshalWriter) str(s string) error {
	if err := m.w.WriteByte(pyTypeUnicode); err != nil {
		return err
	}
	if utf8.ValidString(s) {
		if err := m.count(len(s), "str"); err != nil {
			return err
		}
		_, err := m.w.WriteString(s)
		return err
	}
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = rune(0xdc00 + int(s[i]))
			out = append(out,
				byte(0xe0|(r>>12)), byte(0x80|((r>>6)&0x3f)), byte(0x80|(r&0x3f)))
			i++
			continue
		}
		out = append(out, s[i:i+size]...)
		i += size
	}
	if err := m.count(len(out), "str"); err != nil {
		return err
	}
	_, err := m.w.Write(out)
	return err
}

// count writes a length, which marshal stores as a signed 32-bit number.
func (m *pyMarshalWriter) count(n int, what string) error {
	if n > math.MaxInt32 {
		return fmt.Errorf("a %s of %d bytes is longer than marshal can record", what, n)
	}
	return m.writeLong(int32(n))
}

func (m *pyMarshalWriter) writeLong(n int32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(n))
	_, err := m.w.Write(buf[:])
	return err
}
