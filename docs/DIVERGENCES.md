# Deliberate divergences from borg

Every place borge knowingly behaves differently from the pinned borg, and why.

The rule (docs/PORTING_PLAN.md §0.5) is that the **on-disk format is binding** and
everything else is negotiable. So a divergence is only acceptable here if borg can
still read what borge writes and borge can still read what borg writes. Each entry
below says explicitly why that holds.

Accidental differences are bugs, not divergences. If something is listed here it was a
decision; if it is not listed and the two differ, that is a defect.

---

## 1. `--compression auto,...` records the plaintext size

**Stage 1.2 · `internal/compress/meta.go` · found by the differential test**

borg's `Auto.compress` copies only `ctype`, `clevel` and `csize` out of the inner
compressor's metadata — its local `get_meta` helper lists exactly those three keys
(`src/borg/compress.pyx`). The inner compressor did record `size`, but `Auto` drops it.
The result is that **an object written with `--compression auto,...` carries no
plaintext size at all**, in all three of `Auto`'s outcomes:

```
zstd,3        keys=['clevel', 'csize', 'ctype', 'size', 'type']   size present
lz4           keys=['clevel', 'csize', 'ctype', 'size', 'type']   size present
none          keys=['clevel', 'csize', 'ctype', 'size', 'type']   size present
auto,zstd,3   keys=['clevel', 'csize', 'ctype', 'type']           size ABSENT
auto,lz4      keys=['clevel', 'csize', 'ctype', 'type']           size ABSENT
auto,lzma,6   keys=['clevel', 'csize', 'ctype', 'type']           size ABSENT
```

It costs borg on every read of such an object: without a size, its LZ4 decompressor
cannot allocate the output buffer up front, so it guesses 8 MiB and grows by 50% until
the block fits.

**borge keeps the size.** Its `Auto` assigns the whole metadata copy the inner
compressor filled in, rather than picking three fields out of it.

Safe in both directions:

- *borg reading borge's object* — borg's `check_fix_size` only asserts that a size, if
  present, equals the decompressed length, which it does. Its LZ4 path then takes the
  exact-allocation fast path instead of the guessing one, so borg gets slightly faster
  reads out of it.
- *borge reading borg's object* — `Meta.SizeSet` is false and borge falls back to the
  same grow-and-retry loop borg uses, with the same starting size and growth factor.

This one was not optional. Requiring a size made borge unable to read **anything** borg
had written with `auto`, which is a commonly recommended setting. The differential test
caught it immediately; no unit test written against my reading of the source would
have, because the source reads as though `size` is always set.

## 2. Empty input under `auto` does not crash

**Stage 1.2 · `internal/compress/meta.go` · upstream bug**

`borg.compress.Auto.compress(meta, b"")` raises `ZeroDivisionError`, for every inner
compressor:

```
auto,zstd,3  RAISES ZeroDivisionError: float division
auto,lz4     RAISES ZeroDivisionError: float division
zstd,3       OK  -> ctype=0 csize=0
lz4          OK  -> ctype=0 csize=0
none         OK  -> ctype=0 csize=0
```

The chain takes three steps, which is presumably why it has gone unnoticed:

1. `LZ4._decide` short-circuits an empty input and returns `NONE_COMPRESSOR` with no
   data, so the "cheap" result is zero bytes.
2. `Auto._decide` computes `ratio = len(cheap) / (len(data) + 2)` = `0 / 2` = `0.0`.
   That is below the 0.97 probe threshold, so it selects the *expensive* compressor.
   Note it is the `+ 2` — a leftover from borg 1's two-byte compression prefix — that
   makes this branch reachable at all; without it the expression would be `0 / 0` one
   step earlier.
3. `Auto.compress` then evaluates `len(expensive) / len(cheap)`, and both are zero.

Reachability through `borg create` is doubtful, since the chunker yields no chunks for
a zero-byte file. It is still a latent crash on a public API.

**borge guards the division** and returns the empty result with `ctype = none`.
Refusing to compress an empty buffer is not behaviour worth being bug-compatible
about, and the divergence is invisible to borg because it only appears where borg
would have raised. `TestBorgeHandlesEmptyInputWhereBorgCrashes` pins it down; if
upstream fixes this, two skips in `differential_test.go` can be removed.

*Worth reporting upstream.*

## 3. Compressed bytes are not identical to borg's

**Stage 1.2 · `internal/compress/codecs.go` · by design**

borge uses Go's `compress/zlib`, `klauspost/compress/zstd` and `ulikunitz/xz` rather
than reimplementing zlib, libzstd and liblzma bit for bit. The compressed bytes
therefore differ from borg's for the same input and nominal level.

This is sound because **nothing downstream depends on them matching**: chunk ids are
computed over plaintext, and a pack file's name is the hash of that pack's own
contents. What must match — and what the differential test checks — is the format, the
recorded metadata, and decompressibility in both directions.

One visible consequence: `zstdLevel` maps libzstd's 1..22 range onto klauspost's four
encoder levels, so at a given nominal level the ratio may differ slightly. The `clevel`
byte still records the level the *user asked for*, so borg reports what borge was told.
Stage 9 measures the real difference rather than assuming it is negligible.

A second consequence, observed once in the differential corpus: for `auto,zstd,3` over
1 KiB of zeros, borg settles on lz4 and borge on zstd, because the two zstd
implementations land on opposite sides of the 1% threshold. Both objects decompress
correctly under either tool. The decision test reports such cases rather than failing,
and requires an exact match for `none` and `lz4`, which have no library-dependent
behaviour.

**A third consequence, found in stage 3.** borg's MAC modes are deterministic - no
nonce, no session state - and their docstring notes that two repositories with the same
key material therefore store byte-identical objects for identical input, "which allows
deduplicating them on the filesystem level". borge reproduces that exactly for every
object stored **uncompressed**, which includes the ones where a compressor decided the
data was not worth compressing. It cannot for a **compressed** payload, because the
compressed bytes come from a different library.

Measured over the stage 3 corpus, the difference is small and goes both ways: between
−5 and +12 bytes on objects of a few hundred bytes, and proportionally smaller on
larger ones. The interoperability that matters is unaffected - each tool reads the
other's objects, and both agree on the compression *decision* (the stored `ctype`),
which is the part that is format-visible.

## 4. Cache and config directories are not shared

**Stage 0 · `docs/PORTING_PLAN.md` §0.5 · by design**

borge uses `~/.cache/borge` and `~/.config/borge`, not borg's. Sharing them would let a
borge bug damage a working borg installation, and the interop tests need the two tools
to be independently reproducible. Nothing is lost: the chunks cache and the
known-repositories file are rebuilt from the repository when absent.

Environment variables *are* shared, in a read-only direction: borge reads `BORGE_*`
first and falls back to `BORG_*`, so an exported `BORG_PASSPHRASE` keeps working
without borge squatting on borg's namespace.

## 5. borge reads borg's keys directory but writes into its own

**Stage 4 · `internal/crypto/key/manager.go`, `KeysDirs` · by design**

Divergence 4 says config directories are not shared. Key files are the one place where
that rule, applied strictly, would break the thing the project is for: a key file is
byte-for-byte the same under either tool, and a user who runs `borg repo-create
--key-location=keyfile` and then reaches for borge would find that borge cannot open the
repository borg just made — not because of the format, but because it declined to look
in the obvious place.

So the keyfile search path is a list, not a single directory:

1. `BORGE_KEYS_DIR`, else `BORG_KEYS_DIR` — an explicit setting pins the search to that
   one directory and stops there. Someone who says where the keys are is not asking for
   a search.
2. `BORGE_CONFIG_DIR/keys`, then `BORG_CONFIG_DIR/keys`.
3. `BORGE_BASE_DIR/.config/borge/keys`, then `BORG_BASE_DIR/.config/borg/keys`.
4. Otherwise the platform config directory, `borge/keys` first and `borg/keys` second.

New key files are written to the **first** entry only, so borge never creates anything
inside borg's configuration directory. The asymmetry is deliberate: reading someone
else's directory is harmless, writing to it is not.

The consequence to be aware of is the reverse direction. borg looks in exactly one
place, so a keyfile borge created is not found by borg unless `BORG_KEYS_DIR` points at
it or the file is copied. `borge key export` / `borg key import` is the supported route,
and the stage 4 gate exercises it.

## 6. The paper key template is copied, not ported

**Stage 4 · `internal/crypto/key/paperkey.html` · by necessity**

`ExportPaperKeyHTML` serves borg's `paperkey.html` unchanged. It is a 66 KB self-contained
page carrying an inlined QR generator and SHA-256 implementation (both MIT, see NOTICE),
and it is the *reader* for a printed key as well as its writer. Reimplementing it would
mean a printed key whose QR codes a borg installation might not scan back, which is a
risk with no upside. The numbered-line format, which is what a human types back in, *is*
ported — and `TestPaperKeyMatchesBorg` asserts that borge's printout is byte-identical to
borg's, line for line.

## 7. POSIX ACLs are written without libacl

**Stage 5 · `internal/archive/acl_linux.go` · by design**

borg calls libacl through Cython to parse an ACL's text form and set it. borge writes the
kernel's binary ACL representation to the `system.posix_acl_access` and
`system.posix_acl_default` extended attributes directly, because that is all libacl does
underneath — it is a text parser and a struct packer, not a privileged interface. Avoiding
it keeps borge free of cgo, which §0.4 of the plan asks for.

The stage 5 gate compares ACLs between borg's and borge's extraction of the same archive,
so the two representations are checked against each other rather than assumed equivalent.

What is **not** supported: NFSv4 ACLs (`acl_nfs4`), which borg stores on FreeBSD in a
different format entirely. borge counts them in `ExtractStats.SkippedACL` rather than
failing, so a Linux restore of a FreeBSD archive still works and reports the omission
instead of hiding it. Restoring them belongs with FreeBSD support, which §0.6 puts after
1.0.

## 8. bsdflags are not restored yet

**Stage 5 · not implemented · to be closed before the stage 7 gate**

`item.bsdflags` (the Linux inode flags reachable through `FS_IOC_SETFLAGS`: immutable,
append-only, nodump, and so on) are read into the item structure and preserved on a
round trip, but extraction does not apply them. Nothing in the stage 5 corpus carries
one, so the gate does not currently measure it.

This is a real gap, not a decision, and it is listed here so it is not mistaken for one.
It needs an ioctl and has to run *last* of all attribute restoration — the immutable flag
makes every other change impossible — which is why it is a separate piece of work rather
than a line in `restoreAttrs`.

## 9. Sparseness survives only at chunk granularity

**Stage 7 · not a divergence from borg, but a property worth writing down**

`--sparse` restores an **all-zero chunk** as a hole. A chunk that contains a hole *and*
some data is written out in full, because the chunk is the unit the repository stores and
nothing records where inside it the hole was.

The practical consequence: with the default fastcdc maximum chunk size of 8 MiB, a hole
has to be several times that before any chunk falls entirely inside it. A 4 MiB hole in an
8 MiB file round-trips with the right contents and the right length, and comes back fully
allocated. borg behaves identically — its own documentation calls `--sparse`
"chunk-granularity, independent of the original being sparse".

This is recorded because it looks like a bug in a test: a small sparse file appears not to
restore sparsely, and the fix is a bigger file, not different code. The stage 7 corpus uses
96 MiB files for exactly that reason.

## 10. The inode field is unsigned

**Stage 7 · `internal/item/item.go` · forced by reality, not a choice**

`item.inode` is decoded and encoded as a `uint64`, while every other integer in an item is
`int64`. That asymmetry looks arbitrary and is not: an rclone mount synthesises inode
numbers from a hash, and values above 2^63 occur routinely. borg stores the field as a
msgpack uint64 and Python's arbitrary-precision integers never notice the difference;
a Go port that decodes it as `int64` **cannot read such an archive at all**.

Found by the stage 7 Google Drive corpus, which exists in the corpus list for its I/O
latency and turned out to be the only one that could surface this.

## 11. `break-lock` reports what it broke, and warns on a live lock

**Stage 8 · `internal/cli/lock.go` · an addition, not a behaviour change**

borg's `break-lock` removes every lock and says nothing about them. borge removes exactly
the same locks — refusing on a heuristic would block the one situation the command exists
for — but prints who held each one and when it was last refreshed, and exits with a
**warning** rather than success if any lock was still live.

A stale lock was going to be removed by the next client anyway, so breaking it is free. A
lock refreshed a minute ago means another client is very likely still running, and
breaking that one invites two writers into one repository. Both cases exit 0 in borg, so a
script cannot tell them apart; here the second exits 1.

This is a divergence in **exit code**, which is the kind that can break a caller. It is
recorded rather than quietly adopted: a script that runs `borge break-lock` unconditionally
and checks `$?` will now see a failure where borg gave success, and that is the intended
signal, not a bug.
