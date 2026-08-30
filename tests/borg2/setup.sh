#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Build the pinned borg 2 reference interpreter (plan task 0.7).
#
# borge's correctness is defined as interoperability with a *specific* borg, not with
# whatever borg happens to be installed. The system borg here is 1.2.8, which cannot
# read the borg 2 repository format borge targets, so every differential and interop
# test drives the interpreter this script builds.
#
# The upstream commit is pinned in internal/version/version.go and asserted below.
# Rebasing onto a newer upstream commit is a deliberate, reviewed activity - if this
# script's assertion fires, that is the point, not an inconvenience.

set -euo pipefail

# PYTHONDONTWRITEBYTECODE keeps __pycache__ out of both this repository and the borg
# checkout that gets pip-installed in editable mode - the latter is somebody else's
# working tree and borge has no business leaving build droppings in it.
# PYTHONUNBUFFERED so progress appears immediately when this script's output is
# redirected to a log; without it CPython block-buffers stdout and a long install looks
# stalled. See AGENTS.md.
export PYTHONDONTWRITEBYTECODE=1
export PYTHONUNBUFFERED=1

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

BORG_SRC="${BORGE_BORG_SRC:-/home/renes/projects/borg}"
VENV="${BORGE_BORG2_VENV:-$ROOT/.venv-borg2}"
PINNED=$(sed -n 's/.*BorgUpstreamCommit *= *"\([0-9a-f]*\)".*/\1/p' "$ROOT/internal/version/version.go")

die() { printf 'setup.sh: %s\n' "$*" >&2; exit 1; }

[ -n "$PINNED" ] || die "could not read BorgUpstreamCommit from internal/version/version.go"
[ -d "$BORG_SRC/.git" ] || die "no borg checkout at $BORG_SRC (set BORGE_BORG_SRC)"

actual=$(git -C "$BORG_SRC" rev-parse HEAD)
if [ "$actual" != "$PINNED" ]; then
    cat >&2 <<EOF
setup.sh: borg checkout is not at the pinned commit.

  pinned (internal/version/version.go): $PINNED
  checkout ($BORG_SRC):                 $actual

The interoperability gate in plans/PORTING_PLAN.md section 10 only means something
against a fixed upstream. Either check the pinned commit out:

  git -C $BORG_SRC checkout $PINNED

or, if moving the pin is intended, update BorgUpstreamCommit/BorgUpstreamDate and
review the upstream diff for format changes before re-running.
EOF
    exit 1
fi

echo "setup.sh: borg checkout at pinned commit ${PINNED:0:12}"

# borg derives its version from git tags via setuptools_scm, and src/borg/__init__.py
# asserts the result is a real semver - a checkout with no reachable version tag yields
# "0.1.dev<N>+g<sha>" and borg refuses to start at all.
#
# A fork cloned without tags hits exactly that. The fix is to fetch the upstream tags
# rather than to invent a version, so the version borg reports is the true distance from
# the last release - which is what the evidence bundles record.
nearest=$(git -C "$BORG_SRC" describe --tags --abbrev=0 --match '[0-9]*' 2>/dev/null || true)
if [ -n "$nearest" ]; then
    echo "setup.sh: version from git tags: $(git -C "$BORG_SRC" describe --tags --match '[0-9]*')"
    # setuptools_scm derives the version itself; make sure nothing overrides it.
    unset SETUPTOOLS_SCM_PRETEND_VERSION SETUPTOOLS_SCM_PRETEND_VERSION_FOR_BORGBACKUP
else
    # Fallback: no version tags reachable. Take the version from the top heading of
    # CHANGES.rst and hand it to setuptools_scm. borg computes __version_tuple__ from
    # parse_version(...).release, so a beta suffix like "2.0.0b23" passes its assertion:
    # .release is (2, 0, 0), all ints. This is a guess, so say so loudly.
    BORG_VERSION="${BORGE_BORG_VERSION:-$(sed -n 's/^Version \(2\.[0-9A-Za-z.]*\).*/\1/p' "$BORG_SRC/CHANGES.rst" | head -n 1)}"
    [ -n "$BORG_VERSION" ] || die "no version tags in $BORG_SRC and no 2.x heading in its CHANGES.rst (set BORGE_BORG_VERSION)"
    export SETUPTOOLS_SCM_PRETEND_VERSION="$BORG_VERSION"
    export SETUPTOOLS_SCM_PRETEND_VERSION_FOR_BORGBACKUP="$BORG_VERSION"
    cat >&2 <<EOF
setup.sh: WARNING - no version tags are reachable in $BORG_SRC, so the version is a
guess taken from CHANGES.rst: $BORG_VERSION. It will not show the distance from the
last release. To get the real version, fetch the upstream tags:

  git -C $BORG_SRC remote add upstream https://github.com/borgbackup/borg.git  # if needed
  git -C $BORG_SRC fetch --tags upstream

then re-run this script.
EOF
fi

python3 -m venv "$VENV"
# shellcheck disable=SC1091
source "$VENV/bin/activate"

python -m pip install --quiet --upgrade pip setuptools wheel setuptools_scm

# borg 2 needs these beyond what pyproject.toml pulls in automatically:
#   borghash  - the ChunkIndex hash table, split out of borg (borge ports it in stage 1.6)
#   borgstore - the object store layer  (borge ports it in stage 2)
# Both are Borg Collective projects; their licenses are recorded by
# scripts/check-upstream-licenses.sh (plan task 0.8) before any of their code is read.
# The extras are what borgstore needs to reach each backend, and they are here so the
# reference borg can reach the ones borge is being compared against:
#   rest    - requests; the rest:// client and "borg serve --rest"
#   sftp    - paramiko; the sftp: backend
#   s3      - boto3;    the s3: and b2: backends
#   rclone  - requests; the rclone: backend (the rclone binary itself is not pip's)
#   blake3  - the blake3 id-hash modes
# Without sftp and s3 the venv's borg cannot open those repositories at all, and the
# differential tests for them would quietly become borge-against-borge (PORTING_PLAN 11.5).
python -m pip install --quiet "borghash" "borgstore[rest,sftp,s3,rclone,blake3]~=0.6.0"

# Editable install so the venv tracks the checkout without copying it.
python -m pip install --quiet -e "$BORG_SRC"

python -m pip freeze --exclude-editable > "$HERE/requirements.lock"
{
    git -C "$BORG_SRC" rev-parse HEAD
    echo "# source:  $BORG_SRC"
    echo "# describe: $(git -C "$BORG_SRC" describe --tags --match '[0-9]*' 2>/dev/null || echo '<no tags>')"
    echo "# built:   $("$VENV/bin/python" -c 'import borg; print(borg.__version__)' 2>/dev/null || echo '<unknown>')"
} > "$HERE/borg-commit.txt"

cat > "$HERE/borg2" <<EOF
#!/usr/bin/env bash
# Generated by tests/borg2/setup.sh - the pinned borg 2 reference interpreter.
# Its cache and config are kept apart from any real borg installation on this
# machine, so a test run can never disturb the user's own repositories.
set -euo pipefail
export BORG_BASE_DIR="\${BORG_BASE_DIR:-$ROOT/.venv-borg2/base}"
mkdir -p "\$BORG_BASE_DIR"
exec "$VENV/bin/borg" "\$@"
EOF
chmod +x "$HERE/borg2"

# Verify, and fail loudly if it does not work: a venv that installs cleanly but cannot
# run is worse than none, because every later interop failure gets blamed on borge.
if ! reported=$("$HERE/borg2" --version 2>&1); then
    printf 'setup.sh: the borg2 wrapper does not run:\n%s\n' "$reported" >&2
    exit 1
fi
case "$reported" in
    "borg 2."*) ;;
    *) die "expected a borg 2.x interpreter, got: $reported" ;;
esac

echo "setup.sh: $reported"
echo "setup.sh: wrote $HERE/requirements.lock, $HERE/borg-commit.txt, $HERE/borg2"
