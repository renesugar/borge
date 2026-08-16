# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the live differential test (differential_test.go).
#
# Reads one hex-encoded msgpack value per line on stdin, unpacks it with borg's own
# msgpack wrapper and packs it straight back, writing the result as hex. If borge's
# encoding is what borg would have produced, output equals input line for line.
#
# This is the reverse of the fixture test: fixtures prove borge reproduces borg's
# bytes, this proves borg accepts borge's.

import sys

from borg.helpers import msgpack

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        value = msgpack.unpackb(bytes.fromhex(line))
        sys.stdout.write(msgpack.packb(value).hex() + "\n")
    except Exception as exc:  # report, do not die: the Go side names the failing case
        sys.stdout.write(f"ERROR {type(exc).__name__}: {exc}\n")
    sys.stdout.flush()
