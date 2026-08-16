# borge — plan for porting `borg` to Go

Status: **Stages 0 and 1.1-1.4 complete. Stage 1.5 (item) is next.**
Last updated: 2026-08-16.

This is the working plan. It is versioned in git alongside the code and is expected to
be edited as facts are learned — when a stage's reality diverges from what is written
here, the plan is wrong and gets fixed, not the record.

---

## 0. Decisions

### 0.1 Format target: borg 2.x

`borge` targets the **borg 2.x repository format, repository version 4**, ported from
the local checkout of `borgbackup/borg` at:

```
commit 114bd1e944c4ade6e512be20b36bcdd6398ad78e (2026-08-16, master)
path   /home/renes/projects/borg
```

That commit is **pinned**. Chasing upstream master mid-port makes the interop gate
(Stage 7) meaningless. Rebasing onto a newer upstream commit is a deliberate,
scheduled activity with its own diff review, not something that happens implicitly.

Why borg 2 and not the borg 1.2.8 installed at `/usr/bin/borg`:

- The borg 2 repository is a **`borgstore` object store**: flat namespaces
  (`archives/`, `packs/`, `index/`, `keys/`, `config/`, `locks/`, `cache/`) holding one
  file per object, plus content-addressed pack files. Borg 1's segment files, `hints`,
  `index.N` and log-structured compaction are substantially more code to port, and that
  design is being retired upstream.
- **Borg 2 master already packs small objects.** `PackWriter`/`PackReader` in
  `src/borg/repository.py` buffer chunks into content-hashed pack files under
  `packs/<nesting>/<sha256>`, with the `ChunkIndex` recording
  `(pack_id, obj_offset, obj_size)` per chunk. This is exactly the "pack many small
  files into larger chunks" optimization the project brief calls for — it is already
  designed, and borge inherits it rather than inventing it. Remaining work becomes
  *tuning and restore-side batching*, which is a much better place to start.
- Borg 2's crypto is modern (AES-256-OCB / ChaCha20-Poly1305 AEAD, argon2id KDF,
  BLAKE3 / HMAC-SHA-256 id hashing) and maps cleanly onto Go's stdlib plus
  `golang.org/x/crypto`. Borg 1's AES-CTR + HMAC-SHA-256 envelope with the manual
  nonce ledger does not.
- The port source of truth is the checkout that exists on this machine.

**Consequence for testing:** interop testing needs a borg 2 interpreter, which is not
`/usr/bin/borg` (1.2.8). Stage 0 builds one in a pinned virtualenv. The existing
borg 1.2 repositories on `/media/renes/HD2` are **not** readable by borge and are not
in scope; they stay accessible via the system borg.

**Deferred, not rejected:** a read-only borg 1.x repository reader in borge. Recorded
as a post-1.0 item in §9. It is the sort of thing that is cheap once the borge object
model exists and ruinous if attempted first.

### 0.2 Naming

The executable, the Go module and the GitHub project are all `borge`.
`github.com/renesugar/borge` is the module path (project to be created later; the
module path is set now so imports never have to be rewritten).

The name `borg` appears in borge only as factual provenance and in format
identifiers that are part of the on-disk format and therefore cannot change
(`BORG_OBJ` magic, `borg-session-key-*` KDF domains, `borg-repoobj-mac-*` MAC
domains). Environment variables are a judgment call and are treated in §0.5.

### 0.3 License

Apache-2.0 for borge as a whole, with the upstream BSD notices preserved. Full
analysis in [`LICENSING.md`](LICENSING.md). Settled; not revisited per stage.

### 0.4 Language and dependency policy

- **Pure Go, no cgo, for everything on the default build.** A cgo dependency
  forfeits the single-static-binary property that is reason #1 for the port. If a hot
  path genuinely needs C, it goes behind a build tag with a pure-Go fallback, and only
  after Stage 9 has measured that the Go version is the bottleneck. The Cython
  modules are ported to Go first, unconditionally.
- **Prefer the Go standard library.** `compress/zlib`, `compress/flate`, `crypto/*`,
  `hash/crc32`, `io/fs`, `os`, `syscall`.
- **Use an external package when the stdlib has no equivalent**, choosing the
  best-maintained pure-Go option:

  | Need | Package | Note |
  | --- | --- | --- |
  | zstd | `github.com/klauspost/compress/zstd` | no stdlib zstd; ~6.7x ratio at `SpeedBestCompression`, 13ms decompress |
  | LZ4 | `github.com/pierrec/lz4/v4` | borg's default `LZ4_COMPRESSOR`; must be LZ4 **block** format, not frame |
  | LZMA | `github.com/ulikunitz/xz/lzma` | borg compression id `0x02`; interop-only, cold path |
  | zlib | `compress/zlib` (stdlib) | ids `0x05` and legacy `0x08` |
  | msgpack | `github.com/vmihailenco/msgpack/v5` | evaluate vs. hand-written codec, see Stage 1.1 |
  | BLAKE3 | `lukechampine.com/blake3` | pure Go, SIMD-accelerated |
  | argon2id | `golang.org/x/crypto/argon2` | |
  | ChaCha20-Poly1305 | `golang.org/x/crypto/chacha20poly1305` | |
  | AES-OCB | *no good pure-Go option — see Stage 1.3* | the one real risk in the dependency set |
  | CLI | `github.com/spf13/cobra` + `pflag` | borg uses argparse subcommands; cobra is the closest fit |
  | xattr | `github.com/pkg/xattr` | pure Go via syscalls |
  | SFTP backend | `github.com/pkg/sftp` + `golang.org/x/crypto/ssh` | Stage 8 |
  | S3 backend | `github.com/minio/minio-go/v7` | Stage 8; same choice restic made |
  | terminal/progress | `golang.org/x/term` | |

- **Custom borg algorithms are ported, not replaced.** Buzhash, buzhash64, FastCDC,
  the AES-based chunkers and the `borghash` hash table are format-defining or
  performance-critical. A drop-in substitute (e.g. `github.com/restic/chunker`, which
  is a *different* Rabin chunker with different parameters) would produce different
  chunk boundaries and destroy dedup compatibility. Substitutes get considered only
  in Stage 10, when the format is allowed to change.
- **Every dependency addition is a commit of its own** with a one-line rationale in
  the message, so the dependency set stays auditable.

### 0.5 Compatibility surface: what "the same as borg" means

Three surfaces, and they are *not* equally binding:

| Surface | Binding? | Rule |
| --- | --- | --- |
| On-disk format | **Hard** | Byte-for-byte until Stage 10. Interop gate enforces it. |
| CLI (command names, options, output) | **Soft** | Match borg where it costs nothing; diverge where borg is awkward, and record every divergence in `docs/CLI_DIFFERENCES.md`. |
| Environment variables | **Soft, dual-read** | borge reads `BORGE_*` first, then falls back to `BORG_*`. Both are documented. This avoids surprising a user who has `BORG_PASSPHRASE` exported, without squatting on borg's namespace. |

Cache and config *outside* the repository (`~/.cache/borg`, `~/.config/borg`) are
**not** shared. borge uses `~/.cache/borge` and `~/.config/borge`. Sharing them would
let a borge bug corrupt a working borg installation, and the interop tests need the
two tools to be independently reproducible anyway. The security/known-hosts file and
the chunks cache are rebuilt from the repository when absent, so nothing is lost.

### 0.6 Non-goals for 1.0

Explicitly out of scope, to keep the port finishable:

- borg 1.x repository read support (see §9)
- `borg transfer` from borg 1.x repos (depends on the above)
- FUSE mount (`borg mount`) — deferred to §9; a large platform-specific subsystem
- the `cockpit` TUI
- the WebDAV server (`borg webdav`)
- `borg serve` over ssh in the *first* pass; the REST/stdio serve path lands in Stage 8

---

## 1. Target architecture

Borg's module layout is a known problem upstream — `archive.py` holds 12 classes
(borg #10017), `helpers/__init__.py` is an import shim that makes the dependency graph
cyclic (borg #10016). The port fixes both by construction, since Go forbids import
cycles: **if a proposed package layout compiles, the layering is acyclic.** That is the
single biggest maintainability win available here and it is free.

```
borge/
  cmd/borge/                 main(); nothing but wiring
  internal/
    berr/                    error types, exit codes            (borg helpers/errors.py)
    logging/                 leveled logger                     (borg logger.py)
    msgpackx/                msgpack codec + StableDict         (borg helpers/msgpack.py)
    shellpattern/            fnmatch-alikes                     (borg helpers/shellpattern.py)
    patterns/                include/exclude engine             (borg patterns.py)
    timex/  fsx/  procx/     small helpers, split by concern    (borg helpers/{time,fs,process}.py)
    formatter/               --format placeholders              (borg helpers/parseformat.py, split per #10016)
    location/                repo URL parsing                   (borg helpers/parseformat.py)
    progress/                progress indicators                (borg helpers/progress.py)

    compress/                none|lz4|lzma|zstd|zlib|obfuscate|auto   (compress.pyx)
    crypto/
      aead/                  AES-OCB, ChaCha20-Poly1305, HMAC   (crypto/low_level.pyx)
      key/                   key types, KDF, keyfile/repokey    (crypto/key.py)
      keymgr/                key export/import/change-passphrase (crypto/keymanager.py)
      integrity/             file integrity checksums           (crypto/file_integrity.py)
    chunker/                 fastcdc, buzhash, buzhash64, rabin-aes,
                             goldilocks-aes, toeplitz-aes, fixed, reader   (chunkers/*.pyx)
    hashindex/               ChunkIndex + borghash hash table   (hashindex.pyx + borghash)
    item/                    Item, ArchiveItem, ManifestItem, Key, EncryptedKey  (item.pyx)

    store/                   borgstore port
      store.go               namespaces, nesting levels, permissions
      backend/               posixfs, rest, sftp, s3, rclone
      cache/                 writethrough pack cache
    repoobj/                 BORG_OBJ envelope format           (repoobj.py)
    repository/
      repository.go          open/create/config/version         (repository.py, split per #10017)
      pack_writer.go         PackWriter                         (repository.py)
      pack_reader.go         PackReader                         (repository.py)
      index.go               index/<HASH> persistence + merge   (repository.py)
      lock.go                store-based locking                (storelocking.py)
    manifest/                manifest + archive directory       (manifest.py)
    cache/                   chunks cache, files cache          (cache.py)
    archive/
      archive.go             Archive (read side)                (archive.py, split per #10017)
      builder.go             archive creation                   (archive.py)
      extract.go             extraction                         (archive.py)
      stats.go  diff.go  recreate.go  checker.go                (archive.py)
      itemstream.go          item metadata stream chunking      (archive.py)
    platform/                per-OS: xattr, ACL, bsdflags, sync (platform/*.pyx)
    security/                repo identity / location checks    (security.py)

    cli/                     one file per subcommand            (archiver/*.py)
  docs/
  licenses/
  tests/
    interop/                 the Stage 7 harness
    bench/                   the Stage 9 harness
```

Layering rule, enforced by a CI import-graph check: `cli` → `archive` → `manifest` →
`repository` → `store`; everything may use the leaf helper packages; **nothing under
`internal/` may import `cli`**. Helper packages never import domain packages. This is
borg #10016's "make imports point downward again", made mechanical.

---

## 2. Working method

The brief anticipates that usage limits will interrupt work. Everything below is
organised so that an interruption is cheap:

1. **One task at a time.** A task is finished when it builds, its tests pass, and it
   is committed. The tree is never left broken across a stop.
2. **Git from the first commit.** Plan, docs and code in one repository at
   `/home/renes/projects/borge`. Branch per stage (`stage-3-repository`), squash-merge
   to `main` at the stage gate with the evidence bundle named in the merge commit.
3. **Every stage has an explicit gate.** The gate is a command that either passes or
   fails. No stage is "done" on inspection.
4. **Evidence bundle per stage.** On passing a gate:
   ```
   tests/evidence/mkbundle.sh <stage-id>
   # -> /home/renes/evidence/borge/borge-<stage-id>-<UTC timestamp>.zip
   ```
   The bundle contains: `git rev-parse HEAD`, `git status --porcelain`, the full test
   log, `go test ./... -json` output, benchmark JSON where applicable, the borg-2 venv
   version pin, and a `MANIFEST.txt` listing every file with its sha256. It is copied
   to `/home/renes/evidence/borge/` and its name is recorded in the stage's row in
   §8 below.
5. **Ask before advancing a stage.** After a stage gate passes, stop and ask before
   starting the next one.

### 2.1 Porting discipline, per module

For each borg module, in order:

1. Read the Python/Cython source and its tests in `src/borg/testsuite/`.
2. Write the Go package with an SPDX + provenance header (see `LICENSING.md` §5).
3. **Port the upstream tests first**, then the implementation. borg's test suite is
   the specification; a ported test that fails is a port bug, not a test bug, until
   proven otherwise.
4. Add **differential tests** against the real borg wherever a byte-level answer
   exists: for chunkers, compressors, crypto, msgpack and the object envelope, drive
   the borg-2 venv over stdin/stdout and compare bytes. These catch the errors that
   unit tests written from the same misunderstanding will not.
5. Record any intentional behaviour difference in `docs/DIVERGENCES.md`.

---

## 3. Stage 0 — foundation

**Goal:** a repository that builds, tests, lints, and can run borg 2 for comparison.

| # | Task |
| --- | --- |
| 0.1 | `git init`; Apache-2.0 `LICENSE`, `NOTICE`, `README.md` stating the borg provenance; `licenses/borg/{LICENSE,AUTHORS}` and `licenses/restic/LICENSE` copied verbatim. |
| 0.2 | `docs/LICENSING.md` — the compatibility analysis. |
| 0.3 | This plan. |
| 0.4 | `go mod init github.com/renesugar/borge`; Go 1.26; `cmd/borge` printing version + `borge --license`. |
| 0.5 | Makefile: `build test lint fmt vet cover bench evidence`. `golangci-lint` config. |
| 0.6 | CI-equivalent local script: build, `go vet`, `go test ./...`, SPDX header check, import-layering check. |
| 0.7 | **Pinned borg 2 venv.** `tests/borg2/setup.sh` creates `.venv-borg2` from the pinned checkout, records `pip freeze` to `tests/borg2/requirements.lock`, exposes `tests/borg2/borg2` as a wrapper. Needs `borghash`, `borgstore[rest,blake3]~=0.6.0`, `msgpack`, `argon2-cffi`, `pyzstd`/`backports-zstd`. |
| 0.8 | **License check on `borghash` and `borgstore`** (LICENSING.md §6). Record findings in `licenses/`. Blocking for Stage 1.6 and Stage 2. **Resolved 2026-08-16: both are BSD-3-Clause (borghash 0.2.0, borgstore 0.6.1, © Thomas Waldmann) — porting permitted.** |
| 0.9 | `tests/evidence/mkbundle.sh`. |
| 0.10 | Format reference doc `docs/FORMAT.md`: transcribe the repo layout, object envelope, key types and compression ids from the pinned borg source, with file:line citations. This is the artifact every later stage checks itself against. |

**Gate:** `make check` green; `tests/borg2/borg2 --version` prints a 2.x version;
`tests/borg2/borg2 repo-create --encryption=none` on a scratch path produces a
repository borge's Stage 2 tests can later read.

---

## 4. Stage 1 — primitives

Leaf packages, no dependencies on each other beyond the obvious. Each is
independently testable and independently gated, so this stage survives interruption
well. **Every one gets a differential test against the borg-2 venv.**

### 1.1 `msgpackx` — msgpack codec
borg pins `msgpack >=1.0.3,<=1.2.1` and relies on specific behaviour: `use_bin_type`,
`raw=False`, `strict_map_key=False`, and a `StableDict` whose keys serialise in sorted
order (chunk ids are computed over packed metadata, so key order is format-visible).
Evaluate `vmihailenco/msgpack/v5` against a hand-written encoder; the deciding
question is whether map key ordering and the bin/str distinction can be controlled
exactly. **A hand-written codec is an acceptable and possibly preferable outcome** —
the subset borg uses is small.
*Gate:* round-trip every `StableDict`/`Item` fixture extracted from borg's test suite,
byte-identical in both directions.

> **Done 2026-08-16** (`internal/msgpackx`). Hand-written codec, no dependency.
> 84 fixtures generated by borg's own msgpack wrapper decode and re-encode
> byte-identically; borge's output round-trips through borg's unpacker unchanged at
> sizes up to 100 kB; every prefix and single-byte corruption of every fixture is
> rejected without panicking; two fuzz targets clean.
>
> Two findings changed later stages:
>
> - **Surrogate escapes are the identity mapping in Go.** The packed wire form of a
>   Python surrogate-escaped `str` *is* the original bytes, so a Go `string` maps onto
>   it directly with no transformation. Stage 1.5's "reproduce Python's mapping
>   exactly" is therefore a non-problem — but see the next point.
> - **Ordering is where the surrogate interpretation still matters.** Python sorts by
>   code point, so an undecodable byte (U+DC80–U+DCFF) sorts *below* an astral
>   character whose lead byte sorts *above* it. `comparePyStr` reproduces Python's
>   order; sorting by raw bytes would give a different chunk id for identical content.
>   Any later package that sorts keys must use it.

### 1.2 `compress` — compression
Ids are format-visible and fixed: `none=0x00`, `lz4=0x01`, `lzma=0x02`, `zstd=0x03`,
`obfuscate=0x04`, `zlib=0x05`, `zlib_legacy=0x08` (`compress.pyx`). Port
`DecidingCompressor` (falls back to `none` when compression does not shrink the
chunk), `Auto` (lz4 probe, then the real compressor) and `ObfuscateSize` (adds
padding; note `psize` vs `csize` in the object metadata).
The one trap: **borg's LZ4 is the raw block format**, not the frame format —
`pierrec/lz4/v4`'s `CompressBlock`/`UncompressBlock`, not its `Reader`/`Writer`.
*Gate:* for each algorithm and level, borge decompresses borg's output and borg
decompresses borge's output, over a corpus drawn from the recipedb test data.

> **Done 2026-08-16** (`internal/compress`). Gate green both directions: 22 specs ×
> 20 corpus entries, including 8 real files from `recipe_vault` and `recipe_joplin`.
>
> Scope clarification worth carrying forward: **compressed bytes need not match borg's**
> and this package does not try to make them. Chunk ids are computed over plaintext and
> pack names over the pack's own contents, so only the ids, the metadata fields and
> two-way decompressibility are format-visible. That is what made using Go's zlib and
> klauspost's zstd safe rather than a risk.
>
> Two findings, both from the differential test, both in `docs/DIVERGENCES.md`:
>
> - **`--compression auto,...` records no plaintext size.** borg's `Auto.compress`
>   copies only `ctype`/`clevel`/`csize` out of the inner compressor's metadata. borge's
>   decompressor must therefore work without a size — this blocked reading anything borg
>   wrote with `auto`, a commonly recommended setting. Stage 5 and 6 must not assume
>   `size` is present in object metadata.
> - **`Auto` crashes on empty input** (`ZeroDivisionError`, every inner compressor).
>   Latent upstream bug; borge guards it. Worth reporting to borg.

### 1.3 `crypto/aead` — low-level crypto  ⚠️ **highest-risk item in the port**
Needed: HMAC-SHA-256 (stdlib), BLAKE3 (`lukechampine.com/blake3`), argon2id
(`x/crypto/argon2`), ChaCha20-Poly1305 (`x/crypto/chacha20poly1305`), and
**AES-256-OCB**.

AES-OCB is the problem. Go's stdlib has no OCB; `x/crypto` has no OCB; the pure-Go
implementations on offer are thin and unaudited. borg's `AESOCBKey` (`TYPE=0x10`) is
one of its default modes, so borge cannot skip it and still claim interop. Approach:

1. Implement RFC 7253 OCB3 over `crypto/aes` in `crypto/aead/ocb`, ported from the
   reference and validated against **the RFC 7253 test vectors** plus borg's own
   `low_level.pyx` vectors. Write it to be reviewable — this is the code where a
   subtle bug is silent and catastrophic.
2. Constant-time tag comparison via `crypto/subtle`, always.
3. Get it reviewed independently before Stage 7. Flag it in the README as the
   component to scrutinise.
4. Fallback if the risk proves unacceptable: support ChaCha20-Poly1305 (`TYPE=0x20`)
   and the unencrypted modes for writing, read-only for AES-OCB — but this is a
   visible product limitation and needs a decision, not a silent default.

Also port the session-key derivation: borg builds a session key per
`(chunk_id, session)` with domain `borg-session-key-<CIPHERSUITE>` and a
`1+1+6+24`-byte header (`key.py:1434-1440`); the nonce/session-id construction is
format-visible and must match exactly.
*Gate:* RFC test vectors pass; every AEAD blob borg writes decrypts under borge and
vice versa, for each key type.

> **Done 2026-08-16** (`internal/crypto`, `internal/crypto/ocb`). Gate green.
> **The AES-OCB risk is now bounded, and the fallback in point 4 above is not needed.**
>
> OCB3 was written from scratch against RFC 7253 and checks out against:
>
> - **All 16 primary RFC 7253 vectors.**
> - **All 9 RFC 7253 appendix A iterative vectors** — AES-128/192/256 × tag lengths
>   128/96/64. Each chains 384 encryptions covering every plaintext and
>   associated-data length from 0 to 127 bytes, so one 16-byte comparison pins down
>   every partial-block case. AES-256 with a 128-bit tag is borg's own configuration.
> - **OpenSSL, via borg**, across 2 suites × 5 header/aad-offset combinations × 17
>   payload sizes — and not merely interoperably: the envelopes are **byte-identical**.
>   Unlike compression, that is achievable here because both AEADs are deterministic
>   given (key, iv, plaintext, aad), so any difference at all would mean a real
>   disagreement about the format.
> - Tamper tests in both directions, an opaque-error test (a distinguishable error is a
>   decryption oracle), and three fuzz targets.
>
> Independent review is still worth having before Stage 7, but it is now a
> double-check rather than the only thing standing between the port and a silent
> crypto bug.
>
> One trap worth recording: RFC 7253 appendix A's key is **not** all zeros —
> `K = zeros(KEYLEN-8) || num2str(TAGLEN,8)`, so the last key byte is the tag length in
> bits. Getting that wrong makes all nine vectors fail while the implementation is
> perfectly correct, which is a good way to waste an afternoon chasing a phantom bug.

### 1.4 `chunker` — content-defined chunking
Port, in this order: `fastcdc` (borg 2's **default**, `CHUNKER_PARAMS = FASTCDC_PARAMS`),
`buzhash` (borg 1 compat, still selectable), `buzhash64`, `fixed`, then the AES-based
family (`rabin-aes`, `goldilocks-aes`, `toeplitz-aes`) which exist upstream mainly as
experiments. Also port `chunkers/reader.pyx` — the sparse-file-aware reader that emits
`CH_DATA`/`CH_ALLOC`/`CH_HOLE` runs; getting holes wrong silently changes what gets
stored.
Chunk boundaries **are** the dedup format. Note the C helpers upstream
(`fastcdc_impl.c`, `buzhash64_impl.c`): per policy these are ported to Go first, and
only revisited in Stage 9 if measurement says so.
*Gate:* for a fixed seed and params, borge's boundary offsets on the recipedb corpus
are identical to borg's, verified by dumping boundaries from both. Any single
differing offset is a hard failure.

> **Done 2026-08-16** (`internal/chunker`). Gate green: **77 boundary cases**
> byte-exact across fastcdc (nc 0/2/4), buzhash64 (nc 0/2) and buzhash (two seeds),
> over 11 corpus inputs including 4 MiB of real `deutsche-rezepte` data.
>
> The keyed tables match first (`TestGearTableMatchesBorg`), which means the CSPRNG,
> its rejection sampling and its Fisher-Yates shuffle all reproduce borg exactly — if
> the table were wrong, every boundary would differ and the comparison would say
> nothing useful.
>
> **Finding: EOF must be flagged only on a zero-length read, never on a short one.**
> borg's `fill()` sets `eof` only when the reader returns 0 bytes. Using Go's
> `io.ReadFull` semantics (short read ⇒ EOF) made the *windowed* chunkers emit
> everything buffered one round early, merging the last few chunks of every file —
> boundaries identical everywhere else, then a silent divergence in the tail. fastcdc
> was unaffected, so a test on the default chunker alone would have missed it.
> `TestShortReadsDoNotChangeBoundaries` now covers it without needing the venv.
>
> **Scope:** the three AES-based chunkers (`rabin-aes`, `goldilocks-aes`,
> `toeplitz-aes`) are **not** ported. They are upstream experiments, none is a default,
> and each needs its own PHTE kernel. `New` rejects them with an explanation rather
> than silently substituting another algorithm. Revisit only if a corpus turns up that
> uses one.
>
> Also deferred: the sparse-file reader (`chunkers/reader.pyx`). Boundaries do not
> depend on it — holes read as zeros either way — but `CH_HOLE` versus `CH_ALLOC`
> classification does. It belongs with the file-walking code in **stage 6**, where the
> `fmap`/sparsemap it needs actually exists.
>
> Baseline throughput on this machine, for stage 9 to improve on: fastcdc 186 MB/s,
> buzhash64 134 MB/s (single-threaded, pure Go, no SIMD kernel).

### 1.5 `item` — item and metadata structs
`item.pyx`'s `PropDict` machinery becomes plain Go structs with explicit msgpack
codecs. The subtlety is **surrogate-escaped str**: borg stores POSIX paths that are
not valid UTF-8 using Python's `surrogateescape`. Go strings are byte sequences, which
is actually the *easier* model — but the encode/decode must reproduce Python's
mapping exactly or paths with invalid UTF-8 will not round-trip. Also port timestamp
encoding (int nanoseconds ⇄ msgpack ext) and `hlid` hardlink identity.
*Gate:* every item fixture from borg's test suite round-trips byte-identically;
a fuzz test over arbitrary byte-sequence paths round-trips.

### 1.6 `hashindex` — ChunkIndex
Port `borghash.HashTableNT` (external package — **check its license first, task 0.8**)
plus the `ChunkIndex` wrapper. Entry layout is fixed:
`key256 -> (flags:u32, size:u32, pack_id:[32]byte, obj_offset:u32, obj_size:u32)`.
Flags: `F_USED=1`, `F_COMPRESS=2`, `F_PENDING=4`, system flag `F_NEW=1<<24`, user mask
`0x00ffffff`. The persisted file format must match byte-for-byte, since borg and borge
must both be able to read `index/<HASH>`.
This is the data structure the whole "many small files" problem runs through: 1.6M
chunks in the reference workload. A Go map is not acceptable here — the open-addressed
table exists precisely to control memory and locality. Benchmark it in Stage 9.
*Gate:* borge reads an index written by borg and vice versa; property test against a
reference `map[[32]byte]entry` over a million randomized operations.

**Stage gate:** every 1.x sub-gate green; `go test ./internal/...` clean; a
differential test binary that exercises all five against the borg-2 venv passes.

---

## 5. Stage 2 — `store`: the borgstore port

Port `borgstore` (~0.6): the object store under the repository.

- Namespaces and nesting from `repository.py:684-692`:
  `archives/`, `cache/`, `config/`, `index/`, `keys/`, `locks/` at `levels: [0]`
  (flat), `packs/` at `levels: [1]` (one level of hex-prefix subdirectories).
- The `posixfs` backend: `store`, `load` (with `offset`/`size` range reads — the pack
  reader depends on these), `info`, `delete`, `list`, `move`; temp-file + rename for
  atomicity.
- Soft-delete/undelete semantics (borg's `undelete` command depends on them).
- The permissions model (`borg_permissions()`: `all`, `no-delete`, `write-only`,
  `read-only` mapped to per-namespace `lrwWD` strings).
- The **writethrough pack cache** (`BORG_STORE_CACHE`, `BORG_PACK_CACHE_SIZE`): on a
  miss, fetch the whole pack, cache it, serve subsequent object reads from cache.
  This is load-bearing for restore performance and must not be deferred.

Remote backends (`sftp`, `rest`, `s3`, `rclone`) are **deferred to Stage 8**. Local
`posixfs` is enough for everything through the interop gate, and it keeps this stage
small.

**Gate:** borge's store lists, reads and range-reads a repository created by the
borg-2 venv, and borg reads a store directory borge wrote; nesting and naming
byte-identical. A `GoogleDrive`-backed run (rclone mount, `/home/renes/GoogleDrive`)
exercises the high-latency path even though the network backends are not ported yet —
it is a filesystem, and it is where naive per-object I/O will show up first.

---

## 6. Stage 3 — `repoobj` and `repository`

### 3.1 `repoobj`
The `BORG_OBJ` envelope, fully specified in `repoobj.py`:

```
header (49 bytes, little-endian):  magic[8]="BORG_OBJ" | version:u8 | chunk_id[32] | meta_size:u32 | data_size:u32
body:                              meta_encrypted[meta_size] | data_encrypted[data_size]
```

`version` is `0x02` (`OBJ_VERSION_HEADER_AAD`) for new objects; `0x01` must still be
*readable*. For `0x02` the AEAD AAD is `magic|version|chunk_id` (41 bytes) plus a slot
tag — `b"M"` for the metadata slot, `b"D"` for the data slot — which binds each
ciphertext to its slot. Getting the AAD wrong produces objects borg rejects, so this
is tested first and separately.
Also port the `BORG_ASSERT_ID` policy (`ASSERT_ID_PLACES`, defaults
`repair,transfer,rechunk`): it is a real hot-path performance decision, not a detail.

### 3.2 `repository`
- Open/create; repository version 4; config in `config/`.
- `PackWriter`: buffer `(chunk_id, cdata)` pairs; on `max_count`/`max_size`, join the
  in-flight store, hand the current pack to a background writer, apply the previous
  pack's results to the `ChunkIndex`. In Go this is a goroutine + channel, but the
  **invariant must be preserved exactly**: the `ChunkIndex` is touched only by the
  calling goroutine; the writer goroutine touches only the store. Getting this wrong
  gives a data race that corrupts repositories under load and reproduces rarely.
  Also port `F_PENDING` handling and the failure paths (`_drop_buffered`,
  `_apply_outcome`) — a failed pack store must remove its index entries.
- `PackReader`: `iter_headers()` walks fixed 49-byte headers; a truncated trailing
  header is clean EOF, a bad magic or an object extending past the pack is
  `IntegrityError`. Port `check_pack_objects` overlap detection too.
- `index/<HASH>` persistence, incremental index writes and merge/compaction.
- `storelocking` — lock objects under `locks/`, shared and exclusive, with the
  stale-lock rules.

**Gate:** borge writes packs the borg-2 venv reads and indexes; borge rebuilds a chunk
index from borg-written packs that matches borg's own; `borg check` (venv) passes on a
borge-written repository containing raw objects.

---

## 7. Stage 4 — keys

Port `crypto/key.py` and `crypto/keymanager.py`.

Key types (`constants.py:238-268`) — new crypto only; the `0x00`–`0x07` legacy types
are borg 1.x and out of scope per §0.6:

| Type | Mode | id hash | envelope |
| --- | --- | --- | --- |
| `0x10` | `aes256-ocb` | HMAC-SHA-256 | AEAD |
| `0x20` | `chacha20-poly1305` | HMAC-SHA-256 | AEAD |
| `0x30` | `blake3-aes256-ocb` | BLAKE3 | AEAD |
| `0x40` | `blake3-chacha20-poly1305` | BLAKE3 | AEAD |
| `0x50` | *reserved, dropped* | — | must be rejected, never reused |
| `0x60` | `authenticated-sha256` | HMAC-SHA-256 | MAC tag, no encryption |
| `0x70` | `authenticated-blake3` | BLAKE3 | MAC tag |
| `0x80` | `none-sha256` | SHA-256 | unkeyed checksum |
| `0x90` | `none-blake3` | BLAKE3 | unkeyed checksum |

Note the layering that borg arrived at: the type byte identifies **only the
ciphersuite**; keyfile-vs-repokey storage is *not* encoded in it. Reproduce that —
it is easy to accidentally re-couple them.

Plus: `FlexiKey` (keyfile in `~/.config/borge/keys/` vs repokey in `keys/` in the
store), argon2id passphrase KDF with borg's `ARGON2_ARGS`, the `EncryptedKey` blob
format, `borge key export/import/change-passphrase`, and the paperkey HTML export.

**Start with `none-*` and `authenticated-*`.** They exercise the whole object and
archive path with no crypto risk, which means Stages 5–6 can be built and interop-
tested before Stage 1.3's OCB work is trusted. The AEAD modes join at the Stage 7 gate.

**Gate:** borge unlocks a repokey and a keyfile repository created by borg (all
passphrase-protected modes), and borg unlocks borge-created ones; `borge key export` /
`borg key import` cross-check in both directions.

---

## 8. Stage 5 — read path: `manifest`, `archive`, extraction

First user-visible functionality. Read-only, so it cannot damage a repository — which
is exactly why it comes before `create`.

- `manifest`: `MANIFEST_ID = 32 zero bytes`; `ROBJ_MANIFEST`; version 2; the
  `item_keys` in `config`; the borg-2 rule that `manifest["archives"]` is always empty
  because borgstore's `archives/` namespace *is* the archive directory.
- `archive` (read): `ArchiveItem` v2 — `item_ptrs` (a list of chunk ids of blocks of
  chunk ids of the item metadata stream), `command_line`, `start`/`end`, `tags`.
  Object types: `ROBJ_ARCHIVE_META` `"A"`, `ROBJ_ARCHIVE_CHUNKIDS` `"C"`,
  `ROBJ_ARCHIVE_STREAM` `"S"`, `ROBJ_FILE_STREAM` `"F"`.
- The item metadata stream unpacker, including `RobustUnpacker` (needed by `check`).
- `patterns` + `shellpattern`: `fm:`/`sh:`/`re:`/`pp:`/`pf:` prefixes, `--exclude`,
  `--exclude-from`, `--pattern`, `--patterns-from`. Easy to get subtly wrong and it
  silently changes what gets restored.
- `platform`: restoring mode/uid/gid/times/xattrs/ACLs/bsdflags on Linux first.
- Commands: `repo-list`, `list`, `info`, `repo-info`, `extract`, `export-tar`, `diff`.

**Gate:** for every archive in a borg-created repository over each of the four test
corpora (§10), `borge extract` produces a tree that compares equal to `borg extract`'s
under a strict comparator (path set, content sha256, mode, uid/gid, mtime to ns,
symlink targets, hardlink groups, xattrs, sparse layout). Divergences are enumerated,
not tolerated.

---

## 9. Stage 6 — write path: `create`

- `cache`: the chunks cache and the **files cache** (inode/size/mtime → chunk list),
  which is what makes an incremental borg run fast. Include borg's mtime-granularity
  and `ctime`-vs-`mtime` rules; these decide correctness of incremental backups.
- `archive/builder.go`: walk, chunk, dedup, compress, encrypt, pack, write; item
  metadata stream; `item_ptrs`; stats.
- Hardlink handling (`hlid`), sparse files, special files, `--read-special`,
  `--one-file-system`, `--numeric-ids`, `--files-cache` modes.
- Commands: `repo-create`, `create`, `delete`, `rename`, `tag`, `undelete`.
- Concurrency: borg's `create` is largely serial. borge should pipeline
  read → chunk → compress+encrypt → pack, since compression and encryption are
  CPU-bound and parallelise cleanly. **But not in this stage** — build it serial and
  correct, then parallelise in Stage 9 with the interop gate already passing to catch
  regressions.

**Gate:** `borge create` then `borg check --verify-data` (venv) passes; `borg extract`
of a borge-created archive matches the source tree under the strict comparator.

---

## 10. Stage 7 — the interoperability gate  ⭐

**This is the gate the whole project turns on.** Nothing in Stage 10 starts until it
is green. It is automated in `tests/interop/` and re-run on every commit thereafter.

The matrix, for each corpus × each key mode × each compression setting:

| # | Write with | Read/verify with |
| --- | --- | --- |
| 1 | borg | borge extract |
| 2 | borge | borg extract |
| 3 | borg | borge check --verify-data |
| 4 | borge | borg check --verify-data |
| 5 | borg, then borge create (2nd archive, same repo) | borg extract both, borg check |
| 6 | borge, then borg create (2nd archive, same repo) | borge extract both, borge check |
| 7 | borg create, borge delete + compact | borg check |
| 8 | borge create, borg delete + compact | borge check |

Rows 5–8 matter more than 1–4: they are where a shared chunk index, shared packs and
a shared archive directory get exercised, and where a format misunderstanding that
rows 1–4 miss will actually bite.

**Corpora:**

| Corpus | Path | Character |
| --- | --- | --- |
| Joplin archive | `/home/renes/Documents/Joplin Archive/JoplinExport_2026_07_18/` | very large Joplin RAW dir **with** resources/attachments |
| Joplin recipes | `/home/renes/projects/recipedb/recipe_joplin` | very large Joplin RAW dir, no attachments |
| Obsidian vault | `/home/renes/projects/recipedb/recipe_vault` | same data as above, different layout — good dedup signal |
| recipedb (whole) | `/home/renes/projects/recipedb` | 1.62M files, 2.85 GB, markdown + ZIPs; the main perf corpus |
| pathological dir | `.../recipe_vault/www-wedesoft-de/downloads/deutsche-rezepte` | **118,866 files in one directory** — the target of the whole exercise |
| Google Drive | `/home/renes/GoogleDrive` | rclone mount; high-latency, exercises I/O patterns |
| synthetic edge cases | generated | invalid-UTF-8 paths, sparse files, hardlinks, xattrs, ACLs, >4 GiB files, 0-byte files, deep nesting, unicode normalization pairs |

The synthetic corpus is not optional. The real corpora will not contain the cases
that break a port.

**Comparator** (`tests/interop/compare.go`): path set, content sha256, mode, uid/gid,
mtime/atime/ctime to nanoseconds, symlink targets, hardlink grouping, xattrs, ACLs,
sparse-region layout. Reports every difference; exits non-zero on any.

**Gate:** all 8 rows green across all corpora and key modes. Evidence bundle includes
the full comparator output.

---

## 11. Stage 8 — remaining commands and backends

Everything needed for feature parity, once correctness is established.

- `check` (+ `--repair`), `compact`, `prune`, `recreate`, `repo-compress`,
  `repo-space`, `analyze`, `benchmark`, `find`, `debug *`, `version`, `lock`/`break-lock`,
  `import-tar`, shell completions.
- Remote store backends: `sftp`, `rest` (+ `borge serve --rest`), `s3`, `rclone`.
- `--progress`, `--stats`, `--json`, `--log-json` output shapes.
- Platform coverage: macOS and FreeBSD `platform/` implementations.

**Gate:** `borg check --repair` and `borge check --repair` produce equivalent
repairs on a corpus of deliberately corrupted repositories (bit flips in packs,
truncated packs, missing index, missing archive object, stale lock).

---

## 12. Stage 9 — performance baseline

**Only meaningful once Stage 7 is green.** Optimising an incorrect port wastes the
optimisation.

`tests/bench/` runs borg and borge over the same corpus on the same hardware, cold and
warm cache, and emits JSON: wall time, CPU time, peak RSS, repository size, chunk
count, syscall counts (via `strace -c`), and time-to-first-byte on restore.

Reference point from the brief, borg 1.2.8 on `/home/renes/projects/recipedb`:
1,621,034 files, 2.85 GB → 2.23 GB, 1,623,610 unique chunks, **19m44s**. A borg 2
baseline must be measured fresh; the 1.2 number is context, not a target.

Scenarios: initial create; no-change re-create (files-cache path); create after 1%
churn; full extract; single-file extract from a large archive; `check --verify-data`;
the `deutsche-rezepte` directory alone, create and extract; all of the above on the
GoogleDrive mount.

Then, in order:

1. **Profile before changing anything** (`pprof`, CPU + alloc).
2. Pipeline `create` (read → chunk → compress/encrypt → pack) with bounded queues.
3. Parallelise `extract` similarly.
4. Tune `PackWriter` `max_count`/`max_size` and the pack cache size against the
   pathological directory.
5. Only if a Go hot path is measurably the bottleneck **and** a C implementation is
   measurably better on the same corpus, introduce a cgo-gated implementation with a
   pure-Go fallback — per §0.4, and with the benchmark JSON in the evidence bundle as
   justification. The Cython modules were `compress`, `hashindex`, `item`,
   `crypto/low_level` and the chunkers; those are the candidates.
6. Re-run Stage 7 after every change. Performance work that breaks interop is
   reverted, not patched.

**Gate:** borge ≥ borg on every scenario, with the JSON to show it. Regressions are
listed with an explanation, not hidden.

---

## 13. Stage 10 — format and indexing changes

Only after Stages 7 and 9. Everything here **breaks format compatibility**, so it goes
behind an explicit repository version bump and a documented migration.

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

**Gate:** a migration path exists and is tested (borge reads the old format, converts,
verifies); the change is justified by benchmark JSON in the evidence bundle.

---

## 14. Stage tracker

| Stage | Description | State | Evidence bundle |
| --- | --- | --- | --- |
| 0 | Foundation, licensing, borg-2 venv, format reference | **done** 2026-08-16 | `borge-stage-0-20260816T163704Z.zip` |
| 1 | Primitives: msgpack, compress, crypto, chunker, item, hashindex | 1.1-1.4 done, 1.5-1.6 to go | per-substage bundles |
| 2 | `store` (borgstore port, posixfs) | not started | — |
| 3 | `repoobj` + `repository` + packs + locking | not started | — |
| 4 | Keys | not started | — |
| 5 | Read path: manifest, archive, extract | not started | — |
| 6 | Write path: create | not started | — |
| 7 | **Interoperability gate** | not started | — |
| 8 | Remaining commands + remote backends | not started | — |
| 9 | Performance baseline vs borg | not started | — |
| 10 | Format / indexing changes | not started | — |

## 15. Deferred (post-1.0)

- Read-only borg 1.x repository support, and `borge transfer` from borg 1.x.
- FUSE mount.
- WebDAV server.
- The `cockpit` TUI.
- Windows support (borg's own Windows support is partial).

## 16. Principal risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| ~~**AES-OCB in pure Go**~~ | ~~Interop failure or, worse, a silent crypto bug~~ | **Downgraded 2026-08-16**: all 16 primary + all 9 appendix A RFC 7253 vectors pass, and envelopes are byte-identical to OpenSSL's across every suite and size tested. The ChaCha20-Poly1305 fallback is not needed. Independent review before Stage 7 remains worthwhile as a double-check. |
| Upstream borg 2 format still moving | Interop gate invalidated mid-port | Pin the commit; rebase deliberately with a reviewed diff |
| ~~`borghash`/`borgstore` license unknown~~ | ~~Cannot port those components~~ | **Closed 2026-08-16**: both BSD-3-Clause, porting permitted (LICENSING.md §6) |
| Surrogate-escaped path encoding | Silent path corruption on non-UTF-8 filenames | Fuzz round-trip in Stage 1.5; synthetic corpus in Stage 7 |
| `PackWriter` concurrency ported wrong | Rare, load-dependent repository corruption | Preserve the "index touched only by the calling goroutine" invariant; `-race` in CI |
| Chunker boundary drift | Total dedup loss, invisible until the repo is huge | Byte-exact boundary differential test (Stage 1.4) |
| Scope creep across 10 stages | Never finishing | Explicit non-goals (§0.6); one stage at a time; ask before advancing |
| Usage limits interrupting work | Lost context, broken tree | Stage/task granularity, always-committable state, evidence bundles (§2) |
