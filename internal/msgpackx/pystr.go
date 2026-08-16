// SPDX-License-Identifier: Apache-2.0

package msgpackx

import (
	"fmt"
	"unicode/utf8"
)

// Key comparison, reproducing CPython's ordering rules for the key types borg puts in
// a StableDict. This is not a general Python comparison - it covers str, bytes, int
// and bool, and reports an error for anything else rather than guessing, because a
// wrong order silently changes packed bytes and therefore chunk ids.

// keysEqual reports whether two map keys are the same key, matching Python's dict
// semantics. Note that str and bytes are never equal to each other in Python 3, which
// is why the type switch does not fall through to a generic comparison.
func keysEqual(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	default:
		// int64/uint64/bool and anything else comparable.
		na, aok := asInt(a)
		nb, bok := asInt(b)
		if aok && bok {
			return na == nb
		}
		return a == b
	}
}

// keyLess reports whether key a sorts before key b, using Python's rules.
func keyLess(a, b any) (bool, error) {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		if !ok {
			return false, mixedKeyError(a, b)
		}
		return comparePyStr(x, y) < 0, nil
	case []byte:
		y, ok := b.([]byte)
		if !ok {
			return false, mixedKeyError(a, b)
		}
		return compareBytes(x, y) < 0, nil
	default:
		na, aok := asInt(a)
		nb, bok := asInt(b)
		if !aok || !bok {
			return false, mixedKeyError(a, b)
		}
		return na < nb, nil
	}
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		if n > 1<<63-1 {
			// Out of int64 range. borg never uses such a map key; refusing is better
			// than silently wrapping to a negative value and mis-sorting.
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case bool:
		// Python's bool is a subclass of int: False == 0, True == 1.
		if n {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func mixedKeyError(a, b any) error {
	return fmt.Errorf("msgpackx: cannot order map keys of different types (%T and %T); "+
		"Python would raise TypeError here, so borg cannot have written such a map", a, b)
}

func compareBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// comparePyStr compares two strings the way CPython compares str, i.e. by Unicode
// code point, after interpreting each string as Python would after decoding it with
// the surrogateescape error handler.
//
// For strings that are valid UTF-8 this is the same as comparing bytes, because UTF-8
// preserves code point order. It differs precisely when a string contains a byte that
// is not valid UTF-8: Python holds that byte as the lone surrogate U+DC00+b, which
// sorts *above* every character below U+DC00 - including all of ASCII and most of the
// BMP - whereas the raw byte (0x80..0xFF) would sort above ASCII but below any
// multi-byte UTF-8 sequence.
//
// Concretely, comparing "\xff" against "က" (bytes e1 80 80):
//
//	byte order:       0xff > 0xe1          => "\xff" is greater
//	Python order:     U+DCFF > U+1000      => "\xff" is greater  (agrees here)
//
// but comparing "\xc3" against "￿" (bytes ef bf bf):
//
//	byte order:       0xc3 < 0xef          => "\xc3" is smaller
//	Python order:     U+DCC3 < U+FFFF      => "\xc3" is smaller  (agrees)
//
// and comparing "\xff" against "\U0010FFFF" (bytes f4 8f bf bf):
//
//	byte order:       0xff > 0xf4          => "\xff" is greater
//	Python order:     U+DCFF < U+10FFFF    => "\xff" is smaller  (differs)
//
// The last case is reachable in practice: a StableDict of xattr names where one name
// is not valid UTF-8 and another contains an astral character. Rare, but a mis-sort
// there produces a different chunk id for the same content, which would show up as an
// unexplainable interop failure rather than as an obvious bug.
func comparePyStr(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		ra, na := decodePyRune(a)
		rb, nb := decodePyRune(b)
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// decodePyRune decodes the first character of s the way Python's surrogateescape
// decoder sees it: a valid UTF-8 sequence yields its rune, and any byte that is not
// part of one yields the surrogate U+DC00+byte.
func decodePyRune(s string) (rune, int) {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size <= 1 {
		// Not valid UTF-8: Python maps this single byte to a lone surrogate.
		return rune(0xDC00 + rune(s[0])), 1
	}
	return r, size
}
