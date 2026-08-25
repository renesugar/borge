# borge roadmap

`docs/PORTING_PLAN.md` owns the borg-to-Go port, its compatibility gates, and Stages 0-10.
This file owns work that matters to the product but is not a porting stage.

## Current priorities

### R1. Preserve the pre-GitHub evidence record

State: **first reserve ISO master built and verified 2026-08-25 UTC, before the first
GitHub push.**

- [x] Inventory all 18 stage ZIPs, including failed and superseded runs, in
  `evidence/manifest.json` with SHA-256 hashes and provenance.
- [x] Add a verifier that checks the outer files, ZIP CRCs, internal manifests, and
  provenance without depending on filenames alone.
- [x] Add a reproducible ISO builder that includes the ZIPs, the catalog, and a complete
  Git bundle, then performs an extracted readback verification.
- [x] Document what the archive proves and does not prove in `docs/EVIDENCE.md`.
- [x] First reserve master built at
  `/media/renes/SEAGATE2TB/borge-evidence-isos/borge-evidence-stages-0-8-20260825.iso`
  (4,714,496 bytes), with SHA-256
  `913f4c8b21079c7d4a8341f3beca976507207c78eadda6af5ce9ac0fba239d01`.
  Its `.sha256` sidecar and `.contents.txt` listing are stored beside it; an extracted
  readback verified every payload file and `git bundle verify` passed.
- [ ] Before the next stage closes, choose a persistent signing identity and TSA policy.
  The historical ZIPs are explicitly retrospective and unsigned; do not backdate them.
- [ ] Burn at least two CD-R copies from the verified master, verify each whole-disc
  readback on two readers when practical, and store the copies in different locations.
- [ ] Keep one independently backed-up digital copy of the ISO, sidecar, and burn/custody
  log. Optical media is an additional preservation copy, not the only copy.

Acceptance: the checked-in catalog verifies against the local ZIP directory; the ISO
extracts and its payload manifest verifies; `git bundle verify` passes; the ISO SHA-256 is
recorded beside the image and here; no unlisted ZIP is omitted.

### R2. Complete the documentation system

State: **1 of 7 original doc-anchor items done.** The design and findings remain in
`docs/PORTING_PLAN.md` §2.1; current execution is tracked here because it is documentation
infrastructure, not part of the borg port.

- [ ] Build `docaudit`: parse `//borge:*` anchors, report verification grades per topic,
  and fail on dangling help anchors or claims without registered checks.
- [ ] Generate enumerations already checked ad hoc: environment variables, pattern styles,
  compression specs, and placeholders.
- [ ] Build `docgen --help`, topic templates, and `TestDocsAreCurrent`; migrate the five
  hand-written help topics.
- [ ] Decide whether `docgen --api` adds enough over `go doc ./internal/...` to justify an
  `docs/INTERNALS.md`; record an explicit no if it does not.
- [ ] Build the calibrated, advisory contradiction checker over user-facing anchored prose.
- [x] Execute every help example and assert its effect (`TestHelpExamplesRun`, 2026-08-18).
- [ ] Build `docactionable`: generate a command from each topic and run it against the
  existing scratch-repository harness; keep it advisory.

Acceptance: no dangling anchors or orphan claims; every help topic has at least one
executed example and a recorded verification-grade breakdown; generated documentation is
fresh; advisory checks are calibrated against the known before/after cases in the plan.

### R3. Build a GUI frontend

State: **future, after the CLI and performance work are stable.**

The GUI is a frontend to the command-line JSON API, not a second repository
implementation. Keeping all repository operations in `borge` preserves one tested format
boundary and makes the GUI replaceable.

First useful slice:

- repository profiles with local and remote locations, without storing passphrases in the
  profile itself;
- archive browsing and search;
- backup creation with a preview of paths, exclusions, compression, encryption, and
  destination;
- restore to a chosen directory with conflict handling and a post-restore summary;
- structured progress, warnings, and errors driven by `--json`, `--json-lines`, and
  `--log-json` rather than scraped prose;
- explicit presentation of borge-only options and the borg 1.x non-goal.

Safety and architecture gates:

- [ ] First make the CLI emit stable message ids for errors and the progress objects a GUI
  needs; absence is preferable to invented zero-valued fields.
- [ ] Write a versioned frontend contract from the existing differential JSON-schema
  tests, including non-Unicode path handling and stdout/stderr ownership.
- [ ] Keep destructive actions previewable, name the selected archives, and require a
  deliberate confirmation; an empty selection must remain an error for writes.
- [ ] Use the OS credential store for secrets and never place S3 credentials or
  passphrases in logs, process titles, profiles, or canonical location strings.
- [ ] Start read-only (connect, list, inspect), then restore, then create, then retention
  and deletion.
- [ ] Test Linux first, then measure the packaging and metadata gaps on macOS, Windows, and
  mobile rather than promising them from cross-compilation alone.

Acceptance for a first release: the GUI can connect, browse, create, and restore through
the public CLI JSON contract; every destructive workflow has a non-vacuous end-to-end test;
no repository-format code is duplicated in the frontend.

## Later maintenance

- Rebuild an evidence ISO after each release or when the cumulative catalog changes
  materially. Never overwrite an old master; each image and sidecar are immutable records.
- Re-verify stored ISO masters and physical discs periodically and migrate before readers
  or media become unreliable.
- Publish evidence ZIPs as release assets or in immutable object storage once hosting and
  retention are chosen; the Git manifest is an index, not a substitute for the artifacts.
- Revisit this roadmap at every release. Work moved into the porting plan must be removed
  here so two trackers cannot disagree silently.
