#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Render `go test -json` output as the text `go test` would have printed.

This exists so an evidence bundle can run the suite **once** and still carry both forms.
It used to run it twice, on the stated grounds that "the second pass is nearly free: Go
caches a package's successful test result". That is false: `-json` is not one of the flags
Go treats as cacheable, so the second pass re-ran everything. On this repository that was
`internal/cli` (4009s) and `tests/interop` (2169s) a second time - about 75 minutes of a
bundle's wall time spent re-deriving a result it already had.

Nothing here formats a result itself. The JSON stream carries the original bytes in its
`output` events, so the text is recovered rather than reconstructed, and a line that is not
JSON is passed through untouched - a build failure this script swallowed would be a failure
the bundle does not record.

Two deliberate differences from the raw stream:

- **Quiet on success, whole story on failure.** `-json` implies verbose, so the stream
  always carries every `=== RUN` and `--- PASS`. Plain `go test` prints one line per
  passing package, and that is the more useful artifact; the detail nobody needs on a pass
  is still in `go-test.json` beside this file.
- **Sorted by package.** The stream is in completion order, which varies run to run.
  Evidence is compared across runs, so the order is made deterministic instead.
"""

import json
import sys


def main() -> int:
    packages: "dict[str, dict]" = {}
    preamble: "list[str]" = []

    for line in sys.stdin:
        stripped = line.rstrip("\n")
        if not stripped:
            continue
        try:
            event = json.loads(stripped)
        except (ValueError, TypeError):
            preamble.append(line)  # not an event: a build error, or a runtime message
            continue
        if not isinstance(event, dict):
            preamble.append(line)
            continue

        pkg = event.get("Package", "")
        action = event.get("Action")
        state = packages.setdefault(pkg, {"chunks": [], "failed": False, "done": False})

        if action == "output":
            state["chunks"].append(event.get("Output", ""))
        elif action in ("pass", "fail", "skip") and "Test" not in event:
            # A package-level verdict. Test-level events carry "Test" and are not one.
            state["done"] = True
            state["failed"] = action == "fail"

    out = sys.stdout
    for line in preamble:
        out.write(line)
    for pkg in sorted(packages):
        state = packages[pkg]
        # An unterminated package - a timeout, a signal, a build that never ran - is
        # printed in full. Whatever went wrong, the bundle should carry it.
        if state["failed"] or not state["done"]:
            for chunk in state["chunks"]:
                out.write(chunk)
            continue
        for chunk in state["chunks"]:
            if chunk.startswith(("ok  ", "?   ")):
                out.write(chunk)
    return 0


if __name__ == "__main__":
    sys.exit(main())
