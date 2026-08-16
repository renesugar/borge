// SPDX-License-Identifier: Apache-2.0

// Package ocb implements OCB3 authenticated encryption, as specified in RFC 7253.
//
// # Why this exists
//
// borg's default encryption mode is aes256-ocb (key type 0x10), which it gets from
// OpenSSL's EVP_aes_256_ocb. Go has no OCB: not in crypto/cipher, not in
// golang.org/x/crypto. borge cannot interoperate with borg's default mode without one,
// so this is a from-scratch implementation of the RFC.
//
// docs/PORTING_PLAN.md names this the highest-risk component in the port, and that
// assessment stands. A subtle bug here is silent - it produces ciphertext that looks
// fine and cannot be decrypted later, or worse, accepts data it should reject. The
// code below is therefore written for reviewability rather than speed: it follows the
// RFC's structure and naming (L_*, L_$, L_i, Offset, Checksum, Stretch, bottom) so it
// can be read side by side with section 4 of the specification.
//
// # What it is checked against
//
//   - Every test vector in RFC 7253 section 4 (AES-128, tag lengths 128/96/64) and the
//     RFC's iterative key-derivation test.
//   - borg's own AES-256-OCB output, over randomised sizes, via the differential test
//     in internal/crypto - which means against OpenSSL, the implementation borg uses.
//
// # Scope
//
// Only what borg needs. The nonce is 1..15 bytes (borg uses 12) and the tag is 16
// bytes; other tag lengths are supported because the RFC vectors exercise them.
// There is no streaming interface: borg encrypts whole repository objects, bounded by
// MAX_DATA_SIZE.
//
// # Nonce reuse
//
// OCB, like every OCB-family mode, fails catastrophically if a (key, nonce) pair is
// ever reused: it leaks the XOR of the plaintexts and can enable forgery. This package
// cannot detect that - the caller owns nonce management. borg derives a per-message
// session key and counter for exactly this reason; see internal/crypto/key.
package ocb

import (
	"crypto/cipher"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/bits"
)

const (
	// BlockSize is AES's block size, and OCB's.
	BlockSize = 16
	// MaxTagSize is the largest tag OCB3 defines.
	MaxTagSize = 16
	// MaxNonceSize is the largest nonce OCB3 accepts: the nonce must fit in 120 bits
	// alongside the length-encoding prefix.
	MaxNonceSize = 15
)

// ErrOpen is returned when authentication fails. It deliberately carries no detail
// about *why*: distinguishing a bad tag from malformed input would hand an attacker a
// decryption oracle.
var ErrOpen = errors.New("ocb: message authentication failed")

type ocb struct {
	block     cipher.Block
	tagSize   int
	nonceSize int

	// The key-dependent L values of RFC 7253 section 4.1.
	//
	//	lStar   = L_*  = ENCIPHER(K, zeros(128))
	//	lDollar = L_$  = double(L_*)
	//	l[0]    = L_0  = double(L_$)
	//	l[i]    = L_i  = double(L_{i-1})
	//
	// l is grown on demand. l[i] is consumed at block number 2^i, so a message of n
	// blocks needs indices up to log2(n) - 32 entries covers any message that fits in
	// memory.
	lStar   [BlockSize]byte
	lDollar [BlockSize]byte
	l       [][BlockSize]byte
}

// New returns an OCB3 AEAD using the given block cipher, with a 16-byte tag and a
// 12-byte nonce - borg's parameters.
func New(block cipher.Block) (cipher.AEAD, error) {
	return NewWithSizes(block, 12, MaxTagSize)
}

// NewWithSizes returns an OCB3 AEAD with explicit nonce and tag sizes. The RFC's test
// vectors use tag sizes other than 16, which is the only reason this is exported.
func NewWithSizes(block cipher.Block, nonceSize, tagSize int) (cipher.AEAD, error) {
	if block.BlockSize() != BlockSize {
		return nil, fmt.Errorf("ocb: block cipher must have a %d byte block size, got %d",
			BlockSize, block.BlockSize())
	}
	if nonceSize < 1 || nonceSize > MaxNonceSize {
		return nil, fmt.Errorf("ocb: nonce size must be 1..%d bytes, got %d", MaxNonceSize, nonceSize)
	}
	if tagSize < 1 || tagSize > MaxTagSize {
		return nil, fmt.Errorf("ocb: tag size must be 1..%d bytes, got %d", MaxTagSize, tagSize)
	}

	o := &ocb{block: block, tagSize: tagSize, nonceSize: nonceSize}

	// L_* = ENCIPHER(K, zeros(128)); L_$ = double(L_*); L_0 = double(L_$).
	var zero [BlockSize]byte
	o.block.Encrypt(o.lStar[:], zero[:])
	o.lDollar = double(o.lStar)
	o.l = append(o.l, double(o.lDollar))
	return o, nil
}

func (o *ocb) NonceSize() int { return o.nonceSize }
func (o *ocb) Overhead() int  { return o.tagSize }

// getL returns L_i, extending the precomputed table as needed.
func (o *ocb) getL(i int) [BlockSize]byte {
	for len(o.l) <= i {
		o.l = append(o.l, double(o.l[len(o.l)-1]))
	}
	return o.l[i]
}

// double implements the doubling operation of RFC 7253 section 2: a left shift by one
// bit in GF(2^128), reducing modulo the polynomial x^128 + x^7 + x^2 + x + 1 when the
// top bit was set.
//
// The reduction is applied through a mask rather than a branch so it takes the same
// time either way; the value being doubled is key-dependent, so a timing difference
// would leak information about the key.
func double(s [BlockSize]byte) [BlockSize]byte {
	// 0xff when the top bit of s is set, 0x00 otherwise, computed by arithmetic shift.
	mask := byte(int8(s[0]) >> 7)

	var out [BlockSize]byte
	for i := 0; i < BlockSize-1; i++ {
		out[i] = s[i]<<1 | s[i+1]>>7
	}
	out[BlockSize-1] = s[BlockSize-1] << 1
	out[BlockSize-1] ^= 0x87 & mask
	return out
}

// initOffset computes Offset_0 from the nonce, following RFC 7253 section 4.2.
//
//	Nonce   = num2str(TAGLEN mod 128, 7) || zeros(120 - bitlen(N)) || 1 || N
//	bottom  = str2num(Nonce[123..128])          (the low 6 bits)
//	Ktop    = ENCIPHER(K, Nonce[1..122] || zeros(6))
//	Stretch = Ktop || (Ktop[1..64] xor Ktop[9..72])
//	Offset_0 = Stretch[1+bottom..128+bottom]    (a 128-bit window, bit-aligned)
//
// The bit-level window is the part most likely to be got wrong, because it is the only
// place in OCB that is not byte-aligned.
func (o *ocb) initOffset(nonce []byte) [BlockSize]byte {
	var n [BlockSize]byte

	// The tag length in bits, mod 128, occupies the top 7 bits of the first byte.
	// For the 16-byte tag borg uses this is zero.
	n[0] = byte(((o.tagSize * 8) % 128) << 1)
	// A single 1 bit immediately before the nonce marks where the nonce starts, which
	// is what keeps nonces of different lengths from colliding.
	n[BlockSize-1-len(nonce)] |= 1
	copy(n[BlockSize-len(nonce):], nonce)

	bottom := int(n[BlockSize-1] & 0x3f)

	ktopIn := n
	ktopIn[BlockSize-1] &^= 0x3f // zero the low 6 bits
	var ktop [BlockSize]byte
	o.block.Encrypt(ktop[:], ktopIn[:])

	// Stretch is 24 bytes: Ktop, then the first 8 bytes of Ktop XORed with Ktop shifted
	// one byte along.
	var stretch [BlockSize + 8]byte
	copy(stretch[:BlockSize], ktop[:])
	for i := 0; i < 8; i++ {
		stretch[BlockSize+i] = ktop[i] ^ ktop[i+1]
	}

	// Take the 128-bit window starting at bit `bottom`. bottom <= 63, so the window
	// ends at bit 63+128 = 191 = byte 23, inside Stretch.
	var offset [BlockSize]byte
	byteShift, bitShift := bottom/8, uint(bottom%8)
	for i := 0; i < BlockSize; i++ {
		hi := stretch[byteShift+i] << bitShift
		// Go defines a shift of 8 on a byte as producing 0, which is exactly what is
		// wanted when bitShift is 0 - no special case needed.
		lo := stretch[byteShift+i+1] >> (8 - bitShift)
		offset[i] = hi | lo
	}
	return offset
}

// hashAD computes HASH(K, A) from RFC 7253 section 4.1: the associated data's
// contribution to the tag. It is independent of the nonce and of the plaintext.
func (o *ocb) hashAD(ad []byte) [BlockSize]byte {
	var sum, offset, tmp, enc [BlockSize]byte

	block := 1
	for len(ad) >= BlockSize {
		offset = xorBlock(offset, o.getL(bits.TrailingZeros(uint(block))))
		xorBytes(tmp[:], ad[:BlockSize], offset[:])
		o.block.Encrypt(enc[:], tmp[:])
		sum = xorBlock(sum, enc)
		ad = ad[BlockSize:]
		block++
	}

	if len(ad) > 0 {
		// A partial final block is padded with a 1 bit then zeros, which is what keeps
		// it distinct from a full block of the same content.
		offset = xorBlock(offset, o.lStar)
		tmp = [BlockSize]byte{}
		copy(tmp[:], ad)
		tmp[len(ad)] = 0x80
		tmp = xorBlock(tmp, offset)
		o.block.Encrypt(enc[:], tmp[:])
		sum = xorBlock(sum, enc)
	}
	return sum
}

// Seal encrypts and authenticates plaintext, appending the result to dst.
//
// The output layout is Go's convention, ciphertext || tag. borg's envelope puts the
// tag first; that rearrangement belongs to the envelope layer, not here.
//
// dst must not overlap plaintext. The standard library detects that with an
// unsafe-based alias check; this package has no unsafe and does not, so callers pass
// nil (as borge does) or a buffer they know is separate.
func (o *ocb) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	if len(nonce) != o.nonceSize {
		panic(fmt.Sprintf("ocb: nonce must be %d bytes, got %d", o.nonceSize, len(nonce)))
	}

	ret, out := sliceForAppend(dst, len(plaintext)+o.tagSize)

	offset := o.initOffset(nonce)
	var checksum, tmp [BlockSize]byte

	p, c := plaintext, out
	block := 1
	for len(p) >= BlockSize {
		// Offset_i = Offset_{i-1} xor L_{ntz(i)}
		// C_i      = Offset_i xor ENCIPHER(K, P_i xor Offset_i)
		// Checksum_i = Checksum_{i-1} xor P_i
		offset = xorBlock(offset, o.getL(bits.TrailingZeros(uint(block))))
		xorBytes(tmp[:], p[:BlockSize], offset[:])
		o.block.Encrypt(c[:BlockSize], tmp[:])
		xorBytes(c[:BlockSize], c[:BlockSize], offset[:])
		xorBytes(checksum[:], checksum[:], p[:BlockSize])
		p, c = p[BlockSize:], c[BlockSize:]
		block++
	}

	if len(p) > 0 {
		// The final partial block is encrypted with a keystream pad rather than the
		// block cipher directly, so the ciphertext keeps the plaintext's length.
		offset = xorBlock(offset, o.lStar)
		var pad [BlockSize]byte
		o.block.Encrypt(pad[:], offset[:])
		for i := range p {
			c[i] = p[i] ^ pad[i]
		}
		// Checksum_* = Checksum_m xor (P_* || 1 || zeros)
		for i := range p {
			checksum[i] ^= p[i]
		}
		checksum[len(p)] ^= 0x80
		c = c[len(p):]
	}

	// Tag = ENCIPHER(K, Checksum_* xor Offset_* xor L_$) xor HASH(K, A)
	tmp = xorBlock(xorBlock(checksum, offset), o.lDollar)
	var tag [BlockSize]byte
	o.block.Encrypt(tag[:], tmp[:])
	tag = xorBlock(tag, o.hashAD(additionalData))
	copy(c, tag[:o.tagSize])

	return ret
}

// Open authenticates and decrypts ciphertext, appending the plaintext to dst.
//
// On authentication failure it returns ErrOpen and no plaintext, and it zeroes any
// bytes it had already written into dst - releasing unauthenticated plaintext is the
// classic way to turn an AEAD into a decryption oracle.
//
// dst must not overlap ciphertext; see Seal.
func (o *ocb) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	if len(nonce) != o.nonceSize {
		return nil, fmt.Errorf("ocb: nonce must be %d bytes, got %d", o.nonceSize, len(nonce))
	}
	if len(ciphertext) < o.tagSize {
		return nil, ErrOpen
	}

	tag := ciphertext[len(ciphertext)-o.tagSize:]
	ciphertext = ciphertext[:len(ciphertext)-o.tagSize]

	ret, out := sliceForAppend(dst, len(ciphertext))

	offset := o.initOffset(nonce)
	var checksum, tmp [BlockSize]byte

	c, p := ciphertext, out
	block := 1
	for len(c) >= BlockSize {
		// P_i = Offset_i xor DECIPHER(K, C_i xor Offset_i)
		offset = xorBlock(offset, o.getL(bits.TrailingZeros(uint(block))))
		xorBytes(tmp[:], c[:BlockSize], offset[:])
		o.block.Decrypt(p[:BlockSize], tmp[:])
		xorBytes(p[:BlockSize], p[:BlockSize], offset[:])
		xorBytes(checksum[:], checksum[:], p[:BlockSize])
		c, p = c[BlockSize:], p[BlockSize:]
		block++
	}

	if len(c) > 0 {
		// The partial block uses the *encryption* direction here too: it is a keystream
		// pad, not a block decryption.
		offset = xorBlock(offset, o.lStar)
		var pad [BlockSize]byte
		o.block.Encrypt(pad[:], offset[:])
		for i := range c {
			p[i] = c[i] ^ pad[i]
		}
		for i := range c {
			checksum[i] ^= p[i]
		}
		checksum[len(c)] ^= 0x80
	}

	tmp = xorBlock(xorBlock(checksum, offset), o.lDollar)
	var want [BlockSize]byte
	o.block.Encrypt(want[:], tmp[:])
	want = xorBlock(want, o.hashAD(additionalData))

	if subtle.ConstantTimeCompare(want[:o.tagSize], tag) != 1 {
		// Do not hand back plaintext that failed authentication, and do not leave it in
		// the caller's buffer either.
		for i := range out {
			out[i] = 0
		}
		return nil, ErrOpen
	}
	return ret, nil
}

func xorBlock(a, b [BlockSize]byte) [BlockSize]byte {
	var out [BlockSize]byte
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func xorBytes(dst, a, b []byte) {
	for i := range dst {
		dst[i] = a[i] ^ b[i]
	}
}

// sliceForAppend grows in by n bytes, reusing capacity when it is there. Same helper
// the standard library's AEAD implementations use.
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	tail = head[len(in):]
	return
}
