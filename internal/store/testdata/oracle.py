# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the store differential test.
#
# Drives borgstore itself, with borg's own namespace configuration, so the two sides can
# be compared on layout (where an object lands on disk), on listing semantics (including
# soft-deleted objects), and on range reads.
#
# Line protocol, values hex-encoded, "-" for empty:
#
#   C <path>                     create a store
#   S <path> <name> <hexvalue>   store an object
#   L <path> <name> <off> <size> load (size -1 = to the end)   -> OK <hexvalue>
#   I <path> <name>              info                          -> OK <exists> <size>
#   N <path> <ns> <deleted>      list names                    -> OK <name,name,...>
#   D <path> <name>              soft-delete
#   U <path> <name>              undelete
#   X <path> <name>              hard delete
#   F <path> <name>              find: the nested name          -> OK <nested>
#   T <path>                     tree: every file on disk       -> OK <relpath,relpath,...>

import os
import sys
import traceback

from borgstore.store import Store

# borg's namespace configuration, from Repository.__init__ (src/borg/repository.py).
NS_CONFIG = {
    "archives/": {"levels": [0]},
    "cache/": {"levels": [0]},
    "config/": {"levels": [0]},
    "index/": {"levels": [0]},
    "keys/": {"levels": [0]},
    "locks/": {"levels": [0]},
    "packs/": {"levels": [1]},
}

_stores = {}


def get_store(path):
    """One open Store per path, so state (and the lock) is shared across commands."""
    if path not in _stores:
        store = Store(url="file://" + path, config=NS_CONFIG)
        store.open()
        _stores[path] = store
    return _stores[path]


def unhex(s):
    return b"" if s == "-" else bytes.fromhex(s)


def enhex(b):
    return bytes(b).hex() or "-"


def tree(path):
    """Every regular file below path, as sorted store-relative slash paths."""
    out = []
    for root, _dirs, files in os.walk(path):
        for f in files:
            full = os.path.join(root, f)
            out.append(os.path.relpath(full, path).replace(os.sep, "/"))
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
            store = Store(url="file://" + p[1], config=NS_CONFIG)
            store.create()
            store.open()
            _stores[p[1]] = store
            out = "OK"
        elif cmd == "S":
            get_store(p[1]).store(p[2], unhex(p[3]))
            out = "OK"
        elif cmd == "L":
            size = int(p[4])
            value = get_store(p[1]).load(p[2], offset=int(p[3]), size=None if size < 0 else size)
            out = "OK " + enhex(value)
        elif cmd == "I":
            info = get_store(p[1]).info(p[2])
            out = f"OK {int(info.exists)} {info.size}"
        elif cmd == "N":
            names = [i.name for i in get_store(p[1]).list(p[2], deleted=(p[3] == "1"))]
            out = "OK " + (",".join(names) or "-")
        elif cmd == "D":
            get_store(p[1]).move(p[2], delete=True)
            out = "OK"
        elif cmd == "U":
            get_store(p[1]).move(p[2], undelete=True)
            out = "OK"
        elif cmd == "X":
            get_store(p[1]).delete(p[2])
            out = "OK"
        elif cmd == "F":
            out = "OK " + get_store(p[1]).find(p[2])
        elif cmd == "T":
            out = "OK " + tree(p[1])
        else:
            out = f"ERR unknown command {cmd!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-400:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
