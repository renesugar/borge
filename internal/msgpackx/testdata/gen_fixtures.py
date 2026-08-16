# SPDX-License-Identifier: Apache-2.0
#
# Generate the msgpack differential fixtures for internal/msgpackx.
#
# Run through the pinned borg 2 interpreter so the fixtures inherit borg's own msgpack
# settings rather than a guess at them - this imports borg.helpers.msgpack, the same
# module borg itself packs with:
#
#     make msgpack-fixtures
#
# Output format, one case per line:
#
#     <name> <TAB> <hex of the packed bytes>
#
# The Go test decodes each fixture and re-encodes it, requiring the bytes to come back
# identical. That single check covers both directions at once: borge must understand
# every form borg emits, and must choose exactly the same form when it writes.

import sys

from borg.helpers import msgpack
from borg.helpers.datastruct import StableDict

cases = []


def case(name, value):
    cases.append((name, msgpack.packb(value)))


# --- scalars ---------------------------------------------------------------------
case("nil", None)
case("true", True)
case("false", False)

# Integer format boundaries. Every one of these picks a different msgpack format, and
# a port that gets the boundary off by one still round-trips its own output while
# producing bytes borg would not - which is why they are enumerated rather than fuzzed.
for n in [
    0, 1, 127, 128, 255, 256, 65535, 65536,
    4294967295, 4294967296, 18446744073709551615,
    -1, -32, -33, -128, -129, -32768, -32769,
    -2147483648, -2147483649, -9223372036854775808,
]:
    case(f"int_{n}", n)

case("float_0", 0.0)
case("float_neg", -1.5)
case("float_pi", 3.141592653589793)
case("float_big", 1.7976931348623157e308)

# --- str family ------------------------------------------------------------------
# Length boundaries: fixstr (<32), str8 (<256), str16 (<65536), str32.
for n in [0, 1, 31, 32, 255, 256, 1000]:
    case(f"str_len_{n}", "a" * n)

case("str_utf8", "héllo wörld ✓ 日本語 𝔘𝔫𝔦𝔠𝔬𝔡𝔢")
case("str_emoji", "backup 🗄️ done")

# Paths that are not valid UTF-8. borg decodes these with surrogateescape, so the
# packed payload is the original bytes. This is the case a port is most likely to get
# wrong, and getting it wrong corrupts filenames rather than failing loudly.
for name, raw in [
    ("latin1", b"caf\xe9.txt"),
    ("invalid_ff", b"\xff\xfe\xfd"),
    ("mixed", b"ok-\xe9-\xff-fine.txt"),
    ("high_only", b"\x80\x81\x82\x83"),
    ("trailing", b"file\xc3"),  # truncated 2-byte sequence
    ("astral_and_invalid", b"\xf0\x9d\x94\x98\xff"),  # valid astral char then a bad byte
]:
    case(f"str_surrogate_{name}", raw.decode("utf-8", "surrogateescape"))

# --- bin family ------------------------------------------------------------------
for n in [0, 1, 255, 256, 1000]:
    case(f"bin_len_{n}", b"\x00\x01\xfe\xff" * (n // 4) + b"\x00" * (n % 4))

case("bin_chunkid", bytes(range(32)))

# --- containers ------------------------------------------------------------------
for n in [0, 1, 15, 16, 100]:
    case(f"array_len_{n}", list(range(n)))

case("array_mixed", [None, True, 1, -1, 1.5, "str", b"bytes", [1, 2], {"k": "v"}])
case("array_nested", [[[[1]]]])

for n in [0, 1, 15, 16, 100]:
    case(f"map_len_{n}", {f"k{i:03d}": i for i in range(n)})

# A plain dict packs in insertion order; a StableDict sorts. Both appear in borg, and
# the difference is format-visible because chunk ids are computed over packed bytes.
case("map_insertion_order", {"z": 1, "a": 2, "m": 3})
case("stabledict_sorted", StableDict({"z": 1, "a": 2, "m": 3}))
case("stabledict_empty", StableDict({}))
case("stabledict_numeric_keys", StableDict({10: "a", 2: "b", -5: "c"}))
case("stabledict_bytes_keys", StableDict({b"zz": 1, b"aa": 2, b"a": 3}))

# Sorting where Python's code-point order and raw byte order disagree: the
# surrogate-escaped byte 0xff becomes U+DCFF, which sorts *below* an astral character
# but whose raw byte 0xff sorts *above* the astral character's lead byte 0xf4.
case(
    "stabledict_surrogate_vs_astral",
    StableDict({
        b"\xff".decode("utf-8", "surrogateescape"): "invalid-byte",
        "\U0010ffff": "astral",
        "a": "ascii",
    }),
)
case(
    "stabledict_surrogate_mixed",
    StableDict({
        b"caf\xe9".decode("utf-8", "surrogateescape"): 1,
        "cafe": 2,
        b"\x80".decode("utf-8", "surrogateescape"): 3,
        "zzz": 4,
    }),
)

# --- timestamps ------------------------------------------------------------------
# The three encodings: timestamp32 (4 byte), timestamp64 (8 byte), timestamp96 (12
# byte). Anything before 1970 takes the 96-bit form because the 34-bit seconds test is
# on the unsigned value.
for ns in [
    0,
    1,
    999999999,
    1000000000,
    1755000000123456789,
    4294967295 * 10**9,          # last second representable as timestamp32
    (2**34 - 1) * 10**9,         # last second representable as timestamp64
    2**34 * 10**9,               # first second needing timestamp96
    -1,
    -1000000005,
    -2208988800 * 10**9,         # 1900-01-01
]:
    case(f"timestamp_{ns}", msgpack.int_to_timestamp(ns))

# --- borg-shaped structures ------------------------------------------------------
# Approximations of the real thing: enough structure to exercise the combinations that
# actually occur (a chunk list of 32-byte ids, timestamps, a StableDict of xattrs).
case(
    "item_file",
    StableDict({
        "path": "home/user/notes/recipe.md",
        "mode": 0o100644,
        "uid": 1000,
        "gid": 1000,
        "user": "renes",
        "group": "renes",
        "size": 4096,
        "mtime": msgpack.int_to_timestamp(1755000000123456789),
        "atime": msgpack.int_to_timestamp(1755000001000000000),
        "ctime": msgpack.int_to_timestamp(1755000000123456789),
        "chunks": [[bytes(range(32)), 4096], [bytes(range(32, 64)), 2048]],
    }),
)
case(
    "item_symlink",
    StableDict({
        "path": "home/user/link",
        "target": "../elsewhere/file",
        "mode": 0o120777,
        "mtime": msgpack.int_to_timestamp(1000000000000000000),
    }),
)
case(
    "item_xattrs",
    StableDict({
        "path": "home/user/x",
        "mtime": msgpack.int_to_timestamp(0),
        "xattrs": StableDict({b"user.zzz": b"last", b"user.aaa": b"first", b"security.selinux": b"ctx"}),
    }),
)
case(
    "manifest_like",
    StableDict({
        "version": 2,
        "archives": {},
        "timestamp": "2026-08-16T12:00:00.000000",
        "config": StableDict({"item_keys": ["path", "mtime", "mode"]}),
    }),
)
case(
    "archive_like",
    StableDict({
        "version": 2,
        "name": "recipe-backup-2026-08-16",
        "item_ptrs": [bytes(range(32))],
        "command_line": "borg create ::{now} /home/renes/projects/recipedb",
        "hostname": "rene-hppaviliongaminglaptop15dk0xxx",
        "username": "renes",
        "start": "2026-08-16T12:00:00.000000",
        "end": "2026-08-16T12:19:44.000000",
        "size": 2850000000,
        "nfiles": 1621034,
        "chunker_params": ["fastcdc", 19, 23, 21, 2],
    }),
)

# --- emit -------------------------------------------------------------------------
out = sys.stdout
out.write("# generated by internal/msgpackx/testdata/gen_fixtures.py - do not edit\n")
out.write(f"# msgpack {'.'.join(str(v) for v in msgpack.version)}\n")
import borg  # noqa: E402

out.write(f"# borg {borg.__version__}\n")
seen = set()
for name, packed in cases:
    if name in seen:
        raise SystemExit(f"duplicate fixture name: {name}")
    seen.add(name)
    out.write(f"{name}\t{packed.hex()}\n")
print(f"wrote {len(cases)} fixtures", file=sys.stderr)
