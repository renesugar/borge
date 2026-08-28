# PLAN.md — R2: the documentation system

Current plan for [`ROADMAP.md`](ROADMAP.md) **R2**. The design and the stage-8 findings
that motivated it are `docs/PORTING_PLAN.md` §2.1, §2.1.1 and §2.1.2; this file is the
execution plan and it carries the design forward, so archiving the porting plan will not
archive the specification of unfinished work. When R2 is finished this moves to
`plans/r2-documentation-system-<date>.md`.

R2 is what stands between the project and its first GitHub push.

## The problem, in one paragraph

Four documentation claims went false during stage 8 while the code around them stayed
correct. The two with tests behind them failed loudly; the two that were prose needed a
human to notice. The cause is structural: the sentence lives in `internal/cli/help.go` and
the behaviour lives elsewhere, so a change to one does not put the other in the diff. The
fix is colocation — the user-facing sentence goes in a doc comment on the declaration that
implements it, marked with a directive naming where it belongs — plus tooling that makes
the *unverified share* a number rather than an assumption.

Four grades, most trustworthy first: **executed** (the prose carries an example the suite
runs), **generated** (the text is produced from the code), **claimed** (prose linked by id
to a check), **unverified** (everything else — permitted, but counted).

## Tasks

Sized so the first is useful alone. Each is committable; the tree is never left broken.

- [x] **T1 — `docaudit`, and the anchors it audits.** Done 2026-08-27; what it turned up
  is at the end of this file. A parser for `//borge:doc`,
  `//borge:help`, `//borge:enumerates`, `//borge:claim` and `//borge:checks`; a read-only
  tool that reports the grade breakdown per topic; and failures for a `//borge:help`
  naming a topic that does not exist, a `//borge:claim` with no registered check, and a
  check registered against no claim. Then anchor the five existing topics and the claims
  the current tests already verify. No generation yet. This alone would have caught the
  passphrase-prompting claim, because the sentence would have carried a claim id with
  nothing behind it.
- [x] **T2 — `//borge:enumerates`.** Done 2026-08-27; notes at the end. Generate the
  lists that are checked ad hoc today:
  environment variables, pattern styles, compression specs, placeholders. Delete
  `TestHelpEnvironmentTopicListsEveryVariable` and `TestHelpTopicsCoverTheCode` in favour
  of one mechanism — a generated list cannot drift, so a test that it did not drift is
  dead weight.
- [ ] **T3 — `docgen --help`.** Per-topic templates naming the fragments they want, in the
  order they want them (concatenation in source order would make the document's shape
  depend on file order), generation into `internal/cli/help_generated.go`, and
  `TestDocsAreCurrent` re-running the extraction in memory and diffing. Migrate the five
  topics out of string constants and into anchored comments.
- [ ] **T4 — `docgen --api`, decided.** borge has no exported API; everything is under
  `internal/`, which `go doc ./internal/...` already serves. Record an explicit decision
  either way rather than leaving it as a permanently open item.
- [ ] **T5 — `doccheck`, advisory.** The contradiction pass of §2.1.1 over `//borge:doc
  user` blocks only. Build the five-case calibration set from git **first**, then the
  checker: entailment (supported / contradicted / not determinable), not similarity —
  negation barely moves an embedding, and the claim that motivated all this was a negation.
  Read the code blind, then compare, so the reading is not anchored on the claim. Never a
  gate.
- [x] **T6 — execute every help example.** Done 2026-08-18, `TestHelpExamplesRun`.
- [ ] **T7 — `docactionable`, advisory.** Generate a command from each topic and run it
  against T6's scratch-repository harness. Last: it depends on that harness.

## Gate

`docaudit` reports zero dangling anchors and zero orphan claims; every help topic has at
least one executed example and a recorded grade breakdown; generated documentation is
fresh (`TestDocsAreCurrent`); and the advisory checkers are calibrated against the known
before/after cases from git rather than trusted on their silence.

## What this plan will not do

Chase the unverified share to zero. Rationale — "this exists because the key type is not
known until the manifest is read" — is not a testable assertion and marking it as one
produces permanent *not determinable* noise that teaches everyone to ignore the report.
The goal is that the unverified share is small, visible, and deliberate.

## T1, done 2026-08-27

`internal/docs` parses the anchors, `cmd/docaudit` reports them, `make docaudit` runs it,
and `TestDocAuditIsClean` (in `internal/cli`, where the topic list lives) is the gate.
Twelve findings, each with a case in `internal/docs/audit_test.go` that damages a clean
fixture one way and requires that rule — a case that fails through some other check counts
as a failure, because the check it is about would still be unproven.

What the work turned up:

- **The first report was flattering, and that was a bug in the report.** Anchoring each
  topic as one lump graded all five *executed* and printed "unverified share: 0%", which is
  false about several thousand words of prose. The audit now counts section anchors and
  says `topic-anchored-as-a-whole` for every topic that has none. A number that reads as
  reassurance while measuring almost nothing is worse than no number.
- **Near-misses are reported, not ignored.** `// borge:help patterns` with a space is not a
  directive to Go: it would render into the documentation as prose and register nothing.
  So would `//borge:claims`. Both are findings — a typo that silently registers nothing is
  the exact failure the anchors exist to remove.
- **Two claims needed checks that did not exist**, so they were written:
  `environment/prefix-fallback` (BORGE_ first, BORG_ second, and an empty BORGE_ value is a
  value rather than an absence) and `environment/passphrase-prompt` (the unencrypted modes
  never prompt; with no terminal the error names the variable to set). The second is the
  claim whose sentence was false for part of stage 8.
- **The topic list is passed into the audit, not read by it.** `internal/docs` is a leaf
  package and must not know what borge's topics are; the caller asks the code. A list
  inside the auditor would be a second place for the topics to disagree, which is the bug
  the whole mechanism exists to remove.

## T2, done 2026-08-27

Four lists are now generated: pattern styles, compression specifications, placeholders,
and environment variables. A topic writes `{{enum:name}}` — `{{enum:name:part}}` where one
table serves six sections of one topic — and `renderEnumerations` fills it in when the
package initialises. `docaudit` is given the registry's names, so `//borge:enumerates`
naming a list the code does not define is an error, and a list no topic anchors is a
warning.

What this changed about the tests, which is the part worth recording:

- **The check moved to where the data is.** `TestHelpTopicsCoverTheCode` used to compare
  the topic text against a list written inside the test — a third copy. The tables are now
  checked against the *behaviour* instead, beside it: `patterns.Styles` against the pattern
  parser, `compress.SpecDocs` against `parseSpec`, `placeholders.All` against the expander.
  Each asks the code the reverse question too, by scanning the source for the cases the
  parser switches on, because only the code can say what it accepts.
- **One direction cannot be generated away.** Which environment variables borge *reads* is
  knowable only from the source, so `TestHelpEnvironmentTopicListsEveryVariable` stays; it
  now checks `cli.envVars` rather than the rendered text. Generation removed the drift
  between the table and the topic, not the drift between the table and the code.
- **`placeholders.Names()` was a second list** beside the `switch` in `field()`, and is now
  derived from the documented table, so the error message a user sees when they mistype a
  placeholder cannot list a different set from the one the help topic prints.
- **The renderer refuses rather than degrades.** An unknown list, an argument where none is
  taken, and a missing or unknown argument are all errors, and rendering happens at
  startup: a topic that cannot be rendered stops the binary rather than printing
  `{{enum:...}}` at a user or, worse, printing the surrounding prose with the list silently
  missing.

Not done here, deliberately: the match-archives topic still writes its selectors and sort
keys out by hand, and its test still compares them against a list in the test. They are the
obvious next `//borge:enumerates`, and they belong with T3's section anchors rather than
with this batch.
