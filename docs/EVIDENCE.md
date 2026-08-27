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
claim, and disposition for each ZIP, and since 2026-08-27 the attestations described
below: a detached OpenPGP signature and one RFC 3161 token from each of two authorities,
each recorded by filename and hash. Every one of them is marked `retrospective`, because
that is what they are. Creating them now attests to the artifacts' existence now; it
cannot manufacture a contemporaneous signature or timestamp for August 16-22, and the
catalog does not pretend otherwise.

The catalog also lists the ISO masters, so an image's hash is checked by the same tool as
the ZIPs rather than living only in prose. A master on unmounted media is reported as
unreachable, never as verified.

Each ISO master contains:

- the exact ZIPs listed in the checked-in manifest, and, since 2026-08-27, each ZIP's
  signature and timestamp tokens beside it;
- the signing identity's public key in `keys/` and the pinned TSA roots in `tsa/`, so the
  image verifies with nothing but itself and the tools;
- a Git bundle containing `v0.8.0` and its complete reachable history, so every commit
  named by the stage 0-8 ZIPs survives even if no remote copy exists;
- the checked-in evidence manifest and preservation documentation;
- `ISO-CONTENTS.sha256`, covering every payload file in the image;
- build information identifying the Git commit, xorriso version, fixed image timestamp,
  and command used.

The 2026-08-25 master predates the attestations and does not contain them; it is immutable
and is not rebuilt. Its own signature and tokens are sidecars beside it, and the next
master carries everything inside.

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
ZIPs by default so that a bundle cannot fall between the catalog and the ISO unnoticed,
and rejects an unlisted `.asc` or `.tsr` for the same reason.

Prove the attestation checks can fail before trusting one that passed:

```bash
make evidence-negative
```

It copies part of the real set, damages it nine ways — the artifact, the signature, a
token, a pinned root, an authority attributed to the other's token, a missing token, a
missing record, an unaccounted-for sidecar, and a signature that is genuine but over
different bytes — and requires each to be caught *and* caught by the check it is about.

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

Decided 2026-08-27, after the first reserve master and before the first GitHub push. The
four questions this section used to leave open are answered here; the archive workflow
still refuses to generate an anonymous key merely to make the word "signed" true.

**1. Which identity signs.** `Rene Sugar (Evidence Identity) <rene.sugar@gmail.com>`, an
Ed25519 OpenPGP key created 2026-08-25. The primary key
`AEE5F82F2C216D6D15992C8DC96A1C6039BC8098` is certify-only and its secret half is not on
this machine; artifacts are signed by the subkey
`4ABEB98AF99C8321931BCF282C6A8A4568264005`, which expires 2027-08-25. Signing is
non-interactive: the passphrase comes from the login keyring
(`secret-tool lookup service gpg_evidence type passphrase`) and is passed to `gpg` on a
file descriptor, so it is never an argument and never enters shell history.

A signature identifies a key, not a person, and is worth exactly what the key's custody is
worth. The custody claim here is narrow and checkable: the signing subkey is replaceable
and expires; the primary secret key and the revocation certificate are kept offline, and
where they are kept belongs in the custody log with the ISO masters, not in this
repository.

**2. Where the public material lives.** [`evidence/keys/`](../evidence/keys) — the public
key is checked in, so verification needs no keyserver and no network.
`scripts/verify-evidence.py` builds a temporary keyring from that file alone, so a
signature from an unrelated key already in the operator's keyring cannot pass.

**3. Which authorities, and how their material is retained.** Two independent RFC 3161
authorities, both requested for every artifact:

| authority | endpoint | policy OID |
| --- | --- | --- |
| DigiCert | `http://timestamp.digicert.com` | `2.16.840.1.114412.7.1` |
| Sectigo | `http://timestamp.sectigo.com` | `1.3.6.1.4.1.6449.2.1.1` |

Two authorities rather than one because a single TSA is a single point of trust and of
survival: an authority whose root is distrusted, or which stops publishing, should not be
able to take the whole record's dating with it. Each token embeds its own certificate
chain, and the roots those chains terminate in are pinned in
[`evidence/tsa/`](../evidence/tsa) so verification works offline and does not drift with
the machine's system trust store. The catalog records which pinned root belongs to which
token, and verification uses that one alone.

Revocation data is deliberately *not* fetched at verification time: an offline verifier
cannot reach an OCSP responder or a CRL, and a check that silently passes when the network
is absent is worse than no check. The residual risk is stated rather than hidden — a token
verified here proves the chain and the imprint, not that the TSA certificate was
unrevoked at the moment of use.

**4. What is covered.** Every artifact in the curated catalog — the top-level release ZIPs
and any screenshots — individually, plus each ISO master. Per-artifact attestation is the
point: one damaged or disputed file must not invalidate an attestation over an unrelated
file. The ISO signature is an inventory seal on top of that, not a replacement for it.

**Every attestation on the historical record is retrospective.** The tokens were obtained
in August 2026, long after the stage runs they cover. Each artifact's catalog entry carries
`retrospective: true` and the UTC time it was attested. A token binds bytes to a time *no
later than* the token's; it says nothing about when the tests inside those bytes ran, and
the catalog never suggests otherwise.

Attest and verify:

```bash
make evidence-attest        # sign and timestamp anything not yet attested
make evidence-verify-full   # verify, and require every attestation to be present and good
```

`attest-evidence.py` refuses to attest a file whose SHA-256 disagrees with the catalog,
verifies each token as soon as it arrives, and leaves existing sidecars alone unless
`--force`. The sidecars live beside the artifacts, outside git; their own hashes are
recorded in the checked-in catalog, so a replaced signature is as detectable as a replaced
ZIP.

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
