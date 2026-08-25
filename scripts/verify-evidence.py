#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Verify borge's external evidence ZIPs against the checked-in catalog."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
import zipfile
from pathlib import Path, PurePosixPath


SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
INTERNAL_MANIFEST_RE = re.compile(r"^([0-9a-f]{64})  \./(.+)$")


class VerificationError(Exception):
    """A catalog or artifact did not verify."""


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def load_catalog(path: Path) -> dict:
    try:
        catalog = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise VerificationError(f"cannot read catalog {path}: {exc}") from exc
    if catalog.get("schema_version") != 1:
        raise VerificationError("catalog schema_version must be 1")
    if catalog.get("integrity_algorithm") != "sha256":
        raise VerificationError("catalog integrity_algorithm must be sha256")
    artifacts = catalog.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        raise VerificationError("catalog must contain a non-empty artifacts list")
    return catalog


def provenance_fields(text: str) -> dict[str, str]:
    fields: dict[str, str] = {}
    for line in text.splitlines():
        if line.startswith("#") or ":" not in line:
            continue
        key, value = line.split(":", 1)
        fields[key.strip()] = value.strip()
    return fields


def verify_internal_manifest(archive: zipfile.ZipFile, zip_path: Path) -> None:
    members = [name for name in archive.namelist() if not name.endswith("/")]
    manifests = [name for name in members if PurePosixPath(name).name == "MANIFEST.txt"]
    if len(manifests) != 1:
        raise VerificationError(
            f"{zip_path.name}: expected one internal MANIFEST.txt, found {len(manifests)}"
        )
    manifest_name = manifests[0]
    root = str(PurePosixPath(manifest_name).parent)
    manifest_text = archive.read(manifest_name).decode("utf-8")
    listed: set[str] = set()
    for line_number, line in enumerate(manifest_text.splitlines(), 1):
        match = INTERNAL_MANIFEST_RE.fullmatch(line)
        if not match:
            raise VerificationError(
                f"{zip_path.name}: malformed internal manifest line {line_number}: {line!r}"
            )
        expected_hash, relative_name = match.groups()
        member_name = f"{root}/{relative_name}" if root != "." else relative_name
        if member_name not in members:
            raise VerificationError(
                f"{zip_path.name}: internal manifest names missing member {relative_name}"
            )
        actual_hash = sha256_bytes(archive.read(member_name))
        if actual_hash != expected_hash:
            raise VerificationError(
                f"{zip_path.name}: internal hash mismatch for {relative_name}"
            )
        listed.add(member_name)
    unlisted = set(members) - listed - {manifest_name}
    if unlisted:
        raise VerificationError(
            f"{zip_path.name}: members absent from internal manifest: {sorted(unlisted)}"
        )


def verify_provenance(archive: zipfile.ZipFile, zip_path: Path, artifact: dict) -> None:
    provenance_names = [
        name
        for name in archive.namelist()
        if PurePosixPath(name).name == "PROVENANCE.txt"
    ]
    if len(provenance_names) != 1:
        raise VerificationError(
            f"{zip_path.name}: expected one PROVENANCE.txt, found {len(provenance_names)}"
        )
    fields = provenance_fields(archive.read(provenance_names[0]).decode("utf-8"))
    expected_created = artifact["created_utc"].replace("-", "").replace(":", "")
    expected_created = expected_created.replace("T", "T")
    expected = {
        "stage": artifact["stage_id"],
        "created (UTC)": expected_created,
        "commit": artifact["git_commit"],
        "tree clean": "yes" if artifact["tree_clean"] else "no",
    }
    for key, value in expected.items():
        if fields.get(key) != value:
            raise VerificationError(
                f"{zip_path.name}: provenance {key!r} is {fields.get(key)!r}, expected {value!r}"
            )


def run_attestation(command: list[str], description: str) -> None:
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        detail = (result.stderr or result.stdout).strip()
        raise VerificationError(f"{description} failed: {detail}")


def verify_attestations(
    evidence_dir: Path, zip_path: Path, artifact: dict, require: bool
) -> int:
    signature = artifact.get("detached_signature_filename")
    timestamp = artifact.get("rfc3161_timestamp_filename")
    if signature:
        signature_path = evidence_dir / signature
        if not signature_path.is_file():
            raise VerificationError(f"missing detached signature {signature_path}")
        run_attestation(
            ["gpg", "--batch", "--verify", str(signature_path), str(zip_path)],
            f"{zip_path.name}: GPG signature",
        )
    if timestamp:
        timestamp_path = evidence_dir / timestamp
        ca_filename = artifact.get("tsa_ca_filename")
        if not timestamp_path.is_file() or not ca_filename:
            raise VerificationError(
                f"{zip_path.name}: timestamp verification needs token and tsa_ca_filename"
            )
        ca_path = evidence_dir / ca_filename
        if not ca_path.is_file():
            raise VerificationError(f"missing TSA CA file {ca_path}")
        run_attestation(
            [
                "openssl",
                "ts",
                "-verify",
                "-in",
                str(timestamp_path),
                "-data",
                str(zip_path),
                "-CAfile",
                str(ca_path),
            ],
            f"{zip_path.name}: RFC 3161 timestamp",
        )
    if not signature or not timestamp:
        if require:
            raise VerificationError(
                f"{zip_path.name}: detached signature and RFC 3161 token are required"
            )
        return 1
    return 0


def verify_artifact(
    evidence_dir: Path, artifact: dict, require_attestations: bool
) -> int:
    required = {
        "filename",
        "size_bytes",
        "sha256",
        "stage_id",
        "created_utc",
        "git_commit",
        "tree_clean",
        "disposition",
    }
    missing = sorted(required - artifact.keys())
    if missing:
        raise VerificationError(f"catalog artifact is missing fields: {missing}")
    filename = artifact["filename"]
    if Path(filename).name != filename or not filename.endswith(".zip"):
        raise VerificationError(f"unsafe or non-ZIP artifact filename: {filename!r}")
    if not SHA256_RE.fullmatch(artifact["sha256"]):
        raise VerificationError(f"invalid SHA-256 for {filename}")
    zip_path = evidence_dir / filename
    if not zip_path.is_file():
        raise VerificationError(f"missing artifact {zip_path}")
    actual_size = zip_path.stat().st_size
    if actual_size != artifact["size_bytes"]:
        raise VerificationError(
            f"{filename}: size {actual_size}, expected {artifact['size_bytes']}"
        )
    actual_hash = sha256_file(zip_path)
    if actual_hash != artifact["sha256"]:
        raise VerificationError(
            f"{filename}: SHA-256 {actual_hash}, expected {artifact['sha256']}"
        )
    try:
        with zipfile.ZipFile(zip_path) as archive:
            bad_member = archive.testzip()
            if bad_member:
                raise VerificationError(f"{filename}: ZIP CRC failed for {bad_member}")
            verify_internal_manifest(archive, zip_path)
            verify_provenance(archive, zip_path, artifact)
    except zipfile.BadZipFile as exc:
        raise VerificationError(f"{filename}: invalid ZIP: {exc}") from exc
    return verify_attestations(evidence_dir, zip_path, artifact, require_attestations)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--manifest",
        type=Path,
        default=Path("evidence/manifest.json"),
        help="checked-in evidence catalog",
    )
    parser.add_argument(
        "--evidence-dir",
        type=Path,
        help="directory holding the ZIPs (defaults to the catalog value)",
    )
    parser.add_argument(
        "--allow-unlisted",
        action="store_true",
        help="permit ZIPs in the directory that the catalog does not list",
    )
    parser.add_argument(
        "--require-attestations",
        action="store_true",
        help="fail unless every ZIP has a detached signature and RFC 3161 token",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        catalog = load_catalog(args.manifest)
        evidence_dir = args.evidence_dir or Path(catalog["default_local_directory"])
        filenames = [artifact.get("filename") for artifact in catalog["artifacts"]]
        if len(filenames) != len(set(filenames)):
            raise VerificationError("catalog contains duplicate artifact filenames")
        if not evidence_dir.is_dir():
            raise VerificationError(f"evidence directory does not exist: {evidence_dir}")
        actual_zips = {path.name for path in evidence_dir.glob("*.zip")}
        expected_zips = set(filenames)
        unlisted = sorted(actual_zips - expected_zips)
        if unlisted and not args.allow_unlisted:
            raise VerificationError(f"unlisted ZIPs in evidence directory: {unlisted}")
        unattested = 0
        for artifact in catalog["artifacts"]:
            unattested += verify_artifact(
                evidence_dir, artifact, args.require_attestations
            )
            print(
                f"VERIFIED {artifact['filename']} "
                f"{artifact['sha256']} ({artifact['disposition']})"
            )
        print(
            f"verified {len(catalog['artifacts'])} artifact(s); "
            f"{unattested} lack one or both optional attestations"
        )
        return 0
    except (KeyError, TypeError, VerificationError) as exc:
        print(f"evidence verification failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
