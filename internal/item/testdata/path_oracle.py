# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the path-sanitisation differential test.
#
# Path sanitisation is a security boundary: a stored path with a ".." element or a
# leading "/" would let an archive write outside the extraction directory. borge must
# accept exactly what borg accepts and reject exactly what borg rejects - accepting
# something borg rejects is a vulnerability, and rejecting something borg accepts makes
# borge unable to read valid archives.
#
# Line protocol, one hex-encoded path per line (a path is arbitrary bytes, so hex):
#
#   <hex path>  ->  OK <hex sanitised path>   or   REJECT

import sys

from borg.helpers.fs import make_path_safe

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        raw = b"" if line == "-" else bytes.fromhex(line)
        path = raw.decode("utf-8", "surrogateescape")
        safe = make_path_safe(path)
        out = "OK " + (safe.encode("utf-8", "surrogateescape").hex() or "-")
    except ValueError:
        out = "REJECT"
    except Exception as exc:  # anything else is a difference worth seeing, not hiding
        out = f"ERR {type(exc).__name__}: {exc}"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
