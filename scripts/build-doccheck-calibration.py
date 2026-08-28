#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Build doccheck's calibration set out of this repository's history.

The cases are real documentation claims that were true, went false, and were corrected -
paired with the code they are about. They are extracted from git rather than written here
so that the set cannot contain a case that never happened. That is not a hypothetical
worry: the table in docs/PORTING_PLAN.md 2.1.1 listed a placeholders before/after pair
that git has no trace of, and building this set is what found it.

Each case records the commit, the path and the byte-exact text. TestCalibrationMatchesGit
re-reads all of it and fails if a case has drifted from what git holds, so a case cannot
be edited into agreeing with the checker.

Usage: python3 scripts/build-doccheck-calibration.py [--check]
"""

import json
import pathlib
import subprocess
import sys

OUT = pathlib.Path("internal/doccheck/testdata/calibration")

# The commit that corrected four stale "borge does not prompt" claims. The behaviour was
# added earlier (1a97426); this commit changed only prose, so one code text serves as the
# "what the program actually does" side of all four pairs.
PROMPTING = "094e7b44def97381eb6b2747caaaf2df54a922cb"
# The commit that corrected DIVERGENCES #41: --log-json was claimed on three command
# groups that build no FlagSet and never had it.
LOGJSON = "6d14209ff03d0e463dece2fa13f9b221aa4eff3b"


def show(commit, path):
    return subprocess.run(["git", "show", f"{commit}:{path}"],
                          capture_output=True, text=True, check=True).stdout


def lines(commit, path, first, last):
    """The 1-based inclusive line range, as it stands at commit."""
    return "".join(show(commit, path).splitlines(keepends=True)[first - 1:last])


def case(cid, verdict, why, claim, code, unit):
    """One labelled example. code is a source, or a list of them when the unit spans files."""
    return {
        "id": cid,
        "verdict": verdict,
        "why": why,
        "claim": claim,
        "code": code if isinstance(code, list) else [code],
        "unit": unit,
    }


def src(commit, path, first, last):
    return {"commit": commit, "file": path, "first_line": first, "last_line": last,
            "text": lines(commit, path, first, last)}


def build():
    # The code all four prompting pairs are judged against: the prompt loop and the
    # terminal test it depends on. Unchanged by the commit that fixed the prose, so the
    # same text is the truth for both sides of every pair.
    prompt_code = src(PROMPTING, "internal/cli/passphrase.go", 41, 127)
    passphrase_lookup = src(PROMPTING, "internal/cli/cli.go", 258, 269)

    cases = [
        case("prompting-help-before", "contradicted",
             "The shipped environment topic said borge does not prompt, months after "
             "prompting was implemented. This is the claim the whole documentation system "
             "exists to catch.",
             src(f"{PROMPTING}~1", "internal/cli/help.go", 324, 325),
             prompt_code, "Env.unlockWithPrompt"),
        case("prompting-help-after", "supported",
             "The corrected text, against the same code.",
             src(PROMPTING, "internal/cli/help.go", 324, 331),
             prompt_code, "Env.unlockWithPrompt"),

        case("prompting-passphrase-doc-before", "contradicted",
             "\"Only the environment is consulted\" is denied by the function's own first "
             "statement, which returns a passphrase typed at a prompt.",
             src(f"{PROMPTING}~1", "internal/cli/cli.go", 252, 255),
             passphrase_lookup, "Env.passphrase"),
        case("prompting-passphrase-doc-after", "supported",
             "The corrected text, against the same function.",
             src(PROMPTING, "internal/cli/cli.go", 252, 258),
             passphrase_lookup, "Env.passphrase"),

        case("prompting-key-header-before", "contradicted",
             "The key commands' file header said the same false thing.",
             src(f"{PROMPTING}~1", "internal/cli/key.go", 36, 40),
             prompt_code, "Env.unlockKeyManagerWithPrompt"),
        case("prompting-key-header-after", "supported",
             "The corrected header.",
             src(PROMPTING, "internal/cli/key.go", 36, 43),
             prompt_code, "Env.unlockKeyManagerWithPrompt"),

        case("prompting-test-comment-before", "contradicted",
             "A test comment whose assertion was right and whose stated reason was false - "
             "the case for checking prose that no assertion covers.",
             src(f"{PROMPTING}~1", "internal/cli/key_test.go", 247, 249),
             prompt_code, "Env.promptPassphrase"),
        case("prompting-test-comment-after", "supported",
             "The corrected comment.",
             src(PROMPTING, "internal/cli/key_test.go", 247, 250),
             prompt_code, "Env.promptPassphrase"),
    ]

    # A pair from a different subject, so that a checker cannot pass the set by having
    # learned one word. The claim is that registering --log-json in newFlagSet reaches
    # every command; three command groups dispatch straight to a subcommand and build no
    # FlagSet at all, which the code below shows.
    logjson_code = [src(f"{LOGJSON}~1", "internal/cli/cli.go", 228, 243),
                    src(f"{LOGJSON}~1", "internal/cli/debug.go", 72, 86)]

    cases += [
        case("logjson-divergence-before", "contradicted",
             "The entry claimed newFlagSet reached the key, debug and benchmark parents. "
             "cmdDebug never calls it.",
             src(f"{LOGJSON}~1", "docs/DIVERGENCES.md", 1310, 1314),
             logjson_code, "newFlagSet"),
        case("logjson-divergence-after", "supported",
             "The corrected entry names the three groups as the exception.",
             src(LOGJSON, "docs/DIVERGENCES.md", 1310, 1318),
             logjson_code, "newFlagSet"),
    ]

    # Rationale: true, useful, and not entailed by any code. The plan predicts these come
    # back "not determinable", and a checker that calls them contradicted would flood the
    # report with noise until nobody read it.
    cases += [
        case("rationale-remember-passphrase", "not-determinable",
             "Why the passphrase is remembered - a statement about what repeated prompts "
             "do to people, which no code can confirm or deny.",
             src(PROMPTING, "internal/cli/passphrase.go", 147, 149),
             prompt_code, "Env.rememberPassphrase"),
        case("rationale-retry-not-first-step", "not-determinable",
             "Why the prompt is a retry: the reason lives in the repository format, not in "
             "the function being read.",
             src(PROMPTING, "internal/cli/passphrase.go", 26, 28),
             prompt_code, "Env.unlockWithPrompt"),
        case("rationale-terminal-not-stdin", "not-determinable",
             "Why the terminal is used rather than Env.Stdin: a design reason stated in "
             "terms of testing, which the runtime behaviour does not show.",
             src(PROMPTING, "internal/cli/passphrase.go", 67, 70),
             prompt_code, "Env.terminalFD"),
    ]

    for c in cases:
        (OUT / f"{c['id']}.json").write_text(json.dumps(c, indent=2) + "\n")
    return cases


if __name__ == "__main__":
    OUT.mkdir(parents=True, exist_ok=True)
    built = build()
    print(f"wrote {len(built)} case(s) to {OUT}")
