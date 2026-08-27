#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Verify borge's external evidence ZIPs against the checked-in catalog."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import zipfile
from pathlib import Path, PurePosixPath

sys.path.insert(0, str(Path(__file__).resolve().parent))

from evidence_common import (  # noqa: E402
    AttestationError,
    sha256_file,
    token_fields,
    verify_openpgp_signature,
    verify_timestamp_token,
)

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
INTERNAL_MANIFEST_RE = re.compile(r"^([0-9a-f]{64})  \./(.+)$")


class VerificationError(Exception):
    """A catalog or artifact did not verify."""


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def load_catalog(path: Path) -> dict:
    try:
        catalog = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise VerificationError(f"cannot read catalog {path}: {exc}") from exc
    if catalog.get("schema_version") != 2:
        raise VerificationError("catalog schema_version must be 2")
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


def verify_pinned_material(catalog_dir: Path, policy: dict) -> None:
    """Check the pinned key and roots are the ones the catalog names.

    A pinned file that was quietly replaced would verify every signature made by the
    replacement, so the catalog records each file's SHA-256 and this checks it before
    any artifact is examined.
    """
    key = policy["signing_key"]
    entries = [(catalog_dir / key["public_key_file"], key["public_key_sha256"])]
    for authority in policy["timestamp_authorities"]:
        entries.append((catalog_dir / authority["ca_file"], authority["ca_sha256"]))
    for path, expected in entries:
        if not path.is_file():
            raise VerificationError(f"missing pinned file {path}")
        actual = sha256_file(path)
        if actual != expected:
            raise VerificationError(
                f"{path}: SHA-256 {actual}, catalog says {expected}"
            )


def verify_attestations(
    subject: Path, entry: dict, catalog_dir: Path, policy: dict, require: bool
) -> int:
    """Verify one artifact's signature and tokens. Returns 1 if it has none."""
    record = entry.get("attestations")
    if not record:
        if require:
            raise VerificationError(
                f"{subject.name}: no attestations, and they are required"
            )
        return 1
    key = policy["signing_key"]
    public_key = catalog_dir / key["public_key_file"]
    expected_subkey = key["signing_subkey_fingerprint"]
    authorities = {a["id"]: a for a in policy["timestamp_authorities"]}

    signature_record = record["detached_signature"]
    if signature_record["signing_subkey_fingerprint"] != expected_subkey:
        raise VerificationError(
            f"{subject.name}: recorded signing subkey is not the catalog's identity"
        )
    signature = subject.with_name(signature_record["filename"])
    if not signature.is_file():
        raise VerificationError(f"missing detached signature {signature}")
    actual = sha256_file(signature)
    if actual != signature_record["sha256"]:
        raise VerificationError(
            f"{signature.name}: SHA-256 {actual}, catalog says {signature_record['sha256']}"
        )
    try:
        verify_openpgp_signature(signature, subject, public_key, expected_subkey)
    except AttestationError as exc:
        raise VerificationError(str(exc)) from exc

    seen = set()
    for token_record in record["rfc3161_timestamps"]:
        authority = authorities.get(token_record["authority"])
        if authority is None:
            raise VerificationError(
                f"{subject.name}: token from unknown authority "
                f"{token_record['authority']!r}"
            )
        token = subject.with_name(token_record["filename"])
        if not token.is_file():
            raise VerificationError(f"missing RFC 3161 token {token}")
        actual = sha256_file(token)
        if actual != token_record["sha256"]:
            raise VerificationError(
                f"{token.name}: SHA-256 {actual}, catalog says {token_record['sha256']}"
            )
        try:
            verify_timestamp_token(token, subject, catalog_dir / authority["ca_file"])
            fields = token_fields(token)
        except AttestationError as exc:
            raise VerificationError(str(exc)) from exc
        if fields["policy_oid"] != authority["policy_oid"]:
            raise VerificationError(
                f"{token.name}: policy {fields['policy_oid']}, catalog expects "
                f"{authority['policy_oid']}"
            )
        seen.add(authority["id"])
    missing = sorted(set(authorities) - seen)
    if missing and require:
        raise VerificationError(
            f"{subject.name}: no token from {', '.join(missing)}"
        )
    return 0


def verify_artifact(
    evidence_dir: Path,
    artifact: dict,
    catalog_dir: Path,
    policy: dict,
    require_attestations: bool,
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
    return verify_attestations(
        zip_path, artifact, catalog_dir, policy, require_attestations
    )


def verify_iso_master(
    master: dict, catalog_dir: Path, policy: dict, require: bool
) -> str:
    """Verify one ISO master where it is reachable.

    The masters live on removable media. An unreachable one is reported as unreachable,
    never as verified: a silent skip is how an archive comes to believe it holds
    something it does not.
    """
    iso_path = Path(master["directory"]) / master["filename"]
    if not iso_path.is_file():
        if require:
            raise VerificationError(f"ISO master not present: {iso_path}")
        return f"UNREACHABLE {iso_path} (external media not mounted?)"
    actual_size = iso_path.stat().st_size
    if actual_size != master["size_bytes"]:
        raise VerificationError(
            f"{master['filename']}: size {actual_size}, expected {master['size_bytes']}"
        )
    actual_hash = sha256_file(iso_path)
    if actual_hash != master["sha256"]:
        raise VerificationError(
            f"{master['filename']}: SHA-256 {actual_hash}, expected {master['sha256']}"
        )
    sidecar = iso_path.with_name(iso_path.name + ".sha256")
    if sidecar.is_file():
        recorded = sidecar.read_text(encoding="utf-8").split()[0]
        if recorded != master["sha256"]:
            raise VerificationError(
                f"{sidecar.name}: says {recorded}, catalog says {master['sha256']}"
            )
    unattested = verify_attestations(iso_path, master, catalog_dir, policy, require)
    state = "unattested" if unattested else "attested"
    return f"VERIFIED {master['filename']} {master['sha256']} (ISO master, {state})"


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
        help=(
            "fail unless every artifact and reachable ISO master carries a good "
            "signature and a token from every authority the catalog names"
        ),
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        catalog = load_catalog(args.manifest)
        catalog_dir = args.manifest.resolve().parent
        policy = catalog["attestation"]
        verify_pinned_material(catalog_dir, policy)
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
        # A signature or token the catalog does not name is the same gap as an unlisted
        # ZIP: something in the evidence directory that nothing accounts for.
        expected_sidecars = set()
        for artifact in catalog["artifacts"]:
            record = artifact.get("attestations") or {}
            if record:
                expected_sidecars.add(record["detached_signature"]["filename"])
                for token in record["rfc3161_timestamps"]:
                    expected_sidecars.add(token["filename"])
        actual_sidecars = {
            path.name
            for pattern in ("*.asc", "*.tsr")
            for path in evidence_dir.glob(pattern)
        }
        unlisted_sidecars = sorted(actual_sidecars - expected_sidecars)
        if unlisted_sidecars and not args.allow_unlisted:
            raise VerificationError(
                f"unlisted attestation files in evidence directory: {unlisted_sidecars}"
            )
        unattested = 0
        for artifact in catalog["artifacts"]:
            unattested += verify_artifact(
                evidence_dir, artifact, catalog_dir, policy, args.require_attestations
            )
            print(
                f"VERIFIED {artifact['filename']} "
                f"{artifact['sha256']} ({artifact['disposition']})"
            )
        for master in catalog.get("iso_masters", []):
            print(
                verify_iso_master(
                    master, catalog_dir, policy, args.require_attestations
                )
            )
        print(
            f"verified {len(catalog['artifacts'])} artifact(s) and "
            f"{len(catalog.get('iso_masters', []))} ISO master(s); "
            f"{unattested} artifact(s) carry no attestations"
        )
        return 0
    except (KeyError, TypeError, VerificationError) as exc:
        print(f"evidence verification failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
