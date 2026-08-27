#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Sign and timestamp borge's evidence artifacts, and record it in the catalog.

Everything this produces is retrospective: it attests that these bytes existed by the
time the token was issued, which is not when the tests inside them ran. The catalog says
so per artifact, and this tool never writes a time it did not obtain from the TSA.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from evidence_common import (  # noqa: E402
    AttestationError,
    require_tools,
    run,
    sha256_file,
    token_fields,
    verify_openpgp_signature,
    verify_timestamp_token,
)

TSA_CONTENT_TYPE = "application/timestamp-query"
TSA_TIMEOUT_SECONDS = 30


def now_utc() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def token_time_to_utc(stamped: str) -> str:
    """'Aug 27 23:25:09 2026 GMT' -> '2026-08-27T23:25:09Z'."""
    cleaned = " ".join(stamped.replace("GMT", "").split())
    parsed = dt.datetime.strptime(cleaned, "%b %d %H:%M:%S %Y").replace(
        tzinfo=dt.timezone.utc
    )
    return parsed.strftime("%Y-%m-%dT%H:%M:%SZ")


def load_catalog(path: Path) -> dict:
    catalog = json.loads(path.read_text(encoding="utf-8"))
    if catalog.get("schema_version") != 2:
        raise AttestationError("catalog schema_version must be 2")
    return catalog


def save_catalog(path: Path, catalog: dict) -> None:
    path.write_text(json.dumps(catalog, indent=2) + "\n", encoding="utf-8")


def passphrase() -> bytes:
    """Read the signing passphrase from the login keyring.

    Never an argument: a passphrase on a command line is visible in the process table
    and in shell history. `secret-tool` writes it to stdout and it goes straight to
    gpg's stdin.
    """
    result = subprocess.run(
        ["secret-tool", "lookup", "service", "gpg_evidence", "type", "passphrase"],
        capture_output=True,
        check=False,
    )
    if result.returncode != 0 or not result.stdout:
        raise AttestationError(
            "no signing passphrase in the login keyring "
            "(secret-tool lookup service gpg_evidence type passphrase)"
        )
    return result.stdout.split(b"\n")[0]


def sign(subject: Path, signature: Path, subkey: str, secret: bytes) -> None:
    run(
        [
            "gpg",
            "--batch",
            "--yes",
            "--pinentry-mode",
            "loopback",
            "--passphrase-fd",
            "0",
            # The '!' pins the subkey: without it gpg picks a signing key for the
            # identity, which is the wrong thing to record in the catalog.
            "--local-user",
            f"{subkey}!",
            "--detach-sign",
            "--armor",
            "--output",
            str(signature),
            str(subject),
        ],
        f"{subject.name}: detached signature",
        stdin=secret + b"\n",
    )


def timestamp(subject: Path, token: Path, authority: dict, ca_file: Path) -> dict:
    """Request one RFC 3161 token, verify it, and describe it for the catalog."""
    with tempfile.TemporaryDirectory(prefix="borge-evidence-tsq.") as work:
        query = Path(work) / "request.tsq"
        # A nonce is included by default and is checked below against the reply. It is
        # only meaningful at issuance, which is why the query is not retained.
        run(
            [
                "openssl",
                "ts",
                "-query",
                "-data",
                str(subject),
                "-sha256",
                "-cert",
                "-out",
                str(query),
            ],
            f"{subject.name}: building timestamp query",
        )
        request = urllib.request.Request(
            authority["url"],
            data=query.read_bytes(),
            headers={"Content-Type": TSA_CONTENT_TYPE},
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=TSA_TIMEOUT_SECONDS) as reply:
                body = reply.read()
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise AttestationError(
                f"{subject.name}: {authority['id']} timestamp request failed: {exc}"
            ) from exc
        if not body:
            raise AttestationError(
                f"{subject.name}: {authority['id']} returned an empty response"
            )
        token.write_bytes(body)
        # The nonce ties this reply to this request; check it while the query still
        # exists, then verify against the artifact's bytes and the pinned root.
        run(
            [
                "openssl",
                "ts",
                "-verify",
                "-in",
                str(token),
                "-queryfile",
                str(query),
                "-CAfile",
                str(ca_file),
            ],
            f"{subject.name}: {authority['id']} token against its own request",
        )
    verify_timestamp_token(token, subject, ca_file)
    fields = token_fields(token)
    if fields["policy_oid"] != authority["policy_oid"]:
        raise AttestationError(
            f"{subject.name}: {authority['id']} issued policy {fields['policy_oid']}, "
            f"catalog expects {authority['policy_oid']}"
        )
    return {
        "authority": authority["id"],
        "filename": token.name,
        "sha256": sha256_file(token),
        "policy_oid": fields["policy_oid"],
        "token_time_utc": token_time_to_utc(fields["token_time"]),
    }


def attest_one(
    subject: Path,
    expected_sha256: str,
    catalog_dir: Path,
    policy: dict,
    force: bool,
    secret: bytes,
    existing: dict | None,
) -> tuple[dict, bool]:
    """Attest one artifact. Returns its catalog record and whether anything was made."""
    actual = sha256_file(subject)
    if actual != expected_sha256:
        raise AttestationError(
            f"{subject.name}: SHA-256 {actual} disagrees with the catalog "
            f"({expected_sha256}); refusing to attest it"
        )
    key = policy["signing_key"]
    public_key = catalog_dir / key["public_key_file"]
    subkey = key["signing_subkey_fingerprint"]

    signature = subject.with_name(subject.name + ".asc")
    made = False
    if force or not signature.is_file():
        sign(subject, signature, subkey, secret)
        made = True
    verify_openpgp_signature(signature, subject, public_key, subkey)

    tokens: list[dict] = []
    previous = {
        entry["authority"]: entry
        for entry in (existing or {}).get("rfc3161_timestamps", [])
    }
    for authority in policy["timestamp_authorities"]:
        token = subject.with_name(f"{subject.name}.{authority['id']}.tsr")
        ca_file = catalog_dir / authority["ca_file"]
        if force or not token.is_file():
            tokens.append(timestamp(subject, token, authority, ca_file))
            made = True
            continue
        verify_timestamp_token(token, subject, ca_file)
        fields = token_fields(token)
        record = previous.get(authority["id"], {})
        tokens.append(
            {
                "authority": authority["id"],
                "filename": token.name,
                "sha256": sha256_file(token),
                "policy_oid": fields["policy_oid"],
                "token_time_utc": record.get("token_time_utc")
                or token_time_to_utc(fields["token_time"]),
            }
        )

    # The recorded time is when the attestation was made, not when this ran: a verifying
    # pass over sidecars that already exist must not move it.
    attested = (existing or {}).get("attested_utc")
    record = {
        "retrospective": True,
        "attested_utc": now_utc() if made or not attested else attested,
        "detached_signature": {
            "filename": signature.name,
            "sha256": sha256_file(signature),
            "signing_subkey_fingerprint": subkey,
        },
        "rfc3161_timestamps": tokens,
    }
    return record, made


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=Path("evidence/manifest.json"))
    parser.add_argument(
        "--evidence-dir",
        type=Path,
        help="directory holding the artifacts (defaults to the catalog value)",
    )
    parser.add_argument(
        "--only",
        action="append",
        default=[],
        help="attest only artifacts whose filename contains this (repeatable)",
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="re-sign and re-timestamp even where sidecars already exist",
    )
    parser.add_argument(
        "--skip-isos",
        action="store_true",
        help="do not attest the ISO masters listed in the catalog",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        require_tools(["gpg", "openssl", "secret-tool"])
        catalog_path = args.manifest
        catalog = load_catalog(catalog_path)
        catalog_dir = catalog_path.resolve().parent
        policy = catalog["attestation"]
        evidence_dir = args.evidence_dir or Path(catalog["default_local_directory"])
        secret = passphrase()

        subjects: list[tuple[dict, Path, bool]] = []
        for artifact in catalog["artifacts"]:
            subjects.append((artifact, evidence_dir / artifact["filename"], False))
        if not args.skip_isos:
            for master in catalog.get("iso_masters", []):
                path = Path(master["directory"]) / master["filename"]
                subjects.append((master, path, True))

        made_any = False
        for entry, path, is_iso in subjects:
            if args.only and not any(part in entry["filename"] for part in args.only):
                continue
            if not path.is_file():
                # An ISO master lives on removable media. Say which one is unreachable
                # rather than failing the whole run or passing in silence.
                if is_iso:
                    print(f"SKIPPED {path} (not present; external media not mounted?)")
                    continue
                raise AttestationError(f"missing artifact {path}")
            record, made = attest_one(
                path,
                entry["sha256"],
                catalog_dir,
                policy,
                args.force,
                secret,
                entry.get("attestations"),
            )
            entry["attestations"] = record
            made_any = made_any or made
            print(
                f"{'ATTESTED' if made else 'VERIFIED '} {entry['filename']} "
                f"sig={record['detached_signature']['filename']} "
                f"tokens={','.join(t['authority'] for t in record['rfc3161_timestamps'])}"
            )
        save_catalog(catalog_path, catalog)
        print(
            f"catalog updated: {catalog_path} "
            f"({'new attestations made' if made_any else 'no new attestations needed'})"
        )
        return 0
    except (AttestationError, KeyError, ValueError, OSError) as exc:
        print(f"evidence attestation failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
