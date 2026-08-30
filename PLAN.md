# PLAN.md — R0: format and indexing changes

Current plan for [`ROADMAP.md`](ROADMAP.md) **R0**. The item's own text lives there and is
not duplicated here; this file is the execution plan — what R0 is broken into, in what
order, and the gate that decides it is done. It was written on 2026-08-29, the day the
port closed and the porting plan was archived to
[`plans/PORTING_PLAN.md`](plans/PORTING_PLAN.md), which every section number below cites.

R0 is the first work in this project that **deliberately breaks compatibility with borg**.
Everything before it was a port, guarded by a gate that failed if borge's output differed
from borg's. That gate is what made the port checkable, and R0 removes the thing it
checks. So the first question R0 has to answer is not "what should the format be" but
"which of these wins actually needs the format to move at all".

## What Stage 9 changed about this item, and why it matters here

R0 arrived from the porting plan's §13 with a theory: extracting 118,866 files from
one directory is dominated by per-file overhead — the `create`/`write`/`close`/`utimes`/
`chown` sequence — so batching, deferring and reordering it is where the time is. Stage 9
measured that theory and **it is mostly wrong**:

- **Removing about 1.07 million syscalls returned about three seconds** (§12.1f). Extract
  went from three `openat` per restored file to one, and eight `fcntl` to four. The
  remaining 22 s of system time is the kernel genuinely creating, writing and stamping
  118,866 files. Per-file syscall *count* is not the wall, so deferred metadata and
  batching should be expected to buy less than R0 item 1 assumes.
- **The one large restore win needed no format change at all** (§12.1e). `PosixFS.Load`
  reopened the pack file on every object read. Keeping the handle open bought 1.16x on
  extract, and it is a cache in the storage layer, not a change to what is stored.
- **Two of the three costs R0.2 lists are already gone**, and neither was a format
  problem: the chunker-per-file construction (§12.1a) and the pack reopen (§12.1e).

What Stage 9 did **not** do is the two experiments that would actually settle item 1:
extraction **read order**, and **parallel writers**. Both are no-format-change experiments,
and R0 item 1's own instruction is that they come first — "measurable and possibly
deliverable without any format change at all — try it in Stage 9 first, and only change
the format if Stage 9 proves it is not enough." Stage 9 ran out before it proved anything
either way. So they are T1 and T2 here, and they gate T4.

**The honest position: R0 currently has no measurement that requires a format change.** It
has one item whose case is strong for a different reason (T5, the per-item cost of empty
`xattrs`/`bsdflags`, which is arithmetic rather than a benchmark), one that is a default
change rather than a format change (T6, zstd), and one that is a new capability (T7,
search). Writing that down now is the point: it is much easier to justify breaking a format
before the measurements than after them.

## What replaces the interop gate

For seven stages, `tests/interop` answered "is borge still correct?" by asking "does borg
agree?". Once borge writes a format borg cannot read, that answer is unavailable for new
archives, and losing it silently is the largest risk in R0 — larger than any individual
format decision.

It is replaced by three things, and **T3 builds them before any format change lands**:

1. **The old format stays fully supported and fully gated.** borge continues to read and
   write repository version 4, and `tests/interop` keeps running against it unchanged. A
   format bump adds a version; it does not retire one.
2. **Round-trip becomes the gate for the new format**: write, read back, and compare
   against the *source tree* rather than against borg's restore. Stage 5 could not do this
   because borg's own restore is lossy in its own terms (`--sparse` at chunk granularity,
   DIVERGENCES #9) and the gate had to compare like with like. A format borge owns makes
   "restore reproduces the source" an achievable gate, and R0.1 already says so.
3. **Migration is tested in both directions it claims to work**: borge reads a version-4
   repository, converts it, verifies the conversion, and the converted archives extract
   byte-identically to what the original extracted.

## Tasks

Sized so each is committable on its own, and ordered so the measurements that could
*cancel* later tasks come first. The tree is never left broken across a stop.

- [ ] **T1 — Extraction read order, measured.** Sort extraction by `(pack_id, obj_offset)`
  and measure against the current path on the pathological corpus, now that the pack stays
  open (§12.1e) and the handle cache bounds descriptors at 16. This is the experiment R0.1
  item 1 asked for and Stage 9 did not run. It needs no format change: the order is chosen
  at restore time from the chunk list already in the item. **Report the result whichever
  way it goes** — a null result here removes the main argument for item 1 and is worth as
  much as a win.
- [ ] **T2 — Parallel writers per directory, measured.** §12.1f says the remaining extract
  cost is the kernel doing real work; the untested question is whether that work
  parallelises. Reuse the create pipeline's shape (`internal/archive/pipeline.go`) and its
  lesson: adaptive, with a measured threshold, defaulting to the serial path when the pool
  costs more than it saves, and a knob rather than a `NumCPU` rule — the create default is
  two workers on this i5-9300H, chosen because more bought no measurable time and cost
  measurable memory (§12.1h).
- [ ] **T3 — The version bump, the migration, and the replacement gates.** Repository
  version 5 behind an explicit opt-in, `borge transfer`-based migration from version 4, and
  the three gates above wired into the suite. **Nothing that changes on-disk bytes may land
  before this.** A format change with no migration is not a feature, and a format change
  with no gate is not checkable.
- [ ] **T4 — Restore-side batching and deferred metadata, if T1 and T2 justify it.**
  Explicitly conditional. If T1 and T2 land the requirement without touching the format,
  this task is closed as unnecessary and R0.2's gate is met without a format change — which
  is the outcome the roadmap says to prefer.
- [ ] **T5 — Stop paying per item for "checked, found none".** Every item carries an empty
  `xattrs` dict and a zero `bsdflags`, because in borg the *presence* of the key is what
  says the attribute was examined (DIVERGENCES #8). That distinction is real and must
  survive, but it costs roughly 9 to 18 bytes on every item — 10 to 18 MB of item stream on
  a million files. An archive-level "these attributes were examined" flag plus per-item
  values only where non-empty says the same thing in a few bytes. Measure the saving on the
  pathological corpus first: borge only started recording the fields faithfully in stage 8,
  so any earlier measurement was measuring the bug.
- [ ] **T6 — zstd as the default compression** (borg #10085). Not a format change — both
  codecs are already in the format and borge reads either — so it is a default change with
  a compatibility cost only for readers older than borg 2. The encoders were pooled in
  Stage 9 (3.0x, 139.8 to 420.1 MB/s on a 2 MiB buffer), which removes the objection that
  the measurement would be reading a bug rather than the codec. Re-measure ratio *and*
  speed against lz4 on the real corpora before switching.
- [ ] **T7 — `bluge` for archive and file search.** A new capability, evaluated on its
  merits: `borge find` is a linear scan today. Be honest about the fit — bluge is an
  inverted-index search engine and the chunk index is a 256-bit-key hash table with a
  48-byte value. The likely outcome is bluge for search and borghash retained for the
  chunk index, and the way to be wrong about that is to decide it before measuring.
- [ ] **T8 — Lossless item round-trip.** Unknown msgpack keys are dropped at the `Item`
  struct boundary, which is why `debug dump-archive` reads raw msgpack instead. A format
  borge owns can make the round trip total.
- [ ] **T9 — The R0.1 quirk list.** `shellpattern.translate`'s vacuous guard (a literal
  `(` cannot currently be matched by an `sh:` pattern) and `stat.filemode`'s `?` for an
  unknown file type. Each fix retires a DIVERGENCES entry, and each changes observable
  behaviour, so each lands with the entry rewritten rather than deleted — the record of why
  borge reproduced a bug for a year is worth keeping.

T1, T2, T5, T6 and T7 are measurements before they are changes. Every one of them can come
back "no", and a task list that cannot record a "no" is a list of intentions.

## Gate

R0 is done when all of the following hold, from `ROADMAP.md` R0.2 plus what Stage 9 added:

1. **The pathological-directory scenario shows per-file restore cost flat against
   directory size** — the requirement the project brief opens with, restated as a
   measurement rather than an aspiration.
2. **A migration path exists and is tested**: borge reads the old format, converts,
   verifies, and the converted archive extracts identically to what the original did.
3. **The old format is still gated.** `tests/interop` passes against repository version 4
   in both directions, unchanged. If it does not, the format was not extended — it was
   replaced, which is a different decision needing a different argument.
4. **Every format change is justified by benchmark JSON in the evidence bundle**, on the
   same corpora Stage 9 used, with the tool order and cache state recorded — the harness
   already does this, and the reason it does is that an unpaired before-and-after on this
   machine cannot see an effect smaller than about 50 MB of RSS or 0.5 s of wall time
   (§12.1g).
5. **Anything that came back "no" is written down as a no**, in this file, with the number
   that decided it.
