# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the chunker differential test (differential_test.go).
#
# Chunk boundaries are the deduplication format, so the gate is byte-exact agreement on
# every cut offset - not "both produce plausible chunks". This dumps borg's boundaries
# for a given input and parameter set so the Go side can compare offset by offset.
#
# Line protocol:
#
#   T <algo> <keyhex>
#     -> OK <hex of the derived table>     (algo: fastcdc | buzhash64)
#
#   C <algo> <keyhex|seed> <min_exp> <max_exp> <mask_bits> <window> <nc_level> <datafile>
#     -> OK <comma-separated chunk sizes>
#
#   A <algo> ... (same as C)
#     -> OK <comma-separated "size:allocation" pairs>
#
# The data is passed by file path rather than inline: the corpus runs to megabytes and
# hex on a pipe would dominate the runtime.

import sys
import traceback

from borg.chunkers.buzhash import Chunker as ChunkerBuzHash
from borg.chunkers.buzhash64 import ChunkerBuzHash64, buzhash64_get_table
from borg.chunkers.fastcdc import ChunkerFastCDC, fastcdc_get_gear_table


def build(algo, keyarg, min_exp, max_exp, mask_bits, window, nc_level, sparse=False):
    """Construct a chunker directly from its table key.

    Deliberately not borg's get_chunker(): that derives the chunker's key from a borg
    Key object via derive_key(domain=b"fastcdc" / b"buzhash64", from_id_key=True), which
    belongs to the key layer (stage 4). Here the table key is supplied directly so this
    test covers the chunker alone.
    """
    if algo == "fastcdc":
        return ChunkerFastCDC(bytes.fromhex(keyarg), min_exp, max_exp, mask_bits, nc_level, sparse=sparse)
    if algo == "buzhash64":
        return ChunkerBuzHash64(bytes.fromhex(keyarg), min_exp, max_exp, mask_bits, window,
                                nc_level, sparse=sparse)
    if algo == "buzhash":
        return ChunkerBuzHash(int(keyarg), min_exp, max_exp, mask_bits, window, sparse=sparse)
    raise ValueError(f"unsupported algo {algo!r}")


def chunk_sizes(chunker, path, with_alloc=False):
    out = []
    with open(path, "rb") as fd:
        for chunk in chunker.chunkify(fd):
            # Chunk is a namedtuple of (meta, data); size and allocation live in meta.
            size, alloc = chunk.meta["size"], chunk.meta["allocation"]
            out.append(f"{size}:{alloc}" if with_alloc else str(size))
    return ",".join(out) or "-"


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        if p[0] == "T":
            _, algo, keyhex = p
            key = bytes.fromhex(keyhex)
            table = fastcdc_get_gear_table(key) if algo == "fastcdc" else buzhash64_get_table(key)
            out = "OK " + "".join(f"{v:016x}" for v in table)
        elif p[0] in ("C", "A"):
            _, algo, keyarg, mn, mx, mb, win, nc, path = p
            chunker = build(algo, keyarg, int(mn), int(mx), int(mb), int(win), int(nc))
            out = "OK " + chunk_sizes(chunker, path, with_alloc=(p[0] == "A"))
        else:
            out = f"ERR unknown command {p[0]!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-400:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
