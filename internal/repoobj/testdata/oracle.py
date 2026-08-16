# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the repository object differential test.
#
# Drives borg's own RepoObj with its own key classes, so borge's envelope is checked
# against the real thing - header layout, AAD construction, slot tags and the metadata
# dict all at once.
#
# Line protocol, hex fields, "-" for empty:
#
#   F <mode> <cryptkeyhex> <idkeyhex> <spec> <robjtype> <idhex> <hexdata>
#     Format an object.  -> OK <hex object>
#
#   P <mode> <cryptkeyhex> <idkeyhex> <robjtype> <idhex> <hexobject>
#     Parse an object.    -> OK <hex plaintext>
#
#   M <mode> <cryptkeyhex> <idkeyhex> <robjtype> <idhex> <hexobject>
#     Parse only the metadata slot.  -> OK <ctype> <clevel> <csize> <size|-> <psize|->
#
#   H <mode> <cryptkeyhex> <idkeyhex> <hexdata>
#     Id hash.            -> OK <hex id>

import sys
import traceback

from borg.crypto.key import (
    AuthenticatedKey,
    Blake3AuthenticatedKey,
    Blake3ChecksumKey,
    ChecksumKey,
)
from borg.helpers.parseformat import CompressionSpec
from borg.repoobj import RepoObj


def unhex(s):
    return b"" if s == "-" else bytes.fromhex(s)


def enhex(b):
    return bytes(b).hex() or "-"


def make_key(mode, crypt_key, id_key):
    """Build a borg key with the given material.

    The key classes normally load their material from a repository; here it is injected
    directly, so this test covers the envelope rather than the key storage (stage 4).
    """
    if mode == "none-sha256":
        return ChecksumKey(None)
    if mode == "none-blake3":
        return Blake3ChecksumKey(None)
    if mode == "authenticated-sha256":
        k = AuthenticatedKey(None)
        k.crypt_key, k.id_key = crypt_key, id_key
        k._tag_key = None  # force re-derivation from the injected crypt_key
        return k
    if mode == "authenticated-blake3":
        k = Blake3AuthenticatedKey(None)
        k.crypt_key, k.id_key = crypt_key, id_key
        k._tag_key = None
        return k
    raise ValueError(f"unsupported mode {mode!r}")


def make_repoobj(mode, crypt_key, id_key, spec=None):
    ro = RepoObj(make_key(mode, crypt_key, id_key))
    if spec is not None:
        ro.compressor = CompressionSpec(spec).compressor
    return ro


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        cmd = p[0]
        if cmd == "F":
            _, mode, ck, ik, spec, robjtype, idhex, hexdata = p
            ro = make_repoobj(mode, unhex(ck), unhex(ik), spec)
            obj = ro.format(unhex(idhex), {}, unhex(hexdata), ro_type=robjtype)
            out = "OK " + enhex(obj)
        elif cmd == "P":
            _, mode, ck, ik, robjtype, idhex, hexobj = p
            ro = make_repoobj(mode, unhex(ck), unhex(ik))
            _meta, data = ro.parse(unhex(idhex), unhex(hexobj), ro_type=robjtype)
            out = "OK " + enhex(data)
        elif cmd == "M":
            _, mode, ck, ik, robjtype, idhex, hexobj = p
            ro = make_repoobj(mode, unhex(ck), unhex(ik))
            meta = ro.parse_meta(unhex(idhex), unhex(hexobj), ro_type=robjtype)
            out = " ".join([
                "OK",
                str(meta["ctype"]),
                str(meta["clevel"]),
                str(meta["csize"]),
                str(meta.get("size", "-")),
                str(meta.get("psize", "-")),
            ])
        elif cmd == "H":
            _, mode, ck, ik, hexdata = p
            out = "OK " + enhex(make_key(mode, unhex(ck), unhex(ik)).id_hash(unhex(hexdata)))
        else:
            out = f"ERR unknown command {cmd!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-400:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
