#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Compare borg's subcommand list against borge's, and classify every difference.
#
#   tests/evidence/command-coverage.sh
#
# This is the stage 8 gate. Stage 8 is "feature parity", and the only honest way to
# know whether it has been reached is to ask borg what commands it has rather than to
# consult a list written by hand - a list written by hand is exactly how a stage gets
# declared complete while three commands are missing, which is what happened on
# 2026-08-17 before this script existed.
#
# Exit status: 0 if every borg command is either implemented or has a recorded reason,
# 1 if any command is unaccounted for.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BORG="${BORGE_BORG2:-$ROOT/.venv-borg2/bin/borg}"
BORGE="${BORGE_BIN:-$ROOT/bin/borge}"

if [ ! -x "$BORG" ]; then
    echo "command-coverage: borg 2 not built at $BORG (run 'make borg2')" >&2
    exit 64
fi
if [ ! -x "$BORGE" ]; then
    echo "command-coverage: borge not built at $BORGE (run 'make build')" >&2
    exit 64
fi

# deferred maps a borg command borge does not implement to where that is written down.
# A command missing from both this table and borge is an unexplained gap and fails.
declare -A deferred=(
    [mount]="non-goal for 1.0 (PORTING_PLAN 0.6): FUSE, deferred to section 9"
    [umount]="non-goal for 1.0 (PORTING_PLAN 0.6): pairs with mount"
    [webdav]="non-goal for 1.0 (PORTING_PLAN 0.6)"
    [serve]="stage 8 remote backends (PORTING_PLAN 0.6, 11): not yet implemented"
    [transfer]="non-goal for 1.0 from borg 1.x (PORTING_PLAN 0.6); borg2-to-borg2 transfer is NOT yet decided"
)

borg_commands() {
    # borg lists its subcommands indented under the "For more details" line, one per
    # line as "    name    summary". Taking only that block avoids picking up the help
    # topics listed after it at a shallower indent.
    #
    # One space is enough after the name: both tools pad the column, and a name longer
    # than the padding leaves exactly one. Requiring two silently dropped repo-compress,
    # which is thirteen characters against borge's twelve-wide column - so the gate
    # reported a command it had as missing, which is the same class of error it exists
    # to catch.
    "$BORG" --help 2>&1 |
        sed -n '/For more details of each subcommand/,$p' |
        grep -E '^    [a-z][a-z0-9-]* ' |
        awk '{print $1}' |
        sort -u
}

borge_commands() {
    "$BORGE" 2>&1 |
        sed -n '/^commands:/,$p' |
        grep -E '^  [a-z][a-z0-9-]* ' |
        awk '{print $1}' |
        sort -u
}

mapfile -t BORG_CMDS < <(borg_commands)
mapfile -t BORGE_CMDS < <(borge_commands)

if [ "${#BORG_CMDS[@]}" -lt 20 ]; then
    echo "command-coverage: only found ${#BORG_CMDS[@]} borg commands; the --help format" \
         "has probably changed and this script needs updating" >&2
    exit 64
fi
if [ "${#BORGE_CMDS[@]}" -lt 20 ]; then
    echo "command-coverage: only found ${#BORGE_CMDS[@]} borge commands; the usage format" \
         "has probably changed and this script needs updating" >&2
    exit 64
fi

declare -A have=()
for c in "${BORGE_CMDS[@]}"; do have[$c]=1; done

echo "borge command coverage against $("$BORG" --version 2>&1)"
echo "generated $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
printf '%-16s %s\n' "COMMAND" "STATUS"
printf '%-16s %s\n' "-------" "------"

implemented=0
explained=0
unexplained=0
for c in "${BORG_CMDS[@]}"; do
    if [ -n "${have[$c]:-}" ]; then
        printf '%-16s %s\n' "$c" "implemented"
        implemented=$((implemented + 1))
    elif [ -n "${deferred[$c]:-}" ]; then
        printf '%-16s %s\n' "$c" "absent - ${deferred[$c]}"
        explained=$((explained + 1))
    else
        printf '%-16s %s\n' "$c" "ABSENT AND UNEXPLAINED"
        unexplained=$((unexplained + 1))
    fi
done

# Commands borge has that borg does not. None today, but a port that grew its own
# command without saying so is worth noticing too.
for c in "${BORGE_CMDS[@]}"; do
    found=
    for b in "${BORG_CMDS[@]}"; do [ "$b" = "$c" ] && found=1 && break; done
    if [ -z "$found" ]; then
        printf '%-16s %s\n' "$c" "borge-only (not a borg command)"
    fi
done

echo
echo "borg commands:        ${#BORG_CMDS[@]}"
echo "implemented in borge: $implemented"
echo "absent, recorded:     $explained"
echo "absent, unexplained:  $unexplained"

# The recorded-but-not-done ones are listed again, because "recorded" is not "finished"
# and a summary line of "0 unexplained" reads like completeness otherwise. Only the ones
# that are still absent: an entry left in the table after the command was written would
# otherwise report finished work as a gap.
echo
echo "Of the recorded absences, these are gaps rather than decisions:"
for c in "${!deferred[@]}"; do
    [ -n "${have[$c]:-}" ] && continue
    case "${deferred[$c]}" in
        NOT\ IMPLEMENTED*|*NOT\ yet\ decided*) printf '  %-14s %s\n' "$c" "${deferred[$c]}" ;;
    esac
done | sort

[ "$unexplained" -eq 0 ]
