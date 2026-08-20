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
# Exit status: 0 if every command is within its budget, none is unlisted, and every option
# borge adds is recorded; 1 otherwise.
#
# # Both directions, since 2026-08-19
#
# The gate used to look one way only - options borg has and borge lacks - so half of any
# difference between the two tools was invisible to it. It now also reports the options
# borge adds and fails on one that is not listed in borge_only with a reason, which is what
# the command gate has always done for an absent command. It also compares the subcommands
# of "debug", "key" and "benchmark", which neither tool lists at the top level: both sides
# used to report none, so the comparison was empty rather than wrong. That found "key
# remove --passphrase", missing and unseen through eight stages.
#
# It sees a subcommand borge does not have only if borge has it: the enumeration is borge's.
# A subcommand borg has and borge lacks is command-coverage.sh's business, and was nobody's
# until 2026-08-20 - see the note above GROUP_SUBS.
#
# # What this gate deliberately does not see
#
#   - Semantics. Both tools having an option called "v" says nothing about it meaning the
#     same thing: borg's -v is an alias of --info (a log level), borge's is --verbose.
#     This is the big one: every count below is of spellings, and two tools can agree on
#     every option name while disagreeing on what the options do.
#   - Anything about the *output* an option produces. That is what the differential tests
#     in internal/cli are for; the JSON schemas in particular matched by name and not by
#     shape until they were compared as data.

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
# It went UP to 36 on 2026-08-19, without borge losing anything: the gate started comparing
# the group subcommands and found "key remove --passphrase". A number that only ever falls
# is a number that has stopped measuring. Down to 34 the same day, when --format reached
# check and diff; down to 30 on 2026-08-20, when list and diff both reached zero - the two
# --sort-by options, list's --depth and diff's --same-chunker-params; down to 26 the same
# day, when export-tar and import-tar reached zero - --tar-filter on both and --filter on
# import-tar and create; down to 22 when prune reached zero - --keep, --from and the two
# quarterly rules - which also took the alias gap from 17 to 11, prune having six short
# spellings borge did not have.
declare -A budget=(
    [analyze]=0
    [benchmark]=0
    [break-lock]=0
    [check]=2
    [compact]=0
    [completion]=0
    [create]=5
    [debug]=0
    [delete]=0
    [diff]=0
    [export-tar]=0
    [extract]=3
    [find]=0
    [help]=2
    [import-tar]=0
    [info]=0
    [key]=0
    [list]=0
    [prune]=0
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
    # The three command groups carry no options of their own; their subcommands do, and
    # neither tool's group help lists them. Compared since 2026-08-19; every one was
    # already complete but "key remove", which is missing borg's --passphrase.
    [debug]=0
    [key]=0
    [benchmark]=0
    [debug info]=0
    [debug dump-archive]=0
    [debug dump-archive-items]=0
    [debug dump-manifest]=0
    [debug dump-repo-objs]=0
    [debug search-repo-objs]=0
    [debug get-obj]=0
    [debug put-obj]=0
    [debug delete-obj]=0
    [debug format-obj]=0
    [debug convert-profile]=0
    [debug parse-obj]=0
    [debug id-hash]=0
    [key export]=0
    [key import]=0
    [key change-passphrase]=0
    [key change-location]=0
    [key add]=0
    [key list]=0
    [key remove]=1
    [benchmark crud]=0
    [benchmark cpu]=0
)

# borge_only lists, per command, the options borge has that borg does not. A port may add
# things; an *unrecorded* addition is what this refuses, exactly as the command gate
# requires a reason for every absent command. An extra not listed here fails the gate.
#
# The reasons, by group rather than by line:
#
#   deleted, reverse
#       Shared-group leakage. borge registers its archive-filter group whole, where borg's
#       define_archive_filters_group takes a "deleted" parameter and enables it per
#       command. Deciding these per command is PORTING_PLAN section 11.4c; until then they
#       reach commands borg does not put them on. (first, last and sort-by were the same
#       leakage and are gone from prune, which is where they could change what is deleted.)
#   analyze hotspots
#       How many busy directories to report. borg's analyze computes the same thing and
#       fixes the count.
#   check dry-run, repo-compress dry-run
#       borge lets both say what they would do. borg has neither.
#   delete force
#       borge's own, for deleting an archive whose metadata cannot be read.
#   extract C
#       The destination directory. borg extracts into the working directory only.
#   find short
#       The paths alone, as "list --short" gives them.
#   version long
#       The build and interoperability details. It is where the four keys that used to be
#       in "version --json" live now; see DIVERGENCES.md #42.
declare -A borge_only=(
    [analyze]="hotspots reverse"
    [check]="dry-run reverse"
    [delete]="force"
    [extract]="C"
    [find]="deleted reverse short"
    [info]="deleted reverse"
    [repo-compress]="dry-run"
    [repo-list]="reverse"
    [version]="long"
)

# prune's --first, --last and --sort-by were the last of that leakage and were decided on
# 2026-08-20: removed. All three change which archives prune *considers*, and therefore
# which it deletes; --sort-by changes the order the keep rules walk, which changes the
# decisions themselves. borg has none of them there, and what they were reachable for -
# "the newest N", "everything since X" - is what borg's --keep and --from are. borge's own
# --keep-last, --keep-within and --keep-oldest went the same day and for the same reason:
# all three are spellings of things borg 2 already has. See DIVERGENCES.md #50.

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
    "$BORGE" "$@" -help 2>&1 |
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

# The three command groups carry their options on their subcommands, and neither tool lists
# those at the top level: both sides reported none and the comparison was empty rather than
# wrong.
#
# The list is borge's, so this loop only ever sees a subcommand both tools have - which is
# what an option comparison needs. The comment here used to justify that by saying borg's
# group help "names no subcommand at all", which was untrue: "borg debug --help" lists all
# thirteen of them under "<command>". The false reason is what made the blind spot look
# deliberate, and it cost "debug convert-profile" eight stages unseen - invisible here
# because the list came from borge, and invisible to command-coverage.sh because that
# compared top-level names, where "debug" matched. Since 2026-08-20 the missing direction
# belongs to command-coverage.sh, which descends into the groups and asks borg - both ways,
# so a subcommand borge has and borg does not is reported there too, exactly as a borge-only
# *command* is. Here it simply drops out below, when borg's --help for it fails.
GROUP_SUBS=()
for grp in debug key benchmark; do
    while IFS= read -r sub; do
        [ -n "$sub" ] && GROUP_SUBS+=("$grp $sub")
    done < <("$BORGE" "$grp" --help 2>&1 |
        sed -n '/^commands:/,$p' | grep -E '^  [a-z][a-z0-9-]* ' | awk '{print $1}' | sort -u)
done
if [ "${#GROUP_SUBS[@]}" -lt 10 ]; then
    echo "option-coverage: found only ${#GROUP_SUBS[@]} subcommands under debug, key and" \
         "benchmark; the group help format has changed" >&2
    exit 64
fi
BORGE_CMDS+=("${GROUP_SUBS[@]}")

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

printf '%-24s %6s %8s %7s  %s\n' "COMMAND" "BORG" "MISSING" "BUDGET" "STATUS"
printf '%-24s %6s %8s %7s  %s\n' "-------" "----" "-------" "------" "------"

status=0
total_missing=0
total_own=0
total_alias=0
unlisted=0
declare -A missing_detail=()
declare -A alias_detail=()
declare -A extra_detail=()

for c in "${BORGE_CMDS[@]}"; do
    # Commands borge has and borg does not are the command gate's business. The help text
    # is fetched once and reused: a failure here means borg has no such command.
    # Unquoted: a group subcommand is two words ("debug dump-archive").
    # shellcheck disable=SC2086
    if ! borg_help=$("$BORG" $c --help 2>&1); then
        continue
    fi

    mapfile -t own < <(printf '%s\n' "$borg_help" | borg_options | sed -n 's/^own //p')
    # shellcheck disable=SC2086
    mapfile -t have < <(borge_options $c)

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

    # The reverse direction: options borge has and borg does not. A port may add things -
    # that is not a failure - but an *unrecorded* addition is, exactly as the command gate
    # requires a reason for every absence. Without this the gate could only ever see half
    # the difference between the two tools.
    declare -A borg_names=()
    for g in "${own[@]}" "${COMMON[@]}"; do
        IFS='|' read -ra names <<<"$g"
        for n in "${names[@]}"; do borg_names[$n]=1; done
    done
    extra=()
    for o in "${have[@]}"; do
        [ -n "${borg_names[$o]:-}" ] && continue
        extra+=("$o")
    done
    unset borg_names
    extra_detail[$c]="${extra[*]:-}"

    n_miss=${#miss[@]}
    n_alias=${#aliases[@]}
    total_own=$((total_own + n_own))
    total_missing=$((total_missing + n_miss))
    total_alias=$((total_alias + n_alias))
    missing_detail[$c]="${miss[*]:-}"
    alias_detail[$c]="${aliases[*]:-}"

    if [ -z "${budget[$c]+set}" ]; then
        printf '%-24s %6d %8d %7s  %s\n' "$c" "$n_own" "$n_miss" "-" "NOT IN THE BUDGET TABLE"
        unlisted=$((unlisted + 1))
        status=1
    elif [ "$n_miss" -gt "${budget[$c]}" ]; then
        printf '%-24s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" "OVER BUDGET"
        status=1
    elif [ "$n_miss" -lt "${budget[$c]}" ]; then
        printf '%-24s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" \
            "improved - lower the budget to $n_miss"
    elif [ "$n_miss" -eq 0 ]; then
        printf '%-24s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" "complete"
    else
        printf '%-24s %6d %8d %7d  %s\n' "$c" "$n_own" "$n_miss" "${budget[$c]}" "at budget"
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
echo "options borge has and borg does not (each must be listed in borge_only above):"
unrecorded=0
for c in "${BORGE_CMDS[@]}"; do
    [ -z "${extra_detail[$c]:-}" ] && continue
    declare -A allowed=()
    for o in ${borge_only[$c]:-}; do allowed[$o]=1; done
    bad=()
    for o in ${extra_detail[$c]}; do
        [ -n "${allowed[$o]:-}" ] || bad+=("$o")
    done
    unset allowed
    if [ "${#bad[@]}" -gt 0 ]; then
        printf '  %-24s %s   <- NOT RECORDED: %s\n' "$c" "${extra_detail[$c]}" "${bad[*]}"
        unrecorded=$((unrecorded + ${#bad[@]}))
        status=1
    else
        printf '  %-24s %s\n' "$c" "${extra_detail[$c]}"
    fi
done
# A listed option borge no longer has is stale: the reason above outlived the option, which
# is the same rot the budget table's "improved" line catches in the other direction.
for c in "${!borge_only[@]}"; do
    for o in ${borge_only[$c]}; do
        case " ${extra_detail[$c]:-} " in
            *" $o "*) ;;
            *) printf '  %-24s %s   <- STALE: borge no longer has it\n' "$c" "$o"
               status=1 ;;
        esac
    done
done
if [ "$unrecorded" -gt 0 ]; then
    echo "  $unrecorded option(s) borge adds without a recorded reason"
fi

# Recorded here is not the same as told to the user. An option borge adds has to say so in
# its own help, or somebody reading "borge prune --help" cannot tell which of those rules
# their borg documentation covers. The marker is the phrase "borge only", which also covers
# "borge only on this command" for an option borg has elsewhere.
undocumented=0
for c in "${!borge_only[@]}"; do
    # shellcheck disable=SC2086
    help_text=$("$BORGE" $c -help 2>&1 || true)
    for o in ${borge_only[$c]}; do
        # The option line plus the indented help line that follows it.
        entry=$(printf '%s\n' "$help_text" | grep -A1 -E "^  -$o( |$)" || true)
        case "$entry" in
            *"borge only"*) ;;
            "") ;;  # option gone: already reported as stale above
            *) printf '  %-24s %s   <- NOT MARKED "borge only" in its help\n' "$c" "$o"
               undocumented=$((undocumented + 1))
               status=1 ;;
        esac
    done
done
if [ "$undocumented" -gt 0 ]; then
    echo "  $undocumented borge-only option(s) whose help does not say so"
fi
echo

echo "common options (borg puts these on every command; counted once, not per command):"
# Read once into a set rather than piping into "grep -q" per option: grep -q exits at the
# first match, which sends SIGPIPE up the pipeline, and under "set -o pipefail" that makes
# the whole test report failure. Every option came out "absent", including the -r that
# borge plainly has.
#
# That was only half the bug, and the visible half. COMMON holds option *groups* as borg
# spells them ("r|repo", "info|v|verbose") while borge_options yields one *name* per line,
# so the lookup below could only ever match a group with a single spelling. Every
# multi-spelling common option read as absent whatever borge did, and the report said
# "14 common options, 14 absent" while borge had three of them. The own-options loop above
# had always compared group-wise; this one had not, which is why the two disagreed.
declare -A common_have=()
while IFS= read -r o; do common_have[$o]=1; done < <(borge_options repo-info)

# Two options Go's flag package honours without printing: it handles -h and --help itself
# and lists neither, so scraping the help text cannot see them. Probed instead, because
# "absent" would be false - a user typing "borge repo-info -h" gets the usage.
# Captured into a variable rather than piped into "grep -q", for the reason above: grep -q
# exits at the first match and the SIGPIPE that follows fails the pipeline under pipefail.
# Writing it the obvious way here made the probe always say no, which is the same mistake
# twice in one file.
for probe in h help; do
    probe_out=$("$BORGE" repo-info "-$probe" 2>&1 || true)
    case "$probe_out" in
        "Usage of borge"*) common_have[$probe]=1 ;;
    esac
done

common_absent=0
common_alias=0
for g in "${COMMON[@]}"; do
    IFS='|' read -ra names <<<"$g"
    present=
    absent_names=()
    for n in "${names[@]}"; do
        if [ -n "${common_have[$n]:-}" ]; then present=1; else absent_names+=("$n"); fi
    done
    label=$(longest_name "$g")
    if [ -z "$present" ]; then
        printf '  %-16s absent\n' "$label"
        common_absent=$((common_absent + 1))
    elif [ "${#absent_names[@]}" -gt 0 ]; then
        # Present under one spelling and not another, which is the alias gap the
        # command-specific loop reports separately. borge has -v and --verbose but not
        # borg's --info, which is the same option in borg's help.
        printf '  %-16s implemented, without %s\n' "$label" "${absent_names[*]}"
        common_alias=$((common_alias + 1))
    else
        printf '  %-16s implemented\n' "$label"
    fi
done
echo "  ${#COMMON[@]} common options, $common_absent absent in borge," \
     "$common_alias missing a spelling"

echo
echo "borg command-specific options: $total_own"
echo "missing in borge:              $total_missing"
echo "present but missing a spelling borg also offers: $total_alias"

echo
echo "what is missing, per command:"
for c in "${BORGE_CMDS[@]}"; do
    d="${missing_detail[$c]:-}"
    [ -z "$d" ] && continue
    printf '  %-24s %s\n' "$c" "$d"
done

echo
echo "spellings borg offers and borge does not (short=long):"
for c in "${BORGE_CMDS[@]}"; do
    d="${alias_detail[$c]:-}"
    [ -z "$d" ] && continue
    printf '  %-24s %s\n' "$c" "$d"
done

if [ "$unlisted" -gt 0 ]; then
    echo >&2
    echo "option-coverage: $unlisted command(s) are not in the budget table. Add them," \
         "with the number of options they are missing today." >&2
fi

exit "$status"
