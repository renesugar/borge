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

- [x] **T1 — Extraction read order, measured. Done 2026-08-30, and the answer is no.**
  Sorting by `(pack_id, obj_offset)` is **not worth implementing**, and the reason
  generalises rather than being a property of this corpus. Full findings at the end of this
  file. Nothing was built beyond the diagnostic that answered it,
  `tests/bench/readorder_test.go`, which is kept so the conclusion can be re-checked on
  other repositories rather than believed.
- [x] **T2 — Parallel writers, measured and taken. Done 2026-08-30: about 2.1x.**
  `internal/archive/extract_pool.go`, default 3 workers, `BORGE_EXTRACT_WORKERS` the knob.
  Findings at the end of this file. The phrase "per directory" turned out to be the wrong
  frame and is the first thing the measurement corrected. Original text:
- ~~**T2 — Parallel writers per directory, measured.**~~ §12.1f says the remaining extract
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

---

## What T1 found, 2026-08-30

**The claim under test**, from `ROADMAP.md` R0 item 1: extracting a large directory is
dominated by restore-side costs that sorting extraction by `(pack_id, obj_offset)` would
relieve, "so each pack is read once and sequentially".

**It is already read once and sequentially.** Extraction in item order opens each pack
exactly the minimum number of times — once — in every configuration measured. Sorting
changes pack *switches* enormously and pack *opens* not at all, and only opens cost
anything.

### The measurement

`tests/bench/readorder_test.go` scores a real archive's extraction sequence without
extracting it: the chunk index already knows where every chunk lives, so the sequence of
`(pack, offset)` reads can be evaluated directly, against a modelled LRU.

| archive | packs in repo | distinct packs read | pack switches | opens at LRU 16 | smallest cache that still opens each pack once |
|---|---:|---:|---:|---:|---:|
| fresh, 50 MB packs | 7 | 4 | 3 *(= floor)* | 4 *(= floor)* | **1** |
| fresh, 2 MB packs | 111 | 94 | 93 *(= floor)* | 94 *(= floor)* | **1** |
| 2 generations | 111 | 107 | 23,451 | 107 *(= floor)* | **3** |
| 3 generations | 171 | 118 | 35,729 | 118 *(= floor)* | **4** |
| 6 generations | 171 | 154 | 71,355 | 154 *(= floor)* | **7** |
| 18 generations | 351 | 120 | **118,865** | 120 *(= floor)* | **10** |

The last row is the extreme: **every single read lands in a different pack from the one
before it**, 118,865 switches across 118,866 reads, and it still costs exactly the floor
number of opens.

### Why it generalises: the reads are monotonic

**Backward seeks were zero in every configuration**, which is the finding underneath all the
others. `create` writes an archive's chunks in item order, so a fresh archive's chunks are
laid down in the order extraction will ask for them. Dedup against earlier archives does not
randomise that — it *interleaves* several such sequences, each still monotonic. Interleaving
k monotonic streams needs exactly k open handles and no seeking backwards.

So the working set is **the number of generations an archive still draws chunks from**, not
the size of the repository: 94 packs and 351 packs both need a cache of 1 when fresh. borge
caches 16 descriptors (`maxOpenHandles`, added by §12.1e), which is several times what any
history measured here asks for.

### The wall clock agrees

A model of a read sequence is not a measurement of a restore, so the prediction was tested
end to end: extract `gen1` (perfectly sequential) and `gen18` (switches packs on every
read) from the same repository, paired and interleaved, with the whole repository warmed
first so neither archive pays to fault it in.

| | gen1 | gen18 |
|---|---:|---:|
| mean of 4 | 41.36 s | 41.67 s |
| sd | 0.56 | 1.61 |

Paired difference **+0.30 s, sd 1.54, positive in 2 of 4** — +0.7% on a 41-second
extraction, with the sign split evenly. That is inside this machine's noise, which §12.1g
put at about 0.5 s of wall. **118,865 pack switches are not detectable in the wall clock.**

### The diagnostic is shown to be able to fail

Every number above says "optimal", and a metric that cannot say anything else is not
measuring anything. Scored against a cache of 2 — below the working set — the same `gen18`
sequence misses 118,866 times against a floor of 120, **990x the floor**, and the test fails
if it does not. `gen1` stays at the floor even at a cache of 2, correctly, because it never
needs a second pack open.

### What this does to R0 item 1

The restore-side sort is **withdrawn as an optimisation**. Combined with §12.1f — removing
1.07 million syscalls returned about three seconds — two of the three mechanisms R0 item 1
proposed have now been measured and found not to be where the time goes.

**And if a deep enough history ever did exceed the cache, reordering is still the wrong
fix.** The working set is what overflows, so the lever is `maxOpenHandles`: a one-line
constant, bounded cost, no format change, no change to the order files are written.
Reordering extraction means buffering chunks across file boundaries until the file that
wants them is being written — real memory, real complexity — to buy what raising a constant
would buy. That is worth writing down because the roadmap proposed the expensive fix and
never compared it against the cheap one.

### Limits, stated rather than implied

- **Local filesystem only.** Every number here is `posixfs` on an SSD, where a pack switch
  between open descriptors costs a cache lookup. R0 item 1's original wording worries about
  "a slow or high-latency filesystem", and on `sftp` or `s3` a switch may cost a round trip
  rather than a lookup. **That case is untested and is the one place this result does not
  reach.** It is also the case where the working set, not the switch count, would still be
  the thing to fix.
- **The crossover was not reached.** The construction modifies every tenth file, so after
  ten generations the eleventh-oldest is fully overwritten and the working set saturates at
  10 — below the 16 descriptors borge caches. A history that keeps more than sixteen
  generations live simultaneously would cross it, and what happens there is predicted by
  the control (badly) but not measured.
- One machine, one corpus, warm cache, `none-sha256`.

---

## What T2 found, 2026-08-30

**Result: extraction is about 2.1x faster, and the pool is on by default with three
workers.** `internal/archive/extract_pool.go`; `BORGE_EXTRACT_WORKERS` overrides, and 1
selects the serial path that existed before.

### "Per directory" was the wrong frame

R0 item 1 proposed "parallel writers per directory". The corpus the requirement is about -
§12.1b's pathological directory - is **one flat directory of 118,866 files**, so
per-directory parallelism would give exactly one writer on the case it was written for. The
parallelism has to be *within* a directory, which is where the filesystem has an opinion.

### Where the time actually goes

A CPU profile of a serial extract, before anything was built:

| | | |
|---|---:|---|
| syscalls | 22.85 s | 55% |
| SHA-256 (chunk id verification) | 5.63 s | 13.6% |
| everything else | ~13 s | allocation, msgpack, path cleaning, lz4 at 0.47 s |

So a little under half is user work that parallelises freely, and the rest is the kernel
creating, writing and stamping files in a single directory.

### The kernel half, probed with a control

Extract's own syscall sequence, replayed over 118,866 files with varying goroutines. Two
arms, because a scaling number with nothing to compare it against does not say what limits
it:

| workers | one shared directory | one directory each |
|---:|---:|---:|
| 1 | 22.33 s | 22.50 s |
| 2 | 12.80 s (1.74x) | 11.69 s (1.92x) |
| 4 | 13.09 s (1.71x) | 6.56 s (3.43x) |
| 8 | 14.78 s (**worse**) | 5.27 s (4.27x) |

The device and the kernel will go 4.3x faster given separate directories. One directory
stops at about 1.75x and then *degrades*, which is ext4 serialising creates on the parent's
inode lock. The single-worker figure, 22.33 s, matches the profile's 22.85 s of syscall
time, so the probe is measuring the right thing.

### The real thing, on two shapes

Same 118,866 files, twice: the flat pathological directory, and a tree of 4,953 directories
that drains the pool at every boundary.

| workers | flat, 1 dir | deep, 4,953 dirs |
|---:|---:|---:|
| 1 | 39.21 s | 34.81 s |
| 2 | 18.91 s (2.07x) | 19.33 s (1.80x) |
| 3 | 18.23 s (2.15x) | **16.60 s (2.10x)** |
| 4 | 18.23 s (2.15x) | 17.23 s (2.02x) |

**2.07x beats the 1.75x the syscall probe predicted**, and the gap is the point: the probe
measured only the kernel half. The ~19 s of user work parallelises freely and overlaps the
half that cannot, so the whole is faster than its slowest part.

**The deep tree does not regress**, which was the risk worth measuring before changing a
default for everyone. The barriers cost real time - 1.80x against the flat corpus's 2.07x
at two workers - and a third worker more than recovers it.

**Four workers is measurably worse than three on the deep tree**: 17.23 s against 16.60,
with sds of 0.06 and 0.07. Not noise. Past three, workers contend for the directory lock the
probe found.

### The default moved, and why that is the §12.1h lesson again

It was 2 until the deep tree was measured. On the flat corpus 2 -> 3 buys 3.6% of wall for
15% more CPU, which is not worth it; on the deep tree the same step buys 14% for 5%, which
plainly is. **Real trees have directories.** Choosing a default from the one corpus that
happened to be in hand is the mistake §12.1h records - sampling is not measuring - and the
corpus that would have contradicted it was the ordinary-looking one, not an exotic case.

The constant is deliberately not tied to create's two. Different mechanism, different
bottleneck; unifying them would couple two numbers that agreed by accident, and no longer
do.

### Correctness: what is shared, and what is proved

The pool runs **only `writeFile`**. Everything ordered across items - the pending-directory
stack, the hardlink map, the safe-parent cache - stays on the goroutine driving the item
stream, and the pool is drained before any of it is read. That is the answer to the question
`extractor`'s own comment posed in stage 9, that parallelising extract "has to deal with all
three together": it does not touch them. Only the owner caches and the stats needed locking.

Two barriers, and **both were mutation-tested rather than argued**:

- **Directory attributes.** Removing the drain in `finishDirs` makes directory mtimes come
  out as the time of the extraction (1788071633) instead of the time in the archive
  (981173106). Every file written into a directory restamps it, so a directory stamped
  while its files are still in flight is silently wrong and nothing else in the tree looks
  off.
- **Hard links.** Removing the drain in `tryHardlink` fails with `no such file or
  directory`, because the link is attempted while its target is still queued.

The hardlink barrier **passed the mutation at first**, and that is the finding rather than a
footnote: the fixture linked to a file written 200 items earlier, which had always finished.
It only became a real test once the target was made large and the two names adjacent in the
item stream. A barrier that survives its own mutation test is a barrier nobody has checked.

Strengthening that fixture also exposed that the test had been passing on **timing luck**:
it compared the whole destination, including the parent directories `makeParent` synthesises
for the archived path, which are not in the archive and are stamped with the time of the
extraction. Two runs a second apart differed. It now compares only the archived subtree.

### Limits

- Both corpora are ~1.8 kB files. A tree of large files would spend its time in
  `fetchChunks` rather than in file creation, and the balance would differ.
- ext4 on one SSD. The directory-lock ceiling is a property of the filesystem: XFS, btrfs
  and network filesystems will each have their own, and the 4.3x separate-directory arm
  says the ceiling is the directory rather than this machine.
- `none-sha256`, warm cache, one machine.
- The barrier cost is bounded by measurement on a 4,953-directory tree, not by analysis. A
  tree with far more directories and far fewer files in each would drain more often, and
  where that stops paying is unmeasured.
