// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/helpers/msgpack.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

// Package msgpackx implements the msgpack encoding borg uses, with borg's exact
// settings. It is the foundation of the on-disk format: every metadata dict, item and
// manifest is msgpack, and chunk ids are computed over packed bytes, so the encoding
// is format-visible down to individual bytes.
//
// # borg's settings
//
// borg fixes four msgpack options (src/borg/helpers/msgpack.py) and asserts them at
// every call site rather than letting callers choose:
//
//	use_bin_type    = True                # msgpack 2.0 spec: str and bytes stay apart
//	raw             = False               # unpack str to str, bin to bytes
//	unicode_errors  = "surrogateescape"   # preserve filenames that are not valid UTF-8
//	strict_map_key  = False               # map keys need not be str/bytes
//
// This package hard-codes all four. There is no option to change them: a borge that
// could write with different settings could write repositories borg cannot read.
//
// # Why a hand-written codec
//
// The plan (docs/PORTING_PLAN.md, stage 1.1) left open whether to use an existing Go
// msgpack library. A hand-written codec won on three counts, each of which is a
// correctness requirement rather than a preference:
//
//   - Map key ordering must be controllable exactly, because StableDict's sorted order
//     is format-visible (see Map.Stable).
//   - The str/bin distinction must be explicit and never inferred. Go libraries
//     typically encode string as str and []byte as bin, which happens to match, but
//     they also apply struct-tag reflection rules that would silently reshape data.
//   - The Timestamp extension must produce byte-identical output to msgpack-python's
//     three-way size selection (see Timestamp).
//
// The subset borg uses is small, so the codec is a few hundred lines with no
// dependencies.
//
// # Type mapping
//
//	msgpack            Go                    Python (borg)
//	-------            --                    -------------
//	nil                nil                   None
//	true/false         bool                  bool
//	int family         int64 or uint64       int
//	float32/float64    float64               float
//	str family         string                str
//	bin family         []byte                bytes
//	array family       []any                 list
//	map family         *Map                  dict / StableDict
//	ext -1             Timestamp             msgpack.Timestamp
//	ext other          Ext                   msgpack.ExtType
//
// # Surrogate escapes are the identity mapping
//
// Python cannot hold an arbitrary byte string in a str, so borg decodes non-UTF-8
// filenames with the surrogateescape error handler: each undecodable byte b becomes
// the lone surrogate U+DC00+b, and encoding reverses it. The important consequence,
// verified against msgpack-python 1.2.1, is that the *wire* form of such a str is the
// original bytes, unchanged:
//
//	b"caf\xe9-\xff.txt"  --decode surrogateescape-->  "caf\udce9-\udcff.txt"
//	                     --pack (str family)------->  a a  63 61 66 e9 2d ff 2e 74 78 74
//	                                                       \_________ the original bytes _/
//
// A Go string is already an arbitrary byte sequence, so it maps onto that wire form
// directly. borge needs no surrogate encoding or decoding step at all: reading gives
// the raw bytes, and those bytes are what the operating system wants back when the
// path is recreated. The escape machinery is a Python workaround that Go does not
// need, and the round trip is exact because it is the identity.
//
// The one place the surrogate interpretation still matters is ordering, because Python
// sorts by code point and U+DC80..U+DCFF sort far above any ASCII byte. See
// comparePyStr in pystr.go.
package msgpackx
