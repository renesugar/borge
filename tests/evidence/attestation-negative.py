#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Prove the evidence verifier's attestation checks can fail.

A verifier that passes is worth nothing until something has been seen to make it fail.
This builds a scratch copy of a small part of the real evidence set, checks that it
verifies, then breaks it one way at a time — the artifact, the signature, the token, the
pinned root, an authority swapped for the other, a missing token — and asserts each time
that verification fails *and* that it fails for the stated reason. A case that fails with
the wrong message counts as a failure: it means the check that caught it is not the check
the case is about, and the intended check is still unproven.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
VERIFIER = ROOT / "scripts" / "verify-evidence.py"
# Two artifacts exercise every check, including the one that needs a second signature.
SAMPLE_COUNT = 2


def sha256_of(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def flip_last_byte(path: Path) -> None:
    data = bytearray(path.read_bytes())
    data[-1] ^= 0xFF
    path.write_bytes(bytes(data))


def read_catalog(manifest: Path) -> dict:
    return json.loads(manifest.read_text(encoding="utf-8"))


def write_catalog(manifest: Path, catalog: dict) -> None:
    manifest.write_text(json.dumps(catalog, indent=2) + "\n", encoding="utf-8")


def run_verifier(manifest: Path, evidence_dir: Path) -> tuple[int, str]:
    result = subprocess.run(
        [
            sys.executable,
            str(VERIFIER),
            "--manifest",
            str(manifest),
            "--evidence-dir",
            str(evidence_dir),
            "--require-attestations",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    return result.returncode, result.stdout + result.stderr


def build_scratch(work: Path) -> tuple[Path, Path]:
    """Copy the catalog, the pinned material, and the smallest attested artifacts."""
    catalog = read_catalog(ROOT / "evidence" / "manifest.json")
    source_dir = Path(catalog["default_local_directory"])
    attested = [a for a in catalog["artifacts"] if a.get("attestations")]
    if len(attested) < SAMPLE_COUNT:
        raise SystemExit(
            "attestation-negative: the catalog has fewer than "
            f"{SAMPLE_COUNT} attested artifacts; run `make evidence-attest` first"
        )
    sample = sorted(attested, key=lambda a: a["size_bytes"])[:SAMPLE_COUNT]

    evidence_dir = work / "evidence-dir"
    evidence_dir.mkdir(parents=True)
    for artifact in sample:
        record = artifact["attestations"]
        names = [artifact["filename"], record["detached_signature"]["filename"]]
        names += [token["filename"] for token in record["rfc3161_timestamps"]]
        for name in names:
            shutil.copy2(source_dir / name, evidence_dir / name)

    catalog_dir = work / "catalog"
    catalog_dir.mkdir(parents=True)
    for subdir in ("keys", "tsa"):
        shutil.copytree(ROOT / "evidence" / subdir, catalog_dir / subdir)
    catalog["artifacts"] = sample
    # ISO masters live on removable media and are not what these cases are about.
    catalog.pop("iso_masters", None)
    manifest = catalog_dir / "manifest.json"
    write_catalog(manifest, catalog)
    return manifest, evidence_dir


# Each case damages one thing in a fresh copy. The expected string must appear in the
# verifier's output, so a case cannot pass by failing for an unrelated reason.


def damage_artifact(manifest: Path, evidence_dir: Path) -> None:
    catalog = read_catalog(manifest)
    flip_last_byte(evidence_dir / catalog["artifacts"][0]["filename"])


def damage_signature(manifest: Path, evidence_dir: Path) -> None:
    catalog = read_catalog(manifest)
    record = catalog["artifacts"][0]["attestations"]["detached_signature"]
    flip_last_byte(evidence_dir / record["filename"])


def substitute_signature(manifest: Path, evidence_dir: Path) -> None:
    """Put the second artifact's signature on the first, and record its real hash.

    Both the file and its catalog hash are then internally consistent; only the
    cryptography disagrees. This is the case a hash-only check cannot see.
    """
    catalog = read_catalog(manifest)
    first, second = catalog["artifacts"][0], catalog["artifacts"][1]
    target = evidence_dir / first["attestations"]["detached_signature"]["filename"]
    donor = evidence_dir / second["attestations"]["detached_signature"]["filename"]
    shutil.copy2(donor, target)
    first["attestations"]["detached_signature"]["sha256"] = sha256_of(target)
    write_catalog(manifest, catalog)


def damage_token(manifest: Path, evidence_dir: Path) -> None:
    catalog = read_catalog(manifest)
    token = catalog["artifacts"][0]["attestations"]["rfc3161_timestamps"][0]
    flip_last_byte(evidence_dir / token["filename"])


def damage_pinned_root(manifest: Path, evidence_dir: Path) -> None:
    catalog = read_catalog(manifest)
    flip_last_byte(
        manifest.parent / catalog["attestation"]["timestamp_authorities"][0]["ca_file"]
    )


def swap_authorities(manifest: Path, evidence_dir: Path) -> None:
    """Attribute each token to the other authority, leaving the files untouched.

    Hashes still match; only the pinned root each token is checked against changes.
    """
    catalog = read_catalog(manifest)
    tokens = catalog["artifacts"][0]["attestations"]["rfc3161_timestamps"]
    tokens[0]["authority"], tokens[1]["authority"] = (
        tokens[1]["authority"],
        tokens[0]["authority"],
    )
    write_catalog(manifest, catalog)


def remove_token(manifest: Path, evidence_dir: Path) -> None:
    catalog = read_catalog(manifest)
    token = catalog["artifacts"][0]["attestations"]["rfc3161_timestamps"][1]
    (evidence_dir / token["filename"]).unlink()


def drop_attestations(manifest: Path, evidence_dir: Path) -> None:
    """An artifact that was never attested: no record, and no sidecars either.

    The sidecars have to go with the record. Leaving them behind trips the
    unlisted-attestation check first — correctly, but that is a different case, and the
    "attestations are required" check would then never be reached.
    """
    catalog = read_catalog(manifest)
    record = catalog["artifacts"][0].pop("attestations")
    (evidence_dir / record["detached_signature"]["filename"]).unlink()
    for token in record["rfc3161_timestamps"]:
        (evidence_dir / token["filename"]).unlink()
    write_catalog(manifest, catalog)


def add_unlisted_sidecar(manifest: Path, evidence_dir: Path) -> None:
    catalog = read_catalog(manifest)
    donor = evidence_dir / (
        catalog["artifacts"][0]["attestations"]["detached_signature"]["filename"]
    )
    shutil.copy2(donor, evidence_dir / "unaccounted-for.asc")


CASES = [
    ("a tampered artifact", damage_artifact, "SHA-256"),
    ("a tampered signature", damage_signature, "SHA-256"),
    (
        "another artifact's signature, with its hash recorded honestly",
        substitute_signature,
        "OpenPGP signature",
    ),
    ("a tampered token", damage_token, "SHA-256"),
    ("a tampered pinned root", damage_pinned_root, "SHA-256"),
    ("each token attributed to the other authority", swap_authorities, "RFC 3161 token"),
    ("a missing token", remove_token, "missing RFC 3161 token"),
    ("an artifact with no attestations", drop_attestations, "no attestations"),
    (
        "an unlisted signature in the evidence directory",
        add_unlisted_sidecar,
        "unlisted attestation files",
    ),
]


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="borge-attestation-negative.") as tmp:
        work = Path(tmp)
        pristine = work / "pristine"
        manifest, evidence_dir = build_scratch(pristine)

        code, output = run_verifier(manifest, evidence_dir)
        if code != 0:
            print(output)
            print(
                "attestation-negative: the untouched copy did not verify",
                file=sys.stderr,
            )
            return 1
        print("BASELINE  the untouched scratch copy verifies")

        failures = 0
        for name, damage, expected in CASES:
            attempt = work / "attempt"
            if attempt.exists():
                shutil.rmtree(attempt)
            shutil.copytree(pristine, attempt)
            case_manifest = attempt / manifest.relative_to(pristine)
            case_dir = attempt / evidence_dir.relative_to(pristine)
            damage(case_manifest, case_dir)
            code, output = run_verifier(case_manifest, case_dir)
            if code == 0:
                print(f"FAIL      {name}: verification passed anyway")
                failures += 1
            elif expected not in output:
                last = output.strip().splitlines()[-1] if output.strip() else "(silent)"
                print(f"FAIL      {name}: failed, but not for {expected!r}: {last}")
                failures += 1
            else:
                print(f"DETECTED  {name}")
        if failures:
            print(
                f"attestation-negative: {failures} case(s) did not behave",
                file=sys.stderr,
            )
            return 1
        print(f"attestation-negative: all {len(CASES)} cases detected")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
