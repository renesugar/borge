# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the compression differential test (differential_test.go).
#
# A line protocol over stdin/stdout, driven by the Go test. Both directions of the
# stage 1.2 gate go through here: borg compresses and borge decompresses, and borge
# compresses and borg decompresses.
#
#   C <spec> <robj_type> <hex plaintext>
#     -> OK <ctype> <clevel> <size> <csize> <psize|-> <hex compressed>
#
#   D <ctype> <clevel> <size> <psize|-> <hex compressed>
#     -> OK <hex plaintext>
#
# Errors come back as "ERR <message>" so the Go side can name the failing case rather
# than losing it in a traceback.

import sys
import traceback


def unhex(s):
    """Decode a hex field. "-" means empty, so an empty payload does not collapse the
    space-separated field count."""
    return b"" if s == "-" else bytes.fromhex(s)

from borg.compress import Compressor
from borg.helpers.parseformat import CompressionSpec


def do_compress(spec_str, robj_type, data):
    compressor = CompressionSpec(spec_str).compressor
    meta = {"type": robj_type}
    meta, cdata = compressor.compress(meta, data)
    psize = meta.get("psize")
    # "size" is absent for the Auto meta-compressor: its get_meta helper copies only
    # ctype, clevel and csize. Report that faithfully as "-" rather than papering over
    # it, because borge has to cope with the same absence when reading borg's objects.
    size = meta.get("size")
    return " ".join([
        "OK",
        str(meta["ctype"]),
        str(meta["clevel"]),
        str(size) if size is not None else "-",
        str(meta["csize"]),
        str(psize) if psize is not None else "-",
        bytes(cdata).hex() or "-",  # "-" so an empty payload keeps the field count
    ])


def do_decompress(ctype, clevel, size, psize, cdata):
    # This mirrors what RepoObj.parse does: rebuild the two-byte compression header
    # from the authenticated metadata, dispatch on it, and trim to psize first when the
    # payload was size-obfuscated.
    compressor_cls, level = Compressor.detect(bytes((ctype, clevel)))
    compressor = compressor_cls(level=level)
    meta = {"ctype": ctype, "clevel": clevel, "csize": len(cdata)}
    if size is not None:
        meta["size"] = size
    payload = cdata if psize is None else cdata[:psize]
    if psize is not None:
        meta["psize"] = psize
    _, data = compressor.decompress(meta, payload)
    return "OK " + (bytes(data).hex() or "-")


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        parts = line.split(" ")
        if parts[0] == "C":
            _, spec_str, robj_type, hexdata = parts
            out = do_compress(spec_str, robj_type, unhex(hexdata))
        elif parts[0] == "D":
            _, ctype, clevel, size, psize, hexdata = parts
            out = do_decompress(
                int(ctype), int(clevel),
                None if size == "-" else int(size),
                None if psize == "-" else int(psize),
                unhex(hexdata),
            )
        else:
            out = f"ERR unknown command {parts[0]!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-300:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
