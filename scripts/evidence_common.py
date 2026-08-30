# SPDX-License-Identifier: Apache-2.0
"""Shared attestation helpers for borge's evidence tooling.

Both the attester and the verifier need to sign, timestamp and check the same way; a
second implementation of "how a token is verified" is a second thing that can be wrong
while the first stays right.
"""

from __future__ import annotations

import hashlib
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")

# openssl prints the TST fields in a fixed shape; these read the two the catalog records.
_TS_POLICY_RE = re.compile(r"^Policy OID:\s*(\S+)$", re.MULTILINE)
_TS_TIME_RE = re.compile(r"^Time stamp:\s*(.+)$", re.MULTILINE)
_TS_STATUS_RE = re.compile(r"^Status:\s*(.+)$", re.MULTILINE)


class AttestationError(Exception):
    """A signature or timestamp could not be produced or did not verify."""


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def run(command: list[str], description: str, stdin: bytes | None = None) -> str:
    result = subprocess.run(
        command, input=stdin, capture_output=True, check=False
    )
    if result.returncode != 0:
        detail = (result.stderr or result.stdout).decode("utf-8", "replace").strip()
        raise AttestationError(f"{description} failed: {detail}")
    return result.stdout.decode("utf-8", "replace")


def require_tools(tools: list[str]) -> None:
    missing = [tool for tool in tools if shutil.which(tool) is None]
    if missing:
        raise AttestationError(f"required tool(s) not found: {', '.join(missing)}")


def verify_openpgp_signature(
    signature: Path, subject: Path, public_key: Path, expected_subkey: str
) -> None:
    """Verify a detached signature against *only* the pinned public key.

    gpg is given a keyring built from `public_key` alone. Verifying against the
    operator's own keyring would accept a signature from any key that happens to be in
    it, which is the failure this whole exercise exists to avoid.
    """
    if not public_key.is_file():
        raise AttestationError(f"missing pinned public key {public_key}")
    with tempfile.TemporaryDirectory(prefix="borge-evidence-gpg.") as home:
        base = [
            "gpg",
            "--homedir",
            home,
            "--batch",
            "--no-default-keyring",
            "--quiet",
            "--no-tty",
        ]
        run(base + ["--import", str(public_key)], f"importing {public_key.name}")
        status = run(
            base + ["--status-fd", "1", "--verify", str(signature), str(subject)],
            f"{subject.name}: OpenPGP signature",
        )
    if "[GNUPG:] GOODSIG" not in status:
        raise AttestationError(f"{subject.name}: signature is not good")
    made_by = [
        line.split()[2]
        for line in status.splitlines()
        if line.startswith("[GNUPG:] VALIDSIG")
    ]
    # VALIDSIG's first field is the fingerprint of the signing (sub)key itself.
    if not made_by:
        raise AttestationError(f"{subject.name}: no VALIDSIG status from gpg")
    if made_by[0].upper() != expected_subkey.upper():
        raise AttestationError(
            f"{subject.name}: signed by {made_by[0]}, expected subkey {expected_subkey}"
        )


def token_fields(token: Path) -> dict[str, str]:
    """Read the policy OID and asserted time out of an RFC 3161 response."""
    text = run(
        ["openssl", "ts", "-reply", "-in", str(token), "-text"],
        f"{token.name}: reading timestamp reply",
    )
    status = _TS_STATUS_RE.search(text)
    if not status or not status.group(1).strip().lower().startswith("granted"):
        raise AttestationError(f"{token.name}: TSA status is not Granted")
    policy = _TS_POLICY_RE.search(text)
    stamped = _TS_TIME_RE.search(text)
    if not policy or not stamped:
        raise AttestationError(f"{token.name}: reply has no policy OID or time stamp")
    return {"policy_oid": policy.group(1), "token_time": stamped.group(1).strip()}


def verify_timestamp_token(token: Path, subject: Path, ca_file: Path) -> None:
    """Verify a token against the artifact's own bytes and one pinned root.

    `-data` rather than `-queryfile`: the imprint is recomputed from the file, so the
    token is bound to the bytes on disk rather than to a query that may no longer
    describe them.
    """
    if not ca_file.is_file():
        raise AttestationError(f"missing pinned TSA root {ca_file}")
    run(
        [
            "openssl",
            "ts",
            "-verify",
            "-in",
            str(token),
            "-data",
            str(subject),
            "-CAfile",
            str(ca_file),
        ],
        f"{subject.name}: RFC 3161 token {token.name}",
    )
