#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Enforce the import layering from docs/PORTING_PLAN.md section 1.
#
# borg's helpers/__init__.py is an import shim that made its dependency graph cyclic
# (borg issue #10016). Go forbids import cycles outright, so the compiler already
# prevents the worst of it - but it does not prevent a *downward* layer reaching
# back *up*, e.g. internal/store importing internal/archive. That is legal Go and
# still wrong. This script checks the direction.
#
# Rule: a package may import packages at its own rank or lower, never higher.

set -euo pipefail

cd "$(dirname "$0")/.."

MOD=github.com/renesugar/borge

# Rank 0 is the leaf/helper tier: usable by anything, may import nothing above rank 0.
# Higher rank = closer to the user. Packages not listed default to rank 0, so a new
# helper package needs no edit here, but a new domain package must be added.
rank_of() {
    case "$1" in
        cli)                                          echo 90 ;;
        archive)                                      echo 80 ;;
        cache)                                        echo 70 ;;
        manifest)                                     echo 60 ;;
        repository)                                   echo 50 ;;
        repoobj)                                      echo 40 ;;
        store|store/*)                                echo 30 ;;
        crypto|crypto/*)                              echo 20 ;;
        chunker|compress|hashindex|item|patterns)     echo 10 ;;
        *)                                            echo 0  ;;
    esac
}

fail=0

# go list needs a buildable tree; with no packages yet there is nothing to check.
pkgs=$(go list ./internal/... 2>/dev/null || true)
if [ -z "$pkgs" ]; then
    echo "check-layering: no internal packages yet, nothing to check"
    exit 0
fi

while read -r pkg; do
    rel=${pkg#"$MOD"/internal/}
    r=$(rank_of "$rel")
    deps=$(go list -f '{{range .Imports}}{{.}}{{"\n"}}{{end}}' "$pkg")
    while read -r dep; do
        case "$dep" in
            "$MOD"/internal/*) ;;
            "$MOD"/cli*|"$MOD"/cmd/*)
                echo "LAYERING: internal/$rel imports $dep - nothing under internal/ may import the command layer" >&2
                fail=1
                continue
                ;;
            *) continue ;;
        esac
        drel=${dep#"$MOD"/internal/}
        dr=$(rank_of "$drel")
        if [ "$dr" -gt "$r" ]; then
            echo "LAYERING: internal/$rel (rank $r) imports internal/$drel (rank $dr) - imports must point downward" >&2
            fail=1
        fi
    done <<<"$deps"
done <<<"$pkgs"

# cmd/ is wiring only: it may import anything, but nothing may import it.
if go list -deps ./internal/... 2>/dev/null | grep -q "^$MOD/cmd/"; then
    echo "LAYERING: something under internal/ depends on cmd/" >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo "check-layering: FAILED - see docs/PORTING_PLAN.md section 1" >&2
    exit 1
fi
echo "check-layering: ok"
