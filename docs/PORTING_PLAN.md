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
- `borg transfer` from borg 1.x repos (depends on the above). Transfer *between borg 2
  repositories* is a different thing and **is** in scope — decided 2026-08-18, §11.1 — so
  this line rules out the `--from-borg1` / `--upgrader=From12To20` path, not the command.
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

### 2.1 Doc anchors: tying help text to the code that implements it

**The problem this solves.** Four documentation claims went false during stage 8 while the
code around them was correct: the placeholders topic (twice, in opposite directions), the
stage tracker, and `borge help environment` telling users that borge never prompts for a
passphrase — after prompting had been implemented. The two that were caught automatically
had tests behind them. The two that needed a human to notice were prose.

The cause is structural rather than careless: the sentence lives in `internal/cli/help.go`
and the behaviour lives in `internal/cli/passphrase.go`, so a change to one does not put
the other in the diff. Reviewing a change cannot catch a claim the reviewer never sees.

**The method.** Put the user-facing prose in a Go doc comment *on the declaration that
implements it*, and mark it with a directive that names where it belongs. Go's own doc
tooling makes this work: a comment line matching `^//[a-z0-9]+:[a-z0-9]` is a **directive**,
so it is stripped from `go doc` output and from `ast.CommentGroup.Text()`, while remaining
readable from `CommentGroup.List`. Verified 2026-08-17 against Go 1.26 before designing
around it.

Four anchors:

| directive | meaning |
| --- | --- |
| `//borge:doc user` / `//borge:doc api` | which documentation subset this comment belongs to |
| `//borge:help <topic>[/<section>]` | this comment is the source of that help topic or section |
| `//borge:enumerates <expr>` | the comment lists a set the code defines; the list is **generated**, not written |
| `//borge:claim <id>` | this comment makes a behavioural claim checked by a registered test |

```go
// unlockWithPrompt opens a repository's key, asking for the passphrase when the
// environment did not supply a working one.
//
// The environment is tried first and a terminal is asked only on failure, up to three
// times, with echo off. Only a repository that actually has a passphrase can produce that
// failure, so the unencrypted modes never prompt.
//
//borge:doc user
//borge:help environment/passphrases
//borge:claim prompts-only-on-tty
func (e *Env) unlockWithPrompt(...) { ... }
```

**Three grades of verification, reported honestly.** The point is not to pretend prose can
be tested; it is to make the untested share *visible and small*.

- **Generated** — produced from code: enumerations, flag lists, environment variables.
  Cannot drift, because there is one source.
- **Claimed** — prose linked by id to an executable check. Drifts only if the check is
  deleted, which the audit catches.
- **Unverified** — everything else. Permitted: "this exists because the key type is not
  known until the manifest is read" is rationale, not a testable assertion. But it is
  **counted**, so the gap is a number rather than an assumption.

**The pipeline.**

```
doc comments with anchors
   ├─ docaudit          report: grade coverage, anchors naming topics that do not exist,
   │                    claims with no check, checks with no claim
   ├─ docgen --help  →  internal/cli/help_generated.go   (the "user" subset)
   └─ docgen --api   →  docs/INTERNALS.md                (the "api" subset)
```

with `TestDocsAreCurrent` re-running the extraction in memory and diffing — the standard Go
generated-code freshness pattern, so an edit to a doc comment without regeneration fails
the build rather than shipping.

**Two design points that are not obvious.**

- **Topic structure needs a template, not concatenation.** Assembling a topic by
  concatenating comments in source order makes the document's shape depend on file order,
  which is fragile and unreviewable. Each topic gets a small template naming the fragments
  it wants, in the order it wants them; `docgen` interpolates.
- **Mixing audiences in one comment is the real risk.** A doc comment already serves the
  maintainer, and user-facing prose has a different register. Keep the user paragraph as its
  own block marked `//borge:doc user`, and leave rationale unmarked — a comment that tries
  to be both usually does neither well.

**Why this and not the alternatives.** Generating help from a separate data file keeps the
drift, just in a new location. Testing the rendered text against golden files pins what the
text *is*, not whether it is *true*. Colocation is the mechanism: it puts the user-visible
sentence into the diff of the change that falsifies it.

**Work items** (not started; sized deliberately so the first is useful alone):

1. **`docaudit`** — a read-only tool and a test. Parse anchors, report the three grades per
   topic, fail on a `//borge:help` naming a topic that does not exist and on a
   `//borge:claim` with no registered check. No generation yet. This alone makes the
   existing hand-written topics auditable and would have caught the prompting claim, because
   the sentence would have carried a claim id with no check behind it.
2. **`//borge:enumerates`** — convert the lists that are already checked ad hoc
   (environment variables, pattern styles, compression specs, placeholders,
   `TestHelpEnvironmentTopicListsEveryVariable` and `TestHelpTopicsCoverTheCode`) into
   generated fragments. Deletes those bespoke tests in favour of one mechanism.
3. **`docgen --help`** plus per-topic templates and `TestDocsAreCurrent`; move the five
   topics out of `help.go` string constants and into anchored comments.
4. **`docgen --api`** → `docs/INTERNALS.md`. Lowest value of the four: borge has no exported
   API — everything is under `internal/` — so this is maintainer documentation that
   `go doc ./internal/...` already serves. Do it last, or not at all.
5. **`doccheck`** — the contradiction pass of §2.1.1, over `//borge:doc user` blocks only.
   Build the five-case calibration set from git *first*, then the checker. Advisory output,
   not a gate. Worth doing after item 1 and independently of items 2–4: it needs the anchors
   for pairing, and nothing else.
6. **`TestHelpExamplesRun`** — §2.1.2(a). **Done 2026-08-18**, first of the seven, in
   `internal/cli/help_examples_test.go`. 25 commands from the five topics: 23 run against a
   scratch repository, 2 are prose fragments marked unrunnable with the reason. It found
   two more divergences on its first day (#21, #22) and the discipline that made it worth
   the effort is in §2.1.2 below.
7. **`docactionable`** — §2.1.2(b). Generate a command from each topic and run it. Advisory.
   Last, because it depends on item 6's scratch-repository harness for execution.

**Gate:** `docaudit` reports zero dangling anchors and zero orphan claims; every help topic
has a grade breakdown recorded; and the unverified share is stated in the plan rather than
discovered later. Item 6's own gate is already in place: every command in every topic has
an entry saying what it should do, in both directions.

#### 2.1.2 The strongest check: run the examples, and generate them from the prose

For an API, a code example carries the information. For a command-line tool the equivalent
is a **command line or a configuration**, and it has the property prose does not: it can be
executed.

That reframes what help text is for. The useful question is not *is this sentence true* but
**does this sentence tell me specifically what I can and cannot do**. A true sentence can be
useless — "borge supports patterns" is unfalsifiable and unactionable. The test for
specificity is constructive: **try to produce a working command from the prose alone.** If
you cannot, the prose is too vague, and that is a finding about the documentation even
though nothing in it is false.

Two mechanisms, in order of how cheaply they pay:

**a. Execute every example in the help text.** They are already written; they are already
shipped; nothing checks them. Extract the indented `borge …` lines from each topic,
substitute the placeholders (`REPO`, `ARCHIVE`, `~`) against a scratch repository built for
the purpose, run them, and require the documented exit status. Examples that must not be
run destructively use `--dry-run` where the command has one, and are marked otherwise.

**This was tried by hand on 2026-08-17 and found two wrong out of fifteen**, one of which
was not a documentation bug at all:

- `borge find --pattern 'sh:…'` — `--pattern` needs an action prefix; the documented form
  is an error.
- `borge create -r REPO archive ~ --exclude 'sh:**/.cache'` — **the exclusion silently did
  not happen.** Go's `flag` stops at the first positional, so the option became two more
  paths to archive. borg's archive had 0 `.cache` entries; borge's had 2. Recorded as
  DIVERGENCES #20 and **fixed on 2026-08-18**; the topic now promises both orders mean the
  same thing, and both are executed.

A defect that stores data the user asked to exclude was sitting in the help text, and
running the examples is what surfaced it. That is the argument for this item over every
other item in §2.1.

**b. Generate a command from the prose, then run it.** The specificity test, mechanised: give
a model the topic and nothing else, ask it to produce the command line that does what the
topic describes, and execute the result. A topic that yields a working command is
actionable. A topic that yields a command which fails to parse, or does something else, is
vague or wrong — and the generated attempt is itself the bug report, because it shows what a
careful reader concluded.

This is stronger than the contradiction check of §2.1.1 and subsumes part of it: a claim
that cannot be turned into a command is one the reader cannot act on, whatever its truth.
It is also non-deterministic and advisory, for the same reasons, and it wants the same
calibration discipline — the two examples above are the first known-answer cases.

**Built 2026-08-18** as `internal/cli/help_examples_test.go`. What it actually took, since
none of it was obvious from the description above:

- **Exit status is not the assertion.** `borge list ARCHIVE 're:…'` exits 0 whether it
  matches the right files, the wrong files or nothing at all. Every entry checks what the
  command *did*: which paths the archive holds, which files landed on disk, which archives
  survived a `delete`. A table of expected exit codes would have passed on half the bugs
  below.
- **The fixture is built so that every example has something real to act on** — an archive
  literally named `ARCHIVE`, one matching `sh:daily-*`, one tagged `temporary`, one named
  after this host. An example that matched nothing would exit 0 and prove nothing. That is
  the same vacuity trap as everywhere else in this port, arriving by a new route.
- **Each example gets its own repository, rebuilt from scratch** (0.26 s). Order cannot
  matter, and the destructive examples — `delete`, `prune` — run for real rather than with
  `--dry-run`, which would test something other than what is documented.
- **The commands quoted in prose are in the table too**, not only the indented examples.
  Fragments like "that is why `borge repo-compress` exists" are marked unrunnable with the
  reason. Listing them rather than skipping them is what caught the third broken example.
- **Substitutions are the risk.** Each one is a step away from running what the user reads,
  so each carries its reason in the source. One of them — replacing `REPO` wherever it
  appeared — corrupted `BORGE_REPO=…` into nonsense, so whole-token and substring rules are
  now distinguished.

**What it found on its first run**, beyond the two already known:

- `borge tag ARCHIVE --add @PROT`, in the match-archives topic, **fails**: "tag needs an
  archive". Divergence #20 again, in a command quoted in prose rather than set out as an
  example. Corrected to `borge tag --add @PROT ARCHIVE`.
- The environment topic carried **no example at all**. It now has two, and they are the
  ones worth having: `BORGE_REPO=…` replacing `-r`, which every other topic's examples
  silently depend on, and `BORGE_UNITS=iec`, whose effect nothing tested.
- **DIVERGENCES #21** — a relative source path is archived under its absolute path. Found
  because the patterns topic's `sh:home/me/**/*.txt` could not be made to match, and the
  pattern was not the reason.
- **DIVERGENCES #22** — a repository path must be absolute, where borg accepts a relative
  one. Found on the first attempt to run `-r REPO` verbatim.

**Two mutation checks**, because a test that cannot fail is worse than none. Breaking an
example in a topic fails in both directions at once (the command has no entry; the entry
has no command). Breaking the *code* — making `BORGE_UNITS=iec` return SI units — fails the
environment example, which is the case a doc-only test would have missed.

**A fourth grade.** §2.1's grades gain one, and it sits at the top:

- **Executed** — the prose carries an example that is run in the test suite.
- Generated · Claimed · Unverified, as before.

The target is that every help topic carries at least one executed example, because that is
the grade a user actually relies on: they copy the example.

#### 2.1.1 Attacking the unverified share: does the code contradict the prose?

The three grades leave a bucket that no test reaches — prose that is neither generated nor
claim-linked. The proposal for it: read the anchored code *independently*, then ask whether
that reading contradicts the user-facing sentence, and put the disagreements in front of a
human. This is the only technique here that touches prose as prose.

**Two corrections to the obvious form of it, both load-bearing.**

**Similarity is the wrong measure.** The instinct is to explain the code, embed both texts
and threshold the cosine distance. That would probably have missed the very bug that
motivated this. The false claim was *"borge does not prompt for a passphrase"*; an accurate
explanation of `unlockWithPrompt` says *"prompts for a passphrase, up to three times"*.
Those two sentences are **highly similar** by any embedding measure — negation moves an
embedding very little — so a similarity threshold scores the pair as agreeing. The useful
question is not *are these alike* but **does the code contradict the claim**: entailment,
with three outcomes (supported / contradicted / not determinable), which handles negation
because contradiction is what it is built to detect.

**The two-step is right, for a reason worth stating.** It is tempting to collapse it — show
the model the code and the claim together and ask "is this true?" — but that anchors the
reading on the claim, and a model shown an assertion tends to find support for it. So:
generate the explanation **blind**, with the doc comment withheld, and only then compare.
The lossy extra hop buys independence, which is the whole value.

**Scope matters more than it looks.** `unlockWithPrompt` reads as "prompts in a loop"; that
echo is disabled lives one call down in `promptPassphrase`. Explaining a declaration in
isolation produces confident, incomplete readings. The unit is the declaration plus its
direct callees within the package, to a token budget — and a claim that needs more than that
is a claim that should be anchored somewhere else.

**Exclude rationale.** "This exists because the key type is not known until the manifest is
read" is not entailed by any code and never will be. Checking it produces permanent
*not determinable* noise that trains everyone to ignore the report. Only blocks marked
`//borge:doc user` are checked; rationale stays unmarked and unchecked.

**Advisory, never a gate.** The check is non-deterministic and cannot fail a build
honestly. It emits a triage list — claim, anchor, verdict, the reading that disagreed — for
review by whoever is making the change, and by the human co-author. A *contradicted* verdict
means "look at this", not "this is wrong".

**Calibrate it, or nobody will believe it.** A checker with no known-answer set is a checker
whose silence means nothing. Stage 8 supplies real labelled cases, which is unusual luck:

| case | expected verdict |
| --- | --- |
| `help.go` before `094e7b4`: "borge does not prompt" vs `unlockWithPrompt` | **contradicted** |
| the same topic after `094e7b4` | supported |
| the placeholders topic before `1a97426` ("borge does not substitute") vs `internal/placeholders` | **contradicted** |
| the placeholders topic after `1a97426` | supported |
| any rationale paragraph | not determinable |

Run the checker against those five before trusting it on anything else. A version that
cannot separate the before-and-after pairs is not ready, and the pairs are cheap to keep as
a regression suite because they are recorded in git.

**Honest limits.** An independent reading can share the author's wrong assumption and agree
with a false claim — correlated error, not eliminated by any of the above. It cannot see
behaviour that emerges across packages. And it will produce false alarms on prose that is
true but distant from the code's shape, which is the cost of catching the ones that matter.
It reduces the unverified bucket; it does not empty it.

### 2.2 Porting discipline, per module

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

### 2.3 Check what the code does with *absence*

Four defects in stage 8 were the same defect. In each, the feature itself was right and its
handling of an empty, missing or unusable input was wrong — and every one of them read the
user's explicit input as "nothing was given" and then **reported success**:

| what was given | what borge did | what borg does |
| --- | --- | --- |
| `create A ""` — an unset shell variable | archived the working directory (`filepath.Clean("")` is `"."`) | exit 2 |
| `repo-list --newer ""` — likewise | listed every archive, exit 0 | exit 2 |
| a patterns file whose only root is `R PATH` | "create needs at least one path" | archives the root |
| `--exclude` written after the paths | archived what was excluded, exit 1 | excludes it |

None of these is exotic. `--newer "$SPAN"` with `SPAN` unset is an ordinary shell mistake,
and it is the one where a filter that silently matches everything is least likely to be
noticed — a backup script that then deletes what it "found" is the shape of the accident.

**So this is now a review step of its own, not something to be stumbled on.** For every
option and argument added or ported, ask the three questions and write the answer down as a
test:

1. **Empty is not absent.** Can the option be *given* an empty value, and is that
   distinguishable from not giving it? Go's `flag` makes them look identical for a string
   option; a `flag.Value` that records `set` makes them different. `internal/cli/repo.go`'s
   `timespanFlag` is the pattern.
2. **Absent must not mean "everything" without saying so.** A filter that fails to parse
   and defaults to no filter, or a selector that matches nothing and is treated as matching
   all, is the dangerous direction. Prefer the failure that stops the command.
3. **Parsed input that reaches nothing is still an answer.** `R` roots were parsed and
   dropped; the pattern order was collected and regrouped. Grep for where the value goes,
   not just where it is read.

The recurring shape is worth stating plainly: **a silent no-op looks exactly like success,**
which is why none of the four was caught by the seven stages of differential testing that
preceded them. They were caught by running a command and looking at the result.

**Silence is an answer nobody can act on.** The three questions above are about input; there
is a fourth about output, and `delete --dry-run` is the live example. Without `--list` it
prints *nothing* — in borg and now in borge — so "two archives would be deleted" and
"nothing matched your selector" look exactly alike, and the whole point of a dry run is to
decide something from what it says. Inherited silence is still silence: a command whose
purpose is to inform has to be readable without a second option, or has to say which option
to add. borge does both. The `--dry-run` help on `delete` and `undelete` names `--list`, *and* a
dry run prints a summary of what it would do (DIVERGENCES #31) — because output that says
what happened, including that nothing did, is what the user is there for. The divergence is
scoped to dry runs, where borg prints nothing at all: every real path stays byte-identical,
because that output has a format scripts parse and a dry run's has none.

**The asymmetry that falls out of this.** Applying the rule to selectors produced a policy
worth naming: a **write** command whose filter matched nothing is an error (DIVERGENCES
#28), a **read** command's is not. Asking to list a set that turns out to be empty has been
answered; asking to change one has not. borg exits 0 for both, so this is a deliberate
divergence and the only one §2.3 has produced so far.

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
| 9 | rows 1–2 with a **relative** source path | added 2026-08-18 |

**Row 9 and why it exists.** Rows 1–8 all pass an absolute source path, and that is a blind
spot rather than a choice: borge resolved every path to an absolute one before storing it,
which is a no-op when the path is already absolute, so the matrix could not have caught
DIVERGENCES #21 however long it ran. Row 9 names the tree relatively, from the directory
above it, and requires the round trip — the archive one tool wrote has to extract, in the
other tool, to the same tree — as well as requiring the stored names to be the relative
ones, because an archive holding the absolute path would still extract and still compare
equal.

Writing it taught something about the corpus, twice. The name assertion first split
`list --short` on whitespace, and the synthetic tree contains a filename with a space; then
it split on lines, and the tree contains a filename with a **newline**. Any parsing of
`--short` is wrong for real data; `--json-lines` is what to read.

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
> Current state (2026-08-18): 31 implemented, 5 absent with a recorded reason, 0
> unexplained. Of the five, three are non-goals (`mount`, `umount`, `webdav`, §0.6); the
> other two are `serve` and `transfer`, and `transfer` is **decided as of 2026-08-18**: it
> is in scope for borg 2 to borg 2. The table below is the whole of what stage 8 still
> owes, commands and otherwise; the notes after it give each row its reasoning.

#### What stage 8 still owes — as at 2026-08-18

The stage is not bundled until this table is empty of everything but the last row. Each
state below was measured on the date shown rather than carried forward from the prose: two
entries in this section had already outlived their defect by the time the table was built,
which is the fourth and fifth time that has happened in stage 8, and the reason for having
a table at all.

| # | Item | Recorded in | State |
| --- | --- | --- | --- |
| 1 | `serve` and the remote backends — `sftp`, `rest`, `s3`, `rclone` | §11 | not started; the largest single item |
| 2 | `transfer` borge→borge, `repo-create --other-repo`, `BORGE_OTHER_PASSPHRASE`, the relatedness guards | §11.1 | decided 2026-08-18; four work items, none started |
| 3 | 35 missing per-command options | §11.2, `option-coverage.sh` | measured, down from 111. Largest: `create` 6, `prune` 4, `recreate` 4, `check` 3, `diff` 3, `extract` 3, `repo-create` 3 |
| 4 | `recreate`'s exclusion group — `--exclude-caches`, `--exclude-if-present`, `--keep-exclude-tags`, `--filter` | §11.2 | part of row 3, listed apart because it is one feature over four options and needs the item-stream walk rather than the filesystem one |
| 5 | ~~JSON API: `repo-info`, `info`, `version`, `analyze` schemas~~ | §11.4b | **done 2026-08-19.** All eight of borg's `--json` commands now match, held by `TestJSONSchemaMatchesBorg` |
| 6 | ~~`--log-json`~~ | §11.4b | **done 2026-08-19.** Registered on every command through `newFlagSet`, borge's equivalent of borg's common parser. All of stderr becomes JSON, not only the messages borge thought to convert |
| 7 | ~~Non-unicode paths in JSON~~, and the `--json-lines` schemas | §11.4b | **done 2026-08-19.** Two representations, not one - the framing "should be one implementation" was wrong, see DIVERGENCES #43. Fixing it uncovered three further differences in `find`, `diff` and the item object |
| 8 | ~~`bsdflags` capture and apply; `xattrs` empty key; `--noflags` doing nothing~~ | DIVERGENCES #8 | **done 2026-08-19.** `flags_linux.go`; both keys now record "examined"; a flag borge stores survives a restore by borg |
| 8b | ~~Attribute-based exclusion: nodump, and the two backup-exclusion xattrs~~ | DIVERGENCES #39 | **done 2026-08-19**, the same day it was found. Checked before content is read, and an excluded directory ends the walk into its subtree |
| 9 | ~~Option gate: the reverse direction, and subcommands~~ | §11.4 work 1–2 | **done 2026-08-19.** Both directions, group subcommands included, and the common-option comparison fixed — it had been reporting `-r` and `-h` as absent when borge has both |
| 10 | Every borge-only option documented as borge-only in its help text | §11.4 work 3 | known: `prune --keep-within/--keep-last/--keep-oldest`, `extract -C`, `version --long`, `--reverse`, `delete --force` |
| 11 | `--reverse` and `--deleted` decided per command | §11.4c | they reach every command using `listSelectors`; borg enables `--deleted` per command |
| 12 | `--format` on `check` and `diff` | §11.3 | `diff` needs a third key set, its records being changes rather than paths; `check` needs its output reworked first |
| 13 | Progress output on stderr, where borg puts it | measured 2026-08-18 | `create`, `prune`, `export-tar` done. `borg check -v` writes 526 bytes to stderr and none to stdout; borge writes 158 to stdout and none to stderr. `compact`, `recreate`, `analyze`, `repo-compress`, `repo-space` and `lock` write to stdout and have not been measured against borg |
| 14 | `debug convert-profile` | DIVERGENCES #14 | the only `debug` subcommand not ported |
| 15 | **Stage 8 evidence bundle** | §11 | last, and only once rows 1–14 are closed |

Closed since this section was first written, kept here because how each was *found* is
worth more than the fix: argument permutation (#20), relative source paths (#21), the
slashdot hack (#24), patterns-file roots (#25), archive-name placeholders (#17), the
`--json` surface and four of its schemas (#35), the archive size accounting (#36),
`import-tar`'s command line (#37), and `prune`'s listing — which §11.3 below still
described as outstanding until this table was built.

- **`serve`** and the remote store backends: `sftp`, `rest`, `s3`, `rclone`.
- **Item fields borg writes and borge does not: `bsdflags` and `xattrs`** (DIVERGENCES #8).
  Filed as "bsdflags restoration, to be closed before the stage 7 gate" and not closed; the
  gate passed because nothing in the corpus carries a flag, which makes it a second example
  of a gate measuring only what its corpus happens to contain. Measuring the two tools'
  item streams on 2026-08-18 showed the entry understated it in one direction and
  overstated it in another, so it is restated here:

  **`bsdflags` is not stored either.** The entry says the flags are "read, stored and
  round-tripped", which is true only of an archive *borg* wrote: borge decodes the field
  and re-encodes it, but nothing in the tree calls `FS_IOC_GETFLAGS`, so a borge-made
  archive has no flags in it to restore. Two halves of work, then, not one — capture and
  apply — and the second must run last of all attribute restoration, since the immutable
  flag makes every other change impossible.

  Which makes **`--noflags` an option that does nothing**, the case `AGENTS.md` warns
  about: it is registered, parsed and carried all the way into `CreateOptions.NoFlags` and
  `ExtractOptions.NoFlags`, and no code reads either field. It suppresses a capture that
  never happens. It should either work or be removed before the stage 8 bundle.

  It is also why the `flags` key added to the item JSON on 2026-08-18 is permanently null
  for a borge-made archive where borg sends `0` — "not recorded" against "no flags set".

  **`xattrs` is nearly right and differs on the empty case.** borge reads extended
  attributes at create and writes them back at extract; what it does not do is write the
  key when there are none, where borg writes an empty dict on every item unconditionally
  (`archive.py`, `stat_ext_attrs`). Restores are identical either way, so this is not a
  correctness gap, but it is stored bytes: the two fields together are about 18 bytes an
  item, and they are why two archives of the same tree have item streams of different
  sizes and only same-source size comparisons come out exact (#36).

  Doing both at once is the cheaper order — they are the same two lines of
  `stat_ext_attrs`, they are measured by the same comparison, and either alone leaves the
  item stream still differing from borg's.

  **Done 2026-08-19**, and it uncovered a larger gap immediately: borg does not archive a
  file carrying the nodump flag at all, nor one marked with either of two backup-exclusion
  xattrs, and borge archives all of them (DIVERGENCES #39, table row 8b). That rule reads
  exactly the two fields this row was about, so it could not have been found — or fixed —
  before this landed.
- **`debug convert-profile`** (DIVERGENCES #14), the only `debug` subcommand not ported.
- **`transfer`** between two borg 2 repositories — **in scope, decided 2026-08-18.** With
  it, `repo-create --other-repo` and a `BORGE_OTHER_PASSPHRASE` variable, which it cannot
  work without. Design and the accuracy notes behind the decision are in §11.1.
- ~~**Archive-name placeholders**~~ (DIVERGENCES #17). **Done 2026-08-17**,
  `internal/placeholders`. Not a command, so the coverage gate never saw it; it was found
  by writing `borge help placeholders` from borg's behaviour and then running the command.
- ~~**Options must be accepted after positional arguments**~~ (DIVERGENCES #20).
  **Done 2026-08-18**, `internal/cli/args.go`. A `flagSet` wrapping `flag.FlagSet` permutes
  the arguments in its `Parse`, so no call site changed. Three things carried the work:
  whether an option takes a value is asked of the `FlagSet` rather than guessed (`-e` means
  different things to `create` and `repo-create`, and `--keep-daily -1` must keep its
  argument); `--` ends the options and is re-emitted ahead of the positionals so a
  dash-leading path survives; and `with-lock` opts out, because permuting `sh -c '…'` would
  take the `-c` for borge's own. A mistyped option is now an error rather than a filename,
  which is argparse's behaviour and the reason the old defect was invisible.
- ~~**A relative source path must be stored as typed**~~ (DIVERGENCES #21).
  **Done 2026-08-18.** `filepath.Abs` is gone; each root is cleaned, the walk joins with
  `filepath.Join`, and `archivedPath` is borg's `remove_dotdot_prefixes`. An empty path is
  refused outright, as borg's argument parser refuses it, rather than cleaning to `"."` and
  archiving the working directory. `TestRelativeSourcePathRoundTrip` is the interop row the
  matrix was missing — every other row passes an absolute path, and absolutising an
  absolute path is a no-op, so the gate could not have caught this however long it ran.
- ~~**The rsync slashdot hack**~~ (DIVERGENCES #24). **Done 2026-08-18.** `stripPrefix`
  reads it from the path as typed, before cleaning removes the `.` with the instruction;
  `walker.storedPath` is borg's three-case `create_helper`. Patterns match the **walked**
  path rather than the stored one, which is the half that is easy to get backwards, so the
  test asserts the negative case as well as the positive.
- **Per-command options.** The command gate compares *command names* and nothing else, so
  every missing option was invisible to it. **The gate for this is built**
  (`tests/evidence/option-coverage.sh`, `make option-coverage`, 2026-08-18) and the numbers
  below now come from it rather than from a paragraph. See §11.2 for what it measures and
  what it deliberately does not.

  **Baseline: 254 of borg's command-specific options, 111 of them missing**, plus 19 common
  options of which 15 are absent, plus 17 places where borge has the option but not a
  spelling borg also offers (`-n` for `--dry-run`, `-s` for `--stats`, and prune's
  `-d/-H/-m/-w/-y`).

  **The highest-leverage finding was that the missing options cluster**, and acting on it
  is what the count above measures. `--newer`, `--newest`, `--older` and `--oldest` were
  absent from eight commands; `--format` from six; `--sort-by` from four. These are borg's
  shared archive-filter and formatting groups, registered once and attached to many
  commands.

  - ~~**The four relative date filters**~~ — **done 2026-08-18**, on `listSelectors`, which
    every command taking archive filters already used. **111 → 79 missing**: thirty-two
    options across eight commands from one change, which is the argument for doing the
    other groups the same way rather than command by command.
  - **`--format`** on `list`, `repo-list`, `prune`, `check`, `diff`, `find` — borg's
    placeholder formatter. **Started 2026-08-18**: `internal/formatter` implements the
    Python format-spec subset borg's own templates use (fill, alignment, width,
    precision), and `repo-list` is wired to it, with `BORGE_REPO_LIST_FORMAT`. The five
    remaining commands are wiring plus their key sets.

    **`list` and `find` followed the same day**, with borg's *item* key set in
    `internal/cli/itemformat.go` and `BORGE_LIST_FORMAT` / `BORGE_FIND_FORMAT`. **61
    options left.** Still to do: `diff`, whose items are change records rather than paths,
    and `prune` and `check`, which need more than the option — borge prints its own
    listing layout there where borg prints a formatted template, so wiring `--format`
    means reworking the output and its differential at the same time. Both of borg's
    templates for those two also lack a trailing `{NL}`, the command supplying the newline
    itself; porting them with one appended puts a whole listing on one line.

    Not ported with the key set: `fingerprint` and the content hashes (`md5`, `sha256`,
    `blake3`…). Those read every chunk of every file, turning a listing into a restore's
    worth of I/O — borg guards them behind `format_needs_cache` and a warning. Asking for
    one is an error naming the available keys, not an empty column.

    §11.3 records why this is worth doing at all, why borge's two bracket systems are two
    packages, and why no third-party template or strftime dependency is taken.

    The reason `repo-list` came first is not that it is easiest. Its default column layout
    was a `Printf` with a comment above it *quoting* borg's default template as though the
    two were the same thing — the same shape as the `patternFlags` comment that described
    an intention the code did not implement. The default is now that template, so the
    columns a user sees without `--format` come from the same code path as the ones they
    see with it.
  - **`--sort-by`** where `listSelectors` does not already reach it (`info`, `tag`, `diff`).
  - ~~**`info` and `tag` do not use `listSelectors` at all**~~ — **done 2026-08-18**, and
    both are now complete against borg. **79 → 64.** It was not only options: borg's `info`
    describes the *set* of archives the filters select, and borge described exactly one and
    refused to run without a selector; borg's `tag` acts on the whole selection, and with
    no selector at all on every archive in the repository. Both now match. Declined on the
    way: borg's variadic `--add [TAG ...]`, whose greedy parsing turns
    `borg tag --add Z a2` into "add the tags Z and a2 to every archive" — DIVERGENCES #27.

  The rest is genuine per-command work, `create` being the largest. **Tag-based exclusion
  landed 2026-08-18** — `--exclude-caches`, `--exclude-if-present` and
  `--keep-exclude-tags`, the CACHEDIR.TAG protocol — taking `create` from 23 to 20 and the
  total to **58**. **`--timestamp` followed** on `create`, `recreate` and `import-tar`
  — three commands, one parser — taking the total to **55**, and **`create --dry-run`**
  after it, to **54**, and the **timestamp-storage group** (`--atime`, `--noctime`,
  `--nobirthtime`) to **51**, and **`--list` on `delete`, `undelete` and `export-tar`** to
  **48**, and the **`--paths-from-*` group** (`--paths-from-stdin`,
  `--paths-from-command`, `--paths-from-shell-command`, `--paths-delimiter`) to **44**.
  and the **stdin-content group** (`--stdin-name`, `--stdin-mode`, `--stdin-user`,
  `--stdin-group`, `--content-from-command`, and `-` as a path) to **39**. `create` is down
  from 40 missing to 6: `--sparse`, `--tags`, `--filter`, `--files-changed`,
  `--exclude-dataless`, `--read-special-timeout`.

  The stdin group is where a port drifts silently, because there is no file on disk and
  every piece of metadata is invented. Two details had to be measured: borg sets **all
  three timestamps to the moment of the backup**, whatever `--atime` and `--noctime` say —
  those options are about copying an inode, and a pipe has none — and a failing
  `--content-from-command` **fails the backup**, because a truncated dump stored as a
  complete one is the worst outcome the feature has.

  The paths-from group is not another way to name paths, and two consequences of borg's
  "all control is external: no more, no less" are easy to miss. A directory in the list is
  archived **without its contents**, and the include/exclude patterns are **not applied at
  all** — measured against borg, not taken from the sentence. `CreateOptions.PathsOnly`
  carries both, and it also skips the CACHEDIR.TAG rules for the same reason: the caller
  has already decided.

  The `--list` work came with two changes to output borge already had, both asked for and
  both measured against borg first. **`-v` no longer lists archives**: borg's `-v` is a log
  level and prints exactly what a plain run does, with per-archive lines only under
  `--list`; borge printed its own line under `-v` and a different one under `--dry-run`, so
  one event had three shapes and none was borg's. And **`delete` now ends with borg's
  `Done. Run "borge compact" to free space.`**, which borge omitted entirely — the sentence
  that stops a user wondering why the disk did not shrink, and which `prune` already had.

  The timestamp group turned up a defect worth more than the options: **borge stored an
  access time borg leaves out**, on every item, so every archive was bigger than borg's for
  the same tree and *changed when a file was merely read*. DIVERGENCES #30. The stage 7
  comparator could not have caught it — it checks mtime and deliberately excludes atime and
  ctime from the restore contract — which makes it the third gate in this stage found to be
  measuring only what it looks at.

  `--dry-run` needed a second change to be worth anything: **`--list` never reported
  exclusions.** borg prints a `-` line for every excluded path; borge printed nothing, so
  `--list --exclude` showed only what was kept and could not confirm the exclusion had
  happened — which is exactly the workflow a dry run exists for. That is the §2.3 shape
  again, in output rather than in an argument.

  Two things about `--timestamp` are worth having written down. It takes **a reference
  file or an instant**, and borg stats the argument *before* parsing it, so a file whose
  name looks like a timestamp wins — observable, and reproduced rather than tidied. And
  `recreate --timestamp` **on its own does nothing**, in both tools, because recreate skips
  an archive that needs no rewriting: a test that only ran that form would pass while
  exercising none of the code it claims to cover. The test forces a rewrite with
  `--chunker-params` and asserts the no-op form separately.

  **`recreate` was the other half of that group and is deliberately not done.** borg's
  `recreate --exclude-caches` works — measured: it removed a tagged directory from an
  *existing* archive. Doing the same means detecting the tag in the item stream rather than
  on the filesystem, and detecting it *properly* means **fetching the tag file's content
  out of the repository to check its signature**, because a `CACHEDIR.TAG` with the wrong
  contents must not exclude anything. That is a different implementation from the walk's,
  with a cost the walk does not have, so it is its own item rather than three more lines.

### 11.2 The option gate

`tests/evidence/option-coverage.sh` asks `borg CMD --help` and `borge CMD -help`, so
neither list is written down anywhere. Three decisions in it are worth keeping:

**An option is a group of spellings, not a name.** `-n, --dry-run` is one option, not two.
Counting names inflated the gap (131 rather than 111) and, worse, would have let the budget
be paid down by adding one-letter aliases to options borge already has. A spelling borg
offers and borge does not is reported separately as an alias gap, and is not a missing
option.

**A budget per command, not a reason per option.** A hundred and eleven hand-written
reasons would be a hand-maintained list again, and nobody would keep it true. Each command
carries the number it is missing today; the gate fails when a command is missing *more*
than its budget, and reports when it is missing fewer so the number can be ratcheted down.
A command absent from the table fails outright, so a new command cannot arrive with a
silent gap. Verified by mutation: 22 for `create` fails with OVER BUDGET, 24 reports
"improved — lower the budget to 23".

**What it cannot see, stated in the script rather than left to be discovered.** Semantics
(both tools have a `-v`; borg's is a log level and borge's is `--verbose`), and the options
of `debug`, `key` and `benchmark` subcommands, which neither tool lists at the top level so
both sides report none.

**And one it could see and does not: options borge has that borg has not.** The gate walks
borg's list and asks whether borge has each one; it never walks borge's. That direction was
run by hand on 2026-08-18 and §11.4 records what it found — including a class of option
that exists, parses, and does nothing.
- ~~**`R` roots in a patterns file**~~ (DIVERGENCES #25). **Done 2026-08-18.**
  `patternFlags.roots()` collects them from a patterns file or a `--pattern 'R PATH'`, and
  `create` puts them ahead of the command-line paths as borg does. Fixing it turned up
  **DIVERGENCES #26**, fixed in the same change: the four pattern options were applied in
  fixed groups rather than in the order written, so `--exclude X --pattern +X` archived
  what borg leaves out. The comment on `patternFlags` claimed the opposite, which is why
  reading the file would not have found it.
- ~~**A relative repository path must be accepted**~~ (DIVERGENCES #22).
  **Done 2026-08-18.** `Env.resolveRepo` makes the path absolute after expanding its
  placeholders; the store keeps its absolute-only rule, because a backend rooted at
  something that depends on the process working directory is one nothing else can reason
  about. No `~` expansion — borg does none, and inventing it would surprise a borg script.

### 11.4 Options borge has and borg has not — audited 2026-08-18

The option gate asks, for each of borg's options, whether borge has it. Nobody had asked
the other way. Running it by hand over every command and every `debug`, `key` and
`benchmark` subcommand turns up three kinds of difference, and only the first is harmless.

#### a. Real borge features that are not written down as borge's

Confirmed by running them, not by reading help text:

| option | what it does | borg |
| --- | --- | --- |
| `extract -C DIR` | extract into DIR | no equivalent; borg extracts into the working directory |
| `version --long` | the Go version and platform as well | not present |
| `prune --keep-within`, `--keep-last`, `--keep-oldest` | retention rules | borg 2 has `--keep KEEP` instead, plus `--keep-13weekly` and `--keep-3monthly` |
| `--reverse` | flip the listing order | **borg has no `--reverse` anywhere** |
| `delete --force` | act on a selector matching several archives | borg needs no such thing; it never refuses |

None of these is wrong. All of them are undocumented *as extensions*, which is the problem:
a user reading `borge prune -help` cannot tell that `--keep-within` will not work under
borg, and a script written against borge will fail on a machine that has the other tool.
**Every borge-only option needs to say so in its help text**, in the same breath as what it
does.

#### b. `--json` is borg's API, and borge's is part missing and part fake

This is the one to take seriously, and the framing matters. borg's own
`docs/internals/frontends.rst` opens:

> Borg does not have a public API on the Python level. […] Borg does on the other hand
> provide an API on a command-line level. In other words, a frontend should (for example)
> create a backup archive by invoking `borg create`, provide command-line parameters/options
> as needed, and parse JSON output from Borg.

So `--json`, `--json-lines` and `--log-json` are not conveniences: they are **the**
interface borg offers to other programs, with a documented schema, message IDs, and rules
for what happens to a path that is not valid unicode. A port that gets them wrong is a port
no frontend can drive.

Measured on 2026-08-18 by running every one, then corrected twice the same day — the
first count was wrong in a way worth keeping on the page.

**First reading (wrong):** grepping borg's argparse for `"--json"` and checking which
commands accepted it gave *eleven* commands, including `list`, `find` and `diff`, whose
`--json` output was byte-identical to their `--json-lines`. Recorded as "an alias, nearly
free to implement".

**What it actually was:** argparse expands unambiguous option prefixes. `borg list --json`
is `--json-lines` with three characters missing. `borg list --help` does not offer it, and
`borg list --jsonzzz` is rejected where `--json` is not. The output was identical because
it was the same option. Implementing `--json` on those three would have copied an argparse
artifact into a port that has no prefix expansion at all.

**The measured surface**, from `--help` on both sides rather than from the source:

| | borg | borge (before) | borge (now) |
| --- | --- | --- | --- |
| `--json` | 8: `create`, `import-tar`, `prune`, `info`, `repo-info`, `repo-list`, `version`, `analyze` (+ `benchmark cpu`) | **19** — `commonFlags` gave it to every command | the same 8 (+ `benchmark cpu`) |
| `--json-lines` | 3: `list`, `find`, `diff` (+ `benchmark crud`) | the same 3, working | unchanged |
| `--log-json` | every command | **absent** | absent |

Of borge's nineteen, six produced JSON and **twelve accepted the option and printed prose**
— `check`, `compact`, `delete`, `extract`, `recreate`, `repo-delete`, `break-lock`,
`repo-space` and the rest:

```
borge break-lock --json      no locks are held on this repository
borge repo-space --json      there is 0 B of reserved space in this repository
borge compact --json         (no output)
```

A missing option produces an error a frontend can act on. An ignored one produces a wrong
belief, and for an *API* that means a frontend written against borge gets plain text where
it parsed JSON. See `DIVERGENCES.md` #35.

**And the schemas were wrong too.** Counting commands hid it: every one of the commands
that did emit JSON emitted a document of borge's own shape.

| command | how borge's JSON differed from borg's | state |
| --- | --- | --- |
| `repo-list` | no `encryption` or `repository` envelope; all thirteen archive keys always, where borg sends four plus whatever `--format` names | **fixed 2026-08-18** |
| `prune` | no `--json` at all | **fixed 2026-08-18** |
| `create` | no `--json` at all | **fixed 2026-08-18**, minus three stats keys (below) |
| `import-tar` | no `--json` at all | **fixed 2026-08-18**, minus the same three |
| item JSON (`list`, `find`, `diff --json-lines`) | missing `flags` and `inode` — 11 keys where borg sends 13 | **fixed 2026-08-18** |
| `repo-info` | a different structure entirely: `manifest.*` and `repository.archive_count` where borg has `cache.path`, `encryption.*`, `repository.last_modified` | open |
| `info` | missing `command_line`, `cwd`, `duration`, `chunker_params` and the whole `stats.*` block | open |
| `version` | four keys borg does not have (`borg_commit`, `borg_series`, `repository_version`, `revision`) | open |
| `analyze` | unrelated shapes on both sides | open |

**Three keys borge does not send, deliberately.** borg's `create`/`import-tar` stats block
has six keys; borge sends three. `chunking_time` and `hashing_time` are instrumentation
borge does not collect, and `store_stats` is a per-backend call/volume/latency report from
borg's Store layer. Sending them as zeros would be worse than omitting them: a frontend
charting `hashing_time` would draw a flat line and believe it, where a missing key is a
question it can answer. Same reasoning keeps `command_line` out of the archive-level JSON
until borge reads it back from the metadata.

**The rule that made `repo-list` and `prune` agree**, and that the remaining four need: the
archive-level key set is *not* fixed. borg builds it from the effective `--format` — "the
form of `--format` is ignored, but keys used in it are added" — with four always present
(`name`, `archive`, `id`, `time`) and nine optional. borge sent all thirteen every time,
which happens to match borg for `repo-list`'s default format and matches nothing else.

**The work that remains, in order:**

1. **`repo-info` and `info` onto borg's schema.** Both are structural rewrites rather than
   added keys, and `info` needs `command_line`, `cwd` and `chunker_params` read back from
   the archive metadata — which borge stores and does not currently read.
2. **`version` and `analyze`.** `version` is the smallest: drop four keys or move them
   somewhere that is not the API surface.
3. **`--log-json`**, which is a whole feature rather than an option: structured log lines
   with the message IDs `frontends.rst` documents. It is one of the 15 absent common
   options and the only one that is part of the API.
4. **The non-unicode rule is already half-ported.** `frontends.rst` describes how borg 2
   represents a path that is not valid unicode; `internal/cli/pydump.go` reproduces exactly
   that for `debug dump-*`. Whatever it does there is what the JSON commands must do, and
   the two should share one implementation rather than agreeing by luck.
5. ~~**`original_size` and the stored `{size}`**~~ — **done 2026-08-18.** The stored figure
   counted the item metadata stream where borg's does not, and the reported one was sampled
   before the archive was saved. Both fixed and held by tests; the residual difference in
   the reported figure is the `command_line` spelling (#12) and cannot be closed without
   changing what the archive says about itself. `DIVERGENCES.md` #36.

`TestJSONOptionSurfaceMatchesBorg` locks the *surface* — which commands take which JSON
option — by asking both tools and failing in both directions. It does not compare schemas;
that is what the per-command differential tests are for.

#### c. Shared-group leakage

`--reverse` and `--deleted` reach every command that uses `listSelectors`, because borge
registers the group whole. borg's `define_archive_filters_group` takes a `deleted=False`
parameter and enables it per command, so borg offers `--deleted` on a few and borge on all
of them. Some of those are meaningful in borge and some are not; each needs deciding rather
than inheriting.

#### The work

1. ~~**Extend `option-coverage.sh` to the reverse direction**~~ — **done 2026-08-19.**
   Every option borge adds must appear in the gate's `borge_only` table with a reason;
   one that does not fails the gate, and one listed that borge no longer has fails it as
   stale. Thirteen commands add something, and all of it is now written down.
2. ~~**Extend it to subcommands**~~ — **done 2026-08-19.** Enumerated from borge, because
   borg's group help names no subcommand at all. It immediately found `key remove
   --passphrase`, missing and invisible through eight stages, which took the count *up*
   from 35 to 36.
3. **Document every borge-only option as borge-only**, in its help text.
4. ~~**Treat `--json` as the API it is**~~ — **done 2026-08-18 apart from `--log-json`**:
   out of `commonFlags`, registered only on the eight commands borg has it on, and
   implemented for `create`, `import-tar`, `prune` and `repo-list`. Four schemas and
   `--log-json` remain; see (b) above and table rows 5–7.
5. **Decide `--reverse` and `--deleted` per command** rather than by inheritance.

With 1 and 2 landed, "complete" for a command now means "has exactly what borg has, plus
only what is written down". What the gate still cannot see is *semantics*: it compares
spellings, and two tools can agree on every option name while disagreeing on what the
options do.

**A third fix, not on the original list.** The common-option comparison was not a
comparison at all. It matched borg's option *groups* (`r|repo`, `info|v|verbose`) against
borge's per-spelling names, so any multi-spelling option read as absent whatever borge did:
the report said "14 common options, 14 absent" while borge had three of them, and named
`-r` among the missing. The own-options loop had always compared group-wise; this one never
did. It now reports 10 absent, 1 missing a spelling (`--info`, borg's third name for
`-v`), and 3 implemented — and it probes for `-h`/`--help`, which Go's flag package honours
without printing, so scraping the help text could not have seen them.

### 11.3 Templating: is it worth matching borg's, and how far is it done?

**The question:** borge prints its own layout where borg renders a template. Is basic
templating worth implementing to match borg more closely?

**Yes, and as of 2026-08-18 most of it is done** — which is the useful answer, because the
work already measured what "worth it" means. `internal/formatter` plus the archive and item
key sets took `repo-list`, `list` and `find` from hand-written `Printf` lines to templates,
and all three now produce **byte-identical output to borg** on their defaults and on every
`--format` string tested. Fifty of the 111 missing per-command options have closed, and the
option gate measured each step rather than anyone counting.

**The rule that came out of it, and the reason to keep going.** `repo-list`'s layout was a
`Printf` with a comment above it *quoting* borg's default template as though the two were
the same thing. They could drift and nothing would notice. So: **a command that prints a
record renders it through a template, and its default is that template** — then the columns
a user sees without `--format` come from the same code path as the ones they see with it,
and drift is impossible rather than merely unlikely.

#### There are two bracket systems in borg, not one

This is the correction that matters most, and every account of "borg templating" that
describes only the first will produce a wrong implementation:

| | where | `{name}` | what `:spec` means |
| --- | --- | --- | --- |
| **Placeholders** | archive names, repository paths | `{hostname}`, `{now}` | a **strftime** format: `{now:%Y-%m-%d}` |
| **`--format` templates** | listings | `{archive}`, `{size}` | a **Python format spec**: `{archive:<36}`, `{id:.8}` |

The surface syntax is identical and `:` means two unrelated things. A single engine built on
the placeholder reading would apply strftime to `{size:8}` and emit nonsense; one built on
the format-spec reading would treat `%Y-%m-%d` as a fill character followed by a width.
borge keeps them apart deliberately: `internal/placeholders` for the first,
`internal/formatter` for the second, and neither imports the other.

#### On the suggested Go packages

Assessed against what borge needs, not in the abstract. None is taken as a dependency, and
the reasons are worth recording because they are the same reasons in each case.

- **`valyala/fasttemplate`** — real, fast, and the wrong shape. It substitutes tags; it has
  no format specs at all, so `{archive:<36}` and `{id:.8}` — borg's *own defaults* — cannot
  be expressed. It also has no equivalent of Python's `{{` literal-brace escape. Worst for
  this port: an unknown tag is substituted with nothing or left in place, depending on which
  `Execute` variant is used, where borg raises. A listing quietly missing a column is the
  silent-no-op failure §2.3 exists to stop. (Assessed from its documented API; the package
  is not vendored here, so the exact unknown-tag behaviour per variant is unverified.)
- **`lestrrat-go/strftime`** — real, and it would have been a reasonable choice a week ago.
  borge already has a hand-written strftime in `internal/placeholders`, verified against
  CPython across 31 directives × 11 edge-case instants, which is a stronger guarantee than
  "a strftime library" for a tool whose whole purpose is matching another implementation.
  It also deliberately **refuses** `%c`, `%x` and `%X` rather than approximating them: those
  are locale-dependent in Python, and an archive name that changes with the machine's locale
  is the opposite of a name. A general library would render them in some locale and the
  divergence would be silent.
- **`valyala/quicktemplate`** — a compile-time code generator for whole documents. Unrelated
  to runtime placeholder substitution; listing it is a category error.

The code samples in the source material also carry two mistakes worth not copying: the
import paths are written with an `https://` scheme, which is not a Go import path; and the
regex approach `{(now|utcnow):?([^}]*)}` has no handling for `{{`, so a literal `{{now}}`
would be substituted rather than escaped. The `{now}` fallback of `2006-01-02T15:04:05` is
correct — it is borg's `%Y-%m-%dT%H:%M:%S`.

**The general point:** borge's constraint is not "render a template" but "produce the same
bytes as another program". A dependency helps with the first and is neutral-to-harmful for
the second, because every place its behaviour differs from CPython's becomes a divergence
nobody wrote down. Both engines together are about 350 lines with no dependencies.

#### What is left

- **`diff`** — its records are *changes*, not paths, so it needs a third key set rather than
  a reuse of the item one.
- ~~**`prune`**~~ — **done 2026-08-18.** It prints borg's layout, the label padded to 44
  columns and the archive rendered through `--format`, and takes `--format`, `--short` and
  `BORGE_PRUNE_FORMAT`. This entry described it as outstanding for a day after it landed.
- **`check`** — still needs more than the option, and the divergence is larger than a
  template: borg's `-v` reports index and pack progress where borge reports per-archive
  results, and borg writes all of it to stderr where borge writes to stdout (table row 13).
  Reworking the output comes first; the template is the small part.
- **A trap in both:** borg's `prune` and `check` defaults have **no trailing `{NL}`** — the
  command supplies the newline. Porting them with one appended, or without adding it, puts a
  whole listing on one line.

### 11.1 `transfer`, borge to borge — decided 2026-08-18

**The decision: in scope.** It copies archives from one repository into another without
re-reading the source data, and the case that settles it is the ordinary one — moving a
repository to a new drive, and upgrading it on the way. Nothing else borge has does that:
`repo-compress` and `recreate` rewrite chunks *in place*, so neither can move a repository,
and an `rsync` of the directory copies the old format and the old key along with it.

Transfer *from borg 1.x* stays a §0.6 non-goal: it needs the borg 1 reader, which is a
larger piece of work with its own format reference. So borge's `transfer` implements borg's
`--upgrader=NoOp` path only, and must refuse `--from-borg1` and `--upgrader=From12To20`
with a message that says where the limit is written down rather than "unknown option".

#### What borg's `transfer` actually does

Read from `src/borg/archiver/transfer_cmd.py` and `src/borg/crypto/key.py` at the pinned
commit, not from the documentation:

- **The destination must be a *related* repository.** Two hard checks, before anything is
  written (`transfer_cmd.py:137-144`):

  ```python
  if not using_same_id_hash and not rechunking:
      raise Error("You must either keep the same ID hash or use --chunker-params.")
  if not rechunking and not uses_same_chunker_secret(other_key, key):
      raise Error("You must use the same chunker secret or deduplication will break. Use a related repository!")
  ```

  `uses_same_chunker_secret` is `other_key.chunk_seed == key.chunk_seed`.
  `uses_same_id_hash` compares *families*: keyed HMAC-SHA256, keyed blake3, unkeyed sha256
  (`none-sha256`), unkeyed blake3 (`none-blake3`). So `aes256-ocb` → `chacha20-poly1305` is
  allowed and `aes256-ocb` → `none-sha256` is not, unless `--chunker-params` re-chunks
  everything and re-hashes it under new ids.

- **`repo-create --other-repo SRC` is what makes a repository related** (`key.py:660-679`).
  It inherits the **id key** (so chunk ids match) and the **chunk seed** (so chunk
  boundaries match), and generates a **fresh AE key** — the comment says "borg transfer
  re-encrypts all data anyway, thus we can default to a new, random AE key".
  `--copy-crypt-key` keeps the source's AE key instead. Copying from an unencrypted source
  is refused outright: the `none-*` modes have no key material, and need none, because
  their ids are unkeyed and dedup identically anyway.

- **`--recompress` defaults to `never`**, which keeps each chunk's compressed payload
  byte-for-byte and only decompresses to re-verify the id. `--recompress always`
  decompresses and re-compresses under `--compression`. `--compression` is used for the
  *metadata* either way.

- **It re-verifies chunk ids by default.** `other_manifest.repo_objs.set_assert_id_place("transfer")`,
  with the reasoning in a comment worth keeping: transferring re-anchors content in another
  repository, so this is the trust boundary at which `chunkid == id_hash(content)` should be
  re-certified. borge already reserved this: `BORGE_ASSERT_ID` documents `transfer` as one of
  its four places and includes it in the default `repair,transfer,rechunk`. The place exists
  and nothing uses it yet, which is a small piece of documentation that becomes true when
  this lands.

- **It is resumable, and that is a design property rather than a nicety.** An archive
  already in the destination is skipped, checked by *both* (name, timestamp) and (name, id),
  because borg 2 allows duplicate archive names, so neither key alone identifies an archive.
  Re-running finishes what was interrupted, which is exactly what a multi-hour move to a new
  drive needs.

- **It validates every archive name and comment up front** and refuses the whole transfer if
  any is invalid, rather than failing part-way through.

- **A chunk missing from the source** does not abort: the chunk list entry is written with
  the correct id and size and nothing is transferred, so the gap is recorded rather than
  papered over with zeros.

- **`--dry-run`** reports `transfer_size` and `present_size` per archive and says
  "completed" or "incomplete", which is what makes the documented
  transfer / dry-run / transfer-again idiom work.

#### Accuracy notes on the material this decision was made from

Three of the four purposes given in the reference hold up; one is wrong in a way that
matters, and one needs a borge-specific caveat.

- *"Copy archives from one deduplicating repository to another, handling conversion,
  re-encryption or restructuring without re-creating backups from the source data"* —
  **accurate**, and it is the reason to have the command.
- *"Upgrading repositories … such as from Borg 1.x"* — **accurate for borg, out of scope for
  borge.** The upgrade borge's `transfer` performs is between borg 2 repositories: a new
  format version, a new key, or new chunker parameters. Not 1.x.
- *"Changing encryption or security boundaries: re-encrypting data chunks under a new,
  **independent** key structure"* — **the word "independent" is wrong, and it is the single
  most important correction here.** The AE key is new, which is the re-encryption and the
  new trust boundary; but the id key and the chunker secret are deliberately **inherited**,
  and borg refuses the transfer if they are not. An independent key structure is precisely
  what a related repository is not. Anyone reading "independent" would try
  `repo-create -e aes256-ocb` on a fresh directory and hit
  "You must use the same chunker secret" with no idea why. borge's help text has to say
  *related*, and say why: different secrets mean different chunk boundaries and different
  ids, so every chunk would be stored again.
- *"Global compression changes"* — **accurate** (`--recompress always` with
  `--compression`), with the caveat that borge already has `repo-compress` for doing this
  in place; transfer's version is for doing it while moving.
- *"Re-chunking data"* — **accurate** (`--chunker-params`), and worth noting it is also the
  escape hatch from the same-id-hash rule, because re-chunked content is hashed afresh.

#### Work items

1. **`BORGE_OTHER_PASSPHRASE`** (borg: `BORG_OTHER_PASSPHRASE`, alongside
   `BORG_OTHER_PASSCOMMAND` and `BORG_OTHER_PASSPHRASE_FD`). Two repositories are open at
   once and they need not share a passphrase. It goes in `borge help environment`, which
   `TestHelpEnvironmentTopicListsEveryVariable` checks in both directions.
2. **`repo-create --other-repo SRC`**, plus `--copy-crypt-key`. borge's `item.Key` already
   carries `CryptKey`, `IDKey` and `ChunkSeed` as separate fields, so this is: unlock the
   source key, inherit `IDKey` and `ChunkSeed`, generate a fresh `CryptKey` unless
   `--copy-crypt-key`, refuse an unencrypted source. Gated against borg both ways — borg
   must be able to open a related repository borge created, and transfer into it.
3. **`transfer --other-repo SRC`** with `-n/--dry-run`, `-C/--compression`, `--recompress`,
   `--chunker-params` and the archive filters. Refuse `--from-borg1` and
   `--upgrader=From12To20` pointing at §0.6. Two pieces of prose go false the moment this
   lands and neither has a test behind it: the header comment of `internal/cli/help.go`
   says borge "does not implement `mount` or `transfer`", and DIVERGENCES #14's neighbours
   describe `transfer` as undecided. Grep for `transfer` across `docs/` and `internal/`
   before calling the item done — this is the fourth time in stage 8 that a sentence
   describing an absence outlived it.
4. **The relatedness guards**, ported with their messages, and tested for *refusal*: an
   unrelated destination has to be rejected before a single chunk is written. A transfer
   that silently re-stores every chunk is the failure this prevents, and it looks like
   success.
5. **Resumability test.** Transfer, interrupt, transfer again, and require the result to
   equal a single uninterrupted transfer — and that the second run reports the archives as
   already present rather than duplicating them.
6. **Interop rows.** borge transfers a borg-written repository and borg reads the result;
   borg transfers a borge-written one and borge reads that. Both with `--recompress never`
   (the payload-preserving path) and `--recompress always`, since they exercise different
   code.
7. **Verify, do not assume, the two legacy branches.** borg's transfer prefers
   `item.chunks_healthy` over `item.chunks` when present, and drops borg 1.x `part` items.
   Both are described in borg's source as legacy. Whether either can occur in a borg 2
   archive is a question to answer by testing a repaired borg 2 repository, not by reading
   the comment — the plan should record the answer, and if they cannot occur, say so rather
   than porting dead branches.

**Gate:** `borge transfer` moves a repository borg wrote into a related borge repository
that borg can then read and `check --verify-data`; the reverse direction likewise; a
re-run is a no-op; and an unrelated destination is refused with borg's message.

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
> Two commands need a *second* passphrase, which is what finally forced the prompting the
> CLI had never had — see the note below. `BORGE_NEW_PASSPHRASE` still short-circuits it for
> scripts, and its absence with no terminal is an error rather than an empty passphrase:
> leaving a repository unprotected while reporting success is the one outcome worse than
> refusing.
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
>
> **Passphrase prompting, added 2026-08-17 in the same change** (`internal/cli/passphrase.go`,
> `golang.org/x/term`). Until this, `Env.passphrase` read the environment and nothing else,
> which §0.5 had recorded as arriving "with the write path" — and the write path had arrived
> several stages earlier.
>
> It is structured as a **retry, not a first step**, and that is the whole design. A
> repository's key type is not known until its manifest has been read, and the `none-*` and
> `authenticated-*` modes have no passphrase at all; asking up front would prompt for
> repositories with nothing to unlock. So the environment's passphrase is tried, and a
> prompt happens only on `ErrPassphraseWrong` — a failure the unkeyed modes cannot produce.
> The side effect is that a *wrong* `BORGE_PASSPHRASE` now gets a prompt instead of a bare
> refusal.
>
> Three attempts; echo off via `term.ReadPassword`; the prompt on stderr so redirecting a
> command's output still captures only its output; a passphrase typed once kept for the rest
> of that command so `key change-location`, which unlocks twice, does not ask twice; and a
> new passphrase asked for twice, because nothing can check it afterwards.
>
> `term.IsTerminal` is asked about the *process's* standard input rather than `Env.Stdin`,
> which is an `io.Reader` so the CLI stays testable. In a test that reader is a buffer with
> no terminal behind it, which is exactly the "do not prompt" answer wanted there — the
> tests therefore exercise the cron-job path by construction.
>
> `golang.org/x/term` is the fourth external dependency and is justified under §0.4: reading
> a passphrase without echo needs termios, the standard library has no equivalent, and
> `x/sys` was already a dependency.
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
  against *borg's*, not against the original tree, because borg's restore does not
  reproduce everything it stored: `--sparse` restores holes only at chunk granularity
  (DIVERGENCES #9). Once compatibility is lifted, "restore reproduces the source" becomes
  an achievable gate rather than an aspiration.

  **Corrected 2026-08-19.** This entry used to name `bsdflags` here too — "read and
  preserved but never applied" — which is borge's gap and not borg's. borg captures them
  with `FS_IOC_GETFLAGS` and applies them with `FS_IOC_SETFLAGS`, last of all attribute
  restoration (`archive.py:1112`). The same wrong sentence stood in DIVERGENCES #8 and in
  §11's work list; this was the third copy, and all three are now fixed. It belongs in
  stage 8 as a fidelity gap, not here as a constraint of compatibility.
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

Kept current. A tracker that says "not started" about work that shipped days ago is worse
than no tracker: it is the document a new reader trusts first.

| Stage | Description | State | Evidence bundle |
| --- | --- | --- | --- |
| 0 | Foundation, licensing, borg-2 venv, format reference | **done** 2026-08-16 | `borge-stage-0-20260816T163704Z.zip` |
| 1 | Primitives: msgpack, compress, crypto, chunker, item, hashindex | **done** 2026-08-16 | per-substage: `stage-1.1` … `stage-1.6` |
| 2 | `store` (borgstore port, posixfs) | **done** 2026-08-16 | `borge-stage-2-20260816T212000Z.zip`, plus `stage-2-googledrive` |
| 3 | `repoobj` + `repository` + packs + locking | **done** 2026-08-16 | `borge-stage-3-20260816T215557Z.zip` |
| 4 | Keys | **done** 2026-08-16 | `borge-stage-4-20260817T021409Z.zip` |
| 5 | Read path: manifest, archive, extract | **done** 2026-08-17 | `borge-stage-5-20260817T032303Z.zip` |
| 6 | Write path: create | **done** 2026-08-17 | `borge-stage-6-20260817T071719Z.zip` |
| 7 | **Interoperability gate** ⭐ | **done** 2026-08-17 | `borge-stage-7-clean-20260817T192652Z.zip` (see note) |
| 8 | Remaining commands + remote backends | **in progress** — 31 of borg's 36 commands. Fifteen numbered items remain, tabled in §11 under "What stage 8 still owes": `serve` and the remote backends, `transfer` (§11.1), 35 per-command options (§11.2), four JSON schemas and `--log-json` (§11.4b), `bsdflags` and `xattrs` (DIVERGENCES #8), and `debug convert-profile` | not yet bundled, and not to be bundled until that table is empty but for its last row |
| 9 | Performance baseline vs borg | **investigated** 2026-08-17 (§12.1–12.5); no fix applied yet, no baseline run | not yet bundled |
| 10 | Format / indexing changes | not started | — |
| — | **Doc anchors** (§2.1): tie help text to the code that implements it | **1 of 7 done** — item 6 `TestHelpExamplesRun` 2026-08-18; items 1–5 and 7 not started | — |

**On the three stage-7 bundles.** `stage-7` and `stage-7-rerun` each record a FAIL that was
not a real defect — the first was `/tmp` filling, the second an edit landing mid-build (see
§2). The gate itself passed both times. `stage-7-clean` is the one to cite: no failure
anywhere, and it predates the borg pin drift of §0.1 by 66 minutes.

**What "in progress" means for stage 8.** The command list is gated by
`tests/evidence/command-coverage.sh`, which reports 31 implemented, 5 absent with a recorded
reason, 0 unexplained. Three of the five are §0.6 non-goals (`mount`, `umount`, `webdav`);
the other two are `serve` and `transfer`. `transfer` was the one open *question* rather than
open work, and it is now answered: borg 2 to borg 2 is in scope, borg 1.x is not, and §11.1
holds the design. Of the path and argument defects,
options after positionals (#20), relative source paths (#21), relative repository paths
(#22), the rsync slashdot hack (#24), `R` roots in a patterns file (#25) and pattern-option
ordering (#26) were all closed on 2026-08-18. Sorted directory order (#23) is deliberate and was written down
only when a differential test tripped over it.

**All six came out of one activity**: running the examples in borge's own help text. #20
was the example that lost data; #21 and #22 were found building the fixture for it; #24 was
found reading borg's source closely enough to fix #21; #25 was found checking how far #24
reached; #26 was found measuring what #25's option actually did. Seven stages of differential testing had not surfaced any of them, because each
lives in the gap between what the tests exercise and what a user types.

**What "investigated" means for stage 9.** §12.1 and §12.2 measured; nothing has been
changed as a result. The three pure-Go fixes (zstd encoder reuse, chunker reuse,
`subtle.XORBytes`) and the pipelining work are all still to do, and the `tests/bench/`
harness §12 describes has not been built.

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
| **Upstream borg 2 checkout moving** — *materialised 2026-08-17* | Interop gate silently invalidated: every differential test failed at once with a traceback inside borg, which reads as a borge regression | Pin the commit; rebase deliberately with a reviewed diff. **Neither `borg --version` nor `borg-commit.txt` notices the checkout moving** — both are baked in at install time — so `mkbundle.sh` now reads the borg tree's real `HEAD` and warns on mismatch. See §0.1. |
| ~~`borghash`/`borgstore` license unknown~~ | ~~Cannot port those components~~ | **Closed 2026-08-16**: both BSD-3-Clause, porting permitted (LICENSING.md §6) |
| Surrogate-escaped path encoding | Silent path corruption on non-UTF-8 filenames | Fuzz round-trip in Stage 1.5; synthetic corpus in Stage 7 |
| `PackWriter` concurrency ported wrong | Rare, load-dependent repository corruption | Preserve the "index touched only by the calling goroutine" invariant; `-race` in CI |
| Chunker boundary drift | Total dedup loss, invisible until the repo is huge | Byte-exact boundary differential test (Stage 1.4) |
| Scope creep across 10 stages | Never finishing | Explicit non-goals (§0.6); one stage at a time; ask before advancing |
| Usage limits interrupting work | Lost context, broken tree | Stage/task granularity, always-committable state, evidence bundles (§2) |
| **Pure-Go performance shortfall** | Pressure to take a cgo dependency, which would forfeit the `CGO_ENABLED=0` cross-compilation that §12.3's mobile case depends on | Measured 2026-08-17 (§12.2): three of the four largest gaps are borge's own bugs and cost nothing to fix. Take those and the pipeline first, re-measure, and only then consider `avo`-generated assembly — which is still pure Go. A C library is the last resort, not the first. |
| **A stale plan** | The tracker said stages 4–10 were "not started" while 4–7 had shipped evidence bundles; a new reader trusts that table first | The tracker (§14) is part of finishing a stage, not a postscript. AGENTS.md says so. |
