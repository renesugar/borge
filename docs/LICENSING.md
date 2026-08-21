# Licensing analysis for `borge`

**Question:** is Apache License 2.0 a valid license for `borge`, given that `borge`
is a port of `borgbackup/borg` and may reuse code from `restic/restic`?

**Answer: yes**, with conditions. Apache-2.0 is a valid license for the `borge`
*combined work*. The BSD-licensed portions inherited from `borg` and `restic` keep
their original licenses and their notices must be preserved verbatim.

> This is an engineering analysis, not legal advice. It reflects the standard,
> widely-followed reading of these licenses (and matches how the FSF, the ASF and
> SPDX classify them). If the project ever takes contributions under a CLA, or is
> redistributed commercially in a context where the answer matters financially,
> have a lawyer confirm it.

---

## 1. The upstream licenses

| Project | License | SPDX id | Local copy |
| --- | --- | --- | --- |
| `borgbackup/borg` | BSD 3-Clause ("New BSD") | `BSD-3-Clause` | `licenses/borg/LICENSE` |
| `restic/restic` | BSD 2-Clause ("Simplified BSD") | `BSD-2-Clause` | `licenses/restic/LICENSE` |

`borg`'s `LICENSE` carries two copyright lines — The Borg Collective (2015-2026,
see `licenses/borg/AUTHORS`) and Jonas Borgström (2010-2014) — and three clauses:

1. source redistributions must retain the copyright notice, conditions and disclaimer;
2. binary redistributions must reproduce them in the documentation/materials;
3. the author's name may not be used to endorse or promote derived products.

`restic`'s `LICENSE` is the same minus clause 3.

Neither license is copyleft. Neither requires derivative works to be licensed under
the same license. Both are "notice-preservation" licenses: the only real obligation
is *keep the notice with the code*.

## 2. Why Apache-2.0 works

BSD-2-Clause and BSD-3-Clause are **one-way compatible** with Apache-2.0:

- Apache-2.0 imposes *more* conditions than BSD (patent grant, patent-retaliation
  termination, `NOTICE` file propagation, statement-of-changes). Adding conditions
  on top of a permissive license is exactly what those licenses allow — neither BSD
  variant restricts the terms under which derivative works may be distributed.
- The converse is not true: Apache-2.0 code cannot be relicensed *to* BSD, because
  that would drop the patent and NOTICE obligations. `borge` never needs that
  direction.
- BSD-3's clause 3 (no endorsement using the author's name) and Apache-2.0 §6
  (no trademark license) point the same way; they do not conflict. This is the
  clause that makes some people hesitate, and it is the reason the ASF classifies
  BSD-3-Clause as a "Category A" license — freely includable in Apache products.

The practical consequence: `borge` as distributed is an Apache-2.0 work that
*contains* BSD-licensed material. A recipient gets Apache-2.0 terms for the work
as a whole, and separately gets the BSD terms that continue to govern the
inherited portions. Nothing is "relicensed"; Apache-2.0 is layered on top.

## 3. What a port actually inherits

A line-by-line translation of Python/Cython into Go is a **derivative work** of
`borg`. Choosing a different programming language does not reset copyright: the
structure, sequence and organisation carried across are the protected expression.
So the port is squarely inside the BSD-3 grant, and its conditions apply.

Two things are *not* derivative and carry no obligation:

- **The on-disk format itself.** File formats, wire protocols and algorithms are
  not copyrightable subject matter. `borge` reading and writing borg repositories
  is not, by itself, a copyright question at all.
- **Independently written Go code** that happens to implement the same behaviour
  from a specification. In practice almost none of this port will qualify, so it
  is safer to assume everything derives from `borg`.

## 4. Obligations `borge` must actually meet

These are tracked as Stage 0 acceptance criteria in `docs/PORTING_PLAN.md`.

1. **Ship the upstream license texts.** `licenses/borg/LICENSE`,
   `licenses/borg/AUTHORS` and `licenses/restic/LICENSE` are committed verbatim and
   must never be edited. This satisfies BSD clause 1 for source distributions.
2. **Reproduce them in binary distributions.** `borge`'s release artifacts and its
   documentation must carry the same texts. A `borge --license` subcommand that
   prints `LICENSE` + `NOTICE` + `licenses/**` satisfies BSD clause 2, and the
   texts are also embedded in the docs.
3. **Maintain a `NOTICE` file.** Required by Apache-2.0 §4(d) once one exists, and
   it is the natural place to state the provenance. See `NOTICE`.
4. **State that files were changed.** Apache-2.0 §4(b). Ported files carry a header
   naming the `borg` (or `restic`) source file they were derived from; the
   repository-wide statement lives in `NOTICE` and `README.md`.
5. **Do not use the upstream names as endorsement.** BSD-3 clause 3 and Apache §6.
   `borge` may factually say "started as a port of borg" (that is description, not
   endorsement); it must not imply that the Borg Collective produced, sponsors or
   approves it. The `README.md` wording is deliberately factual for this reason.
6. **Do not use "borg" as the product name.** Hence `borge`. The binary, the module
   path and the GitHub project are all `borge`.
7. **Keep third-party Go dependencies auditable.** Go module dependencies are
   linked, not vendored-and-relicensed; their own licenses continue to apply and
   must be surfaced. `go-licenses`-style reporting into `licenses/third-party/`
   runs in CI.

## 5. Per-file marking convention

Every Go file that is a port of upstream code starts with:

```go
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/repoobj.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.
```

`restic`-derived files use `Apache-2.0 AND BSD-2-Clause` and point at
`licenses/restic/LICENSE`. Files with no upstream ancestry use plain
`// SPDX-License-Identifier: Apache-2.0`.

A CI check enforces that every `.go` file has an SPDX line, and that any file whose
header claims an upstream origin names a path that exists in the pinned upstream
checkout.

## 7. Third-party Go dependencies, and one thing that is deliberately not one

Dependencies are linked, not vendored and not relicensed; their own licenses continue to
apply. The list is short on purpose (PORTING_PLAN §0.4: prefer the standard library), and
each addition is a decision with a reason:

| module | license | why it is here |
| --- | --- | --- |
| `github.com/klauspost/compress` | BSD-3-Clause | zstd, and the fastest pure-Go deflate |
| `github.com/pierrec/lz4/v4` | BSD-3-Clause | lz4, borg's default compression |
| `github.com/ulikunitz/xz` | BSD-3-Clause | lzma, which borg offers |
| `lukechampine.com/blake3` | MIT | BLAKE3, one of borg's two id hashes |
| `golang.org/x/crypto` | BSD-3-Clause | argon2, ssh (the sftp backend's transport) |
| `golang.org/x/sys`, `golang.org/x/term` | BSD-3-Clause | syscalls and terminal handling |
| `github.com/pkg/sftp` | BSD-2-Clause | the SFTP v3 protocol, added 2026-08-21 |
| `github.com/kr/fs` | BSD-3-Clause | pulled in by `pkg/sftp` |

**SFTP is a wire protocol with a specification, not a borg format decision.** Implementing
it by hand over `x/crypto/ssh` is possible and would be a few thousand lines of someone
else's protocol; the "prefer the standard library" rule is about not taking a dependency for
something Go can already do, and Go cannot already do this.

**paramiko is not a dependency and must not become one.** borg reads `~/.ssh/config`
through paramiko, so paramiko's behaviour is what borge has to match for an `sftp://` URL to
mean the same thing in both tools — but paramiko is LGPL-2.1-or-later, and borge is
Apache-2.0. `internal/store/sshconfig.go` is therefore an independent implementation of
rules OpenSSH documents, with paramiko used as a **test oracle**: the differential test runs
both and compares. Behaviour is not copyrightable; an implementation is. Nothing in that
file is copied or translated from paramiko, and its header says so.

## 6. `borghash` and `borgstore` — resolved

`borg` 2 has moved two components into separate PyPI packages that `borge` must also
port: `borghash` (the `ChunkIndex` hash table, borge Stage 1.6) and `borgstore` (the
object store layer, borge Stage 2). Both are Borg Collective projects, but that is not
the same as knowing their license, so Stage 0 task 0.8 checked rather than assumed.

**Finding (2026-08-16), from the installed distributions themselves:**

| Package | Version | `License-Expression` | License text |
| --- | --- | --- | --- |
| `borghash` | 0.2.0 | `BSD-3-Clause` | `licenses/upstream-python/borghash.LICENSE.rst` |
| `borgstore` | 0.6.1 | `BSD-3-Clause` | `licenses/upstream-python/borgstore.LICENSE.rst` |

Both are copyright (C) Thomas Waldmann, BSD 3-Clause — the same license as `borg`
itself. **Porting their code is therefore permitted**, on the same terms and with the
same obligations as the rest of the port: preserve the notice, mark the derived files,
and name the upstream source in the header (§5, using the `Apache-2.0 AND BSD-3-Clause`
expression).

The copyright holder differs from `borg`'s, so their license texts are reproduced
separately under `licenses/upstream-python/` rather than being folded into
`licenses/borg/`, and `NOTICE` credits them in their own section.

Re-run `scripts/check-upstream-licenses.sh` if either package is ever upgraded; it
records the metadata and copies the shipped license files, and fails if a copyleft
license appears. Had either been copyleft, the fallback was to implement that
component from the on-disk format description instead of porting it.
