#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Compare borg's per-command options against borge's, and classify every difference.
#
#   tests/evidence/option-coverage.sh
#
# The companion to command-coverage.sh, and the reason it exists is that the command gate
# compares command *names* and nothing else. borge had all but five of borg's commands and
# was still missing about twenty-five options on "create" alone, "--dry-run" among them -
# entirely invisible to a gate that only counts commands. Counting the options by hand
# instead would repeat, one level down, the mistake command-coverage.sh was built to stop,
# so this asks both tools.
#
# borg's side is the option sections of "borg CMD --help"; borge's is "borge CMD -help",
# which Go's flag package generates from the FlagSet the command actually registered.
# Neither list is written here.
#
# # The budget, and why it is not a list of reasons
#
# Roughly a hundred and fifty options are missing today. A table of a hundred and fifty
# hand-written reasons would be a hand-maintained list again, and nobody would keep it
# true. So each command carries a number instead: how many of its options are missing
# right now. The gate fails when a command is missing MORE than its budget, which catches
# a regression, and reports when it is missing fewer, so the number can only be ratcheted
# down. A command absent from the budget table fails outright, so a newly added command
# cannot arrive with a silent gap.
#
# Exit status: 0 if every command is within its budget and none is unlisted, 1 otherwise.
#
# # What this gate deliberately does not see
#
#   - Semantics. Both tools having an option called "v" says nothing about it meaning the
#     same thing: borg's -v is an alias of --info (a log level), borge's is --verbose.
#   - Subcommand options. "debug", "key" and "benchmark" carry their options on their
#     subcommands, and neither tool lists those at the top level, so both sides report
#     none and the comparison is empty rather than wrong. Extending it is recorded in
#     PORTING_PLAN section 11.
#   - Options borge has that borg does not. Those are printed, but they are not failures:
#     a port is allowed to add something, as long as it says so.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BORG="${BORGE_BORG2:-$ROOT/.venv-borg2/bin/borg}"
BORGE="${BORGE_BIN:-$ROOT/bin/borge}"

if [ ! -x "$BORG" ]; then
    echo "option-coverage: borg 2 not built at $BORG (run 'make borg2')" >&2
    exit 64
fi
if [ ! -x "$BORGE" ]; then
    echo "option-coverage: borge not built at $BORGE (run 'make build')" >&2
    exit 64
fi

export PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1

# budget is how many of borg's command-specific options each command is still missing.
# Lower it as options land; never raise it without saying why in the commit message.
# A command borge implements and this table omits is a failure.
# Baseline measured 2026-08-18 against borg 2.0.0b23.dev377+g114bd1e94: 254 borg
# command-specific options, 111 of them missing. Lowered the same day: 79 when the shared
# archive date filters landed on eight commands at once, 64 when info and tag started
# taking the filter group at all, 63 when repo-list gained --format, 61 when list and find did, 58 when create gained the tag-based exclusions, 55 when --timestamp landed on three commands, 54 with create --dry-run, 51 with the timestamp-storage options, 48 when --list reached delete, undelete and export-tar, 44 with the --paths-from-* group, 39 with the stdin-content group, 35 when prune gained borg's listing.
declare -A budget=(
    [analyze]=0
    [benchmark]=0
    [break-lock]=0
    [check]=3
    [compact]=0
    [completion]=0
    [create]=6
    [debug]=0
    [delete]=0
    [diff]=3
    [export-tar]=1
    [extract]=3
    [find]=0
    [help]=2
    [import-tar]=2
    [info]=0
    [key]=0
    [list]=2
    [prune]=4
    [recreate]=4
    [rename]=0
    [repo-compress]=0
    [repo-create]=3
    [repo-delete]=1
    [repo-info]=0
    [repo-list]=1
    [repo-space]=0
    [tag]=0
    [undelete]=0
    [version]=0
    [with-lock]=0
)

# borg_options prints one *option* per line for a command - not one name per line. An
# option is a group of spellings: "-n, --dry-run" is one option with two names, and
# "--info, -v, --verbose" is one option with three. Counting names instead of options
# overstates the gap and, worse, would let the budget below be paid down by adding
# one-letter aliases to options borge already has.
#
# Format: "own n|dry-run" or "common p|progress", names joined by "|".
#
# The option sections are the ones whose heading ends in "options:", plus argparse's
# "required arguments:" and borg's "Archive filters:". Scoping by heading matters: a borg
# epilog contains prose lines that end in a colon too, and an unscoped scan would read
# whatever followed them.
# Takes the help text on stdin rather than a command name, so that each command costs one
# borg process. borg is Python: at three invocations per command this script took longer
# than the whole interop suite's setup.
borg_options() {
    awk '
        # A heading is at column zero and ends in a colon.
        /^[A-Za-z][^:]*:$/ {
            section = $0
            in_opts = (section ~ /[Oo]ptions:$/) || (section == "required arguments:") ||
                      (section == "Archive filters:")
            is_common = (section == "Common options:")
            next
        }
        # An option line is indented exactly two spaces and starts with a dash. Help text
        # wraps at a deeper indent, so it cannot be mistaken for one.
        in_opts && /^  -/ {
            # The spec is everything before the two-or-more spaces that start the help.
            spec = $0
            sub(/   .*$/, "", spec)
            n = split(spec, parts, /[ ,]+/)
            group = ""
            for (i = 1; i <= n; i++) {
                if (parts[i] ~ /^--?[A-Za-z][A-Za-z0-9-]*$/) {
                    name = parts[i]
                    sub(/^--?/, "", name)
                    group = (group == "" ? name : group "|" name)
                }
            }
            if (group != "") print (is_common ? "common " : "own ") group
        }
    ' | sort -u
}

# longest_name is how an option group is named in a report: its longest spelling, which is
# the long form wherever there is one.
longest_name() {
    local best="" n
    local IFS='|'
    for n in $1; do
        [ "${#n}" -gt "${#best}" ] && best="$n"
    done
    printf '%s' "$best"
}

# borge_options prints one option name per line, without the leading dash. Go's flag
# package prints every registered flag on its own line at a two-space indent, with the
# help text on the following line at a deeper one.
borge_options() {
    "$BORGE" "$1" -help 2>&1 |
        grep -E '^  -[A-Za-z]' |
        sed -E 's/^  -([A-Za-z0-9][A-Za-z0-9-]*).*/\1/' |
        sort -u
}

mapfile -t BORGE_CMDS < <(
    "$BORGE" 2>&1 | sed -n '/^commands:/,$p' | grep -E '^  [a-z][a-z0-9-]* ' | awk '{print $1}' | sort -u
)
if [ "${#BORGE_CMDS[@]}" -lt 20 ]; then
    echo "option-coverage: only found ${#BORGE_CMDS[@]} borge commands; the usage format" \
         "has probably changed and this script needs updating" >&2
    exit 64
fi

# borg's common options are the same on every command, so they are read once and reported
# once rather than counted against each of thirty commands.
mapfile -t COMMON < <("$BORG" repo-info --help 2>&1 | borg_options | sed -n 's/^common //p')
if [ "${#COMMON[@]}" -lt 8 ]; then
    echo "option-coverage: found only ${#COMMON[@]} common options; the extractor is" \
         "broken and every count below would be wrong" >&2
    exit 64
fi
# Common options are recognised by any of their spellings, so a command's own list is
# filtered by name rather than by group.
declare -A is_common=()
for g in "${COMMON[@]}"; do
    IFS='|' read -ra names <<<"$g"
    for n in "${names[@]}"; do is_common[$n]=1; done
done

echo "borge option coverage against $("$BORG" --version 2>&1)"
echo "generated $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo

printf '%-16s %6s %8s %7s  %s\n' "COMMAND" "BORG" "MISSING" "BUDGET" "STATUS"
printf '%-16s %6s %8s %7s  %s\n' "-------" "----" "-------" "------" "------"

status=0
total_missing=0
total_own=0
total_alias=0
unlisted=0
declare -A missing_detail=()
declare -A alias_detail=()

for c in "${BORGE_CMDS[@]}"; do
    # Commands borge has and borg does not are the command gate's business. The help text
    # is fetched once and reused: a failure here means borg has no such command.
    if ! borg_help=$("$BORG" "$c" --help 2>&1); then
        continue
    fi

    mapfile -t own < <(printf '%s\n' "$borg_help" | borg_options | sed -n 's/^own //p')
    mapfile -t have < <(borge_options "$c")

    declare -A haveset=()
    for o in "${have[@]}"; do haveset[$o]=1; done

    miss=()
    aliases=()
    n_own=0
    for g in "${own[@]}"; do
        IFS='|' read -ra names <<<"$g"
        # A group is common if any of its spellings is.
        skip=
        for n in "${names[@]}"; do [ -n "${is_common[$n]:-}" ] && skip=1; done
        [ -n "$skip" ] && continue
        n_own=$((n_own + 1))

        present=
        for n in "${names[@]}"; do [ -n "${haveset[$n]:-}" ] && present=1; done
        if [ -z "$present" ]; then
            miss+=("$(longest_name "$g")")
            continue
        fi
        # borge has the option under some name. Any spelling borg offers and borge does
        # not is an alias gap: a smaller thing than a missing option, and counted apart so
        # that the budget cannot be paid down by adding one-letter aliases.
        for n in "${names[@]}"; do
            [ -n "${haveset[$n]:-}" ] && continue
            aliases+=("$n=$(longest_name "$g")")
        done
    done

    n_miss=${#miss[@]}
    n_alias=${#aliases[@]}
    total_own=$((total_own + n_own))
    total_missing=$((total_missing + n_miss))
    total_alias=$((total_alias + n_alias))
    missing_detail[$c]="${miss[*]:-}"
    alias_detail[$c]="${aliases[*]:-}"

    if [ -z "${budget[$c]+set}" ]; then
        printf '%-16s %6d %8d %7s  %s\n' "$c" "$n_own" "$n_miss" "-" "NOT IN THE BUDGET TABLE"
        unlisted=$((unlisted + 1))
        status=1
    elif [ "$n_miss" -gt "${budget[$c]}" ]; then
        printf '%-16s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" "OVER BUDGET"
        status=1
    elif [ "$n_miss" -lt "${budget[$c]}" ]; then
        printf '%-16s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" \
            "improved - lower the budget to $n_miss"
    elif [ "$n_miss" -eq 0 ]; then
        printf '%-16s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" "complete"
    else
        printf '%-16s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" "at budget"
    fi
    unset haveset
done

# The comparison reading nothing would report a clean sheet, which is the one wrong answer
# that looks like success.
if [ "$total_own" -lt 40 ]; then
    echo >&2
    echo "option-coverage: found only $total_own command-specific options across all of" \
         "borg's commands; the extractor is broken and this report would pass on anything" >&2
    exit 64
fi

echo
echo "common options (borg puts these on every command; counted once, not per command):"
# Read once into a set rather than piping into "grep -q" per option: grep -q exits at the
# first match, which sends SIGPIPE up the pipeline, and under "set -o pipefail" that makes
# the whole test report failure. Every option came out "absent", including the -r that
# borge plainly has.
declare -A common_have=()
while IFS= read -r o; do common_have[$o]=1; done < <(borge_options repo-info)
common_absent=0
for o in "${COMMON[@]}"; do
    if [ -n "${common_have[$o]:-}" ]; then
        printf '  %-16s implemented\n' "$o"
    else
        printf '  %-16s absent\n' "$o"
        common_absent=$((common_absent + 1))
    fi
done
echo "  ${#COMMON[@]} common options, $common_absent absent in borge"

echo
echo "borg command-specific options: $total_own"
echo "missing in borge:              $total_missing"
echo "present but missing a spelling borg also offers: $total_alias"

echo
echo "what is missing, per command:"
for c in "${BORGE_CMDS[@]}"; do
    d="${missing_detail[$c]:-}"
    [ -z "$d" ] && continue
    printf '  %-16s %s\n' "$c" "$d"
done

echo
echo "spellings borg offers and borge does not (short=long):"
for c in "${BORGE_CMDS[@]}"; do
    d="${alias_detail[$c]:-}"
    [ -z "$d" ] && continue
    printf '  %-16s %s\n' "$c" "$d"
done

if [ "$unlisted" -gt 0 ]; then
    echo >&2
    echo "option-coverage: $unlisted command(s) are not in the budget table. Add them," \
         "with the number of options they are missing today." >&2
fi

exit "$status"
