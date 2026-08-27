# PLAN.md — R1 attestations: a signing identity and a timestamp policy

Current plan for one item in [`ROADMAP.md`](ROADMAP.md): **R1, the remaining open work** —
choosing a persistent signing identity and a TSA policy, and applying them to the evidence
record before the project goes to GitHub. When this is finished it moves to
`plans/r1-evidence-attestations-<date>.md` and `ROADMAP.md` records the outcome.
`docs/PORTING_PLAN.md` remains the current plan for the port itself; these two never
describe the same work.

## What is being decided

`docs/EVIDENCE.md` asks four questions and answers none of them. They are now answered:

1. **Signing identity.** The OpenPGP identity `Rene Sugar (Evidence Identity)
   <rene.sugar@gmail.com>`, an Ed25519 certify-only primary key whose secret half is not
   on this machine (`gpg -K` shows `sec#`), with a separate Ed25519 signing subkey that
   expires 2027-08-25. Signing uses the subkey, non-interactively, with the passphrase
   read from the login keyring via `secret-tool`.
2. **Where the public material lives.** The public key is checked into this repository, so
   verification needs no keyserver and no network. Revocation material and the primary
   secret key are offline media and are described, not stored here.
3. **TSA and policy.** Two independent authorities, both requested for every artifact:
   DigiCert (`http://timestamp.digicert.com`, policy `2.16.840.1.114412.7.1`) and Sectigo
   (`http://timestamp.sectigo.com`, policy `1.3.6.1.4.1.6449.2.1.1`). Their root
   certificates are pinned in this repository so a token verifies offline; the token
   itself carries the intermediate chain.
4. **What is covered.** Every artifact in the curated evidence catalog — the top-level
   release ZIPs and any screenshots — individually, plus each ISO master. Per-artifact
   attestation is the point: one disputed file must not invalidate an unrelated one.

**What this does not claim.** Every attestation made now is *retrospective*. The token
time is the time of preservation in August 2026, not the time the tests inside the ZIP
ran, and the manifest says so per artifact. Nothing is backdated.

## Tasks

Each is committable on its own; the tree is never left broken between them.

- [ ] **T1 — material and policy.** Export the public key to `evidence/keys/`, pin the two
  TSA roots in `evidence/tsa/` with a provenance note recording where each came from and
  its SHA-256. Rewrite the open questions in `docs/EVIDENCE.md` as the answers above.
- [ ] **T2 — `scripts/attest-evidence.py`.** Sign and timestamp catalog artifacts and ISO
  masters. Refuse to attest a file whose SHA-256 disagrees with the catalog. Verify each
  token immediately after fetching it. Idempotent: existing sidecars are left alone unless
  `--force`. Record every attestation in `evidence/manifest.json`.
- [ ] **T3 — verification.** Catalog schema 2: per-artifact attestation records with the
  signing subkey fingerprint, and a list of tokens rather than one. `verify-evidence.py`
  checks the signature against the pinned public key, each token against its pinned root
  and the artifact's own bytes, and the sidecar hashes recorded in the catalog. A negative
  self-test proves the checks can fail.
- [ ] **T4 — attest the record.** Run it over the 18 ZIPs and the 2026-08-25 ISO master.
  Commit the updated catalog; the sidecars live beside the artifacts, outside git, and are
  anchored by the hashes in the catalog.
- [ ] **T5 — future masters.** The ISO builder carries the sidecars, the public key and the
  pinned roots into the image, and its `README.txt` stops saying that no signatures are
  claimed.

## Gate

`make evidence-verify` passes with `--require-attestations`: every catalogued artifact has
a good signature from the named subkey and a good token from both authorities, verified
offline against pinned roots and against the artifact's bytes. Tampering with an artifact,
a signature, or a token makes it fail — demonstrated, not assumed. `docs/EVIDENCE.md` no
longer contains an unanswered question, and no attestation is presented as
contemporaneous.
