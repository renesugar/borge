// SPDX-License-Identifier: Apache-2.0

package msgpackx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrShortBuffer means the input ended in the middle of a value.
var ErrShortBuffer = errors.New("msgpackx: unexpected end of input")

// Unmarshal decodes one value from b and requires that it consumes all of b.
//
// borg reads objects whose length is known exactly (a repository object, a key blob),
// so trailing bytes indicate corruption rather than a second value. Use Decoder to
// read a stream of concatenated values, as the item metadata stream needs.
func Unmarshal(b []byte) (any, error) {
	d := NewDecoder(b)
	v, err := d.Decode()
	if err != nil {
		return nil, err
	}
	if rest := d.Remaining(); rest != 0 {
		return nil, fmt.Errorf("msgpackx: %d trailing byte(s) after the decoded value", rest)
	}
	return v, nil
}

// Decoder reads msgpack values from a byte slice.
//
// It decodes from a slice rather than an io.Reader on purpose: borg's objects are read
// whole (a pack range, a key blob, a metadata stream chunk), and slice access lets the
// decoder bound every length against the remaining input before allocating. A
// streaming decoder has to trust the declared length, which is what turns a corrupt
// length field into an out-of-memory kill.
type Decoder struct {
	buf      []byte
	pos      int
	maxDepth int
}

// NewDecoder returns a Decoder reading from buf.
func NewDecoder(buf []byte) *Decoder {
	return &Decoder{buf: buf, maxDepth: DefaultMaxDepth}
}

// SetMaxDepth overrides the nesting limit. A depth of zero or less restores the
// default.
func (d *Decoder) SetMaxDepth(n int) {
	if n <= 0 {
		n = DefaultMaxDepth
	}
	d.maxDepth = n
}

// Remaining reports how many bytes are left undecoded.
func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

// Pos reports how many bytes have been consumed.
func (d *Decoder) Pos() int { return d.pos }

// More reports whether another value is available.
func (d *Decoder) More() bool { return d.pos < len(d.buf) }

// Decode reads the next value. It returns io.EOF exactly when the input is already
// fully consumed, so a caller can loop over a stream of concatenated values.
func (d *Decoder) Decode() (any, error) {
	if d.pos >= len(d.buf) {
		return nil, io.EOF
	}
	return d.decode(0)
}

func (d *Decoder) decode(depth int) (any, error) {
	if depth > d.maxDepth {
		return nil, fmt.Errorf("msgpackx: nesting deeper than %d", d.maxDepth)
	}
	c, err := d.readByte()
	if err != nil {
		return nil, err
	}

	switch {
	case c <= fixintPositiveMax:
		return int64(c), nil
	case c >= fixintNegativeMin:
		return int64(int8(c)), nil
	case c&0xf0 == fixmapPrefix:
		return d.decodeMap(int(c&0x0f), depth)
	case c&0xf0 == fixarrayPrefix:
		return d.decodeArray(int(c&0x0f), depth)
	case c&0xe0 == fixstrPrefix:
		return d.decodeStr(int(c & 0x1f))
	}

	switch c {
	case codeNil:
		return nil, nil
	case codeFalse:
		return false, nil
	case codeTrue:
		return true, nil

	case codeUint8:
		b, err := d.readByte()
		return int64(b), err
	case codeUint16:
		b, err := d.readN(2)
		if err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint16(b)), nil
	case codeUint32:
		b, err := d.readN(4)
		if err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint32(b)), nil
	case codeUint64:
		b, err := d.readN(8)
		if err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint64(b)
		// Python has arbitrary-precision ints, so it never has to choose here. Go does:
		// stay in int64 when it fits, so callers get one type for almost everything,
		// and fall back to uint64 rather than overflowing into a negative number.
		if n <= math.MaxInt64 {
			return int64(n), nil
		}
		return n, nil

	case codeInt8:
		b, err := d.readByte()
		return int64(int8(b)), err
	case codeInt16:
		b, err := d.readN(2)
		if err != nil {
			return nil, err
		}
		return int64(int16(binary.BigEndian.Uint16(b))), nil
	case codeInt32:
		b, err := d.readN(4)
		if err != nil {
			return nil, err
		}
		return int64(int32(binary.BigEndian.Uint32(b))), nil
	case codeInt64:
		b, err := d.readN(8)
		if err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), nil

	case codeFloat32:
		b, err := d.readN(4)
		if err != nil {
			return nil, err
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case codeFloat64:
		b, err := d.readN(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil

	case codeStr8:
		n, err := d.readLen(1)
		if err != nil {
			return nil, err
		}
		return d.decodeStr(n)
	case codeStr16:
		n, err := d.readLen(2)
		if err != nil {
			return nil, err
		}
		return d.decodeStr(n)
	case codeStr32:
		n, err := d.readLen(4)
		if err != nil {
			return nil, err
		}
		return d.decodeStr(n)

	case codeBin8:
		n, err := d.readLen(1)
		if err != nil {
			return nil, err
		}
		return d.decodeBin(n)
	case codeBin16:
		n, err := d.readLen(2)
		if err != nil {
			return nil, err
		}
		return d.decodeBin(n)
	case codeBin32:
		n, err := d.readLen(4)
		if err != nil {
			return nil, err
		}
		return d.decodeBin(n)

	case codeArray16:
		n, err := d.readLen(2)
		if err != nil {
			return nil, err
		}
		return d.decodeArray(n, depth)
	case codeArray32:
		n, err := d.readLen(4)
		if err != nil {
			return nil, err
		}
		return d.decodeArray(n, depth)

	case codeMap16:
		n, err := d.readLen(2)
		if err != nil {
			return nil, err
		}
		return d.decodeMap(n, depth)
	case codeMap32:
		n, err := d.readLen(4)
		if err != nil {
			return nil, err
		}
		return d.decodeMap(n, depth)

	case codeFixExt1:
		return d.decodeExt(1)
	case codeFixExt2:
		return d.decodeExt(2)
	case codeFixExt4:
		return d.decodeExt(4)
	case codeFixExt8:
		return d.decodeExt(8)
	case codeFixExt16:
		return d.decodeExt(16)
	case codeExt8:
		n, err := d.readLen(1)
		if err != nil {
			return nil, err
		}
		return d.decodeExt(n)
	case codeExt16:
		n, err := d.readLen(2)
		if err != nil {
			return nil, err
		}
		return d.decodeExt(n)
	case codeExt32:
		n, err := d.readLen(4)
		if err != nil {
			return nil, err
		}
		return d.decodeExt(n)

	default:
		// 0xc1 is the only byte the specification leaves unassigned.
		return nil, fmt.Errorf("msgpackx: invalid format byte 0x%02x at offset %d", c, d.pos-1)
	}
}

func (d *Decoder) decodeStr(n int) (any, error) {
	b, err := d.readN(n)
	if err != nil {
		return nil, err
	}
	// The bytes are kept verbatim: a Go string is exactly the wire form of a Python
	// surrogate-escaped str, so no decoding step is needed. See the package doc.
	return string(b), nil
}

func (d *Decoder) decodeBin(n int) (any, error) {
	b, err := d.readN(n)
	if err != nil {
		return nil, err
	}
	// Copy: the result must not alias the caller's buffer, which may be a reused
	// read buffer or a memory-mapped pack file.
	out := make([]byte, n)
	copy(out, b)
	return out, nil
}

func (d *Decoder) decodeArray(n, depth int) (any, error) {
	if err := d.checkCount(n, 1); err != nil {
		return nil, err
	}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		v, err := d.decode(depth + 1)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (d *Decoder) decodeMap(n, depth int) (any, error) {
	if err := d.checkCount(n, 2); err != nil {
		return nil, err
	}
	m := &Map{entries: make([]MapEntry, 0, n)}
	for i := 0; i < n; i++ {
		k, err := d.decode(depth + 1)
		if err != nil {
			return nil, err
		}
		v, err := d.decode(depth + 1)
		if err != nil {
			return nil, err
		}
		// Wire order is preserved rather than deduplicated into a Go map: borg's
		// strict_map_key=False means keys need not be strings, and a duplicate key
		// would be a sign of corruption worth surfacing rather than silently losing.
		m.entries = append(m.entries, MapEntry{Key: k, Value: v})
	}
	return m, nil
}

func (d *Decoder) decodeExt(n int) (any, error) {
	t, err := d.readByte()
	if err != nil {
		return nil, err
	}
	body, err := d.readN(n)
	if err != nil {
		return nil, err
	}
	typ := int8(t)
	if typ == TimestampExtType {
		return decodeTimestamp(body)
	}
	data := make([]byte, n)
	copy(data, body)
	return Ext{Type: typ, Data: data}, nil
}

// decodeTimestamp reads the three timestamp encodings defined by the msgpack spec.
func decodeTimestamp(b []byte) (Timestamp, error) {
	switch len(b) {
	case 4:
		return Timestamp{Seconds: int64(binary.BigEndian.Uint32(b))}, nil
	case 8:
		v := binary.BigEndian.Uint64(b)
		nsec := uint32(v >> 34)
		sec := int64(v & 0x3ffffffff)
		if nsec >= nanosPerSecond {
			return Timestamp{}, fmt.Errorf("msgpackx: timestamp64 nanoseconds out of range: %d", nsec)
		}
		return Timestamp{Seconds: sec, Nanoseconds: nsec}, nil
	case 12:
		nsec := binary.BigEndian.Uint32(b[:4])
		sec := int64(binary.BigEndian.Uint64(b[4:]))
		if nsec >= nanosPerSecond {
			return Timestamp{}, fmt.Errorf("msgpackx: timestamp96 nanoseconds out of range: %d", nsec)
		}
		return Timestamp{Seconds: sec, Nanoseconds: nsec}, nil
	default:
		return Timestamp{}, fmt.Errorf("msgpackx: timestamp ext of unsupported length %d", len(b))
	}
}

// checkCount rejects an element count that cannot possibly be satisfied by the
// remaining input, before anything is allocated for it. Each element needs at least
// one byte, so a container claiming more elements than there are bytes left is
// corrupt - and without this check a crafted 4-byte length would allocate 4 GiB.
func (d *Decoder) checkCount(n, perElement int) error {
	if n < 0 {
		return fmt.Errorf("msgpackx: negative element count %d", n)
	}
	if need := n * perElement; need > len(d.buf)-d.pos {
		return fmt.Errorf("msgpackx: container declares %d element(s) but only %d byte(s) remain: %w",
			n, len(d.buf)-d.pos, ErrShortBuffer)
	}
	return nil
}

func (d *Decoder) readByte() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, ErrShortBuffer
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

func (d *Decoder) readN(n int) ([]byte, error) {
	if n < 0 || n > len(d.buf)-d.pos {
		return nil, fmt.Errorf("msgpackx: need %d byte(s) at offset %d, %d remain: %w",
			n, d.pos, len(d.buf)-d.pos, ErrShortBuffer)
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

// readLen reads a big-endian length field of the given width.
func (d *Decoder) readLen(width int) (int, error) {
	b, err := d.readN(width)
	if err != nil {
		return 0, err
	}
	var n uint64
	switch width {
	case 1:
		n = uint64(b[0])
	case 2:
		n = uint64(binary.BigEndian.Uint16(b))
	case 4:
		n = uint64(binary.BigEndian.Uint32(b))
	}
	// On a 32-bit build an int cannot hold a 4-byte length; reject rather than wrap.
	if n > uint64(math.MaxInt) {
		return 0, fmt.Errorf("msgpackx: length %d exceeds this platform's int", n)
	}
	return int(n), nil
}
