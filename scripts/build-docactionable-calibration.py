#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Build docactionable's calibration set out of this repository's history.

plans/PORTING_PLAN.md 2.1.2 names the first known-answer cases itself: three commands that
the help topics documented and that did not work. Each is a before/after pair - the topic
text as it stood when the command was wrong, and as it stands corrected - so the only thing
that varies between the two halves is the prose the model is asked to act on.

The second pair is the one worth having. "borge create -r REPO archive ~ --exclude ..."
does not fail to parse; it archives the directory the user asked to exclude. A checker that
looked at exit status alone would score it actionable, which is why every case carries what
to inspect rather than a code.

TestActionableCalibrationMatchesGit re-reads all of it, so a case cannot be edited into
agreeing with the checker.

Usage: python3 scripts/build-docactionable-calibration.py
"""

import json
import pathlib
import subprocess

OUT = pathlib.Path("internal/cli/testdata/actionable")

# "Run the help examples; two were wrong and one lost data" - the find and create cases.
EXAMPLES = "aba8087a8c5754b26c548463189f1dbaef3a26e4"
# "Run every help example as a test; it found three more bugs" - the tag case, which was
# quoted in prose rather than set out as an example, and so escaped the first pass.
QUOTED = "63b104c"


def show(commit, path):
    return subprocess.run(["git", "show", f"{commit}:{path}"],
                          capture_output=True, text=True, check=True).stdout


def topic(commit, path, first, last):
    text = "".join(show(commit, path).splitlines(keepends=True)[first - 1:last])
    return {"commit": commit, "file": path, "first_line": first, "last_line": last,
            "text": text}


def case(cid, actionable, task, why, expect, src, probe=None, probe_works=None):
    """One labelled example.

    probe is the command the prose documents, kept so that TestActionableCasesStillDiscriminate
    can run it against today's borge and fail when a case has stopped being a case. That is
    not hypothetical: two of the three pairs this set started with were invalidated the day
    they were written, because borge had since been fixed and the commands the topics were
    corrected FOR now work. See PLAN.md R2 T7.
    """
    return {
        "id": cid,
        # actionable: whether a reader following this prose lands on a working command.
        "actionable": actionable,
        "task": task,
        "why": why,
        # expect names what to look at, because exit status is not the assertion.
        "expect": expect,
        # probe: the command the prose leads to, and whether it still works today.
        # None where the prose documents no command at all, which is its own kind of
        # unactionable.
        "probe": probe,
        "probe_works": probe_works,
        "topic": src,
    }


def build():
    cases = [
        # The one pair from 2.1.2 that still discriminates. --pattern requires an action
        # prefix, so the form the topic documented is an error, and it still is.
        case("patterns-find-before", False,
             "search the repository for files whose paths match the shell pattern "
             "**/invoice-*.pdf",
             "The topic's own example writes \"--pattern 'sh:...'\", and --pattern needs "
             "an action prefix, so the documented form is an error.",
             "the command fails",
             topic(f"{EXAMPLES}~1", "internal/cli/help.go", 109, 163),
             probe="find --pattern sh:**/invoice-*.pdf", probe_works=False),
        case("patterns-find-after", True,
             "search the repository for files whose paths match the shell pattern "
             "**/invoice-*.pdf",
             "The corrected topic drops --pattern.",
             "the command succeeds and names the invoice",
             topic(EXAMPLES, "internal/cli/help.go", 109, 175),
             probe="find sh:**/invoice-*.pdf", probe_works=True),

        # A pair of a different kind, and the one 2.1.2 is really about: prose that is true
        # and carries nothing a reader can type. The environment topic shipped with no
        # example at all until 63b104c gave it two.
        case("environment-repo-before", False,
             "list the archives in the repository without passing -r, naming the "
             "repository in the environment instead",
             "The topic listed the variables and showed not one command using them. True "
             "prose that tells a reader nothing they can run is exactly the failure 2.1.2 "
             "names: \"borge supports patterns\" is unfalsifiable and unactionable.",
             "there is no command in the prose to copy",
             topic(f"{QUOTED}~1", "internal/cli/help.go", 313, 378)),
        case("environment-repo-after", True,
             "list the archives in the repository without passing -r, naming the "
             "repository in the environment instead",
             "The corrected topic shows BORGE_REPO=... borge repo-list, chosen because "
             "every other topic's examples silently depend on it.",
             "the command lists the archives",
             topic(QUOTED, "internal/cli/help.go", 313, 383),
             probe="repo-list", probe_works=True),
    ]

    # The two pairs that 2.1.2 also named are deliberately absent, and why is worth
    # keeping: "borge tag ARCHIVE --add @PROT" and "borge create ... ~ --exclude ..." were
    # broken by the flag-order defect of DIVERGENCES #20, and args.go's permute has since
    # fixed it. Both commands work today, so neither pair tells a checker anything. The
    # documentation was corrected and then the program caught up with the old
    # documentation - which is why probe_works is recorded and checked.

    for c in cases:
        (OUT / f"{c['id']}.json").write_text(json.dumps(c, indent=2) + "\n")
    return cases


if __name__ == "__main__":
    OUT.mkdir(parents=True, exist_ok=True)
    print(f"wrote {len(build())} case(s) to {OUT}")
