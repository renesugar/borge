# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the repository differential test.
#
# Drives borg's own Repository, so the pack layout, the chunk index fragments and the
# object round trip are all checked against the real thing.
#
# Line protocol, hex fields, "-" for empty:
#
#   C <path>                       create a repository            -> OK <repo id hex>
#   O <path>                       open it                        -> OK <repo id hex>
#   P <path> <idhex> <hexdata>     put an object
#   F <path>                       flush the pack writer
#   X <path>                       close (persists the index)
#   G <path> <idhex>               get an object                  -> OK <hexdata>
#   L <path>                       list chunk ids from the index  -> OK <id,id,...>
#   I <path>                       index digest                   -> OK <n> <sha256>
#   T <path>                       every file on disk             -> OK <relpath,...>
#   K <path>                       run borg's own repository check -> OK <ok|failed>
#
# The index digest is order-independent: the sorted (id, pack_id, offset, size) tuples,
# hashed. Entry order inside a fragment is not part of the format (docs/FORMAT.md §6.1),
# so comparing digests is the right granularity.

import hashlib
import sys
import traceback

from borg.logger import setup_logging
from borg.repository import Repository

# borg refuses to log before setup_logging() has run, and Repository logs from its
# constructor, so this has to happen first.
setup_logging()

_repos = {}


def get_repo(path, create=False, exclusive=True):
    if path in _repos:
        return _repos[path]
    # A plain absolute path, not a file:// URL: borg's Repository parses a Location,
    # which treats a bare absolute path as a local repository.
    repo = Repository(path, create=create, exclusive=exclusive, lock_wait=5)
    if create:
        repo.create()
    repo.open(exclusive=exclusive, lock_wait=5)
    _repos[path] = repo
    return repo


def close_repo(path):
    repo = _repos.pop(path, None)
    if repo is not None:
        repo.close()


def index_digest(repo):
    rows = []
    for chunk_id, entry in repo.chunks.iteritems():
        rows.append(
            chunk_id
            + entry.pack_id
            + entry.obj_offset.to_bytes(4, "little")
            + entry.obj_size.to_bytes(4, "little")
        )
    rows.sort()
    h = hashlib.sha256()
    for row in rows:
        h.update(row)
    return len(rows), h.hexdigest()


def tree(path):
    import os

    out = []
    for root, _dirs, files in os.walk(path):
        for f in files:
            out.append(os.path.relpath(os.path.join(root, f), path).replace(os.sep, "/"))
    out.sort()
    return ",".join(out) or "-"


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        cmd = p[0]
        if cmd == "C":
            repo = get_repo(p[1], create=True)
            out = "OK " + repo.id.hex()
        elif cmd == "O":
            repo = get_repo(p[1])
            out = "OK " + repo.id.hex()
        elif cmd == "P":
            get_repo(p[1]).put(bytes.fromhex(p[2]), bytes.fromhex(p[3]))
            out = "OK"
        elif cmd == "F":
            get_repo(p[1]).flush()
            out = "OK"
        elif cmd == "X":
            close_repo(p[1])
            out = "OK"
        elif cmd == "G":
            data = get_repo(p[1]).get(bytes.fromhex(p[2]))
            out = "OK " + (bytes(data).hex() or "-")
        elif cmd == "L":
            ids = [cid.hex() for cid, _ in get_repo(p[1]).chunks.iteritems()]
            ids.sort()
            out = "OK " + (",".join(ids) or "-")
        elif cmd == "I":
            n, digest = index_digest(get_repo(p[1]))
            out = f"OK {n} {digest}"
        elif cmd == "T":
            out = "OK " + tree(p[1])
        elif cmd == "K":
            repo = get_repo(p[1])
            ok = repo.check(repair=False)
            out = "OK " + ("ok" if ok else "failed")
        else:
            out = f"ERR unknown command {cmd!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-500:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
