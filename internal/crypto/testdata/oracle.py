# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the crypto differential test (differential_test.go).
#
# borg's AEAD comes from OpenSSL, so driving borg here means checking borge's AES-OCB
# against OpenSSL's - which is the check that actually matters for the highest-risk
# component in the port. RFC vectors prove the algorithm; this proves the envelope,
# the AAD construction and the byte layout borg really uses.
#
# Line protocol, hex fields, "-" for empty:
#
#   E <suite> <header_len> <aad_offset> <keyhex> <ivhex> <headerhex> <aadhex> <plainhex>
#     -> OK <envelope hex>            (header || tag || ciphertext)
#
#   D <suite> <header_len> <aad_offset> <keyhex> <ivhex> <aadhex> <envelopehex>
#     -> OK <plaintext hex>
#
#   H <fn> <keyhex> <datahex>
#     -> OK <digest hex>   (fn: hmac_sha256 | blake2b_256 | blake2b_128 | blake3_keyed)
#
# Errors come back as "ERR <message>".

import sys
import traceback

from blake3 import blake3

from borg.crypto.low_level import AES256_OCB, CHACHA20_POLY1305, hmac_sha256, blake2b_256, blake2b_128

SUITES = {"aes256-ocb": AES256_OCB, "chacha20-poly1305": CHACHA20_POLY1305}


def unhex(s):
    return b"" if s == "-" else bytes.fromhex(s)


def enhex(b):
    return b.hex() or "-"


def do_encrypt(suite, header_len, aad_offset, key, iv, header, aad, plain):
    cipher = SUITES[suite](key, iv=iv, header_len=header_len, aad_offset=aad_offset)
    return "OK " + enhex(cipher.encrypt(plain, header=header, iv=iv, aad=aad))


def do_decrypt(suite, header_len, aad_offset, key, iv, aad, envelope):
    cipher = SUITES[suite](key, iv=iv, header_len=header_len, aad_offset=aad_offset)
    return "OK " + enhex(cipher.decrypt(envelope, aad=aad))


def do_hash(fn, key, data):
    if fn == "hmac_sha256":
        return "OK " + enhex(hmac_sha256(key, data))
    if fn == "blake2b_256":
        return "OK " + enhex(blake2b_256(key, data))
    if fn == "blake2b_128":
        return "OK " + enhex(blake2b_128(data))
    if fn == "blake3_keyed":
        # borg calls blake3(data, key=id_key).digest(length=32); see ID_BLAKE3_256 in
        # src/borg/crypto/key.py. max_threads only affects speed, not the digest.
        return "OK " + enhex(blake3(data, key=key).digest(length=32))
    return f"ERR unknown hash {fn!r}"


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        if p[0] == "E":
            _, suite, hlen, aoff, key, iv, header, aad, plain = p
            out = do_encrypt(suite, int(hlen), int(aoff), unhex(key), unhex(iv),
                             unhex(header), unhex(aad), unhex(plain))
        elif p[0] == "D":
            _, suite, hlen, aoff, key, iv, aad, env = p
            out = do_decrypt(suite, int(hlen), int(aoff), unhex(key), unhex(iv),
                             unhex(aad), unhex(env))
        elif p[0] == "H":
            _, fn, key, data = p
            out = do_hash(fn, unhex(key), unhex(data))
        else:
            out = f"ERR unknown command {p[0]!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-300:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
