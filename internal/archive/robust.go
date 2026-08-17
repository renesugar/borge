// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of valid_msgpacked_dict and RobustUnpacker in borg's
// src/borg/archive.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package archive

import (
	"errors"
	"fmt"

	"github.com/renesugar/borge/internal/msgpackx"
)

// RobustUnpacker reads an item stream that may be damaged.
//
// # Why a normal unpacker is not enough
//
// The item stream has no framing: items are msgpack maps written back to back, and the
// decoder's position is the only thing that says where one ends and the next begins. Lose
// a chunk in the middle and that position is meaningless - every byte after the gap is
// read at the wrong offset, so a single missing chunk destroys the *rest of the archive*
// rather than the files it held.
//
// Resynchronising recovers the rest. After a gap the reader scans forward one byte at a
// time for something that both looks like a packed item map and decodes to one whose
// first key is a key items actually have. That is a heuristic and it can be fooled - file
// contents are not in this stream, but an item's own bytes could coincidentally look like
// a map header - so the caller supplies a validator as a second opinion.
//
// This is what `check --repair` uses. Normal reading uses the plain streaming unpacker in
// stream.go, which fails loudly instead of guessing.
type RobustUnpacker struct {
	// itemKeys holds each known item key as its msgpack encoding, so a candidate can be
	// recognised by a byte comparison rather than by decoding first.
	itemKeys [][]byte
	validate func(*msgpackx.Map) bool

	buf []byte
	off int
	// resync is set until a plausible item has been found again.
	resync bool
}

// NewRobustUnpacker returns an unpacker that resynchronises after damage.
//
// itemKeys is the set of key names an item may start with - the repository's item_keys,
// which is why the manifest records them. validate is asked whether a decoded candidate
// really is an item; passing nil accepts any map.
func NewRobustUnpacker(itemKeys []string, validate func(*msgpackx.Map) bool) (*RobustUnpacker, error) {
	u := &RobustUnpacker{validate: validate}
	for _, k := range itemKeys {
		packed, err := msgpackx.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("archive: cannot encode item key %q: %w", k, err)
		}
		u.itemKeys = append(u.itemKeys, packed)
	}
	return u, nil
}

// Resync tells the unpacker that the stream has a gap: everything buffered is discarded
// and the next read scans for a fresh item boundary.
//
// Discarding rather than keeping the buffer is deliberate. What is buffered is the tail
// of a value whose length prefix pointed into the missing bytes, so it cannot be
// interpreted; keeping it would only give the scan more chances to be fooled.
func (u *RobustUnpacker) Resync() {
	u.buf = u.buf[:0]
	u.off = 0
	u.resync = true
}

// Feed adds stream bytes.
func (u *RobustUnpacker) Feed(data []byte) {
	if u.off > 0 && u.off >= len(u.buf) {
		u.buf = u.buf[:0]
		u.off = 0
	} else if u.off >= compactThreshold {
		n := copy(u.buf, u.buf[u.off:])
		u.buf = u.buf[:n]
		u.off = 0
	}
	u.buf = append(u.buf, data...)
}

// Pending is how many bytes are buffered but not yet decoded.
func (u *RobustUnpacker) Pending() int { return len(u.buf) - u.off }

// Resyncing reports whether the unpacker is still looking for a boundary.
func (u *RobustUnpacker) Resyncing() bool { return u.resync }

// Next returns the next item. ok is false when more data is needed.
func (u *RobustUnpacker) Next() (*msgpackx.Map, bool, error) {
	if !u.resync {
		if u.Pending() == 0 {
			return nil, false, nil
		}
		d := msgpackx.NewDecoder(u.buf[u.off:])
		v, err := d.Decode()
		if err != nil {
			if errors.Is(err, msgpackx.ErrShortBuffer) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("archive: item stream is corrupt: %w", err)
		}
		u.off += d.Pos()
		m, isMap := v.(*msgpackx.Map)
		if !isMap {
			return nil, false, fmt.Errorf("archive: item stream holds a %T, want a map", v)
		}
		return m, true, nil
	}

	// Resyncing: walk forward looking for a plausible item.
	//
	// A rejection near the end of the buffer is only provisional: ValidMsgpackedDict may
	// have said no because the key it needed to compare was not there yet. Advancing past
	// such a position would step over a real boundary that the next Feed completes, so the
	// scan stops there and resumes from the same offset once more data arrives. borg
	// avoids the problem by rescanning its whole buffer on every call; keeping the offset
	// and being careful at the end costs one comparison and stays linear.
	minEvaluable := u.minEvaluableBytes()
	for u.off < len(u.buf) {
		rest := u.buf[u.off:]
		if !ValidMsgpackedDict(rest, u.itemKeys) {
			if len(rest) < minEvaluable {
				return nil, false, nil // provisional; wait for more
			}
			u.off++
			continue
		}
		d := msgpackx.NewDecoder(rest)
		v, err := d.Decode()
		if err != nil {
			if errors.Is(err, msgpackx.ErrShortBuffer) {
				// It may still be an item once more of the stream arrives. Stop here
				// rather than skipping it, or a real boundary at the end of the buffer
				// would be walked past and lost.
				return nil, false, nil
			}
			u.off++
			continue
		}
		m, isMap := v.(*msgpackx.Map)
		if !isMap || (u.validate != nil && !u.validate(m)) {
			u.off++
			continue
		}
		u.off += d.Pos()
		u.resync = false
		return m, true, nil
	}
	return nil, false, nil
}

// minEvaluableBytes is how much buffer ValidMsgpackedDict needs before a "no" is final:
// the largest map header (3 bytes, for map16) plus the longest key encoding.
func (u *RobustUnpacker) minEvaluableBytes() int {
	longest := 0
	for _, k := range u.itemKeys {
		if len(k) > longest {
			longest = len(k)
		}
	}
	return 3 + longest
}

// ValidMsgpackedDict reports whether data starts with something shaped like a packed item
// map: a fixmap or map16 header, then a string key that is one of the given encodings.
//
// It is a cheap filter in front of a full decode, not a decision. Its whole job is to
// make the byte-at-a-time scan affordable.
func ValidMsgpackedDict(data []byte, packedKeys [][]byte) bool {
	if len(data) == 0 {
		return false
	}

	var offs int
	switch {
	case data[0]&0xF0 == 0x80:
		offs = 1 // fixmap, up to 15 entries
	case data[0] == 0xDE:
		offs = 3 // map16, up to 65535 entries
	default:
		// Not a map. map32 is deliberately not accepted: borg never writes an item with
		// more than 2^16-1 keys, so accepting it would only widen the scan's exposure to
		// false positives.
		return false
	}
	if len(data) <= offs {
		return false
	}

	// The first key has to be a string.
	switch b := data[offs]; {
	case b&0xE0 == 0xA0: // fixstr, up to 31 bytes
	case b == 0xD9 || b == 0xDA || b == 0xDB: // str8, str16, str32
	default:
		return false
	}

	rest := data[offs:]
	for _, key := range packedKeys {
		if len(rest) >= len(key) && string(rest[:len(key)]) == string(key) {
			return true
		}
	}
	return false
}
