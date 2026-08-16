# SPDX-License-Identifier: Apache-2.0
#
# Generate the item differential fixtures for internal/item.
#
# Run through the pinned borg 2 interpreter, so the fixtures are produced by borg's own
# Item/ArchiveItem/ManifestItem/Key/EncryptedKey classes and its own msgpack wrapper:
#
#     make item-fixtures
#
# Output, one case per line:
#
#     <kind> <TAB> <name> <TAB> <hex of the packed bytes>
#
# kind is item | archive | manifest | key | enckey.
#
# The Go test decodes each fixture through the corresponding borge type and re-encodes
# it, requiring the bytes back unchanged. That covers both directions at once: borge
# must understand every shape borg emits, and must independently reproduce borg's key
# ordering (StableDict) and value encodings.

import sys

from borg.helpers import msgpack
from borg.helpers.datastruct import StableDict
from borg.item import ArchiveItem, EncryptedKey, Item, Key, ManifestItem

cases = []


def case(kind, name, obj):
    cases.append((kind, name, msgpack.packb(obj.as_dict())))


ID1 = bytes(range(32))
ID2 = bytes(range(32, 64))
NS = 1755000000123456789


# --- items -------------------------------------------------------------------------
# The minimum: only the two required keys.
case("item", "minimal", Item(path="a", mtime=NS))

case(
    "item",
    "regular_file",
    Item(
        path="home/renes/notes/recipe.md",
        mode=0o100644,
        uid=1000,
        gid=1000,
        user="renes",
        group="renes",
        size=4096,
        inode=123456,
        mtime=NS,
        atime=NS + 1000,
        ctime=NS,
        birthtime=NS - 5000,
        chunks=[[ID1, 4096], [ID2, 2048]],
    ),
)

case("item", "empty_file", Item(path="empty", mode=0o100644, mtime=NS, size=0, chunks=[]))

case("item", "directory", Item(path="home/renes", mode=0o40755, uid=0, gid=0, mtime=NS))

case("item", "symlink", Item(path="link", target="../elsewhere/file", mode=0o120777, mtime=NS))

case(
    "item",
    "hardlink",
    Item(path="hl/a", mode=0o100644, mtime=NS, hlid=ID1, nlink=2, chunks=[[ID2, 10]]),
)

case(
    "item",
    "device",
    Item(path="dev/null", mode=0o20666, rdev=259, uid=0, gid=0, mtime=NS),
)

case(
    "item",
    "xattrs",
    Item(
        path="x",
        mode=0o100644,
        mtime=NS,
        xattrs=StableDict({b"user.zzz": b"last", b"user.aaa": b"first", b"security.selinux": b"ctx"}),
    ),
)

# An xattr with an empty value, and one whose name is not valid UTF-8.
case(
    "item",
    "xattrs_edge",
    Item(
        path="x2",
        mode=0o100644,
        mtime=NS,
        xattrs=StableDict({b"user.empty": b"", b"user.\xff\xfe": b"invalid-name"}),
    ),
)

case(
    "item",
    "acls",
    Item(
        path="acl",
        mode=0o100644,
        mtime=NS,
        acl_access=b"user::rw-\ngroup::r--\nother::r--\n",
        acl_default=b"",
        acl_extended=b"\x01\x02\x03",
        acl_nfs4=b"\xff\x00",
    ),
)

case("item", "bsdflags", Item(path="f", mode=0o100644, mtime=NS, bsdflags=16))

case("item", "chunks_healthy", Item(
    path="damaged", mode=0o100644, mtime=NS,
    chunks=[[ID1, 100]], chunks_healthy=[[ID1, 100], [ID2, 200]],
))

case("item", "deleted", Item(path="gone", mtime=NS, deleted=True))

# Paths that are not valid UTF-8 - the case a port is most likely to corrupt silently.
for name, raw in [
    ("latin1", b"caf\xe9.txt"),
    ("invalid", b"dir/\xff\xfe\xfd"),
    ("mixed", b"ok-\xe9-\xff-fine.txt"),
    ("astral_and_invalid", b"\xf0\x9d\x94\x98\xff"),
]:
    case("item", f"path_{name}", Item(path=raw.decode("utf-8", "surrogateescape"), mtime=NS))

# Non-UTF-8 user/group names and symlink targets, too.
case("item", "user_invalid_utf8", Item(
    path="u", mtime=NS,
    user=b"us\xffer".decode("utf-8", "surrogateescape"),
    group=b"gr\xfeoup".decode("utf-8", "surrogateescape"),
))
case("item", "target_invalid_utf8", Item(
    path="l", mode=0o120777, mtime=NS,
    target=b"../t\xffarget".decode("utf-8", "surrogateescape"),
))

# Time extremes: before the epoch, and a value needing the 96-bit timestamp form.
case("item", "time_pre_epoch", Item(path="old", mtime=-2208988800 * 10**9))
case("item", "time_zero", Item(path="epoch", mtime=0))
case("item", "time_nanos", Item(path="ns", mtime=1, atime=999999999))

# A large chunk list, like a big file.
case("item", "many_chunks", Item(
    path="big.bin", mode=0o100644, mtime=NS, size=1000 * 65536,
    chunks=[[bytes([i % 256]) * 32, 65536] for i in range(1000)],
))


# --- archive metadata ---------------------------------------------------------------
case("archive", "minimal", ArchiveItem(
    version=2, name="a", item_ptrs=[ID1], command_line="borg create",
))

case("archive", "full", ArchiveItem(
    version=2,
    name="recipe-backup-2026-08-16",
    item_ptrs=[ID1, ID2],
    command_line="borg create ::{now} /home/renes/projects/recipedb",
    hostname="rene-hppaviliongaminglaptop15dk0xxx",
    username="renes",
    start="2026-08-16T12:00:00.000000",
    end="2026-08-16T12:19:44.000000",
    comment="nightly",
    tags=["nightly", "recipes"],
    chunker_params=("fastcdc", 19, 23, 21, 2),
    size=2850000000,
    nfiles=1621034,
    cwd="/home/renes",
))

case("archive", "buzhash_params", ArchiveItem(
    version=2, name="b", item_ptrs=[ID1], command_line="borg create",
    chunker_params=("buzhash", 19, 23, 21, 4095),
))

case("archive", "name_invalid_utf8", ArchiveItem(
    version=2, name=b"arch\xff".decode("utf-8", "surrogateescape"),
    item_ptrs=[ID1], command_line="borg create",
))


# --- manifest -----------------------------------------------------------------------
case("manifest", "borg2", ManifestItem(
    version=2, archives={}, timestamp="2026-08-16T12:00:00.000000",
    config=StableDict({"item_keys": ["path", "mtime", "mode"]}),
))

case("manifest", "with_config", ManifestItem(
    version=2, archives={}, timestamp="2026-08-16T12:00:00.000000",
    config=StableDict({
        "item_keys": ["path", "mtime"],
        "additional_free_space": 0,
    }),
))


# --- keys ---------------------------------------------------------------------------
case("key", "borg2", Key(
    version=1, repository_id=ID1, crypt_key=ID1 + ID2, id_key=ID2, chunk_seed=305419896,
))
case("key", "with_tam", Key(
    version=1, repository_id=ID1, crypt_key=ID1 + ID2, id_key=ID2, chunk_seed=0,
    tam_required=True,
))

case("enckey", "argon2", EncryptedKey(
    version=1, algorithm="argon2 chacha20-poly1305",
    salt=bytes(range(16)), hash=ID1, data=ID1 + ID2,
    argon2_time_cost=3, argon2_memory_cost=65536, argon2_parallelism=4, argon2_type="id",
))
case("enckey", "with_label", EncryptedKey(
    version=1, algorithm="argon2 chacha20-poly1305",
    salt=bytes(range(16)), hash=ID1, data=ID2,
    argon2_time_cost=3, argon2_memory_cost=65536, argon2_parallelism=4, argon2_type="id",
    label="admin",
))
case("enckey", "legacy_pbkdf2", EncryptedKey(
    version=1, algorithm="sha256", iterations=100000,
    salt=bytes(range(32)), hash=ID1, data=ID2,
))


# --- emit ---------------------------------------------------------------------------
out = sys.stdout
out.write("# generated by internal/item/testdata/gen_fixtures.py - do not edit\n")
import borg  # noqa: E402

out.write(f"# borg {borg.__version__}\n")
seen = set()
for kind, name, packed in cases:
    ident = f"{kind}/{name}"
    if ident in seen:
        raise SystemExit(f"duplicate fixture: {ident}")
    seen.add(ident)
    out.write(f"{kind}\t{name}\t{packed.hex()}\n")
print(f"wrote {len(cases)} fixtures", file=sys.stderr)
