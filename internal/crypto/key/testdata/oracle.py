# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the key differential test.
#
# Drives borg's own key classes, so the AEAD envelope, the session key derivation and the
# passphrase-protected key blob are all checked against the real thing.
#
# Line protocol, hex fields, "-" for empty:
#
#   H <mode> <idkey> <data>                        id hash          -> OK <hex>
#   E <mode> <cryptkey> <idkey> <id> <data> <aad>  encrypt          -> OK <envelope hex>
#   D <mode> <cryptkey> <idkey> <id> <env> <aad>   decrypt          -> OK <plaintext hex>
#   S <pass> <repoid> <cryptkey> <idkey> <seed> <label>   seal a key blob -> OK <blob text hex>
#   O <pass> <repoid> <blobtext>                   open a key blob
#                                    -> OK <cryptkey> <idkey> <seed> <version> <label>
#
# <pass> and <label> are hex-encoded UTF-8 so they can carry spaces; "-" means empty.
#
# BORG_TESTONLY_WEAKEN_KDF=1 is expected in the environment: the real argon2 parameters
# cost 64 MiB and hundreds of milliseconds per attempt, which would make this unusable.

import sys
import traceback

from borg.logger import setup_logging

setup_logging()

from borg.crypto.key import (  # noqa: E402
    AESOCBKey,
    AuthenticatedKey,
    Blake3AESOCBKey,
    Blake3AuthenticatedKey,
    Blake3CHPOKey,
    Blake3ChecksumKey,
    CHPOKey,
    ChecksumKey,
)
from borg.helpers import msgpack  # noqa: E402
from borg.item import Key  # noqa: E402

MODES = {
    "aes256-ocb": AESOCBKey,
    "chacha20-poly1305": CHPOKey,
    "blake3-aes256-ocb": Blake3AESOCBKey,
    "blake3-chacha20-poly1305": Blake3CHPOKey,
    "authenticated-sha256": AuthenticatedKey,
    "authenticated-blake3": Blake3AuthenticatedKey,
    "none-sha256": ChecksumKey,
    "none-blake3": Blake3ChecksumKey,
}


def unhex(s):
    return b"" if s == "-" else bytes.fromhex(s)


def enhex(b):
    return b.hex() if b else "-"


def untext(s):
    return "" if s == "-" else bytes.fromhex(s).decode("utf-8")


def make_key(mode, crypt_key, id_key):
    cls = MODES[mode]
    k = cls(None)
    if crypt_key:
        k.init_from_given_data(crypt_key=crypt_key, id_key=id_key, chunk_seed=0)
    k.init_ciphers()
    return k


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        cmd = p[0]
        if cmd == "H":
            # The id hash needs only the id key; a dummy crypt key satisfies the
            # constructors of the keyed modes, and the unkeyed ones take none at all.
            id_key = unhex(p[2])
            k = make_key(p[1], b"\0" * 64 if id_key else b"", id_key)
            out = "OK " + k.id_hash(unhex(p[3])).hex()
        elif cmd == "E":
            k = make_key(p[1], unhex(p[2]), unhex(p[3]))
            out = "OK " + enhex(k.encrypt(unhex(p[4]), unhex(p[5]), aad=unhex(p[6])))
        elif cmd == "D":
            k = make_key(p[1], unhex(p[2]), unhex(p[3]))
            out = "OK " + enhex(bytes(k.decrypt(unhex(p[4]), unhex(p[5]), aad=unhex(p[6]))))
        elif cmd == "S":
            passphrase, repoid = untext(p[1]), unhex(p[2])
            crypt_key, id_key, seed = unhex(p[3]), unhex(p[4]), int(p[5])
            label = untext(p[6]) or None
            k = AESOCBKey(None)
            k.repository_id = repoid
            k.init_from_given_data(crypt_key=crypt_key, id_key=id_key, chunk_seed=seed)
            material = Key(
                version=2,
                repository_id=repoid,
                crypt_key=crypt_key,
                id_key=id_key,
                chunk_seed=seed,
            )
            blob = k.encrypt_key_file(
                msgpack.packb(material.as_dict()), passphrase, "argon2 chacha20-poly1305", label=label
            )
            import binascii
            import textwrap

            from borg.crypto.key import keyfile_format

            b64 = "\n".join(textwrap.wrap(binascii.b2a_base64(blob).decode("ascii")))
            out = "OK " + keyfile_format(repoid.hex(), b64).encode("utf-8").hex()
        elif cmd == "O":
            import binascii

            from borg.crypto.key import keyfile_parse

            passphrase, repoid = untext(p[1]), unhex(p[2])
            text = unhex(p[3]).decode("utf-8")
            _, b64 = keyfile_parse(text, repoid.hex())
            raw = binascii.a2b_base64(b64)
            k = AESOCBKey(None)
            data = k.decrypt_key_file(raw, passphrase)
            if data is None:
                out = "ERR PassphraseWrong"
            else:
                material = Key(internal_dict=msgpack.unpackb(data))
                out = "OK {} {} {} {} {}".format(
                    material.crypt_key.hex(),
                    material.id_key.hex(),
                    material.chunk_seed,
                    material.version,
                    (k._encrypted_key_label or "").encode("utf-8").hex() or "-",
                )
        elif cmd == "P":
            # borg's paper key export. KeyManager.__init__ wants a live repository to
            # identify the key class, which is irrelevant here - the paper key format
            # depends only on the blob and the repository id - so the object is built
            # directly instead.
            import tempfile
            import types

            from borg.crypto.key import keyfile_parse
            from borg.crypto.keymanager import KeyManager

            repoid = unhex(p[1])
            text = unhex(p[2]).decode("utf-8")
            _, b64 = keyfile_parse(text, repoid.hex())
            km = KeyManager.__new__(KeyManager)
            km.repository = types.SimpleNamespace(id=repoid)
            km.keyblob = b64
            with tempfile.NamedTemporaryFile("w+", suffix=".txt") as fd:
                km.export_paperkey(fd.name)
                fd.seek(0)
                out = "OK " + fd.read().encode("utf-8").hex()
        else:
            out = f"ERR unknown command {cmd!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-500:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
