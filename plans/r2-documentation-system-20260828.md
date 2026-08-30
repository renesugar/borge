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
- [x] **T3 — `docgen --help`.** Done 2026-08-27; notes at the end. Per-topic templates naming the fragments they want, in the
  order they want them (concatenation in source order would make the document's shape
  depend on file order), generation into `internal/cli/help_generated.go`, and
  `TestDocsAreCurrent` re-running the extraction in memory and diffing. Migrate the five
  topics out of string constants and into anchored comments.
- [x] **T4 — `docgen --api`, decided: no.** 2026-08-27; the reasoning is at the end. borge has no exported API; everything is under
  `internal/`, which `go doc ./internal/...` already serves. Record an explicit decision
  either way rather than leaving it as a permanently open item.
- [x] **T5 — `doccheck`, advisory.** Done 2026-08-28; the measurements and the two defects
  building it turned up are at the end. The contradiction pass of §2.1.1 over `//borge:doc
  user` blocks only. Build the five-case calibration set from git **first**, then the
  checker: entailment (supported / contradicted / not determinable), not similarity —
  negation barely moves an embedding, and the claim that motivated all this was a negation.
  Read the code blind, then compare, so the reading is not anchored on the claim. Never a
  gate.
- [x] **T6 — execute every help example.** Done 2026-08-18, `TestHelpExamplesRun`.
- [x] **T7 — `docactionable`, advisory.** Done 2026-08-28; notes at the end. Generate a
  command from each topic and run it against T6's scratch-repository harness. Last: it
  depends on that harness.

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
Twelve findings at the time — eighteen after T5 — each with a case in
`internal/docs/audit_test.go` that damages a clean fixture one way and requires that rule —
a case that fails through some other check counts as a failure, because the check it is
about would still be unproven.

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

## T3, done 2026-08-27

The five topics are gone from `help.go` as text. Each paragraph is a doc comment on the
declaration that implements it - the prompting paragraph on `unlockWithPrompt`, the style
prefixes on `ParsePattern`, the keyfile search on `KeysDirs`, the not-ours variables on the
remote-shell code - and `helptemplate.go` says which fragments each topic wants, in order.
`make docgen` writes `help_generated.go`; `TestDocsAreCurrent` regenerates in memory and
diffs, naming the first line that differs.

The audit finally says something true. Five topics anchored one lump each reported
"unverified share: 0%"; thirty-one fragments report **39%**, with per-topic breakdowns and
no warnings outstanding. That number is the deliverable - not because 39% is good, but
because it is a measurement rather than an assumption, and it is now a number that can be
driven down one claim at a time.

Four things this turned up:

- **gofmt owns doc comments, and it edits them.** It moves `//borge:` directives to the
  bottom of a comment, so the design of "rationale above the directive, user text below"
  was impossible - a comment is one audience or the other, which is what the plan said in
  the first place. It rewrites a line beginning with `-`, `+` or `*` into a list, which
  silently changed `+ PATTERN include` into `- PATTERN include`; the fix is to quote the
  character. And it strips the indentation off a code block that *starts* a comment, so a
  fragment that is only example commands cannot carry its own indentation - the template
  indents it instead.
- **`id:NAME` was an accepted archive selector documented nowhere.** Writing the check that
  compares `manifest.Selectors` against what actually accepts a selector found it: the
  name-pattern styles reach `patterns.CompileName` through `applyMatch`'s default branch,
  so `sh:`, `re:` and `id:` are accepted there rather than in the switch. It is documented
  now. This is the second undocumented behaviour the R2 work has surfaced.
- **One topic, one column.** Each generated list measured its own width, so the environment
  topic's six sections indented their descriptions differently and read as six unrelated
  tables. A list that serves several places now measures across all of them.
- **A quoted command inside a generated list breaks the examples inventory.** The `aid:`
  description said `"borge repo-list --short" prints ids`, and the wrap put a newline
  inside the quotes, so `TestHelpExamplesRun` could no longer find the command it had an
  entry for. Reworded: a table cell is not a good place to quote a command, because where
  it wraps is not up to the author.

The text a user sees is unchanged except where it was meant to change: the three quoted
action characters, the selector list gaining `id:` and losing its blank lines, and the
environment topic's single column. Every topic was diffed against the previous build.

## T4, decided 2026-08-27: no `docgen --api`, and no `docs/INTERNALS.md`

**The decision is no**, and the anchors now enforce it: `//borge:doc api` is an error that
names this decision, because an audience nothing renders is a silent no-op — a maintainer
could mark a comment `api`, believe it was published somewhere, and be wrong forever.

What was measured, rather than assumed:

- **borge has no exported API at all.** Twenty-one packages under `internal/`; the only
  code outside it is three `package main`s (`cmd/borge`, `cmd/docaudit`, `cmd/docgen`).
  There is nothing for an external caller to import, and internal packages never appear on
  pkg.go.dev.
- **`go doc` already serves it, from the same comments a generator would read.**
  `go doc ./internal/repoobj` renders the package's documentation including its format
  diagram; across the tree there are about 794 exported declarations and all 21 packages
  carry a package comment. A generated `INTERNALS.md` would be a second rendering of the
  same source, with nothing added but a file to keep fresh.
- **The narrative a generator cannot produce already exists elsewhere.** The layering and
  its rationale are `docs/PORTING_PLAN.md` §1, the on-disk format is `docs/FORMAT.md`, the
  deliberate differences are `docs/DIVERGENCES.md`, and the map of the tree is AGENTS.md's
  "shape of the code". Those are the documents a new maintainer needs, and none of them is
  a list of declarations.
- **The cost is not zero.** Another generated file, another freshness test, another subset
  in the audit, and a second place for a doc comment to be wrong.

What would change the answer: borge growing a package outside `internal/` that other
programs import. The GUI (R3) does not, by design — it is a frontend to the command-line
JSON API, which keeps one tested format boundary rather than two.

## T5, done 2026-08-28: `doccheck`, and what the calibration set was for

`internal/doccheck` reads the code blind and asks a local model whether that reading
contradicts the prose anchored to it. `cmd/doccheck` runs it, `make doccheck` invokes it,
`make doccalibrate` scores it. It is advisory and it is not in `make check`.

The order in the task mattered: **the calibration set was built first**, from git, before
any checker existed. Everything below came out of that decision.

### The plan's own calibration table was wrong

`docs/PORTING_PLAN.md` §2.1.1 named five cases. Two of them do not exist. The table says
the placeholders topic said "borge does not substitute" before `1a97426` and was corrected
by it — but `1a97426` is the commit that *introduced* placeholders. There is no topic
before it, and `git grep` at `1a97426~1` finds no such claim anywhere in the tree.

The table was written from memory, in the section arguing that prose nobody checks goes
false. Building the set from git is what found it, which is the whole argument for building
it from git.

### What git actually holds: thirteen cases, three subjects

`094e7b4` corrected the same false claim in four places, not one — the environment topic,
`Env.passphrase`'s doc comment, `key.go`'s header, and a test comment whose assertion was
right and whose stated reason was false. Each is a before/after pair against the same code,
which that commit did not touch.

`6d14209` supplies a pair about something else entirely: `--log-json` was claimed to reach
every command through `newFlagSet`, and three command groups build no `FlagSet` at all.
That pair earns its place by not being about passphrases.
`TestCalibrationSubjectsAreNotAllTheSame` requires it: a set where every case is about
prompting cannot tell a checker from a checker that has learned one word.

Three rationale paragraphs complete it — prose that is true, useful, and not entailed by
any code, which must come back *not determinable* or the report fills with noise nobody
reads.

Five contradicted, five supported, three not determinable. A checker that answers the same
label every time scores 5 of 13, and that is the number to beat.

### The set is verified against git, not typed

Every case records a commit, a path, a line range and the byte-exact text.
`TestCalibrationMatchesGit` re-reads all of it and fails when a case has drifted. Without
that, a case can be quietly softened whenever the checker gets it wrong, and the set stops
being evidence at the moment it is most needed. `TestCalibrationIsBalanced` refuses a set
where one label is more than half the cases.

### A second false comment, found by hand while building the set

`6d14209` established that `newFlagSet` does not reach the `key`, `debug` and `benchmark`
parents. It corrected `docs/DIVERGENCES.md` #41 and left the comment *inside `newFlagSet`*
still saying "this is the one place that reaches all of them", naming those three groups.
It was still there on 2026-08-28, nine days later. Corrected now, with the date and the
reason in the comment.

It is worth being precise about what this proves. `doccheck` did not find it — the comment
is not a doc comment and carries no anchor, so the checker would never have looked at it.
Building the calibration set found it. But it is exactly the failure the anchors exist to
prevent: the correction went to the document and not to the code beside the mistake,
because the two were in different files and nothing put them in one diff.

### How the checker works

`ExtractUnit` takes the declaration a claim is anchored to plus the functions it calls
directly in its own package, **with the doc comment removed**, to a 6000-byte budget.
`TestExtractUnitDropsTheDocComment` is the test that keeps the two-step honest: a unit
carrying the comment would hand the model the sentence it is about to judge, and every
verdict would be "supported" while looking like it was working.

The reading is made from that unit alone. Then the claim is broken into simple statements
and each is judged against the reading. Both of those are measured decisions rather than
guesses; see below.

Rationale is excluded by *mechanism* rather than by a list: a paragraph that gives a reason
and asserts nothing about behaviour leaves no statements after decomposition, and a claim
with no statements is not determinable. Only `//borge:doc user` blocks on functions are
looked at at all.

### Three things the model taught us, in order

**A single three-way label is beyond it.** Asked for SUPPORTED / CONTRADICTED / UNRELATED
in one call, the model answered the same label for every case, and *which* label depended
on the prompt's wording rather than on the content. Told "most sentences are UNRELATED", it
said UNRELATED thirteen times.

**It gets the polarity of sentences about "borge" wrong.** This one is worth writing down.
Given facts that say the program prompts for a passphrase:

| statement | asked "is this true?" |
| --- | --- |
| "borge does not prompt for a passphrase." | NO — correct |
| "borge prompts for a passphrase." | **NO — wrong** |
| "The program does not prompt for a passphrase." | NO — correct |
| "The program prompts for a passphrase." | YES — correct |

Seven of eight right with the subject rewritten, and the one failure is the one sentence
that keeps the name. "borge" is a word no training corpus contains, and the model's
handling of an unknown proper noun is not something a verdict can rest on. `Normalise`
rewrites it, `TestNormaliseIsWhyTheNameGoes` records why, and it is careful not to touch
`BORGE_PASSPHRASE` — rewriting inside a word would change what a claim about the
environment says.

**Long sentences defeat it.** The documentation here runs to three clauses and a
subordinate "so that". A verdict on such a sentence is a verdict on whichever clause the
model attended to. Decomposing first helps; it does not help enough.

### What it scored: the model fails its own calibration

`make doccalibrate`, 2026-08-28, `qwen2.5-coder-1.5b-instruct-q4_k_m` on a GTX 1650:

```
4/13 correct (always answering the commonest label scores 5/13)
want -> got:
  contradicted      -> contradicted      4
  contradicted      -> supported         1
  not-determinable  -> contradicted      3
  supported         -> contradicted      5
```

It answered *contradicted* for twelve of the thirteen. It gets four of the five real
contradictions right, and it would get them right by saying "contradicted" to everything —
which is what the fifth column shows it very nearly does.

Seven prompt designs were measured. None beat the baseline:

| design | score | what it actually did |
| --- | --- | --- |
| one three-way label, free text | 5/13 | said CONTRADICTED to everything |
| the same, with a grammar and few-shot examples | 5/13 | still CONTRADICTED to everything |
| relevance gate, then truth | 4/13 | the relevance question was noise |
| truth first, relevance only on a NO | 6/13 | over-abstained: 5 supported cases lost |
| five-vote self-consistency at temperature 0.8 | 4/13 | votes split, so it abstained |
| **decompose the claim, then judge each statement** | **4/13** | the shipped design |
| the same, reading a fixed questionnaire | 5/13 | CONTRADICTED thirteen times out of thirteen |

The two 5/13 rows are not near-misses; they are the constant checker, arrived at twice.
`TestModelIsCalibrated` and `make doccalibrate` both say so in as many words: *this model
does not beat answering the commonest label every time; its verdicts carry no information
and its silence carries none either.*

So `make doccheck` is wired up, runs, and produces a triage list — and on this hardware
that list should not be acted on. What would change the answer is a larger model, which is
a GPU question rather than a design one. The checker takes any llama.cpp server through
`BORGE_DOCCHECK_URL`, and the thirteen cases are there to re-run against it.

**The alternative was to ship it silent.** A checker that emits nothing looks like a
checker finding nothing wrong, and nobody would have known the difference for months. The
calibration set is what makes the difference visible, which is the whole reason §2.1.1 says
to build it first.

### The checker was checking nothing, and said so cleanly

The first end-to-end run reported `no //borge:doc user blocks on functions to check`. All
twenty-six user fragments sit on `var _ = helpText` carriers — T3 put them there because
gofmt moves directives to the end of a comment — so a target list built from "is this
comment on a function?" was empty. A clean report over an empty list is the worst thing a
checker can produce, because it is indistinguishable from a clean tree.

`//borge:about Decl` is the fix: a carrier names the function its prose describes.
Twenty carriers now carry one. The audit errors on a name no function in the directory
answers to (`unknown-declaration`), and **warns** on a user fragment that neither sits on a
function nor names one (`fragment-without-code`) — so the silence cannot come back
unnoticed. One warning stands on purpose: `compression/intro` is about how compression
relates to chunk ids and deduplication, which no single function in `internal/compress`
implements, and pointing it at a plausible neighbour would be worse than admitting it.

This is worth stating plainly, because it is a defect that T3 introduced and T3's own tests
could not see: the gofmt workaround preserved *where the prose lives* and lost *what it is
about*, and everything downstream of that kept working.

### What a report actually looks like

`doccheck -only placeholders`, four claims, three "contradicted" — and reading them is what
settles the question:

```
internal/placeholders/strftime.go:12  placeholders/formats  (strftime)
  claim:   The program formats dates according to the machine's locale.
  claim:   The program formats dates according to the machine's locale.
  ... twenty-two times
```

The decomposition step looped, and one of the statements it invented for the braces
fragment was `The program writes the literal string "{hostnmae}"` — a typo lifted out of
an example of an *unknown* placeholder and asserted as behaviour. Those are not verdicts
that a human triages; they are a tool wasting a reader's attention.

The loop was partly borge's fault and is fixed: `parseStatements` now drops repeats and
stops at `MaxStatements`, with tests for both. The hallucination is the model's, and no
amount of capping addresses it.

### Honest limits, including one this exercise created

**The set is a development set as much as a test set.** Seven prompt designs were tried
against these thirteen cases and the best was kept. With a set that small, that is
selection pressure on the measurement itself, and the reported score is optimistic. The
right fix is a held-out set, and git does not contain one — these are all the labelled
before/after pairs this repository has. Any future work should add cases *before* touching
the prompts, not after.

**Correlated error.** An independent reading can share the author's wrong assumption and
agree with a false claim. Nothing above removes that.

**The unit is one package deep.** Behaviour that emerges across packages is invisible to
the reading, and a claim needing more than one hop is a claim anchored in the wrong place.

**The scope is narrower than the defects.** All three defects found while doing this work
were outside what a *working* `doccheck` would have caught: two were in prose it does not
read (a plan table, an unanchored comment inside a function body), and the third was
`doccheck` itself checking nothing. The checker reads `//borge:doc user` blocks. That is
the right scope — everything else would be noise — but it should not be mistaken for
coverage, and on this hardware it is not even that.

### What T5 leaves behind

Useful now, whatever the model does: thirteen labelled cases verified against git, a
`//borge:about` link from every user fragment to the code it describes, two audit rules
that keep both honest, one corrected false comment, one corrected table in the porting
plan, and a bounded decomposition. Useful when there is a bigger GPU: the checker itself,
and a number to beat.

One more, found while writing this up. Two documents said the audit had "twelve findings",
which stopped being true the moment T5 added a rule. A number in prose cannot check itself,
so `TestEveryRuleHasADamageCase` now reads `audit.go` for the rules it can emit and fails
on any that no case in `TestAuditDetects` produces — which is both a stronger claim than
the count was and one that maintains itself. It was checked by planting a rule name with no
case and watching it fail.


## T7, done 2026-08-28: `docactionable`, and a calibration set that decayed the other way

`internal/cli/docactionable_test.go` gives the model one help topic and a task, runs
whatever command comes back against `newHelpFixture` — T6's scratch repository — and checks
what the command *did*. `make docactionable` runs it. Skipped without
`BORGE_DOCCHECK_URL`, and never in `make check`.

It lives in `internal/cli` rather than in a command because the harness is there and
unexported. That is the whole reason §2.1.2 ordered T7 after T6.

### The question it asks is not the one doccheck asks

Not *is this sentence true* but **does it tell a reader what to type**. A true sentence can
be useless: "borge supports patterns" is unfalsifiable and unactionable. The test is
constructive — produce a command from the prose alone and run it — and it subsumes part of
the contradiction check, because a claim a reader cannot act on is a bad claim whatever its
truth.

### The calibration set went stale because *borge* was fixed

§2.1.2 names its own known-answer cases: three commands the topics documented that did not
work. Built as three before/after pairs, run against today's binary — and **two of them
were no longer pairs**:

| documented command | then | today |
| --- | --- | --- |
| `borge find --pattern 'sh:…'` | error | still an error |
| `borge tag ARCHIVE --add @PROT` | "tag needs an archive" | **works** |
| `borge create … archive ~ --exclude 'sh:**/.cache'` | silently archived the .cache | **works, excludes correctly** |

Both were the flag-order defect of DIVERGENCES #20, and `args.go`'s `permute` has since
fixed it. The prose was corrected in August, and then the program caught up with the prose
that had been wrong.

This is the opposite decay from the one the anchors are for. There, prose goes stale while
code stays right. Here a labelled example stops discriminating because the code was
*fixed* — and a dead case scores the checker on nothing while looking exactly like a live
one. Verified by hand: both commands were run against today's binary and their effects
inspected, not just their exit status.

So `TestActionableCasesStillDiscriminate` now runs each case's documented command and fails
when it no longer behaves as the case says. It needs no model, so it runs in the ordinary
suite. It was checked by flipping a label and watching it fail.

The set is four cases, not six: the `find` pair, which still discriminates, and a pair of a
different kind — the environment topic before and after it had any example at all, which is
§2.1.2's "true prose that tells a reader nothing they can run" made concrete. Two pairs are
better than four when two of the four are dead.

### It passes its calibration, which doccheck did not

**3 of 4, against a 2 of 4 constant-answer baseline**, and not degenerate. Deterministic
across runs. The one miss is the model dropping the `sh:` prefix from
`borge find 'sh:**/invoice-*.pdf'`, producing a command that runs and matches nothing.

That difference from T5 is not luck. Generating a command line from a manual page is close
to what a coding model is trained on; judging whether a negated three-clause sentence is
entailed by a function is not.

**One caveat, recorded because it is the same trap T5 documented.** One check was corrected
after seeing results: the environment check demanded an archive named `ARCHIVE`, which the
*correct* command cannot produce, because the topic's example points at
`/backups/{hostname}`. Fixing a check that the right answer fails is legitimate; it is also
exactly how a set gets tuned into agreement, so it is written down rather than left in the
diff.

### What it says about the topics today, and why that needs triage

3 of 5 topics yielded a working command. Both misses are the **model's**, not the prose:

- **placeholders** — generated `borge create -r /backups/{hostname} ~`, with no archive
  name. The topic's first example is `borge create -r REPO '{hostname}-{now:%Y-%m-%d}' ~`,
  which is precisely the pattern asked for. The model copied a different example.
- **environment** — generated `borge list`. The same topic, given the sharper task in the
  calibration set, produced `BORGE_REPO=… borge repo-list` and passed. The wording of the
  task moves the answer, which is a real limit on reading any single run.

Neither was tuned away. A report whose failures are mostly the model's is the honest state
of this tool at 1.5B, and it is why the output is a triage list.

### Two harness bugs, both of which had made a topic look broken

- **The runner did not change directory.** `TestHelpExamplesRun` does `t.Chdir(f.work)`;
  mine did not, so `borge extract` succeeded and wrote its files into
  `internal/cli/` — the package source directory — while the check looked in the fixture
  and found nothing. The patterns topic was reported unactionable on that basis. Fixed, the
  stray files removed, and the reason recorded on `runGenerated`.
- **The model writes `borg`, not `borge`,** however plainly the manual page in front of it
  says otherwise — the same missing-letter problem doccheck measured. Untreated, every case
  failed on it and the first calibration run scored the baseline with no command ever
  executed. `deBorg` rewrites the program name only, with its reason, and
  `TestGeneratedLineIsCleanedUp` pins it.

Both are worth recording for the same reason the substitutions in `help_examples_test.go`
carry theirs: each is a step away from running what the reader actually reads.
