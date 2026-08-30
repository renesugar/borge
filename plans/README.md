# plans/

Finished and superseded plans, archived unmodified.

A plan lands here when the work it describes is done, or when it is replaced by another
plan. Nothing in this directory is edited afterwards: it is the record of what was
intended and what was learned at the time, and correcting it later would destroy the only
thing it is good for. Corrections belong in the current plan, in `ROADMAP.md`, or in
`docs/`.

Before archiving a plan, move anything in it that still describes **unfinished** work into
`ROADMAP.md` or a document under `docs/`. A reader looking for what to do next must never
have to open this directory.

The workflow, and which document is current, is described in `AGENTS.md`.

| plan | item | archived |
|---|---|---|
| `r1-evidence-attestations-20260827.md` | ROADMAP R1, the signing identity and timestamp policy | 2026-08-27 |
| `r2-documentation-system-20260828.md` | ROADMAP R2, the doc anchors and the two advisory checkers | 2026-08-28 |
| `PORTING_PLAN.md` | ROADMAP RP, the borg-to-Go port: stages 0-9, the compatibility gates, and what each stage actually did | 2026-08-29 |

`PLAN.md` is current as of 2026-08-29, for ROADMAP R0.

## Two notes on the porting plan, which is not like the others

**It kept its name.** The rule that archives a finished `PLAN.md` renames it
`<item>-<YYYYMMDD>.md`; the rule that archives the porting plan says only to move it to
`plans/` unmodified. It is also cited by section from forty source files and from
records that cannot be edited — `evidence/manifest.json` is signed and timestamped, and the
two plans archived before it are frozen by the rule at the top of this file. Keeping the
filename keeps all of those true. The date this table carries is the date the filename does
not.

**Two links inside it were repointed, and nothing else.** It linked `LICENSING.md` and
`EVIDENCE.md` as siblings, which was true while the file lived in `docs/` and is false from
here; both now point at `../docs/`. That is a modification to a file the rule says to move
unmodified, and it is recorded here rather than done quietly, because the rule protects the
record's *claims* — what was intended and what was learned — and a pointer that no longer
points is not one of them. Every other reference in the file is either a section number or
`../ROADMAP.md`, which survives the move unchanged.

## Relative links in the plans archived before it

`r1-...` and `r2-...` were written as `PLAN.md` at the repository root and archived here
unmodified, so their sibling links — `ROADMAP.md`, `PLAN.md`, `AGENTS.md` — resolve against
the root, not against this directory. **Read them as root-relative.** They are left as they
were found, because the rule at the top of this file says so and because rewriting a
finished record to make a link work is the beginning of rewriting it for other reasons.

Recorded on 2026-08-29, when a link check run over the whole tree turned them up. The rule
will keep producing them: every plan is written at the root and archived one level down. The
porting plan is the first one where the breakage was noticed at archive time rather than
afterwards, which is why it is the first one to have been repointed instead.
