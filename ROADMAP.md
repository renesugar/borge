# borge roadmap

This file owns the work that is not a porting stage, and it outlives the porting plan.

While the port is open, [`docs/PORTING_PLAN.md`](docs/PORTING_PLAN.md) owns the
borg-to-Go port, its compatibility gates, and Stages 0-9. When stage 9 closes, that plan
is archived in [`plans/`](plans/) and the current documents become this roadmap plus
`PLAN.md`, the plan for whatever roadmap item is being implemented now. `AGENTS.md`
describes that workflow.

**On the numbering.** R0 is not the first priority; it is the oldest item. It was Stage 10
of the porting plan and moved here on 2026-08-27, because it is work that begins after the
port is complete. The identifier is a name, not a rank. The current priorities are R1 and
R2, in that order.

## Current priorities

### R1. Preserve the pre-GitHub evidence record

State: **catalogued, attested and verified; one item open.** The first reserve ISO master
was built and verified 2026-08-25 UTC and the record was signed and timestamped
2026-08-27, both before the first GitHub push.

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
- [x] Choose a persistent signing identity and TSA policy, and apply them (2026-08-27).
  The identity is an offline-primary OpenPGP key signing through a subkey that expires
  2027-08-25; the timestamps come from DigiCert and Sectigo, both requested for every
  artifact, verified offline against roots pinned in `evidence/tsa/`. All 18 ZIPs and the
  ISO master carry a signature and two tokens, recorded in `evidence/manifest.json` and
  marked `retrospective`: nothing is backdated, and a token dates the bytes rather than
  the tests inside them. `make evidence-negative` demonstrates that the checks fail when
  the record is damaged.
- [ ] Keep one independently backed-up digital copy of the ISO, sidecar, and burn/custody
  log. Optical media is an additional preservation copy, not the only copy.

Acceptance: the checked-in catalog verifies against the local ZIP directory; every
artifact carries a good signature from the named subkey and a good token from each
authority (`make evidence-verify-full`); the ISO extracts and its payload manifest
verifies; `git bundle verify` passes; the ISO SHA-256 is recorded beside the image and
here; no unlisted ZIP or attestation file is omitted. Building the ISO masters in
a directory on `/media/renes/SEAGATE2TB` satisfies this item; physical discs are
preservation work and are tracked under *Later maintenance*, where they block nothing.

### R2. Complete the documentation system

State: **1 of 7 original doc-anchor items done.** The design and findings remain in
`docs/PORTING_PLAN.md` §2.1; current execution is tracked here because it is documentation
infrastructure, not part of the borg port. The design moves into this repository's
non-porting documents when R2 is implemented, so that archiving the porting plan does not
archive the specification of unfinished work.

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

When R2 is complete, a GitHub project is created and the completed project is pushed to
`origin`. After that first push, `main` is protected: work lands on `develop` and is
merged into `main` by pull request, which is the branch model `docs/PORTING_PLAN.md` §2
settled on.

## After the port closes

### R0. Format and indexing changes

State: **not started.** Moved here from `docs/PORTING_PLAN.md` §13 (Stage 10) on
2026-08-27. Only after Stages 7 and 9. Everything here **breaks format compatibility**, so
it goes behind an explicit repository version bump and a documented migration.

1. **Large-directory packing.** borg 2's `PackWriter` already packs *chunks*. The
   remaining problem is the restore side: extracting 118,866 files from one directory
   means 118,866 `create`+`write`+`close`+`utimes`+`chown` sequences, and on a slow or
   high-latency filesystem that, not I/O bandwidth, is the wall. Investigate:
   restore-side batching by pack (sort extraction order by `(pack_id, obj_offset)` so
   each pack is read once and sequentially), deferred metadata application (write all
   content, then apply modes/times/xattrs in a second pass), and parallel writers per
   directory. **Note this is measurable and possibly deliverable without any format
   change at all** — try it in Stage 9 first, and only change the format if Stage 9
   proves it is not enough.
2. **`blugelabs/bluge` for indexing.** Evaluate as a replacement for the chunk index
   and/or as a new capability (content/metadata search across archives — `borg find`
   is currently a linear scan). Bluge worked well in `movenotes-v3`. Be honest about
   the fit: bluge is an inverted-index search engine, and the chunk index is a
   256-bit-key hash table with a 48-byte value — a different data structure for a
   different job. The likely outcome is **bluge for archive/file search, borghash
   retained for the chunk index**, but measure before concluding.
3. **zstd as the default compression** (borg #10085) once the benchmark supports it —
   the reference numbers give zstd `SpeedFastest` a better ratio *and* comparable
   speed versus lz4-class options.
4. Any further on-disk changes the Stage 9 profiles justify.

#### R0.1 borg quirks to fix once compatibility is lifted

Until this work starts, borge reproduces borg's behaviour including its bugs — a port that
"fixes" one silently is a port whose output no longer matches, which is the one thing the
interop gate exists to prevent. Each of these is a place where the compatible behaviour is
worse than the obvious one. The list is collected here so that lifting the constraint is a
review of known items rather than a fresh audit.

**Reproduced bugs, to be corrected:**

- **`shellpattern.translate`'s vacuous guard.** borg's `(`, `|` and `)` passthrough checks
  `pat[i-1] != "\\"` *after* `i` has already advanced, so the guard always passes and
  `\(` becomes a backslash plus a group opener rather than a literal parenthesis. borge
  reproduces it (see `docs/PORTING_PLAN.md` §6 and `internal/patterns`). A user cannot
  currently match a filename containing a literal `(` with an `sh:` pattern. Fixing it
  changes which files a pattern selects, which is why it waits.
- **`stat.filemode` renders an unknown file type as `?`.** borg's C `_stat` and its
  pure-Python fallback disagree on that character; borge reproduces the C one because that
  is the one that runs. Cosmetic, but it is a difference between borge's output and its own
  documentation.

**Already fixed, recorded so they are not "corrected" back:**

- **borg's `RobustUnpacker` is quadratic.** It rescans its whole buffer on every `feed`,
  so resynchronising after damage costs O(n²) in the buffered bytes. borge keeps a scan
  offset and is careful at the buffer's end instead — a provisional rejection near the end
  is re-examined when more data arrives rather than skipped — and stays linear. See
  `internal/archive/robust.go`. This is the quadratic behaviour worth naming: it is in the
  *repair* path rather than the ordinary restore path, and borge does not have it.
- **borge's `debug search-repo-objs` clamps two negative slice indices** that Python reads
  as offsets from the end, so it does not blank the context before a hit or double-report
  one-byte terms. DIVERGENCES #13.

**Not bugs, but constraints worth revisiting:**

- **Restore is lossy in borg's own terms.** The stage 5 gate compares borge's extraction
  against *borg's*, not against the original tree, because borg's restore does not
  reproduce everything it stored: `--sparse` restores holes only at chunk granularity
  (DIVERGENCES #9). Once compatibility is lifted, "restore reproduces the source" becomes
  an achievable gate rather than an aspiration.

  **Corrected 2026-08-19.** This entry used to name `bsdflags` here too — "read and
  preserved but never applied" — which is borge's gap and not borg's. borg captures them
  with `FS_IOC_GETFLAGS` and applies them with `FS_IOC_SETFLAGS`, last of all attribute
  restoration (`archive.py:1112`). The same wrong sentence stood in DIVERGENCES #8 and in
  the stage 8 work list; this was the third copy, and all three are now fixed. It belongs
  in stage 8 as a fidelity gap, not here as a constraint of compatibility.
- **Item decoding is lossy for unknown keys** at the `Item` struct boundary, which is why
  `debug dump-archive` reads the raw msgpack instead. A format borge owns can make the
  round trip total.
- **Every item carries an empty `xattrs` dict and a zero `bsdflags`.** borg writes both on
  every item it examined, and the *presence* of each key is what says it looked: with
  `--noxattrs` or `--noflags` the key is absent instead. That is a real distinction —
  "checked, found none" against "not recorded" — and borge reproduces it as of stage 8
  (DIVERGENCES #8), because a borge archive that could not express it would be
  indistinguishable from one taken with the option.

  What a format borge owns could do is carry the distinction without paying per item: it
  costs roughly 9 to 18 bytes on every item, so a backup of a million files spends 10 to
  18 MB of item stream saying "nothing here". An archive-level "these attributes were
  examined" flag plus per-item values only where non-empty says the same thing in a few
  bytes. Recorded here rather than acted on because the question can only be asked from a
  faithful baseline: until borge records the fields, any measurement of the saving is
  measuring the bug.

#### R0.2 Large directories must not slow restore down

This is the requirement the project brief opens with, restated as a gate rather than an
aspiration: **restoring a directory of 118,866 files must not cost more per file than
restoring a directory of 100.**

borg reads a backup sequentially and recreates the tree as it goes, the way `tar -x` does.
Anything worse than linear in that path defeats the intent. What is known so far:

- The **directory-attribute stack** in `internal/archive/extract.go` is O(1) amortised: it
  pops a directory when the next path leaves it, rather than searching. It does allocate a
  string per item for the prefix comparison, which is 118,866 allocations on the
  pathological corpus and trivially removable.
- The **chunker-per-file construction** (`docs/PORTING_PLAN.md` §12.1) is a per-file
  millisecond cost on the *create* side, worth ~3.5 minutes on that corpus alone.
- **Restore-side ordering** is item 1 above and is the one with real headroom.

Stage 9 measures all three before anything here changes the format. The point of writing
them down together is that only one of them needs a format change, and it is not the
expensive one.

**Gate:** a migration path exists and is tested (borge reads the old format, converts,
verifies); the change is justified by benchmark JSON in the evidence bundle; and the
pathological-directory scenario shows per-file restore cost flat against directory size.

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

- Burn at least two CD-R copies from a verified ISO master, verify each whole-disc readback
  on two readers when practical, and store the copies in different locations. This is
  preservation work: it blocks no roadmap item, because the verified master on
  `/media/renes/SEAGATE2TB` is what R1 requires.
- Rebuild an evidence ISO after each release or when the cumulative catalog changes
  materially. Never overwrite an old master; each image and sidecar are immutable records.
- Re-verify stored ISO masters and physical discs periodically and migrate before readers
  or media become unreliable.
- Publish evidence ZIPs as release assets or in immutable object storage once hosting and
  retention are chosen; the Git manifest is an index, not a substitute for the artifacts.
- Revisit this roadmap at every release. Work moved into the porting plan must be removed
  here so two trackers cannot disagree silently.
