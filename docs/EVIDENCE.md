# Evidence preservation

borge's stage ZIPs are engineering records. They preserve the commit identity, tree state,
toolchain and upstream pin, test output, coverage gates, and an internal SHA-256 manifest
captured when a stage gate ran. They make later review and reproduction materially easier.

They are **not** proof of a strict clean-room process. `docs/LICENSING.md` says plainly that
borge is a source-informed Go port and that much of it is a derivative work used under
borg's BSD license. There was no dirty-room/clean-room firewall to prove. Nor does a ZIP,
hash, signature, timestamp token, ISO, or CD make evidence automatically admissible or
prove that the statements inside it are true.

The narrower claims are useful and defensible:

- a SHA-256 match is strong evidence that two byte sequences are identical;
- a detached signature can identify the key that signed those bytes, subject to how that
  key was controlled and attributed;
- an RFC 3161 token can bind a TSA's asserted time to a message imprint, subject to
  validation of the token, TSA certificate, policy, and revocation information;
- a finalized CD-R is write-once media, but custody, handling, verification, redundancy,
  and readable hardware remain separate questions.

This is an engineering preservation policy, not legal advice. If these records become
relevant to actual litigation, counsel and a qualified evidence custodian should decide
how they are collected, certified, retained, and produced. Federal Rule of Evidence
902(14), for example, describes self-authentication of copied electronic data through a
qualified-person certification; a hash by itself is not that certification.

Authoritative references:

- [Federal Rule of Evidence 902(13)-(14)](https://www.law.cornell.edu/rules/fre/rule_902)
- [NIST IR 8387, Digital Evidence Preservation](https://doi.org/10.6028/NIST.IR.8387)
- [RFC 3161, Internet X.509 Public Key Infrastructure Time-Stamp Protocol](https://www.rfc-editor.org/rfc/rfc3161)
- [GNU xorriso reproducibility documentation](https://www.gnu.org/software/xorriso/man_1_xorriso.html)
- [Library of Congress recordable optical-disc longevity research](https://www.loc.gov/preservation/scientists/projects/cd-r_dvd-r_rw_longevity.html)
- [U.S. Copyright Office Circular 33](https://www.copyright.gov/circs/circ33.pdf)

## What is preserved

The checked-in [`evidence/manifest.json`](../evidence/manifest.json) inventories every ZIP
currently in `/home/renes/evidence/borge`, including superseded runs that recorded a failed
gate. The failed bundles stay in the catalog: they explain why later clean bundles exist
and are part of the chronology. A `disposition` field identifies which bundle is the one
to cite for a completed stage.

The manifest records SHA-256, byte length, stage id, UTC creation time, commit, clean-tree
claim, and disposition for each ZIP. It also records honestly that the historical ZIPs
were catalogued retrospectively and have neither detached signatures nor RFC 3161 tokens.
Creating those now could attest only to their existence now; it cannot manufacture a
contemporaneous signature or timestamp for August 16-22.

Each ISO master contains:

- the exact ZIPs listed in the checked-in manifest;
- a Git bundle containing `v0.8.0` and its complete reachable history, so every commit
  named by the stage 0-8 ZIPs survives even if no remote copy exists;
- the checked-in evidence manifest and preservation documentation;
- `ISO-CONTENTS.sha256`, covering every payload file in the image;
- build information identifying the Git commit, xorriso version, fixed image timestamp,
  and command used.

The ISO's own SHA-256 is a sidecar beside the ISO. It cannot be placed inside the image it
hashes: doing so would change the image. For the same reason the checked-in ZIP manifest
does not pretend to be a self-hash of the ISO.

## Local workflow

Verify the historical ZIP catalog:

```bash
make evidence-verify
```

The verifier checks the outer size and SHA-256, ZIP CRCs, each ZIP's internal
`MANIFEST.txt`, and the stage/commit/tree state in `PROVENANCE.txt`. It rejects unlisted
ZIPs by default so that a bundle cannot fall between the catalog and the ISO unnoticed.

After committing the manifest and documentation, build the reserve ISO on the external
disk:

```bash
make evidence-isos
```

The builder first runs the verifier, creates and verifies the `v0.8.0` Git bundle, fixes the image
timestamps through `SOURCE_DATE_EPOCH`, creates a Rock Ridge/Joliet ISO 9660 image with
xorriso, extracts it again, and verifies `ISO-CONTENTS.sha256` against the extraction. It
then writes the ISO SHA-256 and an xorriso listing beside the image. It refuses to overwrite
an existing master.

The default output directory is `/media/renes/SEAGATE2TB/borge-evidence-isos`. Override
the inputs without editing the script:

```bash
BORGE_EVIDENCE_DIR=/path/to/zips \
BORGE_EVIDENCE_ISO_DIR=/path/to/masters \
make evidence-isos
```

The release ref is deliberately fixed rather than `HEAD`: recording the ISO's hash in a
later roadmap commit must not change the Git bundle inside the ISO and create a circular
self-reference.

The current set fits easily on one CD. The builder uses 650,000,000 bytes as the
conservative payload ceiling and refuses a larger set instead of silently producing an
image that will not fit the intended media. Multi-volume partitioning is added when the
catalog first approaches that bound; ZIPs are never split across volumes.

## Signing and timestamping policy

No secret GPG key is currently configured on this machine, and no signing identity or TSA
retention policy has been chosen. The historical catalog therefore says `null` for both
attestations. The archive workflow must not generate an anonymous key merely to make the
word "signed" true.

Before the next stage closes, decide and document:

1. which long-lived signing identity controls the detached-signature key;
2. where its public key and revocation material are preserved;
3. which TSA and policy are acceptable, how its certificate chain and revocation data are
   retained, and how verification works without network access;
4. whether signatures and RFC 3161 tokens cover each stage ZIP, the ISO, or both.

Per-artifact signatures are preferable because one damaged or disputed file does not
invalidate an attestation over an unrelated file. A final ISO signature is still useful as
an inventory seal, but does not replace per-ZIP attestations. A retrospective token must be
labelled with its real timestamp; it says nothing about when the enclosed tests originally
ran.

## Burning and custody

Burn every physical copy from the preserved ISO master in a single finalized session. Do
not assume that a low numeric burn speed is inherently more reliable for all drives and
media; use a speed supported by the particular combination and retain the burn log. After
burning:

1. read the whole disc back and compare its SHA-256 with the ISO sidecar;
2. verify it on a second drive or machine when practical;
3. assign a copy id and record media brand/lot, drive and firmware, burn command, operator,
   UTC time, readback hash, verification result, storage location, and every custody move;
4. keep at least two copies in different physical locations, plus the ISO master and its
   sidecars on independently backed-up storage;
5. inspect and migrate periodically. CD-R is WORM, not immortal, and access also depends on
   compatible readers remaining available.

Rock Ridge can preserve POSIX metadata in an ISO, but that is not the purpose of this
collection: the evidence payloads are ordinary files whose bytes are authenticated by
hash. ISO 9660 alone does not preserve arbitrary POSIX metadata "exactly", and the archive
policy makes no such claim.
