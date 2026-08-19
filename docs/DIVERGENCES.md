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

## 14. `debug convert-profile` is not implemented

**Stage 8 · nothing to port**

borg's `debug convert-profile` reads a borg profile (msgpack) and writes a **Python
`marshal`** file for `pstats` to open. The output format is a CPython implementation detail
with no reader outside CPython, and borge produces no borg profiles to convert in the first
place — its profiling is Go's `pprof`. Porting it would mean writing a Python bytecode
serialiser to convert a file borge never creates.

Recorded here rather than left as an unexplained gap in the subcommand list.

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

borge sorts each directory's entries before walking into them. borg archives them in
`readdir` order, which is whatever the filesystem returns — not defined, and different
between filesystems and between runs on the same one.

The same tree therefore produces the same archive twice with borge and need not with borg:

```
borg      root, root/sub, root/sub/deep.txt, root/notes.txt
borge     root, root/notes.txt, root/sub, root/sub/deep.txt
```

Both archives hold the same items, and every command in both tools reads either order, so
this costs no interoperability. What it buys is that two archives of an unchanged tree are
comparable — which is how several of the differential tests in this port are able to assert
anything at all.

**It is a trap for tests, and it caught one.** `TestExcludeAfterPositionalsMatchesBorg`
first compared the two tools' `list` output as a sequence and failed, having archived
exactly the same four paths. A differential test that cares *which* items were stored has
to sort both sides; one that compares sequences is asserting this divergence rather than
whatever it meant to check.

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

**Where borg has no label, borge keeps its own reason.** `--keep-within`, the `@PROT` tag
and `--keep-oldest` on its own are not one of borg's counted rules, so there is no
`daily #1` to print and the reason borge already had is used instead.

**`--keep-oldest` is borge's own option** — borg 2 has no such thing, which is worth
recording twice over: once because the label above has to cope with it, and once because
the option gate cannot see it. That gate reports borg options borge lacks and not the
reverse, so borge may carry others nobody has written down. Recorded in `PORTING_PLAN.md`
§11.2 as work on the gate.

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
