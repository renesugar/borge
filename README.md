# borge

**borge** is a deduplicating backup program with compression and authenticated
encryption, written in Go.

## Provenance

**borge started as a port of [BorgBackup](https://github.com/borgbackup/borg) ("borg")
from Python/Cython to Go.** The great majority of this codebase is a translation of
borg's source, and it reads and writes borg's on-disk repository format. Where code
from [restic](https://github.com/restic/restic) is used, the file header says so.

borg is copyright (C) 2015-2026 The Borg Collective and (C) 2010-2014 Jonas Borgström,
licensed under the BSD 3-Clause License. restic is copyright (c) 2014 Alexander Neumann,
licensed under the BSD 2-Clause License. Both license texts are reproduced verbatim
under [`licenses/`](licenses/) and their conditions continue to apply to the portions
of borge derived from them.

borge is **not** produced, sponsored or endorsed by the Borg Collective or by the
restic project. Please report borge bugs to the borge issue tracker, not to theirs.

## License

borge as a whole is licensed under the **Apache License 2.0** — see [`LICENSE`](LICENSE)
and [`NOTICE`](NOTICE). The analysis of why Apache-2.0 is compatible with the upstream
BSD licenses is in [`docs/LICENSING.md`](docs/LICENSING.md).

## Status

**Pre-release, and interoperable.** `v0.8.0` (2026-08-22) implements 33 of borg's 36
commands and every repository backend borg has: local paths, `sftp:`, `s3:`/`b2:`,
`rclone:`, and `rest://` served by `borge serve --rest`. The three commands that are
missing — `mount`, `umount`, `webdav` — are deliberate non-goals, not gaps.

The port targets the **borg 2.x repository format** (repository version 4), pinned to
the upstream commit recorded in `plans/PORTING_PLAN.md`. Correctness is defined as
bidirectional interoperability with that borg: an archive written by borg must restore
identically with borge, and vice versa. That is a test suite rather than an intention —
both tools write into one repository, over each backend, and each reads what the other
wrote.

**What "pre-release" means here.** No interface stability is promised before `v1.0.0`,
performance work has not been done (stage 9), and borg 1.x repositories are not read at
all. Differences from borg that are deliberate — and the ones that were bugs until they
were found — are written down one by one in [`docs/DIVERGENCES.md`](docs/DIVERGENCES.md).

Every stage gate has an evidence bundle: the commit, the full test log, the coverage
gates, the pinned borg version and a sha256 manifest, named in the stage's row in
[`plans/PORTING_PLAN.md`](plans/PORTING_PLAN.md) and in the release tag.

## Why

Three things motivated the port, in priority order:

1. **Single static binary, no Python runtime.** Borg's installation story and its
   Cython build are a recurring source of friction.
2. **Restore performance on directories with very many files.** borg and restic both
   degrade badly here; the reference workload for this project is a directory with
   118,866 files in it.
3. **Maintainability.** borg's `archive.py` (12 classes) and `repository.py` are god
   modules; the port takes the opportunity to split them (borg issues #10016, #10017).

## Documentation

- [`plans/PORTING_PLAN.md`](plans/PORTING_PLAN.md) — the staged porting plan and its gates
- [`ROADMAP.md`](ROADMAP.md) — the work that is not a porting stage: format and indexing
  changes, evidence preservation, the documentation system, and the future GUI
- [`AGENTS.md`](AGENTS.md) — how the repository is built, tested, planned and tracked
- [`docs/EVIDENCE.md`](docs/EVIDENCE.md) — evidence catalog, ISO workflow, and limitations
- [`docs/LICENSING.md`](docs/LICENSING.md) — license compatibility analysis
