#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Build the per-stage evidence bundle (plan section 2, item 4).
#
#   tests/evidence/mkbundle.sh <stage-id> [extra-file ...]
#
# Produces /home/renes/evidence/borge/borge-<stage-id>-<UTC timestamp>.zip containing
# enough to reconstruct what was true when the stage gate passed: the exact commit,
# whether the tree was clean, the full test output, the toolchain, the pinned borg
# version, and a sha256 manifest of everything in the bundle.
#
# The point is that a claim like "stage 3 passed" is checkable later by someone who
# was not there - including by a future session that lost its context.

set -euo pipefail

STAGE="${1:-}"
if [ -z "$STAGE" ]; then
    echo "usage: $0 <stage-id> [extra-file ...]" >&2
    exit 64
fi
shift

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEST="${BORGE_EVIDENCE_DIR:-/home/renes/evidence/borge}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
NAME="borge-${STAGE}-${STAMP}"
WORK="$(mktemp -d)"
OUT="$WORK/$NAME"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$OUT" "$DEST"
cd "$ROOT"

echo "mkbundle: collecting evidence for stage '$STAGE'"

# --- provenance ------------------------------------------------------------------
{
    echo "stage:          $STAGE"
    echo "created (UTC):  $STAMP"
    echo "host:           $(uname -srm) / $(hostname)"
    echo "repo:           $ROOT"
    echo "commit:         $(git rev-parse HEAD 2>/dev/null || echo '<not a git repo>')"
    echo "branch:         $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo '-')"
    echo "tree clean:     $([ -z "$(git status --porcelain 2>/dev/null)" ] && echo yes || echo 'NO - see git-status.txt')"
    echo "go:             $(go version)"
    if [ -x "$ROOT/tests/borg2/borg2" ]; then
        echo "borg2:          $("$ROOT/tests/borg2/borg2" --version 2>&1 | head -1)"
        echo "borg2 commit:   $(cat "$ROOT/tests/borg2/borg-commit.txt" 2>/dev/null || echo '-')"
    else
        echo "borg2:          <not built>"
    fi
    echo "system borg:    $(borg --version 2>/dev/null || echo '<none>')"
} > "$OUT/PROVENANCE.txt"

git status --porcelain > "$OUT/git-status.txt" 2>/dev/null || true
git log -20 --format='%H %ad %s' --date=iso > "$OUT/git-log.txt" 2>/dev/null || true
git diff > "$OUT/git-uncommitted.diff" 2>/dev/null || true

# --- snapshot ----------------------------------------------------------------------
# The tests run against a *copy* of the tree, not the tree itself.
#
# A full run takes the better part of an hour. Editing the working tree during it does
# not merely mix results from two versions: `go test` lists a package's test functions,
# then compiles, so a test renamed in between leaves the generated test main referring
# to a function that no longer exists, and the package fails to build. That has now
# produced two evidence bundles recording failures that were not real - which is worse
# than a slow bundle, because a bundle exists to be trusted without re-running it.
#
# The copy includes .git, so the git-aware checks behave identically. It costs a few
# seconds and a few tens of megabytes, and the build cache is shared, so the tests
# themselves are no slower.
echo "mkbundle: snapshotting the tree"
SRC="$WORK/src"
rsync -a --exclude='/tests/evidence/out/' "$ROOT/" "$SRC/"
cd "$SRC"

# --- checks and tests --------------------------------------------------------------
# Failures are captured, not fatal: an evidence bundle for a *failed* gate is a
# legitimate and useful thing to have.
echo "mkbundle: running checks"
{ go vet ./... 2>&1 || echo "[go vet exited $?]"; }                 > "$OUT/go-vet.txt"
{ ./scripts/check-spdx.sh 2>&1 || echo "[check-spdx exited $?]"; }  > "$OUT/check-spdx.txt"
{ ./scripts/check-layering.sh 2>&1 || echo "[check-layering exited $?]"; } > "$OUT/check-layering.txt"

echo "mkbundle: running tests"
{ go test -timeout 60m ./... 2>&1 || echo "[go test exited $?]"; }  > "$OUT/go-test.txt"
{ go test -timeout 60m -json ./... 2>&1 || true; }                  > "$OUT/go-test.json"
# -short for the race pass only: the differential corpora take minutes on their own and
# far longer under -race, and they exercise no concurrency for -race to find. The full
# (non-race) run above still covers them.
{ go test -race -short -timeout 60m ./... 2>&1 || echo "[go test -race exited $?]"; } > "$OUT/go-test-race.txt"

# --- build inputs ------------------------------------------------------------------
cp -f go.mod "$OUT/" 2>/dev/null || true
cp -f go.sum "$OUT/" 2>/dev/null || true
cp -f tests/borg2/requirements.lock "$OUT/borg2-requirements.lock" 2>/dev/null || true
go list -m all > "$OUT/go-modules.txt" 2>/dev/null || true

# --- caller-supplied artifacts (benchmark JSON, comparator output, ...) -------------
if [ "$#" -gt 0 ]; then
    mkdir -p "$OUT/artifacts"
    for f in "$@"; do
        # Resolved against the real tree, not the snapshot: an artifact is something the
        # caller produced beforehand, and it lives where the caller left it.
        case "$f" in /*) ;; *) f="$ROOT/$f" ;; esac
        if [ -e "$f" ]; then
            cp -r "$f" "$OUT/artifacts/"
        else
            echo "mkbundle: warning: artifact '$f' does not exist, skipping" >&2
        fi
    done
fi

# --- manifest ----------------------------------------------------------------------
( cd "$OUT" && find . -type f ! -name MANIFEST.txt -print0 \
    | sort -z | xargs -0 sha256sum > MANIFEST.txt )

# --- package -----------------------------------------------------------------------
( cd "$WORK" && zip -qr "$NAME.zip" "$NAME" )
cp "$WORK/$NAME.zip" "$DEST/$NAME.zip"

echo "mkbundle: wrote $DEST/$NAME.zip ($(du -h "$DEST/$NAME.zip" | cut -f1))"
echo "$DEST/$NAME.zip"
