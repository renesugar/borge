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

Early. Nothing works yet. The staged plan, its acceptance gates and its current
position are in [`docs/PORTING_PLAN.md`](docs/PORTING_PLAN.md).

The port targets the **borg 2.x repository format** (repository version 4), pinned to
the upstream commit recorded in `docs/PORTING_PLAN.md`. Correctness is defined as
bidirectional interoperability with that borg: an archive written by borg must restore
identically with borge, and vice versa.

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

- [`docs/PORTING_PLAN.md`](docs/PORTING_PLAN.md) — the staged plan and its gates
- [`docs/LICENSING.md`](docs/LICENSING.md) — license compatibility analysis
