#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Plan task 0.8 - blocking for stage 1.6 and stage 2.
#
# borg 2 moved two components borge must port into separate PyPI packages:
#   borghash   - the open-addressed ChunkIndex hash table  (borge stage 1.6)
#   borgstore  - the object store under the repository      (borge stage 2)
#
# They are Borg Collective projects, but that is not the same as knowing their
# license, and docs/LICENSING.md deliberately refuses to assume it matches borg's
# BSD-3-Clause. This script records what they actually say, into licenses/, so the
# question is answered from evidence before any of their code is read.
#
# If either turns out to be copyleft, borge implements that component from the
# on-disk format description instead of porting it.

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"
VENV="${BORGE_BORG2_VENV:-$ROOT/.venv-borg2}"
DEST="$ROOT/licenses/upstream-python"

if [ ! -x "$VENV/bin/python" ]; then
    echo "check-upstream-licenses: no borg2 venv at $VENV - run tests/borg2/setup.sh first" >&2
    exit 1
fi

mkdir -p "$DEST"
fail=0

for pkg in borghash borgstore; do
    echo "== $pkg =="
    meta=$("$VENV/bin/python" - "$pkg" <<'PY'
import sys
from importlib import metadata
name = sys.argv[1]
try:
    d = metadata.metadata(name)
except metadata.PackageNotFoundError:
    print("NOT_INSTALLED")
    sys.exit(0)
print("Name:", d.get("Name"))
print("Version:", d.get("Version"))
print("License:", d.get("License") or "<unset>")
print("License-Expression:", d.get("License-Expression") or "<unset>")
for k in d.get_all("Classifier") or []:
    if k.startswith("License"):
        print("Classifier:", k)
for k in d.get_all("License-File") or []:
    print("License-File:", k)
print("Home-page:", d.get("Home-page") or "<unset>")
for k in d.get_all("Project-URL") or []:
    print("Project-URL:", k)
PY
)
    if [ "$meta" = "NOT_INSTALLED" ]; then
        echo "  NOT INSTALLED in $VENV - cannot determine license" >&2
        fail=1
        continue
    fi
    printf '%s\n' "$meta" | sed 's/^/  /'
    printf '%s\n' "$meta" > "$DEST/$pkg.metadata.txt"

    # Copy any license file the wheel shipped, so the actual text is on record and
    # not just the metadata field, which is frequently wrong or empty.
    "$VENV/bin/python" - "$pkg" "$DEST" <<'PY'
import shutil, sys
from importlib import metadata
from pathlib import Path
name, dest = sys.argv[1], Path(sys.argv[2])
dist = metadata.distribution(name)
found = []
for f in dist.files or []:
    p = str(f)
    if "licenses/" in p or Path(p).name.upper().startswith(("LICENSE", "COPYING", "NOTICE")):
        src = Path(dist.locate_file(f))
        if src.is_file():
            out = dest / f"{name}.{Path(p).name}"
            shutil.copyfile(src, out)
            found.append(out.name)
print("  license files copied:", ", ".join(found) if found else "NONE FOUND")
PY

    expr=$(printf '%s\n' "$meta" | sed -n 's/^License-Expression: //p')
    lic=$(printf '%s\n' "$meta" | sed -n 's/^License: //p')
    decl="${expr}${lic}"
    case "$decl" in
        *GPL*|*AGPL*|*LGPL*|*MPL*|*EUPL*)
            echo "  !! COPYLEFT DECLARED: '$decl'" >&2
            echo "  !! Do not port this package's code. Implement from the format description." >&2
            fail=1
            ;;
        *BSD*|*MIT*|*Apache*|*ISC*)
            echo "  OK: permissive ('$decl') - porting is allowed, preserve the notice"
            ;;
        *)
            echo "  ?? license not determinable from metadata ('$decl')" >&2
            echo "  ?? read $DEST/$pkg.* and the project page, then record the finding manually" >&2
            fail=1
            ;;
    esac
    echo
done

if [ "$fail" -ne 0 ]; then
    echo "check-upstream-licenses: UNRESOLVED - see docs/LICENSING.md section 6" >&2
    exit 1
fi
echo "check-upstream-licenses: ok - findings recorded in licenses/upstream-python/"
