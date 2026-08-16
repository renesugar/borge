#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Enforce the per-file marking convention from docs/LICENSING.md section 5.
#
# Every .go file must carry an SPDX-License-Identifier line. A file that is a port of
# upstream code must additionally name the upstream file it came from, and that file
# must actually exist in the pinned upstream checkout - a provenance header that
# points at nothing is worse than none, because it looks like it was checked.

set -euo pipefail

cd "$(dirname "$0")/.."

BORG_SRC="${BORGE_BORG_SRC:-/home/renes/projects/borg}"
RESTIC_SRC="${BORGE_RESTIC_SRC:-/home/renes/projects/restic}"

fail=0
note() { printf '%s\n' "$*" >&2; fail=1; }

# Only files we wrote; never vendored or generated trees.
mapfile -t files < <(git ls-files '*.go' | grep -v '^vendor/' || true)

if [ ${#files[@]} -eq 0 ]; then
    echo "check-spdx: no Go files tracked yet, nothing to check"
    exit 0
fi

for f in "${files[@]}"; do
    header=$(head -n 20 "$f")

    spdx=$(printf '%s\n' "$header" | sed -n 's|^// SPDX-License-Identifier: *||p' | head -n 1)
    if [ -z "$spdx" ]; then
        note "$f: missing '// SPDX-License-Identifier:' in the first 20 lines"
        continue
    fi

    case "$spdx" in
        "Apache-2.0")
            # Own work. A header that cites a concrete upstream source path is claiming
            # provenance, and must declare the upstream license in SPDX. Prose that
            # merely describes the project ("borge is a port of BorgBackup") is fine -
            # only a cited path counts as a claim about *this file*.
            if printf '%s\n' "$header" | grep -qE 'src/borg/|restic/internal/'; then
                note "$f: header cites an upstream source path but SPDX says plain Apache-2.0"
            fi
            ;;
        "Apache-2.0 AND BSD-3-Clause"|"Apache-2.0 AND BSD-2-Clause")
            # Ported. Must name a source file, and that file must exist upstream.
            # The path must end in an alphanumeric so a sentence-final period is not
            # swallowed into it ("...msgpack.py." would then not exist upstream).
            src=$(printf '%s\n' "$header" | sed -n \
                -e 's|.*\b\(src/borg/[A-Za-z0-9_/.-]*[A-Za-z0-9]\).*|\1|p' \
                -e 's|.*\b\(internal/[A-Za-z0-9_/.-]*\.go\).*|\1|p' | head -n 1)
            if [ -z "$src" ]; then
                note "$f: SPDX declares upstream code but the header names no upstream source file"
                continue
            fi
            case "$spdx" in
                *BSD-3-Clause) root="$BORG_SRC"; who=borg ;;
                *)             root="$RESTIC_SRC"; who=restic ;;
            esac
            if [ ! -d "$root" ]; then
                echo "check-spdx: warning: $who checkout not at $root, skipping existence check for $f" >&2
            elif [ ! -e "$root/$src" ]; then
                note "$f: header cites $who file '$src', which does not exist in $root"
            fi
            if ! printf '%s\n' "$header" | grep -q 'licenses/'; then
                note "$f: ported file must point at the upstream license (licenses/borg/LICENSE or licenses/restic/LICENSE)"
            fi
            ;;
        *)
            note "$f: unexpected SPDX expression '$spdx' (expected 'Apache-2.0', 'Apache-2.0 AND BSD-3-Clause' or 'Apache-2.0 AND BSD-2-Clause')"
            ;;
    esac
done

if [ "$fail" -ne 0 ]; then
    echo "check-spdx: FAILED - see docs/LICENSING.md section 5 for the required header" >&2
    exit 1
fi
echo "check-spdx: ok (${#files[@]} files)"
