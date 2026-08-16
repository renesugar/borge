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

## 4. Cache and config directories are not shared

**Stage 0 · `docs/PORTING_PLAN.md` §0.5 · by design**

borge uses `~/.cache/borge` and `~/.config/borge`, not borg's. Sharing them would let a
borge bug damage a working borg installation, and the interop tests need the two tools
to be independently reproducible. Nothing is lost: the chunks cache and the
known-repositories file are rebuilt from the repository when absent.

Environment variables *are* shared, in a read-only direction: borge reads `BORGE_*`
first and falls back to `BORG_*`, so an exported `BORG_PASSPHRASE` keeps working
without borge squatting on borg's namespace.
