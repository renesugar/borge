# AGENTS.md — orientation for agents working in this repository

`borge` is a Go port of [BorgBackup](https://github.com/borgbackup/borg) 2.x. It reads and
writes the same repositories as `borg`, and that constraint is the reason for most of what
follows.

Read the current plan first — see the next section for which document that is. A plan is
versioned alongside the code and is expected to be **corrected when reality disagrees with
it**: a note that no longer matches the code is a bug in the plan, not a historical record
to preserve.

---

## How work is planned and tracked

There is exactly **one current plan** at a time, and one long-lived roadmap.

| file | what it is |
|---|---|
| `ROADMAP.md` | the numbered items of work that are not porting stages, and their state |
| `docs/PORTING_PLAN.md` | the current plan **while the port is open**: stages 0-9, and the running record of what each stage actually did |
| `PLAN.md` | the current plan for one roadmap item, once the port is closed |
| `plans/` | plans that are finished or superseded, archived unmodified |
| `docs/DIVERGENCES.md` | every place borge deliberately differs from borg, numbered, with the reason |
| `docs/FORMAT.md` | the on-disk format, with citations into borg's source |

The workflow:

1. **While the port is open** (through stage 9), `docs/PORTING_PLAN.md` is the plan and
   `ROADMAP.md` holds everything the port does not own.
2. **When stage 9 closes**, move `docs/PORTING_PLAN.md` to `plans/` unmodified and stop
   editing it. Anything in it that describes unfinished non-porting work must already have
   been moved into `ROADMAP.md` or a document under `docs/` before it is archived — an
   archive is a record, not a place to look up what to do next.
3. **After that**, pick an item from `ROADMAP.md`, write `PLAN.md` for it — what the item
   is, how it is broken into committable tasks, and the gate that decides it is done — and
   implement it one task at a time.
4. **When the item is finished**, record the outcome in its `ROADMAP.md` entry, move
   `PLAN.md` to `plans/<item>-<YYYYMMDD>.md`, and write the next one. Two plans are never
   current at once, and `plans/` is never edited after the fact.

Whichever plan is current, the same rules hold: a task is finished when it builds, its
tests pass, and it is committed; the tree is never left broken across a stop; and after a
gate passes, **stop and ask before starting the next task**.

If you change behaviour that borg also has, you owe an entry in `DIVERGENCES.md`. If you
finish a piece of work, you owe a note in the current plan saying what you found — not
just what you built — **and an update to its tracker** (`PORTING_PLAN.md` §14 for stages,
the item's own checklist in `ROADMAP.md` otherwise).

The tracker is the table a new reader trusts before anything else, which is exactly why it
has to be right: it once claimed stages 4 through 10 were "not started" while four of them
had shipped evidence bundles. Updating it is part of finishing the work, not a postscript.

---

## Build, test, check

```
make build          # ./bin/borge
make test           # the Go test suite
make check          # fmt, vet, lint, spdx, layering, test
make borg2          # build the pinned borg 2 reference interpreter (needed by most tests)
make coverage       # the stage 8 gates: borge's commands and per-command options against borg's
make option-coverage # just the per-command option comparison (docs/PORTING_PLAN.md 11.2)
make interop        # the stage 7 gate: the real-corpus interoperability matrix
make evidence STAGE=stage-N   # build an evidence bundle
```

Every source file needs an SPDX header; `scripts/check-spdx.sh` enforces it. A file ported
from borg carries `Apache-2.0 AND BSD-3-Clause` and names the borg file it came from.

`scripts/check-layering.sh` enforces the import direction: a package may import its own
rank or lower, never higher. Ranks are in that script. A new helper package needs no edit
(rank 0 is the default); a new domain package does.

### Tests need a real borg

Most of the interesting tests are **differential**: they run borg and borge over the same
input and compare. They skip with an explanation when `.venv-borg2/bin/borg` is missing, so
a green run that skipped everything looks the same as a green run that tested everything —
check the output, not just the exit code.

`go test -short` skips the borg gate deliberately. The evidence bundler uses `-short` for
its `-race` pass only.

**Use `make test`, not a bare `go test ./...`.** The Makefile passes `-timeout 60m`;
Go's default is 10 minutes, and `internal/cli` alone now runs for about that long because
almost every command has a differential test that forks borg. A bare `go test` fails with a
goroutine dump pointing at whichever test happened to be running when the deadline fired —
which looks exactly like that test hanging, and is not. If you must invoke `go test`
directly, pass `-timeout 60m`.

### Temporary space

The corpora are large. Set `TMPDIR=/media/renes/HD2/borge-tmp` for full runs; `/tmp` is a
32 GB tmpfs on this machine and a full interop run has filled it before, producing an
evidence bundle that recorded a failure which was not real.

### Do not edit the tree while a test run is in flight

`go test` lists a package's test functions, then compiles. A test renamed in between leaves
the generated test main referring to a function that no longer exists, and the package
fails to build. This has produced two evidence bundles recording failures that never
happened. `tests/evidence/mkbundle.sh` now rsyncs a snapshot and tests *that*, so the
bundler is safe to run while you work — but a plain `go test` is not.

Editing Markdown during a run is fine. Editing `.go` files is not.

---

## Running Python

CPython appears in three places: the pinned borg 2 interpreter, the fixture generators
(`make item-fixtures`, `make msgpack-fixtures`), and the differential tests that compare
against CPython's own `strftime` and `stat.filemode`.

**Always invoke it with both of these set:**

```bash
PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 python3 ...
```

- `PYTHONDONTWRITEBYTECODE=1` — no `__pycache__`. It would otherwise appear in this
  repository *and* in the borg checkout that `setup.sh` pip-installs in editable mode,
  which is somebody else's working tree.
- `PYTHONUNBUFFERED=1` — progress appears immediately when output is redirected to a log.
  Without it CPython block-buffers stdout and a long operation looks stalled.

`tests/borg2/setup.sh` exports both. The Makefile targets set both. New code that shells
out to Python should do the same; `.gitignore` covers the mistake but does not prevent it.

---

## What this port values

These are not style preferences. They are the things that have actually caught bugs here.

**A test that cannot fail is worse than no test.** Several tests in this repository passed
for the wrong reason before someone looked: a recompression test with a 19-byte margin, a
compaction test whose packs were never mixed, a pattern test that matched only because
`TMPDIR` happened to be under `/media`. Differential tests are especially prone to this —
two empty results compare equal. **Assert up front that the comparison is not vacuous**,
and say so in the failure message. Grep for `vacuous` to see the pattern.

**Ask the other tool rather than a list you wrote.** `tests/evidence/command-coverage.sh`
exists because a stage was declared complete against a hand-maintained list of remaining
work while five commands were missing. It asks `borg --help` what commands exist. It caught
a bug in itself on its first run, which is the same failure in miniature.

**A race a test needs must be forced, not hoped for.** `TestCreateFilesChangedDetectsATornFile`
needs a file to change during *every one* of ten read attempts. Its writer first rewrote an
8 MB file and slept a millisecond between rewrites - which usually collided, passed locally,
and failed in the suite when one read fitted between two writes. A test that needs a race
has to make it certain: the writer now writes one byte in a tight loop, moving the timestamp
thousands of times a second. If the certainty cannot be arranged, assert the part that is
deterministic and say what is not being asserted.

**A test that normalises away a difference cannot see that difference come back.** Every
diff test sorted both sides before comparing, with a comment saying the order was "a
difference real and separate from what this test is about" - so when borge's diff turned out
to be sorted where borg's is not, nothing failed. Normalising is sometimes right (key order
inside a JSON object is not a promise), but write down what the normalisation hides, and
when the hidden difference is fixed, take the normalisation out: `TestDiffDefaultOrderIsBorgs`
now asserts borg's order *and* that it differs from the sorted order, so a tree that
happened to stream in sorted order fails the test rather than passing it.

**Ask it at every level.** That gate compared top-level names only, so `debug` matched
`debug` and thirteen subcommands were never compared; the option gate did compare them, but
enumerated them from *borge*, so a subcommand borge lacked was not on the list. Each gate
was right about what it looked at and `debug convert-profile` fell between them for eight
stages. When two gates split a comparison, ask which side each enumerates from - a gap that
belongs to neither is invisible to both.

**Verify a claim before documenting it.** A help topic describing placeholder substitution
was written from borg's behaviour and was false for borge, which had none. It was caught by
running the command. `TestHelpPlaceholdersTopicIsTrue` now checks the claim against the
behaviour, and has since caught the *opposite* error when placeholders were implemented.

**Which stream is part of the behaviour.** `transfer`'s per-archive lines go to *stdout* in
borg, because it writes them with `print()` and not through its logger — most of borg's
progress output goes to stderr, so the reasonable assumption was wrong. borge had them on
stderr, which nothing notices until someone runs `borge transfer … | tee migration.log` and
gets an empty log. Differential tests that compare combined output cannot see it: check
stdout and stderr separately, at least once per command.

**Documentation goes stale silently, and prose is the part that does.** Four claims went
false during stage 8; the two with tests behind them failed loudly, the two that were prose
needed a human to notice. Until the rest of the doc-anchor work in `PORTING_PLAN.md` §2.1
lands, the rule for prose is manual: **if you change behaviour, grep the help topics for
what you just made false.** `borge help <topic>` renders them; `internal/cli/help.go` holds
them.

The *examples* in those topics are no longer manual. `TestHelpExamplesRun`
(`internal/cli/help_examples_test.go`) runs every command in every topic against a scratch
repository and checks what it did, not just that it exited 0. Every command needs a table
entry saying what it should do, and every entry needs a command — so adding an example to a
topic fails the test until somebody says what it is for. That is deliberate: an example
nobody can state the expected outcome of is an example nobody has checked.

**Check what the code does with absence.** Four stage-8 defects were the same defect: the
feature was right and its handling of an empty or missing input was wrong, and each read
the user's explicit input as "nothing was given" and reported success. `create A ""`
archived the working directory; `repo-list --newer ""` listed everything; an `R` root in a
patterns file was parsed and dropped. For every option you add, ask whether *empty* is
distinguishable from *absent*, whether absent silently means "everything", and where the
parsed value actually ends up. `PORTING_PLAN.md` §2.3 has the pattern and the test to write.
A silent no-op looks exactly like success, which is why none of the four was caught by
seven stages of differential testing.

**An option that does nothing is worse than a missing one.** `borge break-lock --json`
parsed, printed plain text and exited 0; borg has no such option at all. `--json` was
registered on 19 commands and read by six. A missing option produces an error the user
can act on; an ignored one produces a wrong belief. Fixed 2026-08-18 and now held by
`TestJSONOptionSurfaceMatchesBorg`, which compares the two surfaces in both directions.
`PORTING_PLAN.md` §11.4, `DIVERGENCES.md` #35.

**`--json` is borg's API, not a formatting option.** borg has no Python-level API and says
so; the command line plus JSON output *is* the interface frontends are written against
(`docs/internals/frontends.rst` in the borg checkout). Treat a change to JSON output the
way you would treat a change to a published API, and compare it against borg's as data
rather than as text — key by key, not "it parsed". Counting commands is not comparing
schemas: borge offered JSON on six commands and matched borg's shape on none of them.

**A stored number is borg's number — unless the archive itself disproves it.** Anything
written into archive metadata is read back by both tools, so borge reproduces borg's
accounting even where it is accidental: the recorded `size` excludes the item metadata
stream because borg's create loses the counter its item buffer writes into, and borge
excludes it too (`DIVERGENCES.md` #36). The line is falsifiability. borg's `import-tar`
records twice the files it imported, and one `borg list` on borg's own archive disproves
it, so borge does not copy that one (#38) — reproducing it would mean writing into the
metadata something the archive contradicts. Note also that "borg's number" may not be one
number: create, import-tar and recreate do not agree with each other here, so match the
path rather than the tool.

**`--help` is the authority on what options exist; acceptance is not.** argparse expands
unambiguous prefixes, so `borg list --json` runs and is `--json-lines` — it appears in no
help text and is not an option. An hour went into "borg's `--json` is byte-identical to its
`--json-lines`, so it is a free alias" before the mechanism was understood; it was the same
option twice. When measuring borg's surface, read `borg CMD --help`, and when a measurement
comes out surprisingly convenient, find out why before building on it.

**Silence is an answer nobody can act on.** `borge delete --dry-run` without `--list`
prints nothing, so "two archives would go" and "your selector matched nothing" look
identical — and that is inherited from borg, which does the same. Where borge must match
borg's output it says which option to add instead (the `--dry-run` help names `--list`);
where borge is deciding for itself, make the informing command informative. `PORTING_PLAN.md`
§2.3.

**Say what you did not do.** A stage note that lists only successes is not a record.

---

## The shape of the code

```
cmd/borge/            the binary
internal/
  cli/                commands, one file per command or command group
  archive/            item streams, create, extract, diff, tar, repair
  cache/              the files cache
  manifest/           the archive directory, pruning
  repository/         packs, chunk index, locks, compaction
  repoobj/            the BORG_OBJ envelope
  store/              the borgstore object store
  crypto/             AEAD suites, key management, OCB
  chunker/ compress/ hashindex/ item/ patterns/ msgpackx/ placeholders/
tests/
  interop/            the stage 7 matrix over real corpora
  borg2/              the pinned reference interpreter
  evidence/           bundle builder and the command-coverage gate
```

Commands are registered in `internal/cli/cli.go`'s `commands()`. It is a function rather
than a variable because `benchmark crud` runs the real commands through `Run`, which is a
cycle Go refuses to order at initialisation.

`borge completion` discovers each command's options by **running it with `-help`** and
collecting the `FlagSet` it built. That works only while every command registers its flags
before calling `fs.Parse`. If you write a command that parses first, the completions lose
its options silently — `TestCompletionSeesEveryCommandsFlags` is what catches it.

---

## Interoperability is the hard constraint

Until stage 10, borge must read and write exactly what borg does. Before changing anything
that touches the on-disk format, run `make interop`. Performance work that breaks interop is
reverted, not patched.

The upstream commit is pinned in `internal/version/version.go` and in
`tests/borg2/borg-commit.txt`. Chasing upstream master mid-port makes the gate meaningless;
rebasing onto a newer commit is a deliberate, reviewed activity.

**borg quirks are reproduced on purpose.** Several are outright bugs — see `DIVERGENCES.md`
and the stage 6 notes. Do not "fix" one without checking whether the port depends on it;
the place to fix them is stage 10, when the compatibility constraint is lifted.

---

## Environment variables

Every one is read as `BORGE_<NAME>` first and `BORG_<NAME>` second, so an existing borg
setup works unchanged. `borge help environment` is the list, and
`TestHelpEnvironmentTopicListsEveryVariable` scans the source to keep it honest in both
directions — a variable the code reads and the topic omits fails, and so does a variable the
topic documents and nothing reads.

`BORGE_TESTONLY_WEAKEN_KDF=1` makes the passphrase KDF cheap so tests are fast. It must
never be set for a real repository.
