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

## 8. bsdflags and xattrs — **fixed 2026-08-19**, except the exclusion rule (#39)

**Stage 5 · filed then, closed in stage 8**

`item.bsdflags` (the Linux inode flags reachable through `FS_IOC_SETFLAGS`: immutable,
append-only, nodump, and so on) are not applied at extraction. Nothing in the stage 5
corpus carries one, so the gate does not measure it.

**Corrected 2026-08-18.** This entry used to say the flags were "read into the item
structure and preserved on a round trip". That is true only of an archive *borg* wrote:
borge decodes the field and re-encodes it, so it survives a `recreate`, but nothing in the
tree calls `FS_IOC_GETFLAGS` and so a borge-made archive has no flags in it to restore.
The gap is two halves, capture and apply, and the entry described neither accurately.

Consequences worth naming separately, because each looks like its own puzzle:

- **`--noflags` does nothing.** It is registered, parsed, and carried into
  `CreateOptions.NoFlags` and `ExtractOptions.NoFlags`; no code reads either field. It
  suppresses a capture that never happens. See #32 for the general case.
- **`flags` in the item JSON is permanently null** for borge-made archives where borg
  sends `0` — "not recorded" against "no flags set" (#35 added the key, 2026-08-18).

**And `xattrs`, found by the same measurement.** borge reads extended attributes at create
and writes them back at extract, so the feature works; what it does not do is write the key
when an item has none. borg writes an empty dict on every item unconditionally
(`archive.py`, `stat_ext_attrs`, which sets `attrs["xattrs"]` and `attrs["bsdflags"]` with
no test for emptiness). Restores are identical either way — this is stored bytes, not
behaviour — but the two fields together are about 18 bytes an item, and they are why two
archives of the same tree have item streams of different sizes, so that only same-source
comparisons of the recorded size come out exact (#36).

Both belong in one piece of work: they are the same two lines of `stat_ext_attrs`, the same
comparison measures them, and either alone leaves borge's item stream differing from
borg's.

**Done 2026-08-19.** `internal/archive/flags_linux.go` reads the flags with
`FS_IOC_GETFLAGS` and maps the three that travel — nodump, immutable, append — to the BSD
values borg stores, and writes them back with `FS_IOC_SETFLAGS`, masked so that inode bits
userspace does not control are preserved (borg's issue #9039). The apply runs *last* of all
attribute restoration at every one of the four call sites, because the immutable flag makes
every further change to the inode impossible: setting it before the timestamps would lock
the file against the rest of its own restore. Both keys are now written whenever the
attribute was examined, so `--noflags` and `--noxattrs` produce byte-identical presence to
borg's, and `--noflags` does something for the first time.

Measured end to end rather than asserted: a file with the nodump flag, archived by borge,
comes back with the flag whether *borg* or borge extracts it.
`TestFileFlagsRoundTripAgainstBorg` and `TestExaminedAttributesAreRecorded` hold both
halves; against the old code they fail on the stored value, on both restores, and on all
three key-presence states.

Immutable and append-only are exercised by neither test, deliberately: setting them needs
`CAP_LINUX_IMMUTABLE`, so an unprivileged run cannot even set one on the source to test
with. A restore that cannot set them fails silently, as borg's does — the data is restored
and the flag is not, which is the better of the two available answers and is borg's.

**What is not done is the rule that uses them; see #39.**

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

## 12. `debug delete-obj` exits with a warning when it could not delete something

**Stage 8 · `internal/cli/debug.go` · exit code**

borg prints `object <id> not found.` or `object id <id> is invalid.`, carries on with the
rest of the list, and exits 0. borge prints the same lines and carries on for the same
reason — a list of ids usually comes from another tool, and abandoning the remainder
because one entry was wrong is not helpful — but exits **1**.

The command's whole purpose is to make a specific object stop existing. A script that
deletes a list and gets exit 0 has no way to learn that half the list was never there,
which for a command used to build corruption corpora means the corpus is silently not the
one that was asked for.

## 13. `debug search-repo-objs` shows the bytes before a hit near the start of an object

**Stage 8 · `internal/cli/debug.go` · borg bug, not reproduced**

Two of borg's slice indices go negative and are then read by Python as offsets from the
end of the buffer:

- `data[offset - context : offset]` in `print_finding`. When the hit is closer to the start
  than the 32-byte context and the object is *larger* than the context, the start index
  wraps past the stop index and the context before the hit prints as `b''`.
- `last_data[-(len(wanted) - 1):]` when stitching two objects together to catch a hit that
  straddles them. For a one-byte search term that is `last_data[-0:]`, which is the whole
  of the previous object rather than nothing, so every hit in it is reported twice.

borge clamps in both places: the context before a hit is the bytes that are actually there,
and a one-byte term is reported once. Everything else about the finding lines — their
format, the hex, the Python `b'...'` renderings — is reproduced exactly, so a diff between
the two tools' output stays readable.

The leading number on a finding line is the object's position in a scan whose order is the
chunk index's, and the two implementations do not promise to agree on it.

## 14. `debug convert-profile` writes a plainer marshal file — **implemented 2026-08-20**

**Stage 8 · `internal/cli/pymarshal.go` · compared against borg as loaded objects**

`BORG_DEBUG_PROFILE=<file>` makes borg write cProfile's statistics as **msgpack** instead of
CPython's **marshal**, and says why in a comment: a profile may be mailed to a developer,
and "a format that is impossible to interpret outside an insecure implementation" is a poor
thing to send over the internet. `debug convert-profile` is the step back — msgpack in,
marshal out — run by whoever has `pstats` or `pyprof2calltree` and wants to open the file.

This entry used to say the command was not ported, for a reason that was wrong twice over:

- **"Porting it would mean writing a Python bytecode serialiser."** marshal serialises
  *data*, not bytecode. A profile holds strings, ints, floats, tuples and dicts, and the
  writer for those is [`internal/cli/pymarshal.go`](../internal/cli/pymarshal.go) — 275
  lines, most of them explaining CPython's format rather than producing it.
- **"borge produces no borg profiles to convert in the first place."** True, and beside the
  point: the input is *borg's*, and borg's profiles do not stop existing because the backups
  moved to borge. A port that replaces borg and leaves the old profiles unreadable by the
  tool that replaced it has left a job undone. (It is also still true that borge has no
  profiler of its own. When it gets one it will write Go's `pprof`, which `pstats` cannot
  read and which this command has nothing to do with — the earlier claim that borge's
  "profiling is Go's `pprof`" described an intention, not any code in the tree.)

**The divergence that remains is the bytes.** marshal has versions, and `marshal.dump`
defaults to the newest: version 3 added back-references, so an object appearing twice is
written once and referred to afterwards, and version 4 added a short form for ASCII strings.
Which objects get a reference is decided by **reference count** — `w_ref` in
`Python/marshal.c` returns early when `Py_REFCNT(v) == 1` — so borg's file records which
strings and small integers the interpreter happened to be sharing at the time. Reproducing
it byte for byte would mean emulating CPython's refcounting.

borge writes the plain forms instead: no references, no short ASCII. Every version of
`marshal.load` reads them, and the object loaded from borge's file is **equal** to the
object loaded from borg's, which is the whole of what a profile reader needs. On the profile
of a `borg repo-list` run — 499 entries, some seven and a half thousand calls — borge's
file is about 12% longer (152,433 bytes against borg's 135,963), loads to the same dict,
and `pstats.Stats` opens it directly.

This is the opposite choice from the debug dumps in [pydump.go](../internal/cli/pydump.go),
which *are* compared byte for byte, and the difference is what the output is for: a dump
exists to be `diff`ed against borg's, so textual noise would defeat it; a profile exists to
be loaded by `pstats`, which never sees the bytes.

Two smaller things the format forces, both tested against borg rather than against what I
expected Python to do:

- A msgpack array becomes a **tuple**, not a list, because borg unpacks with
  `use_list=False`. Here that is not a nicety: a profile's keys are
  `(filename, lineno, funcname)` triples, and a list is unhashable, so lists would make the
  dict unbuildable rather than merely different.
- A str that is not valid UTF-8 has to go out as Python would write it. `TYPE_UNICODE` is
  read back with the `surrogatepass` handler, and borg's unpacker decodes with
  `surrogateescape`, so the byte `0xff` has to become the three-byte encoding of U+DCFF —
  which `utf8.EncodeRune` refuses to produce, since it is not a legal scalar value. Writing
  the raw byte instead makes `marshal.load` raise, which is how the test caught it. It is
  the same convention pydump.go renders as `\udcXX`; here it goes out as bytes.

What borge refuses, borg refuses too: a file that is not msgpack, and one holding a msgpack
timestamp — which Python's unpacker turns into a `Timestamp` object that `marshal.dump`
has no code for. `TestConvertProfileRejectsBadInput` asserts both tools fail on the same
three inputs, because a script that checks the exit code learns something untrue otherwise.

**Found while closing this: `borge debug --help` was an error.** The three command groups
build no `FlagSet`, so nothing in them had ever seen an option — the same gap
`takeParentLogJSON` was written for. borg's groups are argparse parsers and print the
group's usage; borge answered `unknown debug command "--help"` on stderr and exited 2. It
surfaced because `command-coverage.sh` now has to ask borge for its subcommand list the way
it asks borg, and the obvious way to ask was the one spelling that did not work.

## 15. No tcsh completions

**Stage 8 · `internal/cli/completion.go` · a missing feature, stated rather than hidden**

borg generates completion scripts for bash, zsh, tcsh and fish through `shtab`. borge
generates bash, zsh and fish, and refuses tcsh with an explanation.

tcsh matches completions by word *position* and by option *name across all subcommands at
once*. borg's own documentation records what that costs: an archive name is only completed
if no options precede it, and `--sort-by` has to offer the union of the sort keys valid for
every command that has one. For borge's twenty-seven commands the result would complete the
wrong thing more often than the right one.

`borge completion tcsh` therefore exits with an error saying so, rather than either
generating something misleading or reporting "unknown shell" — which would be untrue, since
borg supports it and the omission is deliberate.

## 16. borge's zstd and lzma levels are coarser than borg's

**Stage 8 · `internal/compress/codecs.go` · measured, not estimated**

borge compresses with `klauspost/compress`, whose zstd encoder has four levels where
libzstd has twenty-two. `internal/compress/codecs.go` maps the range onto them, so several
distinct borg levels are one borge level. `borge benchmark cpu --compressing` shows it
directly:

```
zstd,10 (2MiB)   10.00 MiB   0.612s   17.1 MB/s  (4.5x)
zstd,16 (2MiB)   10.00 MiB   2.406s    4.4 MB/s  (4.8x)
zstd,22 (2MiB)   10.00 MiB   1.768s    5.9 MB/s  (4.8x)
```

`zstd,16` and `zstd,22` produce identically sized output; so do `lzma,0`, `lzma,6` and
`lzma,9`. The compression *ratio* column exists in borge's benchmark and not in borg's for
exactly this reason: measuring only throughput would have shown borge as **faster** than
borg at `zstd,22` when what is actually happening is that it does less work.

**This costs no interoperability.** The compression level is not part of the format: the
stored `clevel` byte records the level the user asked for, decompression does not consult
it, and borg reads borge's objects and vice versa. What it costs is compression the user
asked for and did not get, which is worth being able to see rather than discovering from a
repository that is larger than expected.

## 17. Archive names are stored literally: no placeholder substitution — **fixed 2026-08-17**

**Stage 8 · `internal/placeholders` · a gap, not a decision**

**Resolved.** borge substitutes the same placeholders borg does, in archive names and in
repository paths, with a hand-written `strftime` verified against CPython across 31
directives. `TestHelpExamplesRun` runs `borge create -r REPO '{hostname}-{now:%Y-%m-%d}' ~`
and requires an archive named `<hostname>-<date>` to exist afterwards.

This entry was itself stale for a day after the feature landed, which is the hazard the
whole of §2.1 is about: prose describing an absence goes false the moment the absence is
filled, and nothing fails. What follows is what the gap was.

borg substitutes placeholders in an archive name when the archive is created: `{now}`,
`{utcnow}`, `{hostname}`, `{fqdn}`, `{user}`, `{pid}`, `{borgversion}`, and the
`{now:%Y-%m-%d}` format form. borge stores the name exactly as given.

So this, which is the standard borg cron idiom:

```
borge create -r REPO '{hostname}-{now:%Y-%m-%d}' ~
```

creates an archive literally called `{hostname}-{now:%Y-%m-%d}`.

**Nothing warns.** Archive names need not be unique, so a nightly job would keep working
and would simply accumulate archives all sharing one literal name; it surfaces the day
somebody tries to name one of them. Retention still works, because `prune` sorts on the
stored timestamp rather than on the name.

The workaround is the shell:

```
borge create -r REPO "$(hostname)-$(date +%Y-%m-%d)" ~
```

Found while writing `borge help placeholders` — the topic was drafted describing borg's
behaviour, because borg has the feature, and running the command showed borge does not.
`TestHelpPlaceholdersTopicIsTrue` now checks the claim against the behaviour, so the topic
cannot quietly become false when this is implemented.

## 18. `repo-delete` does not remove a directory that holds anything else

**Stage 8 · `internal/cli/repo_delete.go` · deliberate**

borg's `repo-delete` calls `store.destroy()`, which removes the repository directory
outright. borge removes only the namespaces a repository owns — `archives/`, `cache/`,
`config/`, `index/`, `keys/`, `locks/`, `packs/` — and then removes the directory only if
nothing else is left in it.

If something else is there, the repository is still fully deleted, the leftover is named
on stderr, and the exit code is **1** rather than 0.

The case this protects: a repository created inside a directory that also holds other
things. `borge repo-delete -r ~/backups` where the user also keeps notes in `~/backups`
should cost them the backups they asked to delete and nothing else. The command is already
the one irreversible operation in borge; widening its blast radius to "whatever else was
in that directory" is not a reasonable default.

The exit code is the visible part of the divergence: a script that deletes a repository in
such a directory and checks `$?` will now see 1 where borg gave 0.

## 19. `key import` defaults to where the repository's keys already are

**Stage 8 · `internal/cli/key.go` · deliberate**

Without `--key-location`, borg imports a key into the storage its key *class* defaults to,
which for every encrypted mode is `repokey`. borge instead looks at where this
repository's existing keys live and matches that, falling back to `repokey` when there are
none to look at.

The difference only shows on a repository whose keys are deliberately kept **outside** it.
Importing a key there with borg's default puts a copy in `keys/` inside the repository —
next to the data it protects, which is the arrangement the user chose a keyfile to avoid.
Silently undoing that during a recovery, when the user is already having a bad day, is the
wrong default.

`--key-location repokey|keyfile` overrides it in both tools, and the two agree whenever it
is given.

## 20. Options must precede positional arguments — **fixed 2026-08-18**

**Stage 8 · `internal/cli` · a defect, not a decision — and it lost data**

**Resolved.** `internal/cli/args.go` permutes the arguments before `flag.Parse` sees them,
so an option is accepted wherever borg accepts one. The entry is kept because it is cited
from the plan and the tests, and because what it was is worth remembering. What follows
describes the defect; the fix is at the end.

borg parses with Python's `argparse`, which accepts options anywhere on the command line.
borge parses with Go's `flag`, which **stops reading options at the first non-option
argument**. So:

```
borge create -r REPO archive ~ --exclude 'sh:**/.cache'
```

does not exclude anything. `--exclude` and the pattern after it become two more *paths to
archive*. borge warns that they do not exist, exits 1 — and archives the directory the
user asked to leave out.

Measured on the same tree: borg's archive has 0 `.cache` entries, borge's has 2. Two
warnings scroll past in the middle of a backup's output, and the archive looks fine.

**This is the bad kind of divergence.** It is not a different answer to a question; it is
the same command meaning something else, silently, in the direction of storing data the
user tried to keep out. Anyone carrying a borg habit or a borg crontab across hits it.

**How it was found:** by executing the command-line examples in borge's own help topics.
Two of fifteen were wrong, and this one was wrong in a way that mattered. See
`PORTING_PLAN.md` §2.1.2.

### The fix

`internal/cli/args.go` wraps `flag.FlagSet` in a `flagSet` whose `Parse` moves the options
ahead of the positionals first, the way GNU `getopt` does. Three things make that safe, and
each of them is a case that would otherwise be silently wrong:

- **An option that takes a value carries the next argument with it.** Whether it does is
  asked of the `FlagSet` rather than guessed from the spelling, because the answer differs
  per command — `-e` is `create`'s `--exclude` and `repo-create`'s `--encryption` — and
  because `--keep-daily -1` must not lose its argument to the positionals.
- **`--` ends the options**, and one is re-emitted ahead of the positionals, so a path
  beginning with a dash still arrives as a path.
- **`with-lock` opts out entirely.** It runs another program, and permuting that program's
  arguments would pull the `-c` out of `borge with-lock sh -c '...'` and make borge reject
  its own command line.

One deliberate behaviour change comes with it: **an argument that begins with a dash and is
not one of the command's options is now an error** rather than a filename. Before, a
mistyped `--exlude` became a path and the only sign was a warning. That is argparse's
behaviour too.

It also leaves borge *more* permissive than borg in one spot. argparse cannot place an
option between two positionals when the second has `nargs="*"`, so
`borg create -r REPO NAME --exclude P PATH` fails with "unrecognized arguments"; borge
accepts it. Being more permissive than the tool being ported is not a compatibility
problem — every command borg accepts, borge accepts — but it is worth knowing that a
command line tested only against borge may not run under borg.

Verified by `TestExcludeAfterPositionalsMatchesBorg`, which gives borg and borge the same
command with the option last and requires the same archive contents, and by
`TestPermute`'s thirteen cases. The help topic's `OPTIONS COME BEFORE PATHS` section is
gone; `OPTIONS AND PATHS` replaces it and both of its example forms are executed by
`TestHelpExamplesRun`.

## 21. A relative source path is archived under its absolute path — **fixed 2026-08-18**

**Stage 8 · `internal/archive/create_linux.go` · a defect, not a decision**

**Resolved.** `Create` cleans each path instead of absolutising it, and `archivedPath` is
now borg's `remove_dotdot_prefixes`. The entry is kept for what it says about how the gate
missed it. What follows describes the defect; the fix is at the end.

borg stores a source path **as it was typed**, normalised. borge calls `filepath.Abs` on
each root before walking it and stores the result with its leading slash removed. From
`/srv/work`, with `home/me` beneath it:

| command | borg stores | borge stores |
| --- | --- | --- |
| `create A home/me` | `home/me/...` | `srv/work/home/me/...` |
| `create A ./home/me` | `home/me/...` | `srv/work/home/me/...` |
| `create A home/me/` | `home/me/...` | `srv/work/home/me/...` |
| `create A .` | `.`, `home`, `home/me/...` | `srv/work/...` |

An absolute source path behaves identically in both tools, which is why the stage 7 interop
matrix never saw this: every row in it passes absolute paths.

**Why it matters.** The stored path is the path a restore recreates. `borg create A .` run
in a project directory produces an archive that extracts into whatever directory the user
is standing in; borge's produces one that recreates the whole absolute path. Same command,
same tree, different archive — and the difference only becomes visible during a restore,
which is the worst time to discover it.

It also makes a documented pattern wrong: the patterns topic says an archive of `/home/me`
holds `home/me/...`, which is true of an absolute path and false of a relative one.

**How it was found:** building the fixture for `TestHelpExamplesRun` (§2.1.2). The
patterns topic's `sh:home/me/**/*.txt` example could not be made to match, and the reason
was not the pattern.

### The fix

`filepath.Abs` is gone. Each root is cleaned instead, the walk joins with `filepath.Join`
(which cleans, as borg's `normpath(join(...))` does), and `archivedPath` is borg's
`remove_dotdot_prefixes`: strip every leading `/`, then every leading `../`, and map `""`
and `".."` to `"."`. Dropping the `../` rather than refusing it is borg's choice and worth
stating — an archive of `../sibling` stores `sibling`, so what comes back out is a tree the
user can place anywhere rather than one that climbs out of wherever it is extracted.

An empty path is now refused outright, with exit 2, as borg's argument parser refuses it.
It would otherwise clean to `"."` and quietly archive the working directory, which is never
what an empty argument meant.

Verified across `home/me`, `./home/me`, `home/me/`, `home/me/../me`, `.`, `../sibling` and
an absolute path: borg and borge store the same names for all of them.

**The gate had a blind spot, and that is the part worth keeping.** Every row of the stage 7
interoperability matrix passes an *absolute* source path, and absolutising an absolute path
is a no-op — so the matrix could not have caught this however long it ran.
`TestRelativeSourcePathRoundTrip` is now a row that passes a relative one, and it checks
the round trip rather than just the names: the archive one tool wrote from a relative path
has to extract, in the other tool, to the same tree.

Writing that row taught its own lesson twice. The assertion first split `list --short` on
whitespace, and the synthetic corpus contains a filename with a space; then it split on
lines, and the corpus contains a filename with a **newline**. Any parsing of `--short` is
wrong for real data. It reads `--json-lines` now, and so do the other new path tests.

## 22. A repository path must be absolute — **fixed 2026-08-18**

**Stage 8 · `internal/store` · a defect, not a decision**

**Resolved.** `Env.resolveRepo` makes the path absolute after expanding its placeholders.
What follows describes the defect; the fix is at the end.

`borg repo-create -r REPO` works. borge answers:

```
borge: store: path must be absolute: "REPO"
```

borg accepts a relative repository path and resolves it against the working directory.
borge refuses it in the store layer before anything else runs.

This is smaller than #21 — it fails loudly, and the user simply types more — but it breaks
the same borg habit, and `-r .` or `-r ../backups` is a normal thing to type. Recorded in
`PORTING_PLAN.md` §11.

**How it was found:** the same fixture. The help topics write `-r REPO`, and the first
attempt to run them verbatim in a scratch directory was refused.

### The fix

The resolution happens in `Env.resolveRepo` (`internal/cli/cli.go`), not in the store, and
that placement is the decision worth recording. The store's rule — a backend is rooted at an
absolute path — is kept: a backend rooted at something that depends on the process working
directory is one nothing else can reason about. borg resolves at argument parsing too, and
reports the absolute form as the repository's `Location`, which borge now matches.

Placeholders are expanded *before* the path is made absolute, so `-r '{hostname}/repo'`
resolves the way a reader expects rather than producing a path containing a brace.
`TestResolveRepoExpandsBeforeResolving` pins that order.

**No `~` expansion**, because borg does none. `-r '~/backups'` means a directory literally
named `~`, in both tools; expanding it here would be borge inventing behaviour that a user
carrying a borg script would not expect.

Verified against borg in both directions — borge creates a repository at a relative path and
borg opens it, borg creates one and borge opens it — with the same repository id and the
same absolute `Location` from both. And resolution is against the working directory and
nothing else: from one level down, `-r sub/G` names a repository that does not exist, which
is what borg does and what shows the resolution is not a search.

## 23. Directory entries are archived in sorted order

**Stage 6 · `internal/archive/create_linux.go` · deliberate**

borge sorts each directory's entries **by name** before walking into them. borg sorts them
**by inode number** — `scandir_inorder` in `helpers/fs.py`, falling back to the entry's name
for a dirent it cannot stat.

*(Corrected 2026-08-20. This entry used to say borg archives in "`readdir` order, which is
whatever the filesystem returns — not defined, and different between filesystems and between
runs on the same one". borg does sort; just not by name. The error flattered borge's side of
the trade-off, which is the one direction a divergence entry must never be wrong in. Found
by measuring `create --list` while adding `--filter`.)*

Measured on a tree where `f1.txt` has a higher inode than `sub`:

```
borg   tsrc, tsrc/sub, tsrc/sub/deep, tsrc/sub/deep/f3.txt, tsrc/sub/f2.txt, tsrc/f1.txt
borge  tsrc, tsrc/f1.txt, tsrc/sub, tsrc/sub/deep, tsrc/sub/deep/f3.txt, tsrc/sub/f2.txt
```

**What each order is for.** Inode order is chosen for read locality: on a spinning disk, and
on many filesystems without one, reading files in inode order beats reading them in name
order — and a backup reads every file it stores. That is a real cost borge pays and does not
measure; one for stage 9's list. Name order buys the opposite property: it is the same on
every machine and every filesystem, so the same tree gives the same archive after a copy, a
restore, or a move to different hardware. Both are deterministic for an unchanged tree on
unchanged hardware; borge's survives the hardware changing.

Both archives hold the same items, and every command in both tools reads either order, so
this costs no interoperability. What borge's order buys is that two archives of an unchanged
tree are comparable — which is how several of the differential tests in this port are able
to assert anything at all.

**It is a trap for tests, and it caught one.** `TestExcludeAfterPositionalsMatchesBorg`
first compared the two tools' `list` output as a sequence and failed, having archived
exactly the same four paths. A differential test that cares *which* items were stored has
to sort both sides; one that compares sequences is asserting this divergence rather than
whatever it meant to check.

**It reaches `create --list` as well**, measured again on 2026-08-20: the listing follows
the walk, so the two tools print the same lines in different orders. borg also prints a
*directory* after its contents where borge prints it before — the same fact from the other
end, since borg finishes a subtree before reporting the directory that held it. A test of
`create --list` therefore compares sets, and says so.

It does **not** reach `import-tar --list`, whose order comes from the tar file rather than
from a directory walk. That one is compared as a sequence.

## 24. The rsync slashdot hack — **implemented 2026-08-18**

**Stage 8 · `internal/archive` · a gap, not a decision**

**Resolved.** `stripPrefix` and `walker.storedPath` port borg's `get_strip_prefix` and the
prefix handling in `create_helper`. What follows describes the gap; the implementation is
at the end.

borg lets a source path say where the stored path should start, the way rsync does: a `/./`
in the middle splits "the part used to read from the filesystem" from "the part that is
archived".

```
borg  create A /a/b/./c/d     stores  c/d, c/d/f.txt
borge create A /a/b/./c/d     stores  a/b/c/d, a/b/c/d/f.txt
```

borge cleans the path, which removes the `.` element and with it the instruction. The same
command therefore produces a different archive layout in the two tools — silently, and only
visibly at restore, which is the same shape of problem as #21.

It is a feature rather than a bug in what borge already claims to do, and it is not small:
borg computes a `strip_prefix` per root (`get_strip_prefix`), applies it in `create_helper`
with three cases including one that archives the pointed-at directory as `.`, and passes it
through `--pattern` roots as well. It wants its own change and its own tests. Recorded in
`PORTING_PLAN.md` §11.

**How it was found:** reading borg's `create_cmd.py` closely enough to port the path rule
for #21.

### The implementation

`stripPrefix` reads the hack out of the path *as typed*, before cleaning removes the `.`
element along with the instruction. It is borg's `get_strip_prefix`, including the three
edges that are easy to get wrong and are each a test row:

- Only the **first** `/./` counts, so `/a/./b/./c` stores `b/c` and not `c`.
- A `/./` at position zero is not the hack, so `/./x` is an ordinary path.
- A trailing `/.` is not the hack either — the string contains no `/./` — so `/a/b/.`
  stores the whole path, while `/a/b/./` stores `.`.

`walker.storedPath` is borg's `create_helper`: at the dot the item becomes `.`, below it
the prefix is trimmed, and above it there is no item at all. That third case cannot be
reached by a walk that starts at the cleaned root, since that root is always at or below
the dot; it is written anyway, because leaving out one of borg's three cases is the kind of
omission that is silent until something else changes.

**Patterns match the walked path, not the stored one.** With the hack in play those are
different strings, and this is the half that is easy to get backwards: an `--exclude` is
written against the filesystem the user is looking at, not against an archive that does not
exist yet. So `--exclude 'pp:a/b/c/d'` excludes and `--exclude 'pp:d'` does not, in both
tools. `TestPatternsMatchTheWalkedPathNotTheStoredOne` asserts the negative case as well as
the positive one, because matching on the stored path is the plausible wrong implementation
and it would pass a test that only checked the positive.

Verified against borg on ten path shapes including two controls, and mutation-checked:
making `stripPrefix` always return `""` fails every positive row and leaves the controls
passing.

## 25. `R` roots in a patterns file are not used — **fixed 2026-08-18**

**Stage 8 · `internal/cli/archive.go` · a gap, not a decision**

**Resolved.** `patternFlags.roots()` collects them and `create` puts them ahead of the
command-line paths, as borg orders them. What follows describes the gap.

borg's `--patterns-from` file may contain `R PATH` lines, which add recursion roots — paths
to back up — alongside the include and exclude rules. borge parses them and throws them
away: `LoadPatternFile` returns them and the caller assigns them to `_`.

With a patterns file whose only root is an `R` line:

```
borg  create -r REPO A --patterns-from pf.txt     archives the root
borge create -r REPO A --patterns-from pf.txt     borge: create needs an archive name and at least one path
```

It fails loudly rather than backing up the wrong thing, which is the better of the two ways
to be wrong, but it is still a valid borg command that borge refuses. The roots need to join
the positional paths — and, once they do, to carry the slashdot hack of #24 with them, since
borg treats them exactly as it treats a path on the command line.

**How it was found:** checking whether #24 reached `--pattern` roots as well as paths.

### The fix

`patternFlags.roots()` walks the pattern specs and returns every `CmdRootPath` value, from
a `--patterns-from` file or from a `--pattern 'R PATH'` on the command line — borg accepts
both. `create` puts them ahead of the positional paths, which is borg's order
(`args.pattern_roots + args.paths`), and counts them when deciding whether it has anything
to do: `create NAME` with a patterns file whose only root is an `R` line is now a valid
command, and `create NAME` with no paths anywhere is still refused.

Only `create` uses them, as in borg. For every other command a pattern file describes what
to select out of an archive, and a root has nothing to select.

## 26. Pattern options were applied grouped, not in the order written

**Stage 8 · `internal/cli/archive.go` · a defect, not a decision — fixed 2026-08-18**

The first matching pattern decides, so the order of `--exclude`, `--exclude-from`,
`--pattern` and `--patterns-from` *relative to each other* is the whole meaning of a
command line. borge kept a slice per option and walked them in a fixed order — all the
`--pattern`s, then all the `--patterns-from`s, then the `--exclude`s — so the order the
user wrote was discarded:

```
borge create -r REPO --exclude 'sh:**/keep.txt' --pattern '+sh:**/keep.txt' A tree
```

archives `keep.txt`, because the `+` was applied first whatever the user typed. borg leaves
it out. Reversing the two options changes nothing in borge and everything in borg.

The comment above `patternFlags` claimed the options were "collected in the order the user
wrote them, because order decides the outcome". They were collected that way *per option*
and then thrown into groups, so the comment described an intention rather than the code —
which is why nobody looking at the file would have doubted it.

**The fix.** One slice of `patternSpec{kind, value}` shared by all four options. Go's
`flag` calls `Set` in command-line order across every option, so a single `flag.Value` per
kind appending to one list keeps the order for free, and `matcher` walks it once. Argument
permutation preserves the relative order of the options it moves (#20), so this survives an
option written after the paths.

**How it was found:** measuring what `--patterns-from` did, while fixing #25. The two
defects are one restructuring apart, so they were fixed together.

## 27. `tag` takes one tag per option, where borg takes a list

**Stage 8 · `internal/cli/manage.go` · deliberate — and it declines a borg bug**

borg spells the tag options variadically: `--set [TAG ...]`, `--add [TAG ...]`,
`--remove [TAG ...]`. borge's take exactly one value each and are repeatable, so
`--add a --add b` is how two tags are added.

This is not a cosmetic difference. argparse's greedy `nargs="*"` swallows the positional
archive name, so in borg:

```
borg tag --add Z a2      adds the tags "Z" AND "a2" to EVERY archive in the repository
borg tag a2 --add Z      adds the tag "Z" to archive a2          (the intended reading)
borg tag --add Z -- a2   the same, spelled defensively
```

Measured, not inferred: the first form left all three archives of a test repository tagged
`Z,a2`. There is no warning; the command reports what it did in terms of archive ids, and
a reader checking that the tag was added sees that it was.

borge's spelling cannot express the ambiguity, so `borge tag -add Z a2` means the one thing
it looks like. Every borg command line that is *unambiguous* still works, because a
repeated single-value option accepts each tag in turn; what borge declines is the shape
that silently rewrites the whole repository.

**How it was found:** giving `tag` the archive-filter group, and checking what borg does
with a positional and an option together before copying it.

## 28. A write command whose selector matched nothing is an error

**Stage 8 · `internal/cli` · deliberate**

borg exits 0, having done nothing, when an archive filter matches no archive — for
`delete`, for `tag`, and for the rest. borge refuses:

```
borge tag -add PROT -a 'sh:dayly-*'
borge: no archive matched; nothing was changed        exit 2
```

The typo is the point. `sh:dayly-*` matches nothing, borg reports success, and the user
believes their archives are tagged until the day pruning removes them. A write command that
changed nothing while reporting success is the failure shape `PORTING_PLAN.md` §2.3
collects, and it is the one that hides longest.

**Read-only commands are not affected**, and that asymmetry is the whole design: `borge
info -a no-such-archive` and `borge repo-list -a 'sh:no-such*'` print nothing and exit 0,
exactly as borg does. Asking to list a set that turns out to be empty *has* been answered.
Asking to change a set that turns out to be empty has not.

borge already behaved this way for `delete` before this entry existed; `tag` was brought
into line when it gained the archive filters, and the policy is written down here so the
next command to take a selector does the same rather than choosing afresh.

**Scripts that rely on borg's exit 0** will see a failure where borg reported success. That
is the intended trade: the alternative is a backup script whose retention tagging silently
stopped working.

## 29. `create --list` reports a directory before its contents

**Stage 8 · `internal/archive/create_linux.go` · deliberate**

borg prints a directory's `--list` line *after* the lines for everything inside it; borge
prints it before. The archives are identical — both store the directory ahead of its
contents — and only the progress listing differs.

```
borg                        borge
+ src/sub/g.txt             + src
+ src/sub                   + src/f.txt
+ src/f.txt                 + src/sub
+ src                       + src/sub/g.txt
```

borg reports an item once its subtree is finished, which is what puts the parent last.
borge reports as it goes, which means a long backup names the directory it is working in
*before* spending an hour in it, rather than after.

The sibling order differs too, and for a different reason: borge walks each directory's
entries sorted and borg takes them in `readdir` order (#23). So the two listings hold the
same paths in two different orders, and a differential test compares them as sets.

**Not to be confused with the statuses**, which do match: `A` added, `d` directory, `s`
symlink, `i` special file, `h` a further hard link, `U` unchanged, `-` excluded, and `+`
for everything a dry run would have stored.

## 30. borge stored an access time borg leaves out — **fixed 2026-08-18**

**Stage 6 · `internal/archive/create_linux.go` · a defect, not a decision**

**Resolved.** `create` now stores `mtime` always, `ctime` unless `--noctime`, and `atime`
only with `--atime`, which is borg's rule.

borge set all three on every item. borg stores `atime` only when asked. So every borge
archive carried a timestamp borg leaves out:

| | stored per item |
| --- | --- |
| borg, default | `ctime`, `mtime` |
| borge, before | `atime`, `ctime`, `mtime` |

**The cost was not the bytes.** `atime` moves when a file is merely *read*, so two backups
of a tree that nobody changed produced different item metadata — a `borge diff` reporting
files nobody touched, and item-stream chunks that dedupe against nothing. A backup that
changes because something read the disk is a backup that cannot be compared with itself.

**Why no gate saw it.** The stage 7 comparator's `entry` type carries `MTimeNsec` and
nothing else of the three: atime and ctime are *deliberately* excluded from the restore
contract, and rightly — a restore is not expected to reproduce them. But that means the
whole interoperability matrix could run forever without noticing that borge *stored* one
borg does not. It is the third gate in stage 8 found to be measuring only what it looks at,
after the all-absolute source paths (#21) and the corpus with no bsdflags (#8).

**How it was found:** implementing `--atime`, `--noctime` and `--nobirthtime`, and asking
what the defaults were before adding options to change them.

`--nobirthtime` is accepted and does nothing on Linux, in both tools: birthtime is only
reachable through `statx`, which neither reads here. Recorded rather than silently ignored.

## 31. A dry run says what it would do

**Stage 8 · `internal/cli/manage.go` · deliberate**

`borg delete --dry-run` without `--list` prints **nothing**. borge prints a summary:

```
borge delete --dry-run -a 'sh:daily-*' --force
would delete 3 archive(s); nothing was changed (pass --list to see which)
```

The whole point of a dry run is to decide something from what it says, and silence is an
answer nobody can act on. "Three archives would go" and "your selector matched nothing"
look identical when both print nothing — and this is the command where being wrong is
irreversible. See `PORTING_PLAN.md` §2.3, which collects the same failure in arguments.

The summary appears with `--list` too, without the pointer: "nothing was changed" is the
reassurance a dry run exists to give, and it should not depend on which options happened to
be passed. `undelete --dry-run` does the same.

**Scoped to dry runs.** Every real path — `delete`, `delete --list`, `undelete`,
`undelete --list` — is byte-identical to borg's, and stays that way. There is a format
there that scripts parse; there is none in a dry run, because borg emits nothing.

**Fairness to borg's silence.** borge can afford this because it already refuses an empty
selection outright (#28) and requires `--force` for a multi-archive delete, so the summary
is extra detail rather than the only signal. In borg the same silence covers "matched
nothing" as well, which is the worse case, and `--list` is the only cure.

The `--dry-run` help on both commands names `--list`, so the pointer reaches a reader who
never runs the command with the wrong options in the first place.

## 32. Options that would do nothing are reported

**Stage 8 · `internal/cli/create.go` · deliberate**

borg accepts two combinations silently and ignores them:

```
borg create --paths-delimiter '\0' ARCHIVE PATH        the delimiter is not used
borg create --paths-from-stdin --exclude 'sh:x' NAME   the pattern is not applied
```

The second is the dangerous one. `--paths-from-stdin` means "back up all files given — no
more, no less", so the exclusion has no effect, and a user who wrote one is entitled to
believe a filter is in place. borge warns on stderr and carries on:

```
borge: warning: the include/exclude options do not apply to paths read from a list:
       the list is taken as given
borge: warning: --paths-delimiter does nothing without --paths-from-stdin, …
```

Both are warnings rather than errors: the command is still doing something coherent, and
refusing it would break a script that passes a fixed set of options to several invocations.
Both go to stderr, so a piped listing is unaffected. See `PORTING_PLAN.md` §2.3 — an option
that silently does nothing is the same failure as a filter that silently matches everything.

## 33. `--format` prints "None" for a key an item does not carry

**Stage 8 · `internal/cli/itemformat.go` · reproduced on purpose**

borg's item data holds Python's `None` for a key the item does not have, and formatting it
produces the four letters `None`:

```
borge list --format '{path}|{user}|{uid}{NL}' ARCHIVE
stdin|None|None
```

It reaches `uid`, `gid`, `user`, `group`, `flags` and `inode`, and in practice only for
items that were never files — the ones `create ARCHIVE -` and `--content-from-command`
make, which have no inode to take ownership from unless `--stdin-user` is given.

**It is a Python artifact showing through**, and it looks like a bug in Go source, which is
why it is written down here. borge reproduces it because a listing is something people
parse: printing an empty column instead would break a script that greps for `None`, and
"borge is tidier than borg" is not a reason worth a compatibility break before stage 10.

The alternative — an empty column — is the tidier answer and is what borge did first. It
was changed after a differential over a streamed item showed the difference.

## 34. `prune` says what it did

**Stage 8 · `internal/cli/prune.go` · deliberate**

The per-archive listing is borg's, exactly — the label padded to 44 columns, then the
archive rendered through `--format`:

```
Keeping archive (rule: daily #1):            a-1    Mon, 2026-08-17 … [id]
Would prune:                                 a-10   Sat, 2026-08-08 … [id]
Pruning archive (1/3):                       b-10   Sat, 2026-08-08 … [id]
```

What borge adds is one summary line, on stderr, whether or not a `--list` option was given:

```
would prune 1 archive(s), kept 1, policy: daily=1
```

borg prints **nothing at all** without `--list`, `--list-kept` or `--list-pruned`. This is
the same silence as #31, in the command that removes history: "one archive would go" and
"your selector matched nothing" are not distinguishable from an empty screen, and a
retention policy that quietly stops matching is a backup that quietly stops being kept.
`PORTING_PLAN.md` §2.3.

*(Updated 2026-08-20.)* This entry used to record `--keep-within`, `--keep-last` and
`--keep-oldest` as borge's own retention rules, and to explain how the listing label coped
with them. All three are gone: borg 2's `--keep` covers the first two and it keeps the
oldest archive automatically. See #50. What remains borge's is the summary line above, and
`borge prune -v` now also prints borg's own three lines. The paragraph about the option gate
seeing only one direction is also out of date — it has reported borge's additions since
2026-08-19, which is how the six stale entries for this command were caught.)*

## 35. `--json` where borg has none — **fixed 2026-08-18**

borge registered `--json` on every repository command, because it lived in `commonFlags`
alongside `-r` and `-v`. borg has it on eight. So twelve borge commands accepted the
option and printed prose anyway:

```
$ borge check -r repo --json
...checking...            # not JSON, and no complaint about asking for it
$ borg check -r repo --json
error: unrecognized arguments: --json
```

That is worse than not having the option. borg's JSON output *is* borg's API — "Borg does
not have a public API on the Python level […] Borg provides an API on a command-line
level" (`docs/internals/frontends.rst`) — so `--json` is a frontend asking "can I drive
this command programmatically?". borge answered yes twelve times and then didn't.

`--json` now lives in `commonFlags.registerJSON`, called only by the commands borg puts it
on: `create`, `import-tar`, `prune`, `info`, `repo-info`, `repo-list`, `version`,
`analyze`, and `benchmark cpu`. `TestJSONOptionSurfaceMatchesBorg` compares the two
surfaces command by command and fails in *both* directions.

**`borg list --json` works and is not an option.** It is argparse expanding an unambiguous
prefix of `--json-lines`; `borg list --help` does not offer it, and `borg list --jsonzzz`
is rejected where `--json` is not. Measuring `borg list --json` against `--json-lines` and
finding the bytes identical is therefore not evidence of an alias — it is the same option
twice, and an early reading of this recorded it as "borg offers --json on 11 commands"
when the real count is eight. borge implements no prefix expansion (Go's `flag` has none),
so `borge list --json` is an error, and the difference is argparse's, not the JSON API's.

## 36. `original_size` and the stored `{size}` — **fixed 2026-08-18**

Same tree, same files, one run each, before the fix:

| | borg | borge |
|---|---|---|
| `create --json` → `archive.stats.original_size`, 1 MB file | 1000643 | 1000000 |
| `create --json` → `archive.stats.original_size`, 8 B in 5 items | 646 | 8 |
| stored `{size}`, 1 MB file | 1000035 | 1000483 |
| stored `{size}`, 8 B in 5 items | 43 | 1131 |

Two separate faults, in opposite directions.

**The stored figure counted the item metadata stream.** It is written into the archive
metadata and read back by both tools, so `borg info` on a borge-made archive reported
whatever borge had written — this is interop, not formatting. borg's number is the file
content chunks plus the item *pointer* chunks, and excludes the item stream.

That is not what borg's code says. `Archive.save()` reads:

```python
self.items_buffer.flush(flush=True)  # this adds the size of metadata stream chunks
                                     # to stats.osize
```

The comment is untrue. `create_cmd.py:252` does `archive.stats += fso.stats` to fold the
file processor's counts into the archive's, and `Statistics.__add__` **returns a new
object**. `archive.stats` is rebound to it; `archive.items_buffer.stats` still refers to
the old one, so every item-stream chunk written from then on increments a counter nobody
reads.

Measured rather than deduced, because the source reads the other way. 400 empty files
produce an item stream of tens of KB, and borg records `size=35` — msgpack's encoding of a
one-element list holding one 32-byte id, and nothing else. 5000 empty files record 341, the
pointer chunks for a stream chunked into ten pieces. One 1 MB file and 100 empty files both
record exactly 35 bytes of overhead.

borge now matches the behaviour rather than the comment: `Builder.AddChunk` skips
`OriginalSize` for `TypeArchiveStream` chunks. Held by `TestArchiveSizeMatchesBorg`, which
compares the stored size and file count across four tree shapes chosen to separate content
bytes from item count. Against the old code it fails on all four, by 2000× on the
many-items case (74869 against borg's 35).

**Except on `recreate`, where borg counts it after all.** The fold that loses the counter
is in `create_cmd.py`; `ArchiveRecreater` uses `target.stats` throughout and never rebinds
it, so there the item buffer keeps writing into the counter that is read. The same
5000-file tree records `size=341` through borg's create and `size=1284447` through its
recreate — borg disagreeing with itself by a factor of 3700 on identical content.

borge therefore makes it a property of the path (`BuilderOptions.CountItemStreamSize`,
true only for recreate) rather than a global rule. The first version of this fix was
global, which matched borg on create and import-tar and broke recreate; nothing caught it
but a manual check, so `TestRecreateSizeMatchesBorg` now does. It has borg create two
identical archives and recreates one with each tool, comparing the recreaters alone.

**The reported figure was sampled before the archive was saved.** `Create()` copied the
builder's counters when the walk ended, which is before the item stream is flushed, before
the pointers are written and before the archive object exists. borg reads the same counter
*after* all three. The old number was therefore not merely different — it followed a
different rule for a large backup than a small one, because a long walk flushes the item
stream part-way and a short one does not, and for a many-item tree it came out *below* the
size stored in the archive. Now re-read from the builder after `Save`.

**What still differs, and must.** The reported figure includes the archive object, whose
size depends on its contents — and borge spells `command_line` differently from borg (#12).
For the measurement above that is a 42-character difference in the string and a 43-byte
difference in the number, the extra byte being msgpack's length prefix growing from `str8`
to `str16` at 256. So the two tools cannot report the identical figure while #12 stands,
and forcing them to would mean lying about the archive's contents.
`TestCreateReportedSizeFollowsBorgsRule` pins the relationship instead: for both tools the
reported figure exceeds the stored one and the gap is the size of an archive object.

**And the item streams themselves are not the same size.** Recreating trees that the two
tools created *separately* gives 1284447 against 1194429 — about 18 bytes an item — because
borg writes `bsdflags` and `xattrs` on every item and borge writes neither. Both archives
restore identically and each tool reads the other's, so this is not a correctness problem,
but it is why only same-source comparisons are exact. `bsdflags` is #8; the `xattrs` half
was found here and is not yet recorded elsewhere.

## 37. borge records a command line for `import-tar` — **fixed 2026-08-18**

borg's `import-tar` stores `command_line` and `cwd` in the archive metadata, as `create`
does. borge stored neither, so an imported archive had no answer to "what made this?" —
`info` showed an empty field and the JSON API published it as absent. The import path
simply never passed them to `Save`. Fixed; `borge import-tar` now records the same two
fields `borge create` does, in borge's spelling (`borge import-tar …` rather than the
absolute path of the binary — see #12).

**borge fills in `files_stats` where borg leaves it empty.** borg's `import-tar --json`
always reports `"files_stats": {}`; its importer does not route through the accounting
that `create` uses. borge counts the same status characters it counts for `create`, so the
key has the same meaning and is merely populated. Same shape, more information — recorded
because it is a difference a frontend can see.

## 38. borg's `import-tar` records twice the files it imported

A tar holding three regular files, imported by each tool:

```
$ borg  repo-list --format '{archive} size={size} nfiles={nfiles}{NL}'
tar-tiny size=49 nfiles=6
$ borge repo-list --format '{archive} size={size} nfiles={nfiles}{NL}'
tar-tiny size=49 nfiles=3
```

The sizes agree. The file count does not, and borg's is wrong: the archive holds four
items, three of them regular files, and `borg list` on borg's own archive says so.

The cause is two counters for one event. `tar_cmds.py:362` does `archive.stats.nfiles += 1`
for each file the command imports; `archive.py:1775`, inside
`TarfileObjectProcessor.process_file`, does `self.stats.nfiles += 1` for the same file; and
`tar_cmds.py:387` then folds the second into the first with `archive.stats += tfo.stats`.
borg's own `create` of the same tree records 3, so borg disagrees with itself between two
commands on identical content.

**borge does not match this.** The rule elsewhere in this port is that a stored number is
borg's number even when borg's is odd — #36 reproduces an accounting quirk exactly, because
it is stored metadata and a user comparing the two tools should see one answer. This one is
different in the way that matters: it is falsifiable from outside. `nfiles` claims six files
in an archive whose listing has three, and one `borg list` disproves it. Matching would mean
borge deliberately writing into archive metadata something the archive itself contradicts,
and there is no single "borg number" to match in any case, since borg's `create` and
`import-tar` disagree.

`TestImportTarFileCountIsTruthful` pins borge to the item count and asserts borg's doubling
as well, so that the day upstream fixes this the test fails and the decision gets revisited
rather than silently persisting.

## 39. Items excluded by attribute — **fixed 2026-08-19**

borg does not back up a file carrying the **nodump** flag. `archive.py`'s
`maybe_exclude_by_attr` raises `BackupItemExcluded` for three cases, and `create` reports
each as status `-`, the character it uses for an ordinary exclusion:

- `bsdflags & UF_NODUMP` — the standard Unix "do not back this up" marker;
- the xattr `com.apple.metadata:com_apple_backup_excludeItem`, which is how macOS marks a
  path excluded from Time Machine;
- the xattr `user.xdg.robots.backup` set to `false`.

An excluded *directory* is not descended into either (`recurse = False`).

borge implements none of it, so a file its owner marked "do not back up" is backed up:

```
$ borg  create -r repo a src && borg debug dump-archive a - | grep -c nodump.txt
0
$ borge create -r repo b src && borg debug dump-archive b - | grep -c nodump.txt
1
```

This is a difference in *which files are in the archive*, which is a more serious class
than anything in #8 — those were fields on items that were stored either way.

Found on 2026-08-19 while testing the flag capture in #8, and it could not have been found
before it: borge had no flags to test against, so the rule had nothing to read. Fixing #8
is what makes this implementable, which is why the two are adjacent and why this was filed
rather than folded in — it changes what a backup contains, and that deserves its own change
and its own gate.

**Done the same day.** `excludedByAttr` in `internal/archive/create_linux.go`, checked
where borg checks it: after the extended attributes are known and before any content is
read, so an excluded file is never chunked first. An excluded directory ends the walk into
its subtree, as borg's `recurse = False` does.

Two things measured rather than assumed, each of which would have made a plausible-looking
implementation wrong:

**borg's dry run does not apply the rule.** `borg create --dry-run --list` reports `+` for
a nodump file that a real run reports `-`, because the dry run never collects the extended
attributes and so cannot know. borge matches, and `TestDryRunDoesNotApplyAttributeExclusion`
asserts the `+` for *both* tools so that the inconsistency is recorded as borg's rather
than mistaken for borge's. It is worth knowing as a user: a dry run over-reports what will
be stored wherever these markers are in play.

**The Apple marker cannot be set on Linux at all.** Linux permits only the `user`,
`security`, `system` and `trusted` xattr namespaces, and
`com.apple.metadata:com_apple_backup_excludeItem` is in none of them — `Setxattr` returns
EOPNOTSUPP. So that rule is unreachable through `create` on this platform, though it still
matters for archives made on macOS and for anything imported from a tar carrying it. It is
covered by a unit test on the rule itself (`internal/archive/exclude_attr_test.go`) rather
than by the differential, and the differential says why in a comment instead of silently
omitting it.

**A consequence for testing, recorded because it looked like a regression.** Implementing
this invalidated a test written the same morning. `TestFileFlagsRoundTripAgainstBorg`
archived a nodump file and checked the stored flag — and once the exclusion existed, no
unprivileged `create` could produce an archive carrying a flag at all: nodump is the only
flag settable without `CAP_LINUX_IMMUTABLE`, and a nodump file is now archived by neither
tool. The test had been passing only because borge did not yet implement the rule.

It is replaced by three narrower claims rather than one impossible one: the flags are read
and stored (`TestExaminedAttributesAreRecorded`); a real flag *value* is read, since borge
can only exclude a nodump file by having read it (the tests above); and a stored flag is
applied on restore (`TestFlagsSurviveExtraction`), which builds an archive holding a
flagged item directly, because that is how such an archive actually arises — from a
privileged backup or another machine.

The rules are not uniform and the unit test exists mostly to pin that: the Apple attribute
excludes if *present at all* whatever its value, while `user.xdg.robots.backup` excludes
only when it is exactly `false` — that attribute exists to say "yes, back this up", so
reading it as a presence check would silently drop files whose owner asked for the
opposite.

## 40. `create --list` named the stored path, not the one it read — **fixed 2026-08-19**

`borg create A /srv/data --list` reports `A /srv/data/f`; borge reported `A srv/data/f`,
the path as archived. A listing answers "what is being read", and borg names the file on
the filesystem the user is looking at.

The two strings are identical for a relative source, which is how this survived: every test
that compared listings used one. It surfaced only when `--log-json` made the same string
into `file_status.path`, a field a frontend uses to show progress against the source tree.

`TestCreateListPathMatchesBorg` uses an absolute source and fails first if borg's own
listing comes back relative, so the case it exists for cannot quietly stop being exercised.

## 41. `--log-json` — **implemented 2026-08-19**

borg's `--log-json` turns stderr into one JSON object per line, each tagged with a `type`
(`docs/internals/frontends.rst`). It is the other half of the frontend API: `--json` gives a
frontend the command's result, `--log-json` gives it everything the command said on the way.
borge had it on none of its commands.

**The contract is that it is *all* of stderr.** A frontend parses line by line, so a single
plain-text line in the middle is a parse error — worse than not offering the option. Rather
than converting a hundred call sites and hoping none was missed, borge wraps stderr itself:
anything written to it that did not come from a level-aware helper becomes an INFO
`log_message`. A message nobody has thought about yet still comes out as valid JSON.
`TestLogJSONIsAlwaysParseable` runs five commands chosen for the different ways borge writes
— a listing, a summary, a `Done.` hint, a warning, an error — and fails on the first
unparseable line.

Registered in `newFlagSet` rather than in `commonFlags`, because borg has it on commands
borge builds without a repository — `version`, `help`, `completion`. Every borge command
that builds a FlagSet does so through that one function, which makes it borge's equivalent
of borg's common parser.

**Except the three that build no FlagSet at all**, which the first version of this entry
claimed were covered and were not. `debug`, `key` and `benchmark` dispatch straight to a
subcommand, so they never reach `newFlagSet`: their subcommands had the option and the
groups did not. borg accepts it in both places and honours it —

```
$ borg  debug --log-json dump-manifest -r /tmp/nope
{"type": "log_message", ..., "message": "Repository ... does not exist.", "levelname": "ERROR", ...}
$ borge debug --log-json dump-manifest -r /tmp/nope      # before 2026-08-19
borge: unknown debug command "--log-json"
```

— so a frontend putting the option where borg's own help shows it got an error instead of a
stream. `takeParentLogJSON` now takes it at the group, and
`TestLogJSONOnCommandGroups` holds it, checking as well that the same command without the
option still speaks plain text so the test cannot pass by making everything JSON.

The claim was wrong in the commit message too, and was caught by comparing the two tools'
whole JSON option surfaces side by side rather than by any test — the surface test compares
`--json` and `--json-lines`, not `--log-json`, because `--log-json` is on every command and
so says nothing per command. Extending it is worth doing.

Only that one option is taken at the group, not a parent-level parse of borg's whole common
set: borge implements one of those fourteen, and a general parse would have to know which
of the remaining arguments belong to the subcommand.

**Installed only after a successful parse**, which reproduces borg's documented caveat
rather than merely tolerating it: "JSON logging requires successful argument parsing. Even
with `--log-json` specified, a parsing error will be printed in plain text, because logging
set-up happens after all arguments are parsed." `TestLogJSONParseErrorStaysPlainText`
asserts that for *both* tools, so it is recorded as shared behaviour a frontend must expect.

**One documented type differs from the document.** `frontends.rst` says a `log_message`
carries `time`, and its own worked example shows `created` instead. Measuring borg settles
it: the key is `time`. borge follows what borg does, as in #36.

**What borge does not emit, and why.** borg's three progress types — `archive_progress`,
`progress_message`, `progress_percent` — are "not produced unless `--progress` is
specified", and borge has no `--progress` at all: it is one of the absent common options.
There is nothing to report and nowhere to report it from, so the types are absent rather
than empty. The prompt types are likewise absent, borge's only prompt being for a
passphrase and written to the terminal. Both are silence, not wrong output: a frontend that
sees no progress objects learns nothing false.

**`msgid` is never emitted.** borg attaches one to the messages that have it
(`"msgid": "Repository.DoesNotExist"`), and the specification says it "may be *null* or
absent", so omitting it is within the contract — but a frontend that switches on `msgid`
gets less from borge than from borg. It needs borge's errors to carry stable identities,
which they do not yet; that is its own piece of work rather than a formatting change.

**One difference in granularity.** borg emits one object per *log call*, so a multi-line
warning is a single object with newlines inside its message. borge's wrapper sees bytes, so
for output that did not come through `errorf`/`warnf` it emits one object per line. Every
line is a valid object of the right type; the contract holds and the granularity differs.

`file_status` objects are emitted by `create` and `recreate` under `--list`, and by no other
command, as borg does. Making borge's match borg's exposed #40. It also moved `recreate`'s
listing from stdout to stderr, where borg puts it — measured: `borg recreate --list` writes
309 bytes to stderr and none of the listing to stdout.

## 42. The last four JSON schemas — **fixed 2026-08-19**

borg puts `--json` on eight commands. As of 2026-08-18 borge matched four; these are the
other four, and each was wrong in its own way.

**`version`** sent six keys where borg sends two. The four extra — `revision`,
`borg_series`, `borg_commit`, `repository_version` — are useful facts and not borg's
document, and `--json` is an API: a frontend iterating the object saw four fields borg never
produces. All four are already in `version --long`, which is borge's own output and the
right home for them, so nothing was lost by removing them.

**`repo-info`** was a different document under the same name: `repository` carried `version`
and `archive_count` and no `last_modified`, `encryption` said `mode` where borg says
`encryption` and `id_hash`, and `manifest` is borg's key for nothing at all. A frontend
reading `repository.last_modified` found nothing; one reading `encryption.mode` found
something borg never sends. It now carries borg's `cache`, `encryption` and `repository`
blocks, and the numbers it dropped are still in the text output.

borg also sends `security_dir`, the per-repository directory where it records the manifest
and nonce it last saw. borge has no such directory, so the key is omitted rather than
pointed at a path that does not exist. The schema test asserts the key is present in borg's
document *before* removing it, so the exemption fails if it ever stops being true.

**`info`** sent nine of borg's fourteen archive keys. Four of the five missing —
`command_line`, `cwd`, `chunker_params` and `duration` — borge was storing in the archive
metadata all along and simply not reading back; the fifth is the `stats` block, into which
`nfiles` moved and `original_size` was added. `tags` was `null` for an archive with none
where borg sends `[]`, which reads as "unknown" to anything that iterates it.

`info`'s `stats` also carries `chunking_time`, `hashing_time` and `store_stats`, and borge
emits them here though it refuses to in `create` (#36). The difference is that in `info`
they are always empty on borg's side too — nothing in an archive records them, so borg's
Statistics object is fresh — so emitting the same constants says exactly what borg says.
In `create` borg's are real measurements and borge's would be invented.

**`analyze`** had the right numbers in the wrong document: every value matched borg's
already, but borg nests them under `dedup_size` (or `by_name`) with `hotspots` beside them
and the usual envelope, where borge emitted the numbers bare. One value was genuinely
missing: borg's `by_name.total` carries `archives` as well as the two sizes, so a frontend
reading `total.archives` found nothing where borg puts the number the row is a total of.

`hotspots` is `null` rather than absent when it was not computed. That is borg's own value
for the case — its comment reads "not computed, as opposed to computed and empty" — and it
happens when fewer than two archives match. borge computes hot spots and its output already
agreed with borg's exactly, so only the nesting changed.

**Not fixed, and not a schema question:** `borg version` takes `-r` and borge's does not, so
`borge version -r REPO --json` is an error where borg reports the server's version. It is
one of the thirteen common options borge still lacks, and it means something only once the
remote backends exist — until then borge's server *is* its client. Table row 9.

## 43. The `--json-lines` streams — **fixed 2026-08-19**

`TestJSONSchemaMatchesBorg` covers the eight commands with `--json`. `list`, `find` and
`diff` produce a *stream* rather than a document and had no comparison at all, so three
differences had survived every stage. Looking for the non-unicode handling of table row 7
is what found them.

**The item object is driven by `--format`, and borge's was fixed.** Eleven keys are always
there and the rest appear only when the format names them — the same rule as the archive
object (#42). `borg list --json-lines --format '{path}'` sends eleven keys; borge sent
thirteen. borge's coincided with borg's for the default format, which is why the earlier
"13 keys, zero value differences" check passed and proved less than it looked.

**`find` sent an envelope of its own.** borg emits the item flat, naming the archive with
the `archivename` and `archiveid` keys *inside* it and only when the format asks. borge
sent `{archive_id, archive_name, archive_time, item: {…}}` — four keys, none of them
borg's, and `archive_time` is a key borg has nowhere.

**`diff` renamed every key in a change.** borg sends `{"type": …}` with `item1`/`item2`;
borge sent `{"kind": …}` with `from`/`to` and a `description` borg does not have. The
values differed too: timestamps in borg's human layout rather than ISO-8601, and an owner
change as one `"user:group"` string where borg sends a two-element array. borg's `type`
names are not uniform, so borge spells them out rather than deriving them — a content
change is the bare word (`added`, `modified`), a timestamp is the bare attribute (`mtime`),
and the rest are phrases (`changed mode`). borg also records *that* a link changed without
saying to what, where borge's text form names both; the JSON now matches borg and the text
form keeps the more useful answer.

## The non-unicode rule, and the framing that was wrong

A path on Linux is bytes and JSON is text. borg's rule (frontends.rst) gives a value that
does not decode cleanly *two* keys: the named one holding an approximation with each bad
byte as `?`, and `<key>_b64` holding base64 of the original bytes.

borge emitted neither. Go's encoder replaces invalid bytes with U+FFFD, so a path came out
mangled with no way to recover it — lossy output that looks fine:

```
borg  {"path": "…/bad??name.txt", "path_b64": "…YmFk//5uYW1lLnR4dA=="}
borge {"path": "…/bad��name.txt"}
```

**The plan said `debug dump-*` and the JSON commands "should be one implementation rather
than agreeing by luck". That was wrong, and measuring settled it.** borg deliberately uses
*two* representations: `debug dump-*` **and `diff --json-lines`** write Python's surrogate
escapes (`\udcff`), while the item, archive and `file_status` objects use `?` plus `_b64`.
borge already matched the first — `pydump.go` — and unifying them would have broken it.
What the two share is the question, not the answer.

So `diff` now writes through `pydump.go`'s encoder, and everything else through `putText`.
`TestNonUnicodePathsMatchBorg` checks both forms, decodes the base64 back to the original
bytes, and fails first if borg's own output stops exercising the case.

## 44. `--reverse` and `--deleted`, decided per command — **2026-08-19**

Both reached every command that took borge's archive-filter group, because borge registered
the group whole where borg's `define_archive_filters_group` takes a `deleted` parameter.
That was inheritance rather than a decision, and this is the decision.

**The rule:** an option stays where it changes what the command *does or shows*, and goes
where it only reorders work whose result does not depend on the order.

**`--deleted`** now appears on `repo-list`, `info` and `find`. borg has it on `repo-list`
alone — measured, not assumed: `deleted=True` is passed once in the whole archiver, and not
even to `undelete`, which does not need it because undeleting is what it does. borge's
`undelete` behaves the same, verified. It is kept on `info` and `find` as borge's own,
because looking inside a soft-deleted archive is how somebody decides whether to undelete
it, and borg offers no way to do that at all. Removed from `analyze`, `check`, `delete`,
`prune`, `recreate`, `tag` and `undelete`.

**`--reverse`** is borge's everywhere: borg has no such option on any command. It stays
where the output is a listing in archive order — `repo-list`, `info`, `find`, `analyze`,
`check` — and is removed from the mutating commands.

It is removed from `prune` for a stronger reason than tidiness. prune's rules walk the
archives newest-first, so reversing the input would change **which archives are kept**. An
option that quietly alters a retention decision is worse than no option.

**Not decided, and left in place: `prune --first`, `--last` and `--sort-by`.** borg's prune
takes no archive-filter group at all, so all three are borge's there too. They are marked
as such in the help but not removed, because each changes which archives prune *considers*
and therefore which it deletes — `--sort-by` changes the order the keep rules walk, so it
changes the decisions themselves. That is a bigger question than the one this entry
answers, and it deserves its own evidence rather than being swept along.

## 45. borge-only options now say so — **2026-08-19**

Every option borge has that borg does not is marked in its own help text. There are two
markers because there are two cases, and calling them the same thing would have been
inaccurate:

- **`(borge only)`** — borg has no such option on any command: `prune --keep-within`,
  `--keep-last`, `--keep-oldest`, `analyze --hotspots`, `version --long`, `--reverse`.
- **`(borge only on this command)`** — borg has the option elsewhere but not here:
  `check --dry-run`, `repo-compress --dry-run`, `delete --force`, `find --short`,
  `info`/`find` `--deleted`, `prune --first`/`--last`/`--sort-by`.

`extract -C` gets a longer note, because it is the one that can mislead rather than merely
surprise: borg's `-C` is `--compression` on five commands, and borge's `extract -C` is the
destination directory. borg's own `extract` has no `-C`, so nothing is shadowed — but a
borg habit typing `-C zstd,3` here would name a directory rather than a compression, and
the help now says so.

The option-coverage gate fails on a borge-only option whose help does not carry a marker,
so this cannot rot: recording a reason in the gate's table and telling the user are two
different things, and only the second is visible to somebody reading `--help`.

## 46. Reports on stdout, where borg puts them on stderr — **fixed 2026-08-19**

stdout carries a command's data; stderr carries what it says about the work. borg draws
that line and borge did not. Measured, byte counts from the same repository:

| | borg out/err | borge out/err (before) |
| --- | --- | --- |
| `check -v` | 0 / 405 | **420 / 0** |
| `compact -v` | 0 / 456 | **276 / 0** |
| `repo-compress -v` | 0 / 38 | **180 / 0** |
| `repo-create` | 0 / 121 | **206 / 0** |
| `extract --list` | 0 / 412 | **404 / 0** |
| `break-lock` | 0 / 0 | **37 / 0** |

Every one of those is a report about work, not the result of it, and every one is now on
stderr. `create`, `prune` and `export-tar` had been fixed earlier for the same reason —
each time because something needed stdout to be clean and found it was not.

**What was already right, and was checked rather than assumed:** `analyze`, `repo-space`,
`repo-info`, `list` and `info` write to stdout in both tools, because there their output
*is* the data. Moving them would have been the same mistake in the other direction.

**The failure this prevents** is not cosmetic. `borge extract --stdout --list` would have
interleaved the item names into the file contents on the same stream — the same defect as
`export-tar --list` (#28) and `create --list --json` (#41), which is three times now. borge
has no `--stdout` yet, so `TestExtractStdoutStaysData` skips rather than passing, and turns
itself on the moment the option lands.

**Two places where borg disagrees with itself**, recorded because reproducing them would be
copying a mistake:

- `borg recreate` writes `Processing <name> <id>` to **stdout** while its `--list` listing
  goes to stderr. borge's recreate has no equivalent of that line — it prints a summary
  instead — and puts it on stderr with the rest.
- `borg break-lock` says nothing at all. borge reports whether any lock was held, on
  stderr, because "no locks are held" and "the command did nothing" are different answers
  (`PORTING_PLAN.md` §2.3).

`TestReportingCommandsWriteToStderr` holds the rule for six commands and fails if fewer
than four of them say anything, so it cannot pass by testing silence.

## 47. `--format` on `check` and `diff`, and borg's third key set — **fixed 2026-08-19**

borg has three format key sets, not two, and borge had two.

**`check`** formats with the *archive* set — the one `repo-list`, `prune` and `info`
already share — so the option itself was small. What was missing was the thing it formats:
borg announces every archive before checking it, and borge announced none.

```
Analyzing archive v2 Wed, 2026-08-19 17:45:39 -0700 157482e4…8937 (1/1)
```

The count is part of the line rather than of the format, as in borg. The default is
`{archive} {time} {id}`.

**And the format is validated before any work starts, which took a second attempt.** The
first version called `formatter.Keys`, which parses a template without checking that its
keys exist — the error only surfaced when something was rendered, and the line is rendered
only under `-v`. So `borge check --format '{nosuchkey}'` ran a whole repository check and
exited 0, where borg fails immediately with exit 4. `checkArchiveFormat` now renders an
empty archive up front, the way `checkItemFormat` has always done for the item keys. A
check of a large repository is a long time to wait to be told the output format was wrong.

The same latent gap exists in `repo-list` and `prune`, which also call `formatter.Keys` and
rely on rendering to catch a bad key. There it is harmless, because both always render, so
the user sees the error either way — just after the repository has been opened rather than
before.

**`diff`** needed the third set: seventeen keys whose records are *changes* rather than
paths or archives — `change`, `content`, `mode`, `type`, `owner`, `user`, `group`, `link`,
`directory`, `blkdev`, `chrdev`, `fifo`, `mtime`, `ctime`, `isomtime`, `isoctime`, `path`.

Implementing it turned out to be most of the work, because the renderings are the format:

- `{content}` is a padded field — `added: %20s`, `removed: %18s`, `modified: %8s %8s` with
  signed sizes at one decimal — and `{link}` and its relatives are padded to 27, which
  borg's source explains as "the length of the content change". Get a width wrong and every
  line of a long diff is ragged.
- A presence change is filed by the **kind** of thing that appeared: a directory that came
  or went is reported under `{directory}`, not `{content}`, and only a regular file goes to
  `{content}` with a size. borge had that kind folded into a description string, so
  `Change` now carries it as data.
- `{owner}` reports both halves, while `{user}` and `{group}` report only the half that
  actually differs.
- `{change}` is every key concatenated in borg's own order — the insertion order of its
  `call_keys` dict, not alphabetical — with the empty ones dropped and the ISO forms
  excluded, so a default listing does not print each timestamp twice.

**So this closed a difference nobody had recorded.** borge's `diff` text output matched
borg's in none of its parts: `changed mtime: X -> Y` against `[mtime: X -> Y]`,
`+15 B -5 B` against `modified:    +15 B     -5 B`, `changed link: keep.txt -> dir` against
a padded `changed link`. Routing the text through the same formatter made the whole output
borg's, byte for byte, which is what `TestDiffFormatMatchesBorg` now checks across six
format strings.

~~**One difference remains and is not this row's:** borge sorts its diff output and borg
does not, so the two agree line by line but not in order.~~ **Fixed 2026-08-20**, see #48.

**`borge diff --format '{content}'` also gained `BORG_UNITS` for free**, because the size
rendering now goes through the same `formatBytesIn` the rest of borge uses. borg's
`format_file_size` honours that variable everywhere too.

---

## 48. `--sort-by`, and the order borge printed instead — **fixed 2026-08-20**

**Stage 8 · `internal/cli/sortspec.go`, `internal/archive/diff.go`**
**· found by reading borg's default**

Four options — `list --sort-by`, `list --depth`, `diff --sort-by` and
`diff --same-chunker-params` — which took both commands to zero missing. Two of them are
the reason this entry is here.

### borge's diff was sorted; borg's is not

borge collected both item streams into maps and walked the union sorted by path. The
comment in the code called that "the same answer with less state", and it was neither: it
is borg's `--sort-by path` output, not borg's default, so **every line matched borg's and
the sequence did not**. borg zips the two streams positionally, yields a comparison as soon
as both sides produce the same path, and holds anything unmatched aside as an *orphan* —
emitting archive2's leftovers at the end as additions, then archive1's as removals:

```
modified:     +9 B    -13 B [mtime: …] src/zebra.txt     borg, default
modified:    +34 B    -13 B [mtime: …] src/apple.txt
added:                  4 B src/aaa_new.txt
removed:               14 B src/middle.txt

added:                  4 B src/aaa_new.txt              borge, before
modified:    +34 B    -13 B [mtime: …] src/apple.txt
removed:               14 B src/middle.txt
modified:     +9 B    -13 B [mtime: …] src/zebra.txt
```

It survived because two archives of the same tree usually stream in the same order, so the
difference only shows when a path is added or removed, or when the roots were given in a
different order. And because **every test sorted both sides before comparing**, which is
the exact shape of a test that cannot see an ordering bug. `TestDiffDefaultOrderIsBorgs`
now asserts borg's order *and* that it differs from the sorted order, so a tree that
happened to stream in sorted order fails the test rather than passing it.

Reproducing borg's zip needs both streams stepped in lockstep, which a callback iterator
cannot do; `iter.Pull2` turns each into a pull iterator. Only one runs at a time and each
holds its own unpacker, so their reads interleave exactly as two sequential walks' would.
It also buys borg's memory profile: only the orphans are held, where the map walk held
every item of *both* archives.

### Sorting, where borg puts it

borg has **two** `--sort-by` options and its own `sorting.py` says so at the top: `repo-list`
sorts archives by a spec with no direction prefixes, while `list` and `diff` sort an
archive's contents and take `<` and `>` per field. borge had the first; this is the second.
The key sets are a third and fourth set, sharing nothing with the format keys — `size_added`
can be sorted by and not printed, `{content}` printed and not sorted by.

Two behaviours are worth naming because they are not what a fresh implementation would do:

- **A stable pass per field, applied last to first.** A compound comparison would give the
  same answer, but this is borg's, and it means a field borge computes slightly differently
  cannot reorder what the fields above it settled. Descending is a reversed *comparison*,
  not a reversed slice, so ties keep their input order — Python's `sort(reverse=True)` does
  not reverse ties either.
- **`diff --sort-by user` sorts removed paths under `""`.** borg fills the missing side of
  an `ItemDiff` with `Item.create_deleted(path)` — an item carrying its path and nothing
  else — and its key function reads the plain attributes from item2. So for a removed path
  the owner is empty, the uid is `-1` and the timestamps are `0`. That reads like a bug and
  is not worth diverging over: sorting by user asks about the state in the second archive,
  which a removed path has none of.

### Three things the options were adjacent to

- **An empty spec is an error.** `--sort-by ''` fails in borg with "unsupported sort field:
  empty spec", while *omitting* the option means do not sort. A Go flag holding `""` cannot
  tell those apart, so `flagSet.wasSet` asks whether the option appeared at all. `--depth`
  needed the same: `--depth -1` lists nothing, and omitting it lists everything.
- **The chunker-params warning was conditional.** borg prints "--chunker-params might be
  different between archives, diff will be slow." unconditionally; borge printed its own
  wording only under `-v`. That is the wrong way round — the run that needs it is the one
  whose every byte count silently becomes `(can't get size)`, and that run is usually not a
  verbose one. Now borg's wording, both lines, always, exit code unchanged.

  Matching it exposed a smaller one underneath. Writing the two lines to stderr made
  `--log-json` emit **two records at INFO**, because the JSON logger wraps stderr and splits
  on newlines, where borg emits **one record at WARNING** with a `\n` in its message — so a
  frontend filtering on `levelname` saw no warning at all. `Env.warnRaw` now emits borg's
  text unprefixed in text mode and as a single record in JSON mode.
- **borge's own "N path(s) differ" line was on stdout.** borg has no equivalent, and diff's
  stdout is the data: a script reading `borge diff -v` into a parser found a count at the
  end of it. Moved to stderr, which is where #46 put every other report.

---

## 49. `--tar-filter`, and the compressed name that held a plain tar — **fixed 2026-08-20**

**Stage 8 · `internal/cli/tarfilter.go` · found by asking what borge did without the option**

Four options: `--tar-filter` on `export-tar` and `import-tar`, and `--filter STATUSCHARS` on
`import-tar` and `create`. Both tar commands are now at zero missing.

### The file name decides the compression, and borge decided differently

borge had no `--tar-filter` and compressed a `.gz` name with `compress/gzip`. Measured
against borg, one row per suffix:

| file name | borg writes | borge wrote |
|---|---|---|
| `.tar.gz`, `.tgz` | gzip | gzip ✓ |
| `.tar.bz2`, `.tbz` | bzip2 | **plain tar** |
| `.tar.xz`, `.txz` | xz | **plain tar** |
| `.tar.lz4` | lz4 | **plain tar** |
| `.tar.zst`, `.tar.zstd`, `.tzst` | zstd | **plain tar** |
| `foo.gz` | plain tar | **gzip** |

The first four rows are the bad ones: `borge export-tar arch backup.tar.xz` produced a file
that no `xz` can open, named as though it could, and **exited 0**. The last row is the same
mistake from the other side — borg's suffixes are `.tar.gz` and `.tgz`, not any `.gz`, so a
`dump.gz` that borg leaves alone was gzipped by borge.

Nothing caught it because the tar tests round-tripped borge against itself for compression
and against borg only for `.tar` — two halves that were each true.

### Filter programs, not libraries

borg pipes the tarball through an external program and its source says why: a compressor
in-process competes with borg for CPU, a library limits which formats are possible, and a
system may ship something better than the built-in — `pigz`, `pxz`. borge inherits the
decision rather than re-deciding it, because the decision is **visible**: `--tar-filter
'pigz -9'` has to mean the same thing in both tools, and a borge that quietly compressed
in-process would ignore what it was told.

zstd is borg's one exception — in-process, since libzstd's threading runs outside the GIL
and there is no better external tool — and borge follows that too, through
`compress.NewZstdStreamWriter`. It is the one format that needs no program installed.

`--tar-filter` is split the way borg splits it, with Python's `shlex` in POSIX mode and no
shell — borg's own comment is "Sorry pal, shell mode is a no-no". `splitCommandLine` is
checked against CPython's `shlex.split` case by case, including both of its error messages
("No closing quotation", "No escaped character").

**One difference on Ctrl-C.** borg's filter ignores SIGINT so that borg gets it first and
kills the filter itself; Go cannot run code between fork and exec, so borge's filter dies
first and borge then fails writing to a closed pipe. Both tools leave a truncated output
file. Only the message differs.

**And one on threads.** borg compresses a `.tar.zst` on one thread unless
`BORG_ZSTD_MT_WORKERS` says otherwise; borge's zstd library defaults to one worker per CPU.
`BORGE_ZSTD_MT_WORKERS` sets it explicitly in borge too. The bytes a decompressor sees are
the same either way — this is speed, not format.

### `--filter`, and the stream `import-tar --list` was writing to

`--filter STATUSCHARS` prints only the statuses named, and in borg it is one condition in
one place (`print_file_status`), which is why it costs nothing on the commands that have it.
borge now keeps it on the `Env` the same way.

Wiring it up found that **`import-tar --list` wrote to stdout**, where borg writes to stderr
— and with its own `Fprintf` rather than through `logFileStatus`, so it was not JSON under
`--log-json` either. That is DIVERGENCES #46 again, missed by that sweep because this call
site did not go through the shared printer. `create` had been fixed; its sibling had not.

### What the `--filter` comparison could not assert

`create --list` cannot be compared as a sequence: borg walks a directory in inode order and
borge in name order, and borg reports a directory *after* its contents where borge reports
it before (#23, whose description of borg's order was wrong until this work corrected it).
So that test compares sets and says so. `import-tar --list` **is** compared as a sequence —
its order comes from the tar file, not from a directory walk.

---

## 50. prune's retention policy was borg 1's — **fixed 2026-08-20**

**Stage 8 · `internal/manifest/prune.go`, `internal/cli/prune.go`**
**· found by reading borg's option list**

`prune` is the command that deletes history, and borge implemented a **different interface
from a different major version of borg**. borg 1 had `--keep-within` and `--keep-last`;
borg 2 has neither, and has `--keep`, `--from`, two quarterly rules, and a *value* on every
rule that may be a count or an interval. borge had borg 1's set plus two of its own.

| what borge had | what borg 2 has |
|---|---|
| `--keep-last N` | `--keep N` — every archive its own group, so a count keeps the newest N |
| `--keep-within 7d` | `--keep 7d` — the same rule with an interval |
| `--keep-oldest` (opt-in flag) | automatic, for the last rule given |
| `--keep-daily N` (count only) | `--keep-daily N` **or** `--keep-daily 30d` |
| — | `--keep`, `--from`, `--keep-13weekly`, `--keep-3monthly` |
| `--first`, `--last`, `--sort-by` | none of them |

### The one that mattered

**borge deleted the oldest archive where borg keeps it.** borg's last active rule also
keeps the oldest archive if that rule still has room — `keep_oldest=(rule ==
active_rules[-1][0])` — so a policy whose coarsest rule is satisfied by recent archives
still keeps the start of the history. borge made that an opt-in flag, so the default
behaviour of the two tools differed on *which archives survive*:

```
--keep-yearly 4, on archives spanning three years
borg   … Keeping archive (rule: yearly[oldest] #4):   a-2024-01-01
borge  … Would prune:                                 a-2024-01-01
```

"If that rule still has room" is the part that is easy to get wrong: with a *count*, the
room is gone as soon as the rule has kept that many groups, so `--keep-daily 3` over five
days keeps three archives and not four. The oldest is kept only when there were fewer
groups than the count allowed.

Two more places where "which rule is last" is decided by something invisible:

- **A rule given as zero is still a rule.** `--keep-daily 3 --keep-yearly 0` makes *yearly*
  the last active rule; it keeps nothing, so nothing keeps the oldest archive either. A Go
  `int` holding 0 cannot tell "given 0" from "not given", which is why the option type
  tracks whether it was set.
- **`--from` is not a rule.** Archives at or after its timestamp are held back before any
  rule runs, so they cannot occupy a retention period that an older archive would otherwise
  have filled. They appear in the listing under the rule name `skip`.

### The options that went

`--keep-last`, `--keep-within` and `--keep-oldest` are removed rather than kept as aliases:
each is a spelling of something borg 2 already has, and three ways to write one policy is
how a retention rule gets misread. `--first`, `--last` and `--sort-by` are removed too,
which closes the question left open on 2026-08-19 (`PORTING_PLAN` table row 11): borg has
none of the three on `prune`, and each changes what prune *deletes* rather than what it
shows — `--first`/`--last` hide archives from the rules, and `--sort-by` reorders the walk
the rules are defined against.

### Smaller things measured on the way

- **Protected archives are not considered at all.** borg drops `@PROT` archives before
  anything happens, so they are absent from the listing and from the JSON, and are not
  counted in "Applying rules to the matching N archives". borge reported
  `Keeping archive (rule: protected by @PROT)`, which is friendlier and is not what a
  frontend reading borg's output gets.
- **`--keep-13weekly` and `--keep-3monthly` are mutually exclusive** — two answers to "what
  is a quarter", and borg's parser refuses both at once.
- **"all" is in both halves of the validation.** borg rejects a policy where a finer rule
  reaches at least as far as a coarser one, checking counts and intervals separately —
  and `-1` is *both* a count and an infinity, so `--keep-daily all --keep-monthly 5` is
  rejected while `--keep-daily 30 --keep-monthly all` is accepted. Putting `-1` in one
  group only let the first of those through.
- **The error messages name rules, not options.** borg builds them from a dict keyed by
  `rule.key`, so `--keep-13weekly` appears as `quarterly_13weekly`, and an interval appears
  as Python's `str(timedelta)`: `7 days, 0:00:00`. Both are reproduced, because the messages
  are compared against borg's.
- **`prune -v` now prints borg's three lines** ("Repository contains N archives." and the
  two after it). They are `logger.info` in borg, so a plain run still shows nothing from
  borg and borge's own summary line from borge (#34).

### What the tests compare

A timeline of fourteen archives at fixed offsets from now — deliberately not round, so an
archive cannot land on either side of `--keep 7d` depending on which second each tool
started — run through twenty-seven policies, comparing the decision lines. Plus the JSON
form, `--from`, the protected case, and every refusal. The matrix is guarded against
vacuity by requiring `--keep all` and `--keep 1` to disagree.

---

## 51. The numbers borge did not measure — **fixed 2026-08-20**

**Stage 8 · `internal/store/stats.go`, `internal/cli/storestats.go`**
**· found by implementing an option**

`extract --stdout`, `--continue` and `--stats`, which take `extract` to zero missing. The
third is the interesting one: **borg's `extract --stats` is not about the archive**. It
prints what the *repository* did — thirty-one lines of call counts, times, volumes and
throughputs — and borge's store counted none of it.

### Sending nothing was right; measuring is better

`create --json` carried three of borg's six `stats` keys. `chunking_time`, `hashing_time`
and `store_stats` were omitted deliberately, and the reason was sound:

> Sending them as zeros would be worse than omitting them: a frontend charting
> `hashing_time` would draw a flat line and believe it, where a missing key is a question
> it can answer. — `PORTING_PLAN` §11.4

That argument is an argument for measuring, not for omitting forever, and `extract --stats`
forced the issue. `internal/store` now counts what it is asked to do — per method: calls,
time, and for load and store the volume — and the builder times the chunker and the id
hash. The JSON schema test's omission list is gone with them; it had a guard that failed if
borg ever *stopped* sending the keys, and that guard is what would have caught a stale
exemption.

### Three groups of counters, and what they mean apart

- The **per-method** counters are what the caller asked the store to do.
- The **backend** counters are what reached storage, so `backend_load_calls` below
  `load_calls` is a cache doing its job.
- The **cache** counters are that job from the other side.

With no cache configured the first two are equal and the third is zeroes — which is not a
fabrication but the truth about a store that has no cache. **`cache_disabled` is `True` in
borge where borg prints `False`**, and that difference is real: borgstore always has a cache
object for a local repository and leaves it unused, while borge's extract path configures
none at all.

### `import-tar --json` sends an empty `store_stats`, in both tools

borg fills `store_stats` in `create_cmd.py` and nowhere else, so `borg import-tar --json`
reports `"store_stats": {}` however much work the import did. It reads like an oversight
rather than a decision — the numbers are just as available there — but it is what a frontend
reading borg's API gets, so borge sends `{}` too. Found by removing the omission list: the
schema comparison failed on `import-tar` alone, with borge sending thirty-one keys where
borg sent none.

### `--continue`, and how not to test it

`--continue` skips an item whose extracted copy is already there and already right: borg's
`same_item` compares the type, the whole mode, the size and the mtime, and borg's own
comment bounds the claim — "good enough for the intended use case: continuing an extraction
of same archive that initially started in an empty directory".

The first attempt to test it counted store loads before and after, and showed borg's
`--continue` doing **nothing at all**: borgstore reports one load per pack range, so
skipping a file need not change the count. The test now compares outcomes instead — a file
zeroed but left at its original size and mtime is *skipped* by both tools, re-extracted by
both without the option, and re-extracted by both when truncated.

### Still open, and now written down

**`create --stats` prints seventeen lines in borg and seven in borge**, sharing five labels.
borg has `Repository`, `Time (nominal)`, `Time (start)`, `Time (end)`, `Duration`, `Time
spent in hashing`, `Time spent in chunking`, `Added files`, `Unchanged files`, `Modified
files`, `Error files`, `Files changed while reading`; borge has `Chunks` and `Files cache`,
which borg does not, and formats its sizes as bare byte counts where borg writes `632 B`.

Everything borg reports there is now measured — the timings landed with this entry, the
file counts are in `files_stats`, the times are in the archive metadata — so this is
formatting work rather than missing information. It is one command's summary and it is not
in the option gate's sight, which is exactly why it is recorded here rather than left to be
noticed.

---

## 52. A file that changed while borge read it — **fixed 2026-08-20**

**Stage 8 · `internal/archive/create_linux.go` · found by implementing `--files-changed`**

`create`'s last five options — `--files-changed`, `--sparse`, `--tags`,
`--read-special-timeout` and `--exclude-dataless` — which take `create` to zero missing.
The first of them was not a missing flag but **a missing safety feature**.

### borge stored torn files and called them whole

A file written while it is being read is stored as a mix of before and after: not the old
contents, not the new ones, and nothing that ever existed. borg stats the file again after
reading it, and if the timestamp moved it throws the work away and starts over — up to ten
times, sleeping longer each time — on the theory that whatever was writing will stop. If it
never stops, the last attempt is stored and marked **`C`**, counted in
`files_changed_while_reading`, and **not memorized in the files cache**, so the next run
reads it again rather than trusting the copy known to be wrong.

borge did none of that. It read the file, stored it, and reported `A`, and a database or a
log rotated mid-backup looked exactly like a file that had been sitting still.

Two details of borg's check are worth keeping, because a straightforward implementation
misses both:

- **A timestamp that merely falls inside the read window counts as a change.** borg's answer
  to its issue #3536: if the file was changed just before the first stat and again during
  the read, the second timestamp can equal the first because the filesystem's clock
  granularity hid it. So the window is widened by 20 ms at each end and anything landing
  inside it is treated as suspicious.
- **Special files are never checked.** borg: "fifos change naturally, because they are fed
  from the other side. no problem." Checking them anyway is not merely noisy — it made borge
  re-read a fifo, and the second read found the writer gone and the data lost. Measured,
  having written the check without the exemption first.

**The test for it was flaky first, and the flake is worth recording.** The writer goroutine
rewrote the whole 8 MB file and slept a millisecond between rewrites; an 8 MB read from the
page cache takes a few milliseconds, so a read could fit entirely between two writes and see
nothing. It passed locally and failed in the suite - three retries, then a clean read, and
the file reported "A". The detection needs **every one of borg's ten attempts** to collide
before it reports "C", so "usually collides" is not enough. The writer now writes *one byte*
in a tight loop on an open handle, which moves the timestamps thousands of times a second.

### Two more differences the same work uncovered

**Every fifo, character device and block device was reported as `i`.** borg has a letter for
each — `f`, `c`, `b` — and `i` is its letter for something else entirely: content read from
stdin or a pipe (`archive.py`: `status = "i"  # stdin (or other pipe)`). So `create --list`
named the wrong kind of thing and `--filter f` selected nothing. A special file read under
`--read-special` now reports `A`/`M`/`C` like the regular file it has become, which is also
borg's behaviour.

**A failed item was reported not at all.** borg prints `E` for a file it could not read and
counts it in `files_stats`, which is where its "Error files: N" comes from. borge printed
the warning and left the listing silent, so a file that was neither stored nor mentioned
looked like one that was never there.

### `--read-special-timeout`, and why an empty read is not the end

A fifo opened `O_RDONLY|O_NONBLOCK` with no writer returns zero bytes from `read(2)` — which
everywhere else means end of file. Here it means "nobody has connected yet". borge's first
attempt used Go's `SetReadDeadline`, which cannot tell those apart, and stored an **empty
file** for a fifo where borg reports an error.

borg's rule, ported: zero bytes counts as EOF only once some data has actually arrived, and
the timeout is a *gap* — it restarts whenever data arrives, so a slow writer trickling bytes
forever never times out. borg spells out the consequence and it is reproduced: a writer that
connects and closes without writing produces a timeout, not empty content.

### `--tags`: three forms, and borge can have two of them

borg's help says "comma-separated or multiple arguments". Measured, neither is quite true:

```
borg --tags one two          ->  one,two   (argparse nargs="+")
borg --tags one --tags two   ->  two       (the second overwrites the first)
borg --tags one,two          ->  error     (its validator forbids "," in a tag)
```

Go's flag package cannot express `nargs="+"`, so the form that works in borg is the one
borge cannot have. borge accepts a **comma-separated list**, which is safe precisely because
borg refuses commas inside a tag: no valid borg tag contains one, so splitting cannot
mis-read a legitimate tag, and `--tags a,b` fails loudly under borg rather than doing
something different. Repetition **overwrites** in borge as it does in borg — accumulating
would silently lose tags when the same command line was run under the other tool.

borge had **no tag validation at all**, on `create --tags` or on the `tag` command that
already existed: `borge tag --add 'my tag'` wrote a tag borg refuses to create, and one a
comma-separated `{tags}` listing cannot be read back from. borg's rules — 1 to 10
characters, none of `` ,$``, a leading `@` only for `@PROT` — now apply in both places, with
borg's wording.

### `--sparse` changes no stored byte

It is a read optimisation: an all-zero region is recorded by size with no data whether or
not it is given, because both tools detect an all-zero block "regardless of sparse mode"
(borg's `reader.pyx`). What it changes is that a hole is *seeked over* rather than read, so a
100 GB file holding 1 GB costs 1 GB of reads. The test asserts exactly that: an archive made
with `--sparse` is byte-identical to one made without it and to borg's.

### `--exclude-dataless` is inert on Linux, and says so

`SF_DATALESS` marks a macOS placeholder whose content lives in cloud storage; reading one
makes macOS download it, which is why the check happens before the file is opened. Nothing
on Linux sets that flag, so the option excludes nothing here. It is implemented against the
flag word borge already stores, so it will mean the same thing when borge is built for
macOS — an option that did nothing on the platform it was written for would be worse than
one that does nothing on this one.

---

## 53. The rest of row 3, and why four options are still missing — **2026-08-20**

**Stage 8 · `option-coverage.sh` · the alias gap is now zero**

`help --usage-only`, `help --epilog-only`, `key remove --passphrase` and the eight short
spellings borg offers that borge did not (`-n` on `compact`, `delete`, `undelete` and
`recreate`; `-s` on `compact`, `import-tar`, `recreate` and `repo-compress`). With those,
**every option spelling borg has, borge has**, and eleven options remain missing — four of
which belong to other rows.

### What `help --usage-only` and `--epilog-only` can mean here

borg's help for a command has two parts: an argparse usage block and an epilog of prose.
borge has neither in that form. Its usage is the option list Go's `flag` package builds from
the FlagSet the command registered, and its "epilog" is the one-line summary in the dispatch
table. Those are what the two options print, so a script asking for either gets the nearest
thing borge has rather than an error.

For a help *topic* — `patterns`, `placeholders` — borg prints the topic text under either
option, because a topic has no usage block to separate out. borge does the same.

One detail worth its comment: printing the usage cannot be done by running the command with
`-help`, which is how `completion.go` enumerates options. That path ends in `flag.ErrHelp`
and **exit 2**, and asking for help is not an error. The FlagSet is captured instead and its
defaults printed.

### The four that are not coming, and why

**`check --max-age` and `--max-duration`** configure a **pack-level integrity check borge
does not have**. borg 2 verifies packs and index objects by re-hashing the store object and
comparing with its name, records the results in `cache/checked-packs`, and then either
reuses recent records (`--max-age`) or bounds a partial run (`--max-duration`). borge's
repository check walks the *chunk index* and reads every chunk instead. The options are
meaningless without the record they filter, so they wait on that check being ported.

**`repo-delete --keep-security-info`** keeps the per-repository **security directory**,
which borge does not keep at all — already noted in `repo-info`'s JSON, which omits
`security_dir` for the same reason. borg stores there the manifest and nonce it last saw and
uses them to notice a repository that has been replaced or moved. An option to preserve a
directory that does not exist would be an option that does nothing.

**`repo-list --from-borg1`** reads a **borg 1.x** repository, a §0.6 non-goal for 1.0.
`repo-create --from-borg1` is the same decision from the other side and belongs to row 2.

The first two are gaps with a shape: each names a subsystem borge has not ported. The third
is a decision already taken. None of them is a flag somebody forgot.
