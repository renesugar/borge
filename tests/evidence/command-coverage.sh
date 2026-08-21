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
#
# # The command groups, since 2026-08-20
#
# "debug", "key" and "benchmark" carry their real commands one level down, and until this
# date neither gate could see a missing one. This script compared top-level names, where
# "debug" is a single name and matched; the option gate compared the groups' subcommands but
# enumerated them from *borge*, so a subcommand borge did not have was never on the list it
# compared. Both halves were true and "debug convert-profile" still fell between them and
# went unseen through eight stages, exactly as "key remove --passphrase" had one level down.
#
# borg does publish the list - under "<command>" in each group's own --help - so there is a
# side to ask, which is the only thing that makes this a gate rather than another table
# written by hand.

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
#
# A group's subcommand is keyed by its full name, "debug convert-profile", so a deferred
# subcommand records its reason the same way a deferred command does.
declare -A deferred=(
    [mount]="non-goal for 1.0 (PORTING_PLAN 0.6): FUSE, deferred to section 9"
    [umount]="non-goal for 1.0 (PORTING_PLAN 0.6): pairs with mount"
    [webdav]="non-goal for 1.0 (PORTING_PLAN 0.6)"
    [serve]="stage 8 remote backends (PORTING_PLAN 0.6, 11): not yet implemented"
)

# gap says which of the deferred commands are work still to do, as against settled
# non-goals. It is a field of its own rather than something inferred from the wording of
# the reason above, because that is what it used to be: the classification pattern matched
# "NOT IMPLEMENTED*" and "*NOT yet decided*" against the reason text, so serve's lowercase
# "not yet implemented" matched neither and the largest gap in stage 8 was silently left
# out of the very list this script prints to name the gaps.
declare -A gap=(
    [serve]=1
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

# GROUP_CMDS are the commands whose subcommands are the real commands. Named here rather
# than discovered: both tools show a group in the top-level list exactly like any other
# command, so there is nothing in either help text that says "this one has more inside".
#
# Not called GROUPS. That is one of bash's own variables - the caller's Unix group ids - and
# an assignment to it is silently discarded, so the loop below ran over group numbers, found
# no command named "1000", and reported nothing at all while still exiting 0.
GROUP_CMDS=(debug key benchmark)

borg_subcommands() {
    # The group's own help lists them under "<command>", indented one level deeper than the
    # option sections above it - so the block is taken from that line on, as the top-level
    # list is taken from "For more details".
    "$BORG" "$1" --help 2>&1 |
        sed -n '/^  <command>/,$p' |
        grep -E '^    [a-z][a-z0-9-]* ' |
        awk '{print $1}' |
        sort -u
}

borge_subcommands() {
    "$BORGE" "$1" --help 2>&1 |
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

# The groups, one level down. Counted into the same three totals: a subcommand is a
# command, and splitting the score would let a whole group empty out while the summary
# still read "every borg command implemented".
for grp in "${GROUP_CMDS[@]}"; do
    if [ -z "${have[$grp]:-}" ]; then
        # The group itself is absent, which the loop above has already reported. Descending
        # into it would report each of its subcommands as a second, dependent failure.
        continue
    fi
    mapfile -t SUBS < <(borg_subcommands "$grp")
    if [ "${#SUBS[@]}" -lt 2 ]; then
        echo "command-coverage: found only ${#SUBS[@]} subcommands under '$grp'; the group" \
             "help format has changed and this script needs updating" >&2
        exit 64
    fi
    declare -A have_sub=()
    while IFS= read -r sub; do
        [ -n "$sub" ] && have_sub[$sub]=1
    done < <(borge_subcommands "$grp")
    if [ "${#have_sub[@]}" -lt 2 ]; then
        echo "command-coverage: borge lists only ${#have_sub[@]} subcommands under '$grp';" \
             "the usage format has changed and this script needs updating" >&2
        exit 64
    fi

    for sub in "${SUBS[@]}"; do
        full="$grp $sub"
        if [ -n "${have_sub[$sub]:-}" ]; then
            printf '%-16s %s\n' "$full" "implemented"
            implemented=$((implemented + 1))
        elif [ -n "${deferred[$full]:-}" ]; then
            printf '%-16s %s\n' "$full" "absent - ${deferred[$full]}"
            explained=$((explained + 1))
        else
            printf '%-16s %s\n' "$full" "ABSENT AND UNEXPLAINED"
            unexplained=$((unexplained + 1))
        fi
    done

    # And the other way, as for the top-level commands.
    for sub in "${!have_sub[@]}"; do
        found=
        for b in "${SUBS[@]}"; do [ "$b" = "$sub" ] && found=1 && break; done
        [ -z "$found" ] && printf '%-16s %s\n' "$grp $sub" "borge-only (not a borg subcommand)"
    done
    unset have_sub
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
echo "borg commands:        ${#BORG_CMDS[@]} top-level, plus the subcommands of ${GROUP_CMDS[*]}"
echo "implemented in borge: $implemented"
echo "absent, recorded:     $explained"
echo "absent, unexplained:  $unexplained"

# The recorded-but-not-done ones are listed again, because "recorded" is not "finished"
# and a summary line of "0 unexplained" reads like completeness otherwise. Only the ones
# that are still absent: an entry left in the table after the command was written would
# otherwise report finished work as a gap.
echo
echo "Of the recorded absences, these are gaps rather than decisions:"
# Collected into a variable rather than piped straight into sort: a counter incremented
# inside "done | sort" is incremented in a subshell, so the "none" branch below would
# have fired every time - a check that cannot fail.
gap_lines=""
for c in "${!deferred[@]}"; do
    [ -n "${have[$c]:-}" ] && continue
    [ -z "${gap[$c]:-}" ] && continue
    gap_lines+="$(printf '  %-14s %s' "$c" "${deferred[$c]}")"$'\n'
done
if [ -n "$gap_lines" ]; then
    printf '%s' "$gap_lines" | sort
else
    echo "  (none - every recorded absence is a settled non-goal)"
fi

[ "$unexplained" -eq 0 ]
