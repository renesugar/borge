# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the chunk index differential test.
#
# Both directions of the stage 1.6 gate go through here: borg writes an index and borge
# reads it, and borge writes one and borg reads it.
#
# Line protocol:
#
#   W <path> <n> <seed>
#     Write an index of n deterministic entries to <path>, using borg's ChunkIndex.
#     -> OK <sha256 of the entry set>
#
#   R <path>
#     Read an index with borg's ChunkIndex and report what it contains.
#     -> OK <len> <sha256 of the entry set>
#
# The digest covers the (id, flags, size, pack_id, obj_offset, obj_size) tuples sorted
# by id, so it is independent of the order entries happen to be stored in - which is
# exactly the part that legitimately differs between the two implementations.

import hashlib
import sys
import traceback

from borg.hashindex import ChunkIndex, ChunkIndexEntry


def entry_digest(index):
    """Order-independent digest of an index's contents."""
    rows = []
    for key, value in index.iteritems():
        rows.append(
            key
            + value.flags.to_bytes(4, "little")
            + value.size.to_bytes(4, "little")
            + value.pack_id
            + value.obj_offset.to_bytes(4, "little")
            + value.obj_size.to_bytes(4, "little")
        )
    rows.sort()
    h = hashlib.sha256()
    for row in rows:
        h.update(row)
    return h.hexdigest()


def make_entries(n, seed):
    """Deterministic entries, matching what the Go side generates for the same seed."""
    for i in range(n):
        # A chunk id is a hash, so use one: sha256 of the counter, which the Go side
        # can reproduce exactly.
        key = hashlib.sha256(f"{seed}:{i}".encode()).digest()
        pack_id = hashlib.sha256(f"pack:{seed}:{i // 100}".encode()).digest()
        yield key, ChunkIndexEntry(
            flags=(i % 4) | 1,  # exercise the user flag bits, always FLAG_USED
            size=(i * 7 + 1) % 100000,
            pack_id=pack_id,
            obj_offset=(i * 13) % 4000000,
            obj_size=(i * 3 + 5) % 100000,
        )


def do_write(path, n, seed):
    index = ChunkIndex()
    for key, value in make_entries(n, seed):
        index[key] = value
    index.write(path)
    return "OK " + entry_digest(index)


def do_read(path):
    index = ChunkIndex(path=path)
    return f"OK {len(index)} {entry_digest(index)}"


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        if p[0] == "W":
            out = do_write(p[1], int(p[2]), int(p[3]))
        elif p[0] == "R":
            out = do_read(p[1])
        else:
            out = f"ERR unknown command {p[0]!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-400:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
