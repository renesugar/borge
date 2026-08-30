# borge roadmap

This file owns the work that is not a porting stage, and it outlives the porting plan.

The port closed on 2026-08-29 and [`plans/PORTING_PLAN.md`](plans/PORTING_PLAN.md) is
now an archive: it owns the record of the borg-to-Go port, its compatibility gates and
Stages 0-9, and a great deal of the source still cites it by section for *why the code is
this shape*. The current documents are this roadmap plus [`PLAN.md`](PLAN.md), the plan for
whatever roadmap item is being implemented now. `AGENTS.md` describes that workflow.

**On the numbering.** RP is the port; it has letters rather than a number because it
predates this file and its stages are numbered already. R0 is not the first priority; it is
the oldest item. It was Stage 10
of the porting plan and moved here on 2026-08-27, because it is work that begins after the
port is complete. The identifier is a name, not a rank.

**What is next, as of 2026-08-29.** RP is complete, R2 is complete, and R1 has one item
open — an independently backed-up digital copy of the ISO and its custody log, which blocks
nothing. **R0 is current**, planned in [`PLAN.md`](PLAN.md).

Stage 9 has now said what it justifies, and the answer changes R0's shape rather than
confirming it. R0 item 1 was explicitly a Stage 9 experiment first — large-directory
restore may be deliverable with no format change at all, and the format must not move until
the measurement shows it has to. Stage 9 ran part of that experiment and **found against the
theory the item is built on**: removing about 1.07 million syscalls from an extract returned
about three seconds, and the one large restore win needed no format change at all. The two
experiments that would settle it — read *order* and parallel writers — were not run, and
they are the first two tasks in `PLAN.md` precisely because either could remove the
argument for changing the format.

## Current priorities

### RP. The borg-to-Go port (Stages 0-9)

State: **complete (2026-08-29).** All nine stages done.
[`plans/PORTING_PLAN.md`](plans/PORTING_PLAN.md) is the plan it was executed from, now
archived; its §14 tracker is the authority on stage state and is not duplicated here.

**Evidence: `borge-stage-9-20260830T041853Z.zip`.** It is named here rather than in the
plan's §14 because it is built from the commit that archives that file, so the name cannot
exist before the archive is frozen — and rule 4 puts an item's outcome in its roadmap entry
anyway. Built from a clean tree at `3479a1d`: every package green including the
interoperability gate (1496 s) and `internal/cli` (3071 s), zero failures across 18,950 test
events, the `-race` pass green, both coverage gates reporting no unexplained absence, and
`go vet` silent. It carries the nine benchmark baselines the §12 gate demands — including
the closing run, `baseline-20260830T000800Z.json` — and the drafted
[golang/go#81029][go81029] comment. **Not yet catalogued or attested**: adding it to
`evidence/manifest.json` and signing it is R1 work and needs the offline subkey.

**The outcome.** borge implements 33 of borg's 36 commands — the other three, `mount`,
`umount` and `webdav`, are declared non-goals — reads and writes borg 2's repository format
in both directions under a gate that runs on every commit, and on the pathological corpus
of 118,866 files is **5.5x faster than borg on create and 4.2x on extract**, with one
regression: create peak RSS, 337 MiB against borg's 249, which tracks `BORGE_PACK_MAX_SIZE`
and falls to 244 MiB at 2 MB packs. **No cgo dependency was taken**, so the
`CGO_ENABLED=0` cross-compilation the desktop and mobile goal depends on is intact.

**Two things worth carrying forward.** Six of the seven performance wins were borge's own
bugs rather than anything about Go, and five of those six were the same bug in different
clothes: work meant to happen once per *process* happening once per *item* — a chunker, an
lz4 compressor, a zstd encoder, a user lookup, a pack file open. The remaining headroom is
therefore smaller than the ratio suggests; the seventh win, the create pipeline, is the only
one that is engineering rather than repair, and it is the one that helped least. And one ceiling is not borge's to fix: `crypto/cipher`'s single-block API caps
*any* Go OCB implementation near 154 MB/s, which makes AES-OCB 19x slower than borg's
OpenSSL and *inverts* the mode ranking — the mode that should win on AES-NI hardware loses
to ChaCha20-Poly1305 by 5.7x. It is filed as [golang/go#81029][go81029], and the real-corpus
number it asked for was measured and **posted on 2026-08-29** as
[`#issuecomment-5465158031`](https://github.com/golang/go/issues/81029#issuecomment-5465158031).

**Correcting the archive, which cannot correct itself.** `plans/PORTING_PLAN.md` §12.5 step
1 and its §14 tracker row both say the comment "awaits the author posting it". That was
already false when written: it was posted at 22:07 UTC, hours before those sentences were.
The archived plan is not edited, so the correction lives here — and in
`docs/upstream/go81029-comment.md`, which carries the posted text and the note on how the
error survived four separate statements without anyone querying the issue.

[go81029]: https://github.com/golang/go/issues/81029

**Listed here because every milestone belongs on this roadmap, and this one was missing.**
The roadmap was written after the port had started, so the port was never given an entry —
and the convention is that development moves to the next roadmap item and `PLAN.md` is
written for it. With the port absent and R1 and R2 completed *during* it, the sequence
stopped being legible: after R2 closed there was no `PLAN.md`, which reads as a mistake
rather than as "the port is current again". It is not a mistake, and now it is written
down. Stage 9 closed on 2026-08-29, the porting plan was archived to `plans/`, and
`PLAN.md` became the plan for R0.

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

State: **complete (2026-08-28).**
[`plans/r2-documentation-system-20260828.md`](plans/r2-documentation-system-20260828.md)
is the plan it was executed from and carries the design forward; `plans/PORTING_PLAN.md` §2.1 remains the record of where it came from
and what stage 8 found. Execution is tracked here because it is documentation
infrastructure, not part of the borg port.

- [x] Build `docaudit`: parse `//borge:*` anchors, report verification grades per topic,
  and fail on dangling help anchors or claims without registered checks (2026-08-27).
  `internal/docs` parses, `cmd/docaudit` reports, `make docaudit` runs it and
  `TestDocAuditIsClean` gates it; every rule has a case that damages a clean fixture and
  requires that rule, and `TestEveryRuleHasADamageCase` reads the audit for rules no case
  produces — a count in prose could not check itself, and had gone stale by 2026-08-28. It also reports when a topic is anchored in one piece,
  so the grade breakdown cannot read as reassurance it has not earned.
- [x] Generate enumerations already checked ad hoc: environment variables, pattern styles,
  compression specs, and placeholders (2026-08-27). The topics render `{{enum:...}}`
  markers from `patterns.Styles`, `compress.SpecDocs`, `placeholders.All` and
  `cli.envVars`; each table is checked against the behaviour beside it rather than against
  a list inside a test. The match-archives selectors are the remaining hand-written list.
- [x] Build `docgen --help`, topic templates, and `TestDocsAreCurrent`; migrate the five
  hand-written help topics (2026-08-27). Every paragraph now lives in the file that
  implements it, on a carrier declaration beside the code — and, since 2026-08-28, naming
  that code with `//borge:about`, because "beside" turned out not to be a link anything
  could follow; `make docgen` assembles them and the freshness test diffs. The audit's
  grade breakdown went from a flattering 0% unverified over five lumps to 39% over
  thirty-one fragments, which is the number the exercise existed to produce.
- [x] Decide whether `docgen --api` adds enough over `go doc ./internal/...` to justify an
  `docs/INTERNALS.md`. **Decided 2026-08-27: no.** borge has no exported API — 21 packages
  under `internal/`, three `package main`s outside it — and `go doc` already renders all
  ~794 exported declarations from the same comments. `//borge:doc api` is now an error
  naming this decision, so nobody can mark a comment for a subset nothing renders.
- [x] Build the calibrated, advisory contradiction checker over user-facing anchored prose
  (2026-08-28). `internal/doccheck` reads a declaration and its direct callees with the doc
  comment withheld, then asks a local model whether that reading contradicts the prose;
  `make doccheck` runs it, `make doccalibrate` scores it, and it is deliberately absent
  from `make check`. **The checker is built and calibrated; the 1.5B model available here
  fails the calibration** — 4 of 13 against a 5 of 13 constant-answer baseline — so its
  verdicts are recorded as noise rather than acted on. Building the labelled set first
  found three documentation defects: two of the five calibration cases §2.1.1 named never
  happened; a comment inside `newFlagSet` had been false for nine days after the divergence
  entry beside it was corrected; and the checker's own first run over the tree found no
  blocks at all, because every user fragment sits on a `var _ = helpText` carrier. That
  third one is closed by `//borge:about`, which names the function a fragment describes,
  with an audit error for a name nothing answers to and a warning for a fragment with
  neither.
- [x] Execute every help example and assert its effect (`TestHelpExamplesRun`, 2026-08-18).
- [x] Build `docactionable`: generate a command from each topic and run it against the
  existing scratch-repository harness; keep it advisory (2026-08-28).
  `internal/cli/docactionable_test.go` gives the model one topic and a task, runs the
  command that comes back against `newHelpFixture`, and checks what it did.
  **Its calibration passes** — 3 of 4 against a 2 of 4 baseline — unlike doccheck's:
  producing a command line from a manual page is a far easier task than judging entailment.
  Building the set found that two of the three known-answer cases §2.1.2 named had stopped
  discriminating, because `permute` fixed the flag-order defect that broke those commands;
  `TestActionableCasesStillDiscriminate` now catches that decay without a model.

Acceptance: no dangling anchors or orphan claims; every help topic has at least one
executed example and a recorded verification-grade breakdown; generated documentation is
fresh; advisory checks are calibrated against the known before/after cases in the plan.

Met, with one qualification worth stating rather than burying. Both advisory checks are
calibrated against labelled cases taken from git; `docactionable` **passes** its
calibration and `doccheck` **fails** its own, on the 1.5B model this hardware can hold.
Acceptance asked that they be calibrated, not that they succeed, and a checker that
measures itself and reports that its verdicts are noise is doing the job the calibration
requirement exists for. What R2 delivers either way is the anchors, the generated help,
the grade breakdown, the two labelled sets, and the audit rules that keep them honest.

~~After Stage 9 and R0~~ **After Stage 9, and before R0 rather than after it**, a GitHub
project is created and the project is pushed to `origin`. **Done 2026-08-30:**
[renesugar/borge](https://github.com/renesugar/borge), public, `main` and `develop` and
`v0.8.0`, with `develop` merged to `main` by pull request #1 and both branches at
`a33e5a5`.

**The order changed, and the reason it was safe is the reason the order existed.** This
paragraph said to push after R0. The stated ground was that "R0 changes the on-disk format,
which is not a thing to do first in public" — an argument about what the *first public act*
should be, not about waiting for R0 as such. Pushing the borg-compatible port first
satisfies it more directly than waiting would have: what is public is the version whose
correctness the interoperability gate can still check. R0 will land in public as a version
bump with a migration, which is what it should look like.

After that first push, `main` is protected: work lands on `develop` and is merged into
`main` by pull request, which is the branch model `plans/PORTING_PLAN.md` §2 settled on.
**Protected 2026-08-30**, with force-pushes and deletion blocked and a pull request
required. Two settings are deliberately left open on a solo project and should not be
"fixed" by a later reader without deciding to: `enforce_admins` is off, so the owner can
still push directly to `main` in an emergency, and `required_approving_review_count` is 0,
because a lone maintainer cannot approve their own pull request. Required status checks are
absent for a different reason — there is nothing to require yet, which is R4.

## After the port closes

### R0. Format and indexing changes

State: **current, planned in [`PLAN.md`](PLAN.md) (2026-08-29); no task started.** Moved
here from `plans/PORTING_PLAN.md` §13 (Stage 10) on 2026-08-27, and reached on 2026-08-29
when Stage 9 closed. Everything here **breaks format compatibility**, so it goes behind an
explicit repository version bump and a documented migration.

**Stage 9 weakened item 1's premise rather than confirming it**, and `PLAN.md` is ordered
around that: the first two tasks are the read-order and parallel-writer experiments that
were never run, both of which need no format change and either of which could close R0.2's
gate without one. As it stands, **no measurement in this project yet requires the format to
move.** The strongest case for a change is the last entry under R0.1's *"not bugs, but
constraints worth revisiting"* — the 9 to 18 bytes every item spends saying "checked, found
none", 10 to 18 MB on a million files — and that case is arithmetic, not a benchmark.

1. **Large-directory packing.** *(Stage 9 note, 2026-08-29: part of the restore-side cost
   this item is about was simply that `PosixFS.Load` reopened the pack on every object read
   - 118,866 opens for a handful of packs. Keeping the handle open bought 1.16x on extract
   with no format change, and `plans/PORTING_PLAN.md` §12.1e has the numbers. What remains
   unmeasured was whether read *order* matters once the file stays open. **Measured
   2026-08-30 by R0 T1, and it does not: the sort-by-`(pack_id, obj_offset)` proposal below
   is withdrawn.** Extraction in item order already opens each pack exactly once, in every
   configuration tested from 4 packs to 351 and from one generation of history to eighteen,
   because `create` lays chunks down in the order extraction asks for them and dedup
   interleaves monotonic streams rather than randomising them - backward seeks were zero
   everywhere. An archive that switches packs on *every one* of its 118,866 reads extracts
   in the same time as one that never switches: +0.30 s on 41 s, sd 1.54, paired and
   interleaved. `PLAN.md` has the tables and the limits. And a caution added 2026-08-29 from §12.1f: removing about 1.07 million
   syscalls from an extract - three `openat` per file down to one - returned only about
   three seconds, so per-file syscall overhead is *not* what dominates a large-directory
   restore. The deferred-metadata and batching ideas below should be expected to buy less
   than this item assumes.)* borg 2's `PackWriter` already packs *chunks*. The
   remaining problem is the restore side: extracting 118,866 files from one directory
   means 118,866 `create`+`write`+`close`+`utimes`+`chown` sequences, and on a slow or
   high-latency filesystem that, not I/O bandwidth, is the wall. Investigate:
   ~~restore-side batching by pack (sort extraction order by `(pack_id, obj_offset)` so
   each pack is read once and sequentially)~~ **- withdrawn 2026-08-30, it already is** -
   deferred metadata application (write all content, then apply modes/times/xattrs in a
   second pass), and parallel writers per directory. Of the three mechanisms this item
   proposed, two are measured and are not where the time goes - read order (T1) and per-file
   syscall count (§12.1f) - and the third **is** where it goes: **R0 T2 parallelised the
   writers on 2026-08-30 for about 2.1x, with no format change.** "Per directory" was the
   wrong frame, though: the corpus this requirement is about is a single flat directory, so
   per-directory parallelism would have given one writer. The parallelism is *within* a
   directory, and its ceiling is ext4 serialising creates on the parent inode lock - the
   same device reaches 4.3x across separate directories and about 1.75x inside one. **Note this is measurable and possibly deliverable without any format
   change at all** — try it in Stage 9 first, and only change the format if Stage 9
   proves it is not enough.
2. ~~**`blugelabs/bluge` for indexing.**~~ **Evaluated and declined 2026-08-30 (R0 T7).**
   The predicted outcome was "bluge for archive/file search, borghash retained for the chunk
   index". borghash is retained, but bluge lost the search half too, on measurement: below
   about 200,000 paths a plain linear scan is *faster* than bluge, and at a million it wins
   by roughly 2x for an index 3.9x the size of the raw path text, 87 s to build, and 67
   transitive modules against borge's 17.

   **The useful finding is what `find` actually spends its time on.** It costs 7,069 ms over
   200,020 items; a regex scan of those same paths already in memory costs 85 ms. The time
   is item-stream decoding — decompress, verify the chunk id, msgpack-decode — not matching.
   And paths repeat once per archive, so a repository of 20 archives holds 200,020 items
   over 10,001 distinct paths.

   **So the open opportunity is a path cache**, not an inverted index: distinct paths
   stored beside the repository, no format change, worth roughly 80x on this workload. It is
   now **`PLAN.md` T10**, added 2026-08-30, with the shape left to measurement - per-archive
   lists win the decode cost, a global path table with per-archive membership also wins the
   storage - and with the correctness property named as the real risk: a stale or missing
   path cache must change how long `find` takes and never what it returns. Full-text search over file *contents* remains the one thing bluge
   would genuinely add, and it costs a full decompression pass per backup to index.
3. ~~**zstd as the default compression** (borg #10085) once the benchmark supports it —
   the reference numbers give zstd `SpeedFastest` a better ratio *and* comparable
   speed versus lz4-class options.~~ **Done 2026-08-30 (R0 T6): the default is `zstd,1`.**
   The reference claim was half right and the half that failed is worth keeping: the ratio
   is real - a quarter smaller repository on text - but the speed is not comparable, at 28%
   more wall time on create. Not a format change; the codec is recorded per chunk, borg
   reads zstd, and the interoperability gate passes both ways. `DIVERGENCES.md` #62 has the
   decision and the numbers.
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
  reproduces it (see `plans/PORTING_PLAN.md` §6 and `internal/patterns`). A user cannot
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
- The **chunker-per-file construction** (`plans/PORTING_PLAN.md` §12.1) is a per-file
  millisecond cost on the *create* side, worth ~3.5 minutes on that corpus alone.
- **Parallel writers**, added to this list 2026-08-30 because T2 found the headroom the
  other two did not have: about 2.1x, and the only one of the three mechanisms that paid.
- **The requirement itself is met**, measured 2026-08-30 (R0 T4) rather than inferred from
  the three mechanisms above. All three are now answered and none of them needed the format
  to move, which is the outcome this item said to prefer.
- ~~**Restore-side ordering** is item 1 above and is the one with real headroom.~~
  **Measured 2026-08-30 (R0 T1): no headroom.** It was the one of the three with a
  plausible story and no measurement, which is why it was worth running first. If a backup
  history ever keeps more packs live at once than the 16 descriptors borge caches, the fix
  is that constant rather than a reordering.

Stage 9 measures all three before anything here changes the format. The point of writing
them down together is that only one of them needs a format change, and it is not the
expensive one.

**Gate:** a migration path exists and is tested (borge reads the old format, converts,
verifies); the change is justified by benchmark JSON in the evidence bundle; and
~~the pathological-directory scenario shows per-file restore cost flat against directory
size~~ — **that last clause is met as of 2026-08-30 (R0 T4), and was met without any format
change.** Per-file restore cost is flat across a 2,400x range of directory sizes: from
10,000 files to 240,000, per-file cost rises 6.5%, where O(n log n) would predict about 34%.
Most of even that drift is ext4's own directory index rather than borge — the same
file-creation loop without borge drifts about 7%, and non-monotonically. The remaining two
clauses still bind whatever format change R0 eventually makes, if it makes one.

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

### R4. Continuous integration, and status checks worth requiring

State: **not started.** Added 2026-08-30, when `main` was protected and the branch
protection rule had no required status checks to list — because the project has no CI at
all. Nothing on GitHub currently blocks a pull request that breaks the suite.

**The obstacle is not writing a workflow file.** It is that this repository's suite, as it
stands, cannot run on a hosted runner, and the reasons are worth stating before anyone
tries:

- **It needs borg.** Every differential test compares borge against a *pinned build of borg
  2* — a Cython extension built from a specific commit in a virtualenv (`tests/borg2/`).
  That is buildable in CI, but it is a build step measured in minutes, and
  `plans/PORTING_PLAN.md` §0.1 pins the commit precisely so it cannot drift underneath the
  gate. A CI that silently built a different borg would be worse than no CI: it would turn
  the strongest check in the project into a source of false failures.
- **It needs corpora that cannot be published.** The interoperability matrix runs against
  real directories on the author's machine — a Joplin archive, an Obsidian vault, a Google
  Drive folder, a 118,866-file recipe corpus. They are personal data and are not going to a
  public runner. What CI can have is the synthetic and pathological corpora the suite
  builds for itself; what it cannot have is the evidence that borge handles *real* trees,
  which is the evidence that has actually caught bugs here.
- **It takes about 75 minutes**, dominated by `internal/cli` (3071 s) and `tests/interop`
  (1496 s) in the stage-9 bundle. A required check that takes over an hour on a solo
  project will be waited on, worked around, or disabled.
- **Some of it must never run in CI.** `tests/bench` measures wall time and peak RSS and is
  meaningless on a shared runner — §12.1g put this machine's floor at about 50 MB of RSS
  and 0.5 s of wall, and a noisy neighbour swamps that. The two advisory documentation
  checkers need a local model on a GPU. Both already decline to run without an explicit
  environment variable, which is the right shape; CI simply must not set it.

**So the work is choosing what to require, not automating everything.** The likely answer
is a fast tier that gates merges — `gofmt`, `go vet`, `check-spdx`, `check-layering`,
`docaudit`, and the packages that need neither borg nor private corpora — plus a slower
tier that runs the borg-dependent suite on a schedule or on demand and reports rather than
blocks. The gate should be the part that is fast, deterministic, and reproducible off this
machine; everything else stays an evidence bundle, which is what evidence bundles are for.

- [ ] Split the suite into a tier that can run on a hosted runner and a tier that cannot,
      and make the split explicit in the code rather than in a CI config that drifts from
      it.
- [ ] Build and cache the pinned borg 2 interpreter, verifying the commit it actually
      built rather than the one it recorded — the failure `mkbundle.sh` already guards
      against, and which materialised once on 2026-08-17.
- [ ] Add the workflow, then add its checks to `main`'s protection rule as required.
- [ ] **Authenticate as the workflow, not as a person.** Noted 2026-08-30, from a failure
      rather than from principle: the `gh` token authenticating pushes went invalid partway
      through a working session, and git fell back to a desktop askpass dialog that no
      unattended job could ever answer. A scheduled or triggered workflow authenticating as
      the maintainer would hit exactly that, and would hit it silently - as a job that hangs
      or fails on credentials, days after the token was fine. Use Actions' own
      `GITHUB_TOKEN`, which is minted per run and scoped to the repository, or a
      repo-scoped token if something genuinely needs more; never a personal token carried
      from a developer's machine.
- [ ] Keep `tests/bench` and the model-backed documentation checkers out, and add a test
      that fails if CI ever sets the variables that would turn them on.

Acceptance: a pull request that breaks the fast tier cannot be merged; the slow tier runs
somewhere and its failures are visible; no benchmark number in the repository was ever
produced by a hosted runner.

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
- Re-run `make doccalibrate` whenever the model or the hardware changes. `doccheck` is
  built and its thirteen labelled cases are checked into the tree; the 1.5B model a GTX
  1650 can hold scores below the constant-answer baseline, so the tool is dormant rather
  than useful. A larger model may cross it, and the score is the only thing that decides.
  If cases are added, add them *before* touching the prompts — seven designs were already
  tried against these thirteen, and that is as much selection pressure as they will bear.
- **Get the OCB implementation independently reviewed.** Carried out of the porting plan's
  risk register on 2026-08-29, where it had sat as "worthwhile before Stage 7" and did not
  happen. The risk was downgraded on evidence, not on argument — all 16 primary and all 9
  appendix A RFC 7253 vectors pass, and envelopes are byte-identical to OpenSSL's across
  every suite and size tested — so this is a double-check of a component that already
  agrees with a reference implementation, not an open defect. It is here rather than in the
  archive because an archive is not a place to look up what to do next.
- Revisit this roadmap at every release. Work moved into the porting plan must be removed
  here so two trackers cannot disagree silently.
