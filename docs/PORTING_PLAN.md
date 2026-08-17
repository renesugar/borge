# borge — plan for porting `borg` to Go

Status: **Stages 0-7 complete — the interoperability gate is green. Stage 8 in progress: `compact`, `check` (including `--repair`), `diff`, `export-tar`, `import-tar`, `prune`, `recreate`, `repo-compress`, `find`, `break-lock`, `with-lock`, `version`, `analyze`, `repo-space`, `debug *`, `benchmark`, `completion`, `key`, `repo-delete` and `help` done: **31 of borg's 36 commands**, with `tests/evidence/command-coverage.sh` as the gate. What remains: `serve` and the remote backends, and a decision on borg2-to-borg2 `transfer`. **Stage 9's investigation is done (§12.1-12.5): the largest wins are pure Go and are borge's own bugs, and no cgo dependency is currently justified.** `mount`/`umount`/`webdav` are §0.6 non-goals.**
Last updated: 2026-08-17.

`AGENTS.md` at the repository root orients a new agent on how to build, test and check
the repo, and on the working habits that have actually caught bugs here. Read it before
touching anything; read this plan for why the code is the shape it is.

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

> **The pin came loose on 2026-08-17** and it is worth recording how it showed up, because
> the symptom pointed at borge. The checkout at `/home/renes/projects/borg` moved from
> `114bd1e9` to `aa39d832` at 20:33 UTC. The virtualenv installs borg in *editable* mode, so
> the new Python source was picked up immediately while the compiled Cython extensions were
> not rebuilt — and `borg repo-info` began failing with
> `ImportError: cannot import name hostid` from `platform/posix.pyx`. Every differential
> test failed at once, in borge's test output, with a traceback inside borg.
>
> Two things made it harder to see than it should have been. `borg --version` still
> reported `2.0.0b23.dev377+g114bd1e94` — setuptools_scm baked that string at install time,
> so it names the pinned commit it is no longer running. And `tests/borg2/borg-commit.txt`
> is likewise written once at setup. **Neither notices the checkout moving.**
>
> `mkbundle.sh` now reads the borg working tree's real `HEAD`, records it as
> `borg2 actual:`, notes a modified tree, and prints a loud warning when it differs from the
> pin — an evidence bundle that records a borg version it did not actually test against is
> worse than one that records nothing. The stage-7-clean bundle of 19:26 UTC predates the
> drift by 66 minutes and is unaffected.
>
> Restoring the pin is `git -C /home/renes/projects/borg checkout 114bd1e9` followed by
> `make borg2`. Per this section, moving to a newer upstream commit is a deliberate,
> reviewed rebase with its own diff review — not something to absorb by rebuilding at
> whatever HEAD happens to be.

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

   The bundle tests a **snapshot** of the tree, not the tree itself (added 2026-08-17).
   A full run takes the better part of an hour, and editing during it does not merely mix
   two versions: `go test` lists a package's test functions and *then* compiles, so a test
   renamed in between leaves the generated test main calling a function that no longer
   exists and the package fails to build. Two stage-7 bundles recorded failures that were
   not real — one from this, one from `/tmp` filling — which is worse than a slow bundle,
   because a bundle exists to be trusted without re-running it.
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

> **Done 2026-08-16** (`internal/item`). Gate green: **34 fixtures** produced by borg's
> own `Item`/`ArchiveItem`/`ManifestItem`/`Key`/`EncryptedKey` classes round-trip
> byte-identically, including non-UTF-8 paths, users, groups and symlink targets.
>
> **Surrogate escapes were a non-problem, as stage 1.1 predicted.** A Go string is the
> wire form, so there is no encode/decode step to get wrong. What did need care was
> everything around it:
>
> - **Absent is not zero.** Every optional field is a pointer, and `ChunksSet` /
>   `XAttrsSet` flags distinguish an empty list or map from a missing key. Writing
>   `mode 0` and not writing `mode` are different bytes; conflating them would give
>   every item mode `0000` on its first rewrite.
> - **Unknown keys are preserved.** A key written by a newer borg is kept and written
>   back. Dropping it would silently strip metadata on any rewrite — `recreate`,
>   `transfer`, a repair — which is losing data while reporting success.
> - **xattrs must be sorted explicitly.** A Go map iterates in random order, so without
>   the sort the same item would hash to a different chunk id on every run.
>
> **Path sanitisation is the one security boundary in this package** and is tested
> twice: a table of stated-intent cases, and a differential against borg's own
> `make_path_safe` over 98 inputs — 72 sanitised identically, 26 rejected by both, no
> disagreement in either direction. Accepting a path borg rejects would be a
> path-traversal hole; rejecting one borg accepts would make borge unable to read valid
> archives. Both directions are asserted, plus an idempotence fuzz target.
>
> Note for stage 5: `REQUIRED_ARCHIVE_KEYS` lists `time`, but borg 2 writes
> `start`/`end` and only borg 1.x archives carry `time`. borge requires
> `version`/`name`/`item_ptrs`/`command_line` only — requiring `time` would reject
> every archive borg 2 writes.

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

> **Done 2026-08-16** (`internal/hashindex`). Gate green both directions across entry
> counts spanning several table rebuilds (0 … 50,000), plus a full loop — borg writes,
> borge reads and rewrites, borg reads again — and a header comparison field by field.
> The 1,000,000-operation property test passes.
>
> **Open question #1 is answered.** The serialised format is:
> `"BORGHASH"` (8) ‖ version u32 LE (=1) ‖ meta_size u32 LE ‖ JSON metadata ‖
> *used* × (key ‖ value). The JSON names the key/value sizes, the byte order, the
> namedtuple field names and their Python `struct` formats, the capacity and the entry
> count. Verified against a real `index/` file from a borg 2.0.0b23 repository and
> transcribed into `docs/FORMAT.md`.
>
> **Byte-identical output is deliberately not required here** (unlike the crypto
> envelope, where it is). Entries are written in the table's internal bucket order,
> which depends on capacity and insertion history, so matching borg's bytes would mean
> reproducing its whole resize history for no benefit — the reader inserts each entry
> by key, so any order round-trips to the same index. The differential test compares an
> order-independent digest of the entry set, and separately requires the *header* to
> match field for field, since that is what borg would reject.
>
> **The "not a Go map" claim, measured** (`TestMemoryFootprint`): at 1,623,610 entries,
> 143 MB against 185 MB for `map[[32]byte]Entry` — a 1.29x saving. Real but modest, and
> the package comment now says so rather than implying more. The stronger reason to
> port the structure is that a kv index is a stable 32-bit handle for a 256-bit key,
> which is what borg's `k_to_idx`/`idx_to_k` abbreviation depends on.
>
> Baseline for stage 9: `Set` 1043 ns/op, `Get` 413 ns/op at 1M entries (dominated by
> cache misses on the 143 MB working set, as expected).

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

> **Done 2026-08-16** (`internal/store`). Gate green: 21 objects across all seven
> namespaces lay out identically on disk, both read directions work including range
> reads, listings agree, and soft-delete/undelete interoperate in both directions —
> borge soft-deletes and borg undeletes, and the reverse.
>
> **Finding: `Store.List` yields the *bare key*, not a namespaced name.** borgstore's
> recursion returns each directory entry's own name, so both the namespace and the
> nesting path fall away: `"0123…cdef"`, not `"packs/01/0123…cdef"`. borg's callers
> use the listed name directly as an object id, so any code ported from borg depends
> on it.
>
> **The GoogleDrive part of the gate ran** (after the rclone mount was restored — on
> the first attempt every operation on it failed with `EIO`, including listing the
> root). `TestHighLatencyFilesystem` honours `BORGE_SLOW_FS_DIR` and skips with a
> specific reason when the mount is unusable.
>
> Measured on the real mount:
>
> | | |
> | --- | --- |
> | store one 100 kB object | **2.673 s** |
> | one object-header read, uncached | **2.889 ms** |
> | one object-header read, cached | **115 µs** (**25×** faster) |
>
> **A single object write costs 2.7 seconds on this mount.** That is the number that
> matters for stage 10, and it is far worse than the 5 ms per operation the simulated
> test assumed. It says the restore-side problem the whole project is aimed at is
> dominated by *operation count*, not bandwidth: 118,866 files in one directory at
> anything like this cost is hopeless no matter how fast the chunker is. Pack-oriented
> restore (read each pack once, sort extraction by `(pack_id, obj_offset)`) is
> therefore the right thing to try first in stage 9, before any format change.
>
> The same effect is also pinned down deterministically, so it is checked on every run
> without needing the network: walking 40 object headers in one pack costs **40 backend
> loads without the pack cache and 0 with it**.
>
> Not ported: the `sftp`, `rest`, `s3` and `rclone` backends (stage 8), and the quota
> tracking, which borg does not use for local repositories.

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

> **Done 2026-08-16** (`internal/repoobj`, `internal/crypto/key`,
> `internal/repository`). Gate green, all four parts:
>
> - borg opens a borge-written repository, computes the **same index digest**, and reads
>   every object back.
> - borge opens a borg-written repository, computes the same digest, and reads every
>   object back.
> - With every `index/` fragment deleted, borge rebuilds the index by walking borg's
>   packs and gets **the same digest** — the index really is a cache.
> - **`borg check` passes** on a borge-written repository.
> - Pack contents are **byte-identical**: same objects, same order, same pack, so the
>   content-addressed names match.
>
> **Finding: borg's object metadata dict is insertion-ordered, not sorted** —
> `type, size, ctype, clevel, csize`, with `size` second because `DecidingCompressor`
> sets it before the compressor base adds the rest. A sorted map changed the bytes of
> every object, which matters because borg's MAC modes are deterministic and advertise
> byte-identical objects across repositories with the same key material.
>
> **How much byte-identity is achievable, precisely:** everything the *envelope*
> contributes matches exactly — header, AAD, tag, metadata key order — and so does the
> compression *decision*. A compressed payload cannot match, because the bytes come from
> a different library; measured, the difference is −5 to +12 bytes. So an object stored
> uncompressed is byte-identical, one stored compressed is not.
> See `docs/DIVERGENCES.md` §3.
>
> The MAC key modes (`none-*`, `authenticated-*`) were implemented here rather than
> waiting for stage 4, which is what §7 asks for: they exercise the whole object path
> with no crypto risk. The AEAD modes still need their session-key derivation and blob
> handling, and `key.ByName` refuses them with that explanation.
>
> The `PackWriter` concurrency invariant is pinned down by tests rather than left to
> chance: results arrive one pack behind, a store failure surfaces one pack later *and*
> drops the failed pack's index entries plus anything still buffered, and the async and
> sync paths produce identical indexes. `make race` covers it.

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

> **Done 2026-08-16** (`internal/crypto/key`: `aead.go`, `blob.go`, `manager.go`,
> `paperkey.go`; `internal/repository/keys.go`). Gate green:
>
> - **All ten mode/location combinations** borg can create — `aes256-ocb`,
>   `chacha20-poly1305`, both with `sha256` and `blake3` id hashes, `authenticated-*`,
>   `none-*`, in `repokey` and `keyfile` — are opened by borge, which finds the key where
>   borg put it and **decrypts the manifest object** with the key it derives.
> - borg keeps working after borge writes into it: a key borge added opens the
>   repository, `borg key list` shows both keys, and a passphrase borge changed takes
>   effect for borg (old refused, new accepted).
> - `borg key export` → borge reads it; `borge key export` → `borg key import` → borg
>   still opens the repository, and borge finds the keyfile borg wrote.
> - Every mode's **id hash matches borg exactly**, and the deterministic MAC modes
>   produce **byte-identical envelopes**. The AEAD modes are checked by round trip in
>   both directions, which is what tests the session key derivation: borg picks a random
>   session id and borge has to rederive the same key from it.
> - The **paper key printout is byte-identical to borg's**, line for line, and borge
>   reads borg's.
>
> **Finding, and a real bug fixed: borge's lock records were unreadable to borg.**
> Two defects, both in stage 3 code, both invisible until borg had to open a repository
> borge was holding:
>
> - The timestamp was timezone-naive. borg parses it with `datetime.fromisoformat` and
>   compares it against an aware `now`, so borg **crashed with TypeError** on every
>   access to a repository borge had touched. Fixed by writing `+00:00`, matching
>   Python's `isoformat(timespec="milliseconds")`.
> - `threadid` was a random hex string. borg asserts it is an `int`, so borg **aborted**
>   rather than merely ignoring the lock. It is now always `0`, as in borg.
>
> The second one has a consequence worth stating: with borg's identity tuple
> `(hostid, pid, 0)`, two locks taken by *one process* no longer conflict.
> `TestSameProcessLocksDoNotConflict` records that, because it looks like a bug and is
> not — it is borg's behaviour, and it is what lets one process hold a lock across
> nested operations.
>
> The same class of mistake was closed in the other direction too: borge used to *skip*
> any lock it could not parse, which silently turns "somebody is using this repository"
> into "nobody is". It now refuses to proceed and names the object.
>
> **Finding: `BORG_TESTONLY_WEAKEN_KDF` weakens the derivation but records the real
> argon2 parameters** in the blob, so a key written under it cannot be opened without
> it. borge routes sealing and opening through one chokepoint that reproduces this;
> writing the weakened parameters into the blob instead would have looked more honest
> and made the two tools unable to read each other's test keys.
>
> Two departures recorded in `docs/DIVERGENCES.md`: the keyfile search path reads borg's
> directory but writes only borge's (§5), and `paperkey.html` is copied verbatim rather
> than ported (§6).
>
> Concurrency work landed here as well, because the AEAD path is the first thing stage 6
> will use from a worker pool: `ocb` precomputes its `L` table so `Seal`/`Open` no longer
> write to the receiver, and the message counter is mutex-guarded with
> `TestAEADCounterIsUnique` asserting uniqueness under `-race`. A repeated
> (session key, nonce) pair is not a survivable bug.

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

> **Done 2026-08-17** (`internal/manifest`, `internal/archive`, `internal/patterns`,
> `internal/cli`, plus `item.FormatMode`). Gate green, in five parts:
>
> - **5a manifest.** borge's archive listing matches `borg repo-list --json` on ids,
>   names, hosts, users and timestamps; borge rewrites the manifest and `borg check`
>   still passes; soft delete and undelete agree with borg.
> - **5b item stream.** borge walks the stream and gets the same items
>   `borg list --json-lines` reports, in the same order, verified across a **33-chunk
>   stream of 20k+ items** so the boundary-straddling case is actually exercised.
> - **5c extraction.** Both tools extract the same archive into their own directory and
>   the results are compared: **27 entries, zero differences**, across content sha256,
>   mode, uid/gid, mtime to the nanosecond, symlink targets, hard link grouping, xattrs
>   and POSIX ACLs. The comparator reports what it reached and **fails if the corpus
>   missed symlinks or hard links**, so it cannot pass vacuously.
> - **5d patterns.** 4464 (style, pattern, path) triples across `fm:`, `sh:`, `pp:` and
>   `pf:` agree with borg exactly, plus `re:`; whole include/exclude sequences agree on
>   the decision *and* the recursion flag.
> - **5e commands.** `repo-list`, `repo-info`, `list`, `info` and `extract`, compared
>   against borg's output as data (JSON) and by fields (text).
>
> **Not done, and tracked rather than quietly dropped:** `export-tar` and `diff`, and
> `bsdflags` restoration (`docs/DIVERGENCES.md` §8). The corpora in §10 were exercised
> through the stage 3 and 4 gates; the extraction comparator runs on a purpose-built
> tree, because the point is to reach the awkward shapes (sparse files, hard link
> groups, ACLs, sub-second times) rather than to move volume.
>
> **Findings:**
>
> - **borge was not restoring timestamps on symlinks, fifos or device nodes** - only on
>   regular files and directories. borg's `restore_attrs` does times for every item type.
>   Caught by the comparator's mtime check.
> - **`repo-list --short` prints archive *ids*, not names.** An id is what uniquely
>   selects an archive; names are not unique. Printing names would have looked friendlier
>   and been wrong.
> - **`stat.filemode` renders an unknown file type as `?`, not `-`** - the C `_stat`
>   implementation is the one that runs, and it differs from the pure-Python fallback in
>   exactly that character.
> - **borg's `shellpattern.translate` has a vacuous guard**: its `(`/`|`/`)` passthrough
>   checks `pat[i-1] != "\\"` after `i` has already advanced, so the guard always
>   passes and `\(` becomes a backslash plus a group opener. Reproduced as-is.
> - **`RobustUnpacker` cannot keep a scan offset naively**: a candidate rejected only
>   because the key it needed had not arrived yet must be re-examined, not skipped.
>
> Two security properties are ported deliberately and tested against crafted input:
> a path containing `..` or below an existing symlinked parent is refused, and a hard
> link whose group leader is a symlink recreates the symlink rather than linking the
> file it points at (CVE-2026-62268).

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

> **Done 2026-08-17** (`internal/archive/builder.go`, `create_linux.go`,
> `internal/cache`, `internal/cli/create.go`, `manage.go`). Gate green:
>
> - **`borg check --verify-data` passes** on a borge-written repository, for an encrypted
>   and an unencrypted one. That reads every chunk of every archive and re-verifies it
>   against its id.
> - **`borg extract` of a borge-written archive reproduces the source tree**: 27 entries,
>   zero differences, including xattrs, POSIX ACLs, hard link groups, symlink targets and
>   nanosecond mtimes.
> - A repository borge created is one borg opens, backs up into and verifies, in all four
>   modes tried; and borge reads back what borg wrote into it.
> - `create`, `delete`, `undelete`, `rename` and `tag` are each checked by asking **borg**
>   what the repository looks like afterwards, with `borg check --verify-data` at the end.
>
> **The chunker result is the one worth keeping.** Given the same tree and the same key,
> borge and borg produce **identical chunk ids** - same boundaries, so borge's archive
> deduplicated entirely against borg's and stored only its own metadata. One test
> exercises the whole chain: the fastcdc gear table, the `derive_key(domain="fastcdc",
> from_id_key=True)` derivation, and the id hash.
>
> **Findings:**
>
> - borge set `hlid` on symlinks and device nodes but **not on regular files**, so borg
>   restored a hard link group as independent files with identical contents. Sharing the
>   chunk list saves space; the hlid preserves the *relationship*. Two mechanisms, and
>   only one was implemented.
> - borge wrote named ACL entries as `user:0:r--`. borg's `acl_set` reads `fields[3]`
>   unconditionally for any entry with a non-empty name field, so restoring one raises
>   IndexError - the archive would be unreadable by the tool it exists to interoperate
>   with. Named entries now carry the name *and* the id.
> - The files cache's newest-timestamp exclusion has a consequence that looks like a bug:
>   **a single-file tree caches nothing**, because every entry is then the newest. borg
>   behaves identically. `TestOneFileCachesNothing` records it.
>
> Concurrency is deliberately absent, as §9 asks: the write path is serial. Stage 9
> pipelines it with this gate already in place to catch regressions.
>
> The files cache is borge's own on-disk format, not borg's - the caches are not shared
> (`docs/DIVERGENCES.md` §4), so there is nothing to interoperate with. It stores full
> chunk ids rather than borg's index-compressed form, which is simpler and costs memory;
> stage 9 measures whether that matters.

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

> **Ordering correction.** Rows 3, 6 and 8 need `borge check --verify-data`, and row 7
> needs `borge compact` — both of which this plan filed under stage 8 (§11). That was an
> ordering mistake in the plan, not a discovery about the format. `borge check` was
> implemented here rather than deferring three rows; `borge compact` was not, and row 7's
> compaction is done by borg with the substitution stated in the test.

> **Done 2026-08-17** (`tests/interop/`, `internal/cli/check.go`). Gate green:
>
> | Row | What runs | Result |
> | --- | --- | --- |
> | 1 | borg writes, borge extracts | ✅ 5 key modes |
> | 2 | borge writes, borg extracts | ✅ 5 key modes |
> | 3 | `borge check --verify-data` over both | ✅ 5 key modes |
> | 4 | `borg check --verify-data` over both | ✅ 5 key modes |
> | 5 | borg then borge into one repository | ✅ both tools extract both archives |
> | 6 | borge then borg into one repository | ✅ both tools extract both archives |
> | 7 | borg creates, borge deletes and compacts | ✅ (see the note below) |
> | 8 | borge creates, borg deletes and compacts | ✅ |
>
> **584 entries identical** on every comparison — the synthetic corpus, which covers
> invalid-UTF-8 names, NFC/NFD pairs, a bidi override, control characters in names, 40
> levels of nesting, zero- and one-byte files, setuid/setgid/sticky modes, sparse files,
> six kinds of symlink, three hard link groups including a hard-linked symlink, a fifo,
> binary and 3 KB extended attributes, and access and default POSIX ACLs.
>
> Also green: all four compression settings interoperate in both directions (including
> `auto`, which decides per chunk); `--sparse` restores holes; and rows 5 and 6 confirm
> **deduplication across tools** — two archives of one tree, written by different tools,
> occupy ~256 KB of packs rather than twice one archive.
>
> The Google Drive corpus is archived **in place** rather than copied to a local subset:
> it is in the list for its high-latency I/O pattern (stage 2 measured one 100 kB write at
> 2.673 s through it), and copying it to local storage first would remove exactly the
> property it exists to exercise.
>
> **And it earned its place.** It found a real bug that no other corpus could: an rclone
> mount synthesises inode numbers from a hash, so they exceed 2^63. borg stores the inode
> as a msgpack uint64 and Python's arbitrary-precision integers never notice; borge
> decoded it as int64 and **refused to read the archive at all** with "inode value
> 16477067133423719032 does not fit in an int64". `item.Item.Inode` is now unsigned, and
> is encoded as unsigned so it round-trips as the number that went in.
>
> **Row 7's substitution has since been lifted.** At the time this gate first ran,
> `borge compact` did not exist and borg did the compaction. `compact` landed at the start
> of stage 8 and row 7 now runs as written — borge deletes *and* compacts, borg verifies —
> with 584 entries identical. The stage 8 record has the detail.
>
> **`export-tar` and `diff` have since landed** in stage 8; nothing from stage 5 is
> outstanding any more.
>
> Real corpora run as bounded subsets (4000 files each) so the gate stays runnable on
> every commit; the counts are logged, and stage 9 is what runs them whole. Rows 1–4 over
> each, with `aes256-ocb`:
>
> | Corpus | Entries compared | Result |
> | --- | --- | --- |
> | Joplin archive (with attachments) | 4000 | ✅ both directions |
> | Joplin recipes | 4000 | ✅ both directions |
> | Obsidian vault | 4104 | ✅ both directions |
> | recipedb (whole) | 4613 | ✅ both directions |
> | pathological dir (118,866 files in one directory) | 4000 | ✅ both directions |
> | Google Drive (rclone mount) | 84 | ✅ both directions, archived **in place** |

---

## 11. Stage 8 — remaining commands and backends

Everything needed for feature parity, once correctness is established.

> **`compact` done 2026-08-17** (`internal/repository/compact.go`,
> `internal/cli/compact.go`). It is the gap that kept stage 7's row 7 from running as
> written, so it was taken first.
>
> Mark and sweep: walk every archive for the chunk ids it references, remove the
> soft-deleted archives' directory entries, then drop packs that are wholly dead and
> rewrite packs that are dead enough to be worth the copy (10% by default). The index is
> rewritten whole afterwards, because removing entries is not something an incremental
> write can express.
>
> **It refuses to run on an incomplete scan.** If any archive is unreadable, or if a
> referenced chunk is not in the index, it stops and changes nothing. A garbage collector
> that proceeds from a partial view of what is referenced deletes live data, and does it
> silently — the archive that needed the chunk is not read again until a restore, which is
> exactly when the user cannot afford to find out.
>
> Soft-deleted archives are nuked *after* the live scan comes back clean, for the same
> reason: while a repository is damaged, `borge undelete` may be the way out.
>
> Deliberately not ported: borg's per-archive reference caches, tiny-pack merging and the
> all-packs gate. Those change *when* a compaction runs, not what it produces. They are
> performance work and belong with stage 9's measurements rather than with a guess.
>
> Verified: reclaims 1.2 MB of a 1.2 MB dead archive; rewrites mixed packs and the
> survivors still restore byte-identically; a dry run changes nothing and leaves an
> undelete possible; `borg check --verify-data` passes after every case. Two test-shaping
> notes worth keeping — a *contiguous* surviving subset can line up exactly with a pack
> boundary and produce no mixed pack at all (the test interleaves instead), and a
> repository whose damage removed archive metadata is legitimately unlistable afterwards,
> so "still listable" is the wrong assertion for a refused compaction; "nothing further was
> deleted" is the right one.

> **`diff` and `export-tar` done 2026-08-17.** These were stage 5's two outstanding
> commands, so they were taken next.
>
> `diff` compares two archives by their chunk lists, which answers the question without
> reading any content — but only while both archives were chunked the same way. With
> different `--chunker-params` the same bytes produce different ids, so borge compares the
> recorded parameters and, when they differ, falls back to size comparison and says so
> rather than reporting every file as modified.
>
> `export-tar` writes PAX by default because it is the only format that carries extended
> attributes, ACLs and sub-second times. GNU is available and **warns per item** about what
> it is dropping: an export that silently loses a file's ACLs is worse than one that says
> so, because the user believes they have a faithful copy. Verified by GNU tar itself
> reading and extracting the output, with hard links emitted as tar link entries rather
> than second copies.

> **`prune` done 2026-08-17** (`internal/manifest/prune.go`, `internal/cli/prune.go`).
>
> A rule is not "keep the last N archives" — it is "keep one archive from each of the last
> N *periods*". Seven backups taken in one afternoon satisfy `--keep-daily=7` with a single
> archive. Period keys are computed in **local time**, as borg's are: a user in UTC+13
> whose backups run at 20:00 would otherwise find them all landing in the next UTC day.
>
> **A real bug the differential test caught.** A rule's quota counts only the archives *it*
> keeps: one already kept by a finer rule does **not** consume a coarser rule's budget. So
> `--keep-daily=7 --keep-monthly=6` keeps seven days *and* six further months. borge
> counted them, which is a plausible reading and a wrong one — it made coarser rules stop
> short and silently discard older history. Two of five policies disagreed with borg before
> the fix; all five agree after it.
>
> Pruning is a **soft** delete, so a misread rule is recoverable until a compaction runs,
> and the command says so. An empty policy is refused outright rather than confirmed: with
> no rules every archive would be pruned, which is never what anybody meant to type.
> `@PROT`-tagged archives are never pruned and never consume a quota.

> **`recreate` and `repo-compress` done 2026-08-17.**
>
> `recreate` rewrites archives: exclude paths already stored, or re-chunk. Excluding a
> path from *future* backups does nothing about the copies already in the repository —
> recreate is what removes them. Re-chunking reads at the `rechunk` assert-id place, since
> fresh ids computed from re-read plaintext would otherwise launder a chunk whose content
> did not match its id.
>
> **A finding that cost a test rewrite.** `recreate --compression` *appears* to work and
> changes nothing: a chunk's id is the hash of its **plaintext**, so compression lives
> below the id, a recompressed chunk has the same id, and every chunk-writing path
> deduplicates it away. borg behaves identically and says nothing. borge refuses the flag
> and points at `repo-compress`.
>
> That is why `repo-compress` rewrites **whole packs** rather than replacing objects.
> Replacing one object leaves the old copy as bytes no index entry covers, and compaction
> deliberately preserves those (borg #8572) — so an object-at-a-time recompression made
> the repository **41% larger** before this was fixed. Rewriting the pack leaves no stale
> copy: measured 58.8% smaller on text that separates lz4 from zstd,19, with a second run
> a no-op.
>
> Choosing that test corpus mattered. The first version used trivially repetitive data,
> which lz4 compresses almost as well as zstd — it passed on a 19-byte margin out of 8324
> while the code was doing nothing at all.

> **`import-tar` done 2026-08-17, and the `BORG` tar format with it.**
>
> The inverse of `export-tar`: a tar stream becomes an archive without ever landing on the
> filesystem, so it works on a pipe and so device nodes and ACLs survive on a machine where
> creating them would need root.
>
> Implementing it forced the **`BORG` tar format** onto the export side too, which had only
> `PAX` and `GNU`. A tar header cannot express birthtime, BSD flags, or the hard link group;
> `PAX` adds sub-second times, xattrs and ACLs and stops there. `BORG` carries the whole
> item as a msgpacked, base64'd pax record, and is the only format an import restores
> *exactly*. Without it borge could neither read nor write the format borg uses for archive
> transfer between repositories — an interop gap, not a missing nicety.
>
> The record **replaces** the tar header rather than merging with it, matching borg: two
> sources of truth for one field is how they drift apart. Its chunk list is dropped on
> import — those ids name objects in whatever repository the tar came from — and refilled
> from the stream, so an import can never write an item pointing at chunks that do not
> exist here.
>
> **A fidelity limit worth stating.** tar's hard link model is "first entry is the file,
> later ones name it", and borg turns each later entry into an ordinary item reusing the
> first one's chunk list. The `hlid` is not reconstructed, so an `export-tar`/`import-tar`
> round trip through `PAX` silently **unshares hard links**: two files with identical
> content instead of two names for one inode. `BORG` format keeps the `hlid`, because it
> keeps everything. This is borg's behaviour and borge matches it.
>
> `--ignore-zeros` reads concatenated tars. Go's tar reader stops at the end-of-archive
> marker, so a fresh reader is started on the same buffered source; peeking is what
> separates "more blocking-factor padding" from "actually finished". Checked against a real
> `tar --concatenate` output, not a hand-built one, because the padding is exactly where the
> two implementations could differ.
>
> Seven tests, two of them differential against borg (a tar imported by both tools item for
> item, and the padded concatenation), plus both directions of the `BORG` format:
> borge reads borg's tar and borg reads borge's. The differential test asserts up front that
> all six entry types are present with populated mode/mtime/target/size — two empty structs
> would otherwise agree and prove nothing.

> **`find`, `break-lock`, `with-lock` and `version` done 2026-08-17.**
>
> `find` answers "which archives contain this file?". There is no path index, so it reads
> the item stream of every selected archive — the same cost as a `list` loop in the shell,
> but with the archive ordering, the pattern styles and the soft-delete handling right.
> Newest first, because the question behind it is nearly always "when did it last exist".
>
> **The bug it uncovered was not in `find`.** Writing its tests showed `sh:**/file1.txt`
> matching nothing — and then showed `list` doing the same. `Matcher.AddIncludePaths`
> passed every positional path to `NewPattern(StylePathPrefix, …)`, where borg passes it to
> `parse_pattern(p, PathPrefixPattern)` — a **fallback**, not a style. So a positional path
> carrying `sh:`, `re:`, `fm:`, `pp:` or `pf:` was read as a literal path starting with
> those characters, and matched nothing at all on `list`, `extract`, `diff` and
> `export-tar` alike. No error, no warning: an empty result indistinguishable from a
> correct "no such file". Fixed to `ParsePattern(p, StylePathPrefix, true)`, and pinned by
> a differential test over all six styles — with a second test asserting the patterns are
> not *all* empty, since two empty lists agree.
>
> **The emptiness guard earned its place on 2026-08-17.** The path-anchored patterns in
> that test were written as `pp:media` and `pf:media/renes`, which matched because borg
> archives an absolute source path with the leading slash removed and `TMPDIR` pointed at
> `/media/renes/HD2/borge-tmp`. With that disk unmounted the temporary directory moved to
> `/tmp` and both patterns matched nothing — which the *comparison* accepted happily, since
> borg matched nothing either. Only the guard failed. The patterns are now derived from the
> archived tree, so the test measures the same thing wherever it runs.
>
> `break-lock` breaks unconditionally, as borg does; refusing on a heuristic would block
> the one case it exists for. What it adds is saying **what** is being broken: a stale lock
> was going to be removed anyway, while a lock refreshed a minute ago means somebody is
> probably still running, which now yields `ExitWarning`. The test has borg take the lock,
> so it also checks borge reads borg's lock records.
>
> `with-lock` refreshes the lock on a ticker while the command runs, and returns **the
> command's** exit code — a wrapper that swallows it makes `borge with-lock … && …`
> silently wrong. The shutdown waits for the refresher to stop: closing the channel only
> ends the *next* iteration, so without the wait a refresh in flight would touch the lock
> while the deferred `Close` released it. Checked under `-race`.
>
> `version` prints `<client> / <server>` even though borge has no remote backends and the
> two are the same value — the shape is what scripts parse, and adding the second field
> later would break them then. `--json` also reports the pinned borg commit and repository
> format version, which is what answers "can this build read that repository?".

> **`analyze` and `repo-space` done 2026-08-17.**
>
> `analyze` answers the question an archive listing cannot: "the repository is 400 GB, what
> would I delete to shrink it?" Deduplication means the archives do not add up — a chunk
> shared by twenty archives is stored once, and deleting nineteen frees nothing — so every
> figure is about **chunks**, each counted exactly once. The default mode reports what a set
> of archives costs and what deleting the whole set would actually free; `--by-name`
> decomposes the entire repository, giving each name its exclusive size with everything
> shared between names in its own row.
>
> **Checked against borg, number for number**, on a corpus built so that the shared and
> unreferenced buckets are both non-empty — a comparison where they were zero would agree
> without testing them. The exclusive figure is additionally checked against reality:
> delete the set, compact, and confirm the repository actually shrank by about that much.
>
> `--by-name` refuses archive filters, as borg does: "shared" and "unreferenced" are only
> true statements about the whole repository.
>
> Hot spots (`chunks added or removed between consecutive archives, by directory`) answer a
> different question — not what is big, but what keeps *changing*, which is what makes a
> repository grow when nothing gets bigger. Its test deliberately makes the churning
> directory much **smaller** than the stable one, so a report that merely ranked by size
> would name the wrong directory and fail.
>
> **Cost divergence:** borg reads a per-archive references cache written by `compact`, so
> unchanged archives are never opened. borge has no such cache yet and re-reads every
> archive's item stream. Same answer, longer wait.
>
> `repo-space` manages the emergency reserve — a pile of incompressible bytes held back so
> that a repository which fills its filesystem is not wedged, since the operations that free
> space (`prune`, `delete`, `compact`) all need a lock, and locking is a write. Filled from
> the random source rather than with zeroes: a reserve that a compressing filesystem stores
> in no space reserves nothing. Reserving **replaces** rather than accumulates, or a cron
> job reserving after each backup would fill the disk it protects. Tested in both directions
> against borg, since both tools manage the same `config/space-reserve.N` objects and a
> naming disagreement would mean `--free` silently freeing nothing in an emergency.
>
> This also added `internal/cli/size.go`: borg's `parse_file_size` and `format_file_size`,
> including that suffixes are **decimal** (`1G` is 10^9, not 2^30) and that `BORG_UNITS`
> selects si/iec/raw. The two tools have to agree on the size of a reservation they share.

> **`debug *` done 2026-08-17** (`internal/cli/debug.go`, `internal/cli/pydump.go`).
>
> Twelve subcommands: `info`, `dump-archive-items`, `dump-archive`, `dump-manifest`,
> `dump-repo-objs`, `search-repo-objs`, `get-obj`, `put-obj`, `delete-obj`, `id-hash`,
> `parse-obj`, `format-obj`. borg's thirteenth, `convert-profile`, writes a CPython
> `marshal` file and is recorded as DIVERGENCES #14 rather than ported.
>
> **These are the port's own debugging tools**, which is why they were worth the effort of
> matching borg exactly. When `borge list` and `borg list` disagree about an archive, the
> way to find out which is wrong is to dump the same object with both and diff — and that
> only works if the dumps are comparable. So `pydump.go` reproduces `prepare_dump_dict`
> *and* enough of CPython's json encoder to be byte-identical: `ensure_ascii` escaping,
> `", "` separators, insertion order for the dicts borg builds and sorted order for the
> ones it decodes (`StableDict.items()` sorts).
>
> The awkward part is the bytes/str split. A chunk id and a filename are both `bytes` on
> the wire; borg shows the one that decodes as UTF-8 as text and marks the other with
> U+007F plus hex. A filename that is *not* UTF-8 — which Linux allows — reaches Python as
> a surrogate-escaped `str` and comes out as `\udcXX`, and borge reproduces that from the
> raw Go string. All three dumps are compared against borg byte for byte, over a tree built
> to contain a non-UTF-8 name, a CJK name, an emoji, a hard link, a symlink and a binary
> xattr value — the comparison asserts each of those is present in *borg's own* output
> first, since a tree of ASCII files would agree while testing none of it.
>
> **Two borg bugs are not reproduced** (DIVERGENCES #13), both the same cause: a negative
> Python slice index read as an offset from the end. One blanks the context before a search
> hit near the start of a large object; the other reports every hit twice for a one-byte
> search term.
>
> `delete-obj` needed `Repository.DeleteObject`, which rewrites the object's pack without
> it. Its test works out pack membership **from the bytes on disk** — searching each pack
> file for the stored object borg's `get-obj` produced — rather than asking borge's chunk
> index, since the index is the thing under test. It then asserts the fullest pack held at
> least three objects before deleting from it: the round-trip test could not show this,
> because the object it deletes was put there by itself and has no neighbours to lose.
> Mutation-checked by making the rewrite drop the survivors, which fails the test.
>
> `put-obj` and `delete-obj` take the **exclusive** lock where borg takes a shared one.
> They rewrite the chunk index at close and two concurrent writers would lose one of the
> two objects; for commands that exist to corrupt a repository deliberately, refusing to
> run alongside another borge is the safer default.

> **`benchmark` and `completion` done 2026-08-17**, which completes stage 8's command list.
>
> `benchmark crud` drives the ordinary commands against a real repository - create with the
> files cache disabled, create again to fill it, a dry-run extract, a delete - at three
> scales of all-zero and random files. `benchmark cpu` measures the primitives in memory:
> chunkers, hashes, AEAD suites, compressors, msgpack.
>
> The volumes, buffer sizes and output columns are borg's, so the two tools' output can be
> read side by side, and the compression input is generated by borg's own xorshift word
> soup - ported byte for byte and pinned by a test - so both compress identical data. A
> differential test compares the crud rows with every number replaced by `#`, which checks
> the format a script would parse without comparing the measurements themselves.
>
> Two columns are borge's own, and both exist because a plain throughput number would have
> hidden something (see §12).
>
> `completion` generates bash, zsh and fish scripts. borg builds its from argparse via
> `shtab`; Go's flag package has no parser object to walk from outside, and keeping a second
> copy of every option would drift the first time somebody adds a flag. So `describeCLI`
> runs each command with `-help`: every command registers its flags and *then* calls
> `fs.Parse`, so the FlagSet is fully populated when Parse rejects the argument, and
> `newFlagSet` hands it to a collector on the Env. A test asserts every command still has
> that shape, and another checks the probe creates nothing in the repository or the working
> directory.
>
> Positional arguments cannot be derived that way - the flag package has no notion of them -
> so which commands take an archive name is a stated list, with a test that every entry is a
> real command. The bash script is tested by *running* it in bash and checking what it
> offers; a syntax check would pass on a script that completes nothing. tcsh is refused with
> an explanation rather than generated (DIVERGENCES #15).

> **The stage 8 gate is `tests/evidence/command-coverage.sh`**, added 2026-08-17 after
> this section had already been written as if it were the gate.
>
> It asks *borg* what commands it has, asks borge the same, and classifies every
> difference against a table of recorded reasons - failing on any command that is in
> neither. It exists because the paragraph above listed "`benchmark`, `debug *`, shell
> completions" as what remained, those three were finished, and the status line was
> changed to say the command list was complete. It was not: borg has 36 commands and
> borge had 28. A list of remaining work maintained by hand is exactly how that happens.
>
> It also caught a bug in itself on the first run, which is worth recording because it is
> the same failure in miniature: the extraction required two spaces after a command name,
> and `repo-compress` is thirteen characters against borge's twelve-wide column, so the
> gate reported a command borge *has* as missing.
>
> Current state: 28 implemented, 8 absent with a recorded reason, 0 unexplained. Of the
> eight, three are non-goals (`mount`, `umount`, `webdav`, §0.6) and five are work:

- **`serve`** and the remote store backends: `sftp`, `rest`, `s3`, `rclone`.
- **`transfer`** between two borg 2 repositories. §0.6 rules out transfer *from borg 1.x*
  because it depends on the borg 1 reader; it says nothing about borg 2 to borg 2, which
  is a decision still to make rather than one already taken.
- **Archive-name placeholders**, which are not a command and so are invisible to the
  coverage gate — see DIVERGENCES #17 and the note below.

> **`key`, `repo-delete` and `help` done 2026-08-17**, closing the three gaps the coverage
> gate found.
>
> **`key`** is seven subcommands over a library that was already finished and gated at
> §1.3: `list`, `export`, `import`, `change-passphrase`, `change-location`, `add`,
> `remove`. Every one is checked against borg in both directions, because a key is the one
> thing in a repository that cannot be regenerated — a borge-written key borg cannot read
> is data loss with the repository still intact. `key list` output is byte-identical to
> borg's; so is the plain export and the QR HTML page; the paper key's *data* lines are
> identical and only its instruction line differs. The paper key is tested as the last
> resort it is: export it, take the keyfile away, confirm borg can no longer open the
> repository, restore from the printout, confirm borg can.
>
> Two commands need a *second* passphrase, and borge does not prompt anywhere — so
> `BORGE_NEW_PASSPHRASE` is required and its absence is an error. Proceeding with an empty
> passphrase would leave a repository unprotected while reporting success. **Prompting is
> still missing across the whole CLI** and is now the most user-visible gap; it needs
> terminal handling and belongs in its own change.
>
> **`repo-delete`** is the only irreversible command in borge, so most of its tests are
> about what it refuses. It will not touch a path that is not a repository — `borge
> repo-delete -r ~` must not be a way to lose a home directory to a typo — and it removes
> only the namespaces a repository owns, leaving (and naming) anything else in the
> directory. DIVERGENCES #18.
>
> **`help`** carries borg's five topics rewritten for borge, because a copy would be wrong
> in every one: different environment variables, fewer chunkers, coarser compression
> levels. They are pinned by `TestHelpEnvironmentTopicListsEveryVariable`, which scans the
> source for the variables the code reads and fails if the topic omits one — **or documents
> one nothing reads**. It found two on its first run (`BORGE_CONFIG_DIR`, `BORGE_BASE_DIR`).
>
> **And the placeholders topic was wrong when first written.** It was drafted from borg's,
> describing `{now}` and `{hostname}` substitution — and borge has none: `borge create
> '{hostname}-{now}'` creates an archive with that literal name, silently. Caught by
> running the command rather than by review. The topic now says so, and
> `TestHelpPlaceholdersTopicIsTrue` checks the claim against the behaviour so it cannot
> quietly become false once placeholders are implemented.
- `--progress`, `--stats`, `--json`, `--log-json` output shapes.
- Platform coverage: macOS and FreeBSD `platform/` implementations.

**Gate:** `borg check --repair` and `borge check --repair` produce equivalent
repairs on a corpus of deliberately corrupted repositories (bit flips in packs,
truncated packs, missing index, missing archive object, stale lock).

> **`check --repair` done 2026-08-17** (`internal/archive/repair.go`,
> `internal/cli/check.go`, `Repository.RebuildChunkIndex`).
>
> Repair cannot bring data back. What it can do is stop one lost chunk from making
> everything after it unreadable: the item metadata stream has no framing, so a missing
> chunk in the middle costs the *rest of the archive* rather than the files it held.
> Repair resynchronises past the gap and rewrites what survived. A repaired archive is a
> smaller archive, honestly labelled.
>
> Three safety properties, none of them optional:
>
> - It **rebuilds the chunk index from the packs first**. An index that disagrees with
>   what the packs hold would make repair "fix" archives against a fiction, which is worse
>   than leaving them broken.
> - It **never removes the original** of anything it rewrites: the repaired archive is
>   written alongside and the original soft-deleted, so somebody who wants to try harder
>   still can. borg deletes; borge soft-deletes.
> - Everything is read at the **repair** assert-id place, re-certifying
>   `id == hash(content)`. Repair re-packs the stream under fresh ids, so a chunk whose
>   content did not match its id would become valid data under a new id and the violation
>   would be unnoticeable afterwards.
>
> Corruption corpus, all green: a flipped bit inside a pack (caught by `--verify-data`,
> and borg agrees the repository is broken); a missing pack; a missing index, rebuilt and
> written back; missing content chunks, with the archive still listable so the user can
> see what they lost; a missing archive object, where repair says what is unrecoverable,
> removes the dangling directory entry and leaves the other archives usable; and
> `--find-lost-archives`, which recovers an archive whose pointer was deleted by finding
> its metadata object in the packs.
>
> A repair of a **healthy** repository changes nothing — the most important thing it can
> do.
>
> One observation worth recording: the archive-level check tests *index membership*, not
> readability, so with a stale index a missing chunk is caught by the repository check
> rather than the archive check. borg's does the same. Both together catch it; either
> alone would not.

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

### 12.1 First measurements, 2026-08-17

`borge benchmark cpu` against `borg benchmark cpu` on the development machine, the moment
the command existed. These are single runs on a machine that was not otherwise idle, so
treat the ratios as orders of magnitude rather than figures - but three of them are far too
large to be noise, and all three are *structural* rather than a matter of tuning.

| row | borg | borge | |
|---|---:|---:|---|
| aes-256-ocb | 860.4 MB/s | 49.8 MB/s | **borge 17x slower** |
| blake3 (64MiB) | 3219.8 MB/s | 1577.9 MB/s | borge 2.0x slower |
| blake3 (2MiB) | 2604.7 MB/s | 1241.2 MB/s | borge 2.1x slower |
| chacha20-poly1305 | 435.9 MB/s | 332.2 MB/s | borge 1.3x slower |
| hmac-sha256 | 97.6 MB/s | 84.0 MB/s | comparable |
| blake2b-256 | 135.1 MB/s | 221.2 MB/s | *borge 1.6x faster* |
| lz4 (2MiB) | 125.9 MB/s | 64.5 MB/s | borge 2.0x slower |
| zstd,3 (2MiB) | 120.8 MB/s | 26.1 MB/s | borge 4.6x slower |

**1. AES-256-OCB is 17x slower, and it is borg 2's default.** borge's OCB is the pure-Go
`internal/crypto/ocb`; borg's is OpenSSL. This is the clearest candidate §0.4 and step 5
below have in mind, and it is the one measurement that most affects a real backup.
ChaCha20-Poly1305 is only 1.3x slower because Go's implementation is assembly, which is
also the shape of the fix.

**2. borge builds a chunker per file; borg builds one per archive.** `archive.Builder.ChunkFile`
calls `chunker.New` for every file, and borg's `FilesystemObjectProcessors.__init__` calls
`get_chunker` once. The gear and buzhash tables are not free:

| chunker | setup per file | share of an 8 MiB chunking run |
|---|---:|---:|
| fastcdc,19,23,21,2 | 1.75 ms | 7% |
| buzhash64,19,23,21,4095,2 | 4.35 ms | 10% |
| buzhash,19,23,21,4095 | 1.67 ms | 3% |
| fixed,1048576 | 0.41 ms | 20% |

Irrelevant beside a large file and dominant beside a small one. For `deutsche-rezepte`
(118,866 files in one directory) that is roughly **3.5 minutes of pure table construction**
with the default fastcdc, before a single byte is chunked - which is exactly the corpus the
project brief singles out. The fix is a `Reset(io.Reader)` on the chunker so a Builder
constructs one and reuses it. This is why the chunker rows report setup separately: borg's
benchmark puts construction in the timeit setup and would never show it.

**3. borge's zstd levels collapse.** `klauspost/compress` has four encoder levels against
libzstd's twenty-two, so `zstd,16` and `zstd,22` produce identical output, as do `lzma,0`,
`lzma,6` and `lzma,9`. Visible in the ratio column, which is why it is there:

    zstd,10 (2MiB)   4.5x     zstd,16 (2MiB)   4.8x     zstd,22 (2MiB)   4.8x

Costs no interoperability - the level is metadata, and the stored `clevel` records what the
user asked for - but it does cost compression the user asked for. Recorded as
DIVERGENCES #16.

Two of these three are ordinary bugs with ordinary fixes; only the first is an argument for
cgo. None were visible from the interop gate, which is the point of having stage 9 at all.

### 12.2 Can borge be fast without cgo? Measured 2026-08-17

The question §0.4 defers to this stage: is a cgo dependency needed, or can pure Go get
there? Measured on this machine (i5-9300H, AVX2, no AVX-512), with the scratch benchmarks
described in each row.

**Three of the four largest wins are borge's own bugs, not missing libraries.**

| finding | measured | fix | needs cgo |
|---|---|---|---|
| a fresh zstd encoder per chunk | **183.7 → 871.8 MB/s (4.7x)** | reuse one encoder | no |
| a fresh chunker per file | 1.75–4.35 ms per file | `Reset(io.Reader)` | no |
| OCB's byte-at-a-time XOR | 50.2 → 72.6 MB/s (1.45x) | `crypto/subtle.XORBytes` | no |
| OCB's per-call AES ceiling | capped at ~154 MB/s | batched AES | no, but needs assembly |

**1. `compress.Zstd.Compress` calls `zstd.NewWriter` on every chunk.** A klauspost encoder
allocates window buffers and starts goroutines at construction; the library is designed to
have one encoder reused, and `EncodeAll` on a reused encoder is safe for concurrent use.
Reusing it is 4.7x on a 2 MiB buffer and the output is unchanged. This is the single
largest win available and the cheapest to take. It also explains the CLI benchmark's
`zstd,3` at 26.1 MB/s against borg's 120.8: with 10 MiB split into small pieces,
construction dominated entirely.

**2. The chunker, already recorded in §12.1.** Same shape of bug: per-item construction of
something meant to be built once.

**3. OCB's `xorBytes` is a byte loop**, called three times per 16-byte block.
`crypto/subtle.XORBytes` is assembly on every architecture Go supports. Verified
byte-identical against the current implementation across the block-boundary cases.

**4. And then OCB hits a wall that is not borge's fault.** A bare
`cipher.Block.Encrypt` on one 16-byte block costs **103.6 ns** — about 154 MB/s — because
Go's single-block API cannot pipeline AES-NI, whose instructions have ~4-cycle latency and
1-cycle throughput. Stdlib AES-GCM reaches **560 MB/s** on the same CPU precisely because
its assembly does eight blocks at once. So *any* OCB written against `cipher.Block` is
capped near 154 MB/s, against borg's OpenSSL-backed 860 MB/s.

Breaking that ceiling in pure Go means a batched AES-ECB primitive, which the standard
library does not expose. The options, in increasing order of commitment: generate AES-NI /
ARMv8-crypto assembly with `mmcloughlin/avo`; adopt a third-party pure-Go AES exposing a
bulk API; or reconsider the default mode (see §12.3).

**What is not a problem.** `lukechampine.com/blake3`, which borge already uses, measured
**930 MB/s at 2 MiB and 1199 MB/s at 64 MiB against `zeebo/blake3`'s 724 and 708** — the
suggested replacement is 1.3–1.7x *slower* here, despite advertising wider SIMD coverage,
because this CPU has AVX2 and not AVX-512. Digests are identical, so the swap would be
safe; it would also be a regression. Re-measure before adopting it for an ARM or AVX-512
target rather than taking the claim.

**The remaining gap to borg is parallelism, not implementation quality.** borg
multi-threads BLAKE3 above a size threshold and hands zstd its own thread pool; borge is
serial everywhere. That is what step 2 below already exists to fix, and fixing it lifts
hashing, compression and encryption together rather than one at a time.

### 12.3 Desktop and mobile

The goal: a laptop or phone app points borge at a directory — possibly a cloud mount — and
writes a backup to the cloud or a USB drive. What that needs, and where borge stands:

- **No cgo.** Already true: `CGO_ENABLED=0 go build ./...` succeeds today, and the port
  reaches the OS through `golang.org/x/sys/unix` rather than through C. This is what makes
  `android/arm64` and `ios/arm64` cross-compilation a `go build` away, and it is worth
  protecting — a cgo AES would cost exactly this property. See §12.4.
- **Encryption mode matters more on ARM than on x86.** ChaCha20-Poly1305 already measures
  332 MB/s against borg's 435 — 1.3x, because Go's implementation is assembly end to end —
  while AES-OCB is 17x behind. On a phone, ChaCha20-Poly1305 is the better default
  regardless of borge's OCB work, and borge is *already* near parity there. **This makes the
  mobile question largely independent of the OCB ceiling.**
- **Memory has to be bounded.** The pack writer buffers whole packs, the chunk index is
  held in memory, and `analyze` walks every archive. A phone needs `BORGE_PACK_MAX_SIZE`
  and an index that can spill. Not yet measured; it belongs in this stage's scenarios.
- **High-latency storage is already exercised.** The stage 7 Google Drive corpus runs over
  an rclone mount where a single object write measured 2.7 s, and the gate passes there.
  That is the same I/O shape a phone writing to cloud storage sees.

None of this is blocked on a C dependency. The honest summary: **borge on mobile is a
memory-bounding and packaging question, not a cryptography question**, provided the default
mode is ChaCha20-Poly1305.

### 12.4 The cgo decision, restated with numbers

§0.4 allows a cgo-gated implementation only when measurement justifies it. It now nearly
does for one thing and one thing only — AES-OCB — and even there the case is weak:

- the pure-Go fixes above are worth 4.7x, 1.45x and a per-file millisecond count, and cost
  nothing but the edit;
- parallelism is worth more than any single primitive;
- ChaCha20-Poly1305, at 1.3x, is already a reasonable default and the *right* default on
  mobile;
- and a cgo dependency forfeits the cross-compilation property §12.3 depends on.

**Recommendation: take the pure-Go fixes and the pipeline first, then re-measure.** If
AES-OCB is still the bottleneck for users who need that specific mode, write batched AES-NI
with `avo` — pure Go assembly, no C toolchain, no loss of cross-compilation. BearSSL, named
in the references, is a C library and would forfeit exactly what §12.3 needs.

### 12.5 The suggested references, checked

Claims worth checking before acting on them. Verdicts are from this machine and this
repository, not from the descriptions.

| claim | verdict |
|---|---|
| Go can replace borg's `platform/` directory without cgo | **Confirmed, and already done.** `CGO_ENABLED=0 go build ./...` succeeds. ACLs, xattrs and file flags go through `golang.org/x/sys/unix` directly. |
| `github.com/pkg/xattr` is needed for extended attributes | **Not needed.** borge calls `unix.Lgetxattr`/`Lsetxattr` directly — one fewer dependency than suggested. |
| `unix.SyncFileRange`, `os/user` cover the rest | Correct; both are in use or available. |
| `zeebo/blake3` is faster (AVX-512/AVX2/SVE2/NEON) | **False here.** 1.3–1.7x *slower* than the incumbent on AVX2 hardware. Digests match, so the swap is safe but would regress. Re-measure per target. |
| `mmcloughlin/avo` for generating x86 assembly | **The right tool** if borge writes batched AES-NI. Pure Go, no C toolchain, keeps cross-compilation. |
| `Yawning/bsaes` (bitsliced constant-time AES) | Relevant for constant-time guarantees and for CPUs *without* AES instructions; it is slower than AES-NI, so not a speedup on the targets measured. |
| BearSSL | A C library: cgo, and forfeits the cross-compilation §12.3 depends on. |
| `ProtonMail/go-crypto` (OpenPGP fork of x/crypto) | Not applicable; borge uses no OpenPGP. |

The headline claim — that a Go port needs no C for the platform layer — is not just
plausible, it is the state the port is already in. The reference is if anything
conservative about how few dependencies that takes.

Then, in order:

0. **Fix the chunker-per-file construction** (finding 2). It is a bug, not a tuning
   question, and it makes every later measurement of the small-file corpora wrong.
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

### 13.1 borg quirks to fix once compatibility is lifted

Until this stage, borge reproduces borg's behaviour including its bugs — a port that
"fixes" one silently is a port whose output no longer matches, which is the one thing the
interop gate exists to prevent. Each of these is a place where the compatible behaviour is
worse than the obvious one. The list is collected here so that lifting the constraint is a
review of known items rather than a fresh audit.

**Reproduced bugs, to be corrected:**

- **`shellpattern.translate`'s vacuous guard.** borg's `(`, `|` and `)` passthrough checks
  `pat[i-1] != "\\"` *after* `i` has already advanced, so the guard always passes and
  `\(` becomes a backslash plus a group opener rather than a literal parenthesis. borge
  reproduces it (see §6 and `internal/patterns`). A user cannot currently match a filename
  containing a literal `(` with an `sh:` pattern. Fixing it changes which files a pattern
  selects, which is why it waits.
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
  against *borg's*, not against the original tree, precisely because borg's restore does
  not reproduce everything it stored — `bsdflags` are read and preserved but never applied
  (DIVERGENCES #8), and `--sparse` restores holes only at chunk granularity
  (DIVERGENCES #9). Once compatibility is lifted, "restore reproduces the source" becomes
  an achievable gate rather than an aspiration.
- **Item decoding is lossy for unknown keys** at the `Item` struct boundary, which is why
  `debug dump-archive` reads the raw msgpack instead. A format borge owns can make the
  round trip total.

### 13.2 Large directories must not slow restore down

This is the requirement the project brief opens with, restated as a gate rather than an
aspiration: **restoring a directory of 118,866 files must not cost more per file than
restoring a directory of 100.**

borg reads a backup sequentially and recreates the tree as it goes, the way `tar -x` does.
Anything worse than linear in that path defeats the intent. What is known so far:

- The **directory-attribute stack** in `internal/archive/extract.go` is O(1) amortised: it
  pops a directory when the next path leaves it, rather than searching. It does allocate a
  string per item for the prefix comparison, which is 118,866 allocations on the
  pathological corpus and trivially removable.
- The **chunker-per-file construction** (§12.1) is a per-file millisecond cost on the
  *create* side, worth ~3.5 minutes on that corpus alone.
- **Restore-side ordering** is item 1 above and is the one with real headroom.

Stage 9 measures all three before anything here changes the format. The point of writing
them down together is that only one of them needs a format change, and it is not the
expensive one.

**Gate:** a migration path exists and is tested (borge reads the old format, converts,
verifies); the change is justified by benchmark JSON in the evidence bundle; and the
pathological-directory scenario shows per-file restore cost flat against directory size.

---

## 14. Stage tracker

| Stage | Description | State | Evidence bundle |
| --- | --- | --- | --- |
| 0 | Foundation, licensing, borg-2 venv, format reference | **done** 2026-08-16 | `borge-stage-0-20260816T163704Z.zip` |
| 1 | Primitives: msgpack, compress, crypto, chunker, item, hashindex | **done** 2026-08-16 | per-substage bundles |
| 2 | `store` (borgstore port, posixfs) | **done** 2026-08-16 | `borge-stage-2-*.zip` |
| 3 | `repoobj` + `repository` + packs + locking | **done** 2026-08-16 | `borge-stage-3-*.zip` |
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
