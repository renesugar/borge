// SPDX-License-Identifier: Apache-2.0

package msgpackx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// DefaultMaxDepth bounds nesting on both encode and decode. borg's own structures are
// shallow (an item's deepest nesting is the chunk list inside an item dict, about 4),
// so this is generous while still refusing a structure crafted to exhaust the stack.
const DefaultMaxDepth = 64

// Format byte prefixes, from the msgpack specification. Named rather than inlined
// because the decoder switches on them and the two must agree exactly.
const (
	fixintPositiveMax = 0x7f
	fixintNegativeMin = 0xe0

	fixmapPrefix   = 0x80
	fixarrayPrefix = 0x90
	fixstrPrefix   = 0xa0

	codeNil      = 0xc0
	codeFalse    = 0xc2
	codeTrue     = 0xc3
	codeBin8     = 0xc4
	codeBin16    = 0xc5
	codeBin32    = 0xc6
	codeExt8     = 0xc7
	codeExt16    = 0xc8
	codeExt32    = 0xc9
	codeFloat32  = 0xca
	codeFloat64  = 0xcb
	codeUint8    = 0xcc
	codeUint16   = 0xcd
	codeUint32   = 0xce
	codeUint64   = 0xcf
	codeInt8     = 0xd0
	codeInt16    = 0xd1
	codeInt32    = 0xd2
	codeInt64    = 0xd3
	codeFixExt1  = 0xd4
	codeFixExt2  = 0xd5
	codeFixExt4  = 0xd6
	codeFixExt8  = 0xd7
	codeFixExt16 = 0xd8
	codeStr8     = 0xd9
	codeStr16    = 0xda
	codeStr32    = 0xdb
	codeArray16  = 0xdc
	codeArray32  = 0xdd
	codeMap16    = 0xde
	codeMap32    = 0xdf
)

// Marshal encodes v with borg's msgpack settings.
//
// The encoding is byte-identical to msgpack-python's, which matters because borg
// computes chunk ids over packed bytes. In particular the smallest representation is
// always chosen, and a non-negative integer is written with an unsigned format even
// when it would fit a signed one - matching msgpack-python's msgpack_pack_long_long.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	e := &Encoder{w: &buf, maxDepth: DefaultMaxDepth}
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Encoder writes msgpack values to a stream.
type Encoder struct {
	w        io.Writer
	maxDepth int
	scratch  [16]byte
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w, maxDepth: DefaultMaxDepth}
}

// SetMaxDepth overrides the nesting limit. A depth of zero or less restores the
// default.
func (e *Encoder) SetMaxDepth(d int) {
	if d <= 0 {
		d = DefaultMaxDepth
	}
	e.maxDepth = d
}

// Encode writes one value.
func (e *Encoder) Encode(v any) error {
	return e.encode(v, 0)
}

func (e *Encoder) encode(v any, depth int) error {
	if depth > e.maxDepth {
		return fmt.Errorf("msgpackx: nesting deeper than %d", e.maxDepth)
	}
	switch x := v.(type) {
	case nil:
		return e.writeByte(codeNil)
	case bool:
		if x {
			return e.writeByte(codeTrue)
		}
		return e.writeByte(codeFalse)

	case int:
		return e.encodeInt(int64(x))
	case int8:
		return e.encodeInt(int64(x))
	case int16:
		return e.encodeInt(int64(x))
	case int32:
		return e.encodeInt(int64(x))
	case int64:
		return e.encodeInt(x)
	case uint:
		return e.encodeUint(uint64(x))
	case uint8:
		return e.encodeUint(uint64(x))
	case uint16:
		return e.encodeUint(uint64(x))
	case uint32:
		return e.encodeUint(uint64(x))
	case uint64:
		return e.encodeUint(x)

	case float32:
		// borg sets use_single_float=False, so it never writes float32. Widening keeps
		// borge's output identical to borg's for the same logical value.
		return e.encodeFloat64(float64(x))
	case float64:
		return e.encodeFloat64(x)

	case string:
		return e.encodeStr(x)
	case []byte:
		return e.encodeBin(x)

	case Timestamp:
		return e.encodeTimestamp(x)
	case Ext:
		return e.encodeExt(x.Type, x.Data)

	case []any:
		if err := e.writeArrayHeader(len(x)); err != nil {
			return err
		}
		for _, elem := range x {
			if err := e.encode(elem, depth+1); err != nil {
				return err
			}
		}
		return nil

	case *Map:
		return e.encodeMap(x, depth)

	case map[string]any:
		// A Go map has no defined iteration order, so encoding one directly would make
		// borge's output non-deterministic. Sort, which is also what borg's StableDict
		// does, and document it rather than silently picking an arbitrary order.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return comparePyStr(keys[i], keys[j]) < 0 })
		if err := e.writeMapHeader(len(x)); err != nil {
			return err
		}
		for _, k := range keys {
			if err := e.encodeStr(k); err != nil {
				return err
			}
			if err := e.encode(x[k], depth+1); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("msgpackx: cannot encode %T", v)
	}
}

func (e *Encoder) encodeMap(m *Map, depth int) error {
	entries := m.Entries()
	if m.Stable {
		sorted, err := m.sortedEntries()
		if err != nil {
			return err
		}
		entries = sorted
	}
	if err := e.writeMapHeader(len(entries)); err != nil {
		return err
	}
	for _, kv := range entries {
		if err := e.encode(kv.Key, depth+1); err != nil {
			return err
		}
		if err := e.encode(kv.Value, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// encodeInt mirrors msgpack-python's msgpack_pack_long_long: negative values pick the
// smallest signed format, non-negative values fall through to the unsigned path.
func (e *Encoder) encodeInt(n int64) error {
	if n >= 0 {
		return e.encodeUint(uint64(n))
	}
	switch {
	case n >= -32:
		return e.writeByte(byte(n)) // negative fixint, 0xe0..0xff
	case n >= math.MinInt8:
		return e.write2(codeInt8, byte(n))
	case n >= math.MinInt16:
		e.scratch[0] = codeInt16
		binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
		return e.writeN(3)
	case n >= math.MinInt32:
		e.scratch[0] = codeInt32
		binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
		return e.writeN(5)
	default:
		e.scratch[0] = codeInt64
		binary.BigEndian.PutUint64(e.scratch[1:], uint64(n))
		return e.writeN(9)
	}
}

func (e *Encoder) encodeUint(n uint64) error {
	switch {
	case n <= fixintPositiveMax:
		return e.writeByte(byte(n))
	case n <= math.MaxUint8:
		return e.write2(codeUint8, byte(n))
	case n <= math.MaxUint16:
		e.scratch[0] = codeUint16
		binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
		return e.writeN(3)
	case n <= math.MaxUint32:
		e.scratch[0] = codeUint32
		binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
		return e.writeN(5)
	default:
		e.scratch[0] = codeUint64
		binary.BigEndian.PutUint64(e.scratch[1:], n)
		return e.writeN(9)
	}
}

func (e *Encoder) encodeFloat64(f float64) error {
	e.scratch[0] = codeFloat64
	binary.BigEndian.PutUint64(e.scratch[1:], math.Float64bits(f))
	return e.writeN(9)
}

// encodeStr writes the str family. With use_bin_type=True, str8 is available; borg
// always sets it, so a 32..255 byte string uses str8 rather than str16.
func (e *Encoder) encodeStr(s string) error {
	n := len(s)
	switch {
	case n < 32:
		if err := e.writeByte(fixstrPrefix | byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint8:
		if err := e.write2(codeStr8, byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint16:
		e.scratch[0] = codeStr16
		binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
		if err := e.writeN(3); err != nil {
			return err
		}
	case int64(n) <= math.MaxUint32:
		e.scratch[0] = codeStr32
		binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
		if err := e.writeN(5); err != nil {
			return err
		}
	default:
		return errors.New("msgpackx: string longer than 4 GiB")
	}
	_, err := io.WriteString(e.w, s)
	return err
}

func (e *Encoder) encodeBin(b []byte) error {
	n := len(b)
	switch {
	case n <= math.MaxUint8:
		if err := e.write2(codeBin8, byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint16:
		e.scratch[0] = codeBin16
		binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
		if err := e.writeN(3); err != nil {
			return err
		}
	case int64(n) <= math.MaxUint32:
		e.scratch[0] = codeBin32
		binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
		if err := e.writeN(5); err != nil {
			return err
		}
	default:
		return errors.New("msgpackx: bytes longer than 4 GiB")
	}
	_, err := e.w.Write(b)
	return err
}

// encodeTimestamp reproduces msgpack-python's three-way size selection exactly.
//
//	seconds fits in 34 bits and nanoseconds == 0 -> timestamp32, fixext4,  uint32 seconds
//	seconds fits in 34 bits otherwise            -> timestamp64, fixext8,  (nsec<<34)|sec
//	seconds does not fit in 34 bits              -> timestamp96, ext8(12), uint32 nsec + int64 sec
//
// The 34-bit test is on the *unsigned* value, so any negative timestamp - anything
// before 1970 - takes the 96-bit form. Verified against msgpack-python 1.2.1.
func (e *Encoder) encodeTimestamp(t Timestamp) error {
	if t.Nanoseconds >= nanosPerSecond {
		return fmt.Errorf("msgpackx: timestamp nanoseconds out of range: %d", t.Nanoseconds)
	}
	sec := uint64(t.Seconds)
	if t.Seconds >= 0 && sec>>34 == 0 {
		combined := uint64(t.Nanoseconds)<<34 | sec
		if combined&0xffffffff00000000 == 0 {
			// timestamp32: nanoseconds are zero and seconds fit in 32 bits.
			e.scratch[0] = codeFixExt4
			e.scratch[1] = timestampExtByte
			binary.BigEndian.PutUint32(e.scratch[2:], uint32(combined))
			return e.writeN(6)
		}
		// timestamp64
		e.scratch[0] = codeFixExt8
		e.scratch[1] = timestampExtByte
		binary.BigEndian.PutUint64(e.scratch[2:], combined)
		return e.writeN(10)
	}
	// timestamp96
	var buf [15]byte
	buf[0] = codeExt8
	buf[1] = 12
	buf[2] = timestampExtByte
	binary.BigEndian.PutUint32(buf[3:], t.Nanoseconds)
	binary.BigEndian.PutUint64(buf[7:], uint64(t.Seconds))
	_, err := e.w.Write(buf[:])
	return err
}

func (e *Encoder) encodeExt(typ int8, data []byte) error {
	n := len(data)
	switch n {
	case 1:
		if err := e.write2(codeFixExt1, byte(typ)); err != nil {
			return err
		}
	case 2:
		if err := e.write2(codeFixExt2, byte(typ)); err != nil {
			return err
		}
	case 4:
		if err := e.write2(codeFixExt4, byte(typ)); err != nil {
			return err
		}
	case 8:
		if err := e.write2(codeFixExt8, byte(typ)); err != nil {
			return err
		}
	case 16:
		if err := e.write2(codeFixExt16, byte(typ)); err != nil {
			return err
		}
	default:
		switch {
		case n <= math.MaxUint8:
			e.scratch[0], e.scratch[1], e.scratch[2] = codeExt8, byte(n), byte(typ)
			if err := e.writeN(3); err != nil {
				return err
			}
		case n <= math.MaxUint16:
			e.scratch[0] = codeExt16
			binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
			e.scratch[3] = byte(typ)
			if err := e.writeN(4); err != nil {
				return err
			}
		case int64(n) <= math.MaxUint32:
			e.scratch[0] = codeExt32
			binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
			e.scratch[5] = byte(typ)
			if err := e.writeN(6); err != nil {
				return err
			}
		default:
			return errors.New("msgpackx: ext data longer than 4 GiB")
		}
	}
	_, err := e.w.Write(data)
	return err
}

func (e *Encoder) writeArrayHeader(n int) error {
	switch {
	case n < 16:
		return e.writeByte(fixarrayPrefix | byte(n))
	case n <= math.MaxUint16:
		e.scratch[0] = codeArray16
		binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
		return e.writeN(3)
	case int64(n) <= math.MaxUint32:
		e.scratch[0] = codeArray32
		binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
		return e.writeN(5)
	default:
		return errors.New("msgpackx: array longer than 4 Gi elements")
	}
}

func (e *Encoder) writeMapHeader(n int) error {
	switch {
	case n < 16:
		return e.writeByte(fixmapPrefix | byte(n))
	case n <= math.MaxUint16:
		e.scratch[0] = codeMap16
		binary.BigEndian.PutUint16(e.scratch[1:], uint16(n))
		return e.writeN(3)
	case int64(n) <= math.MaxUint32:
		e.scratch[0] = codeMap32
		binary.BigEndian.PutUint32(e.scratch[1:], uint32(n))
		return e.writeN(5)
	default:
		return errors.New("msgpackx: map longer than 4 Gi entries")
	}
}

func (e *Encoder) writeByte(b byte) error {
	e.scratch[0] = b
	return e.writeN(1)
}

func (e *Encoder) write2(a, b byte) error {
	e.scratch[0], e.scratch[1] = a, b
	return e.writeN(2)
}

func (e *Encoder) writeN(n int) error {
	_, err := e.w.Write(e.scratch[:n])
	return err
}
