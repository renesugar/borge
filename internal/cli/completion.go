// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go reimplementation of the completion command in borg's
// src/borg/archiver/completion_cmd.py. borg generates its scripts with the shtab
// library by introspecting argparse; Go's flag package has no equivalent, so the
// approach here is different - see describeCLI.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Shell completion.
//
// # Where the option names come from
//
// borg builds its completions from argparse: the parser knows every option of every
// subcommand, and shtab walks it. Go's flag package offers the same introspection - a
// FlagSet can be walked with VisitAll - but borge's flags are registered inside each
// command's function body, so there is no parser object to walk from outside.
//
// Rather than keep a second copy of every option (which would drift from the first the
// day somebody adds a flag and forgets), describeCLI runs each command with "-help". Every
// command registers its flags and then calls fs.Parse before doing anything else, so at
// the moment Parse rejects "-help" the FlagSet is fully populated and no repository has
// been opened. newFlagSet hands it to the Env's collector on the way past.
//
// The consequence worth knowing: this only works while every command keeps that shape.
// TestCompletionSeesEveryCommandsFlags fails if one stops.
//
// # What cannot be derived
//
// Positional arguments. The flag package has no notion of them - "list ARCHIVE PATH..." is
// just fs.Arg(0) and fs.Args()[1:] inside the function - so which commands take an archive
// name has to be stated, in archiveTakingCommands below. A test checks that every name in
// it is a real command; nothing can check the reverse, so it is a list to maintain.

// completionShells are the shells borge generates scripts for.
//
// borg also supports tcsh through shtab. borge does not: tcsh matches completions by word
// position and by option name across all subcommands at once, which for a tool with
// twenty-seven subcommands produces something misleading rather than helpful. Saying so is
// better than shipping a script that completes the wrong thing.
var completionShells = []string{"bash", "zsh", "fish"}

func cmdCompletion(e *Env, args []string) int {
	fs := newFlagSet(e, "completion")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("completion needs a shell: %s", strings.Join(completionShells, ", "))
		return ExitError
	}
	shell := fs.Arg(0)

	spec := describeCLI(e)
	switch shell {
	case "bash":
		writeBashCompletion(e.Stdout, spec)
	case "zsh":
		writeZshCompletion(e.Stdout, spec)
	case "fish":
		writeFishCompletion(e.Stdout, spec)
	case "tcsh":
		e.errorf("borge does not generate tcsh completions; tcsh matches options by name " +
			"across all subcommands, which for borge would complete the wrong thing more " +
			"often than the right one")
		return ExitError
	default:
		e.errorf("unknown shell %q; borge generates completions for %s",
			shell, strings.Join(completionShells, ", "))
		return ExitError
	}
	return ExitOK
}

// ---------------------------------------------------------------- describing the CLI

// cmdSpec is one command as a completion script needs to see it.
type cmdSpec struct {
	Name    string
	Summary string
	// Flags are the option names as a user types them: "-r", "--repo".
	Flags []string
	// Sub are this command's subcommands, for "debug" and "benchmark".
	Sub []cmdSpec
	// Archive says the first positional argument is an archive.
	Archive bool
}

// archiveTakingCommands is the set whose first positional is an archive name.
//
// See the note at the top of the file: positional arguments cannot be derived from a
// FlagSet, so this is stated rather than discovered.
var archiveTakingCommands = map[string]bool{
	"list":       true,
	"extract":    true,
	"diff":       true,
	"export-tar": true,
	"rename":     true,
	"tag":        true,
	"delete":     true,
	"undelete":   true,
	"recreate":   true,
}

// archiveTakingSubcommands is the same for the two command groups.
var archiveTakingSubcommands = map[string]bool{
	"debug dump-archive":       true,
	"debug dump-archive-items": true,
}

// subcommandsOf returns a command group's subcommands.
//
// The two groups are named explicitly because they are the only ones, and a command's
// entry in the dispatch table has no field saying so. TestCompletionCoversSubcommands
// notices if a third appears.
func subcommandsOf(name string) []command {
	switch name {
	case "debug":
		return debugCommands()
	case "benchmark":
		return benchmarkCommands()
	}
	return nil
}

// describeCLI enumerates the commands, their subcommands and their options.
func describeCLI(e *Env) []cmdSpec {
	var out []cmdSpec
	for _, c := range commands() {
		spec := cmdSpec{
			Name:    c.name,
			Summary: c.summary,
			Flags:   flagsOf(e, c.run, nil),
			Archive: archiveTakingCommands[c.name],
		}
		for _, sub := range subcommandsOf(c.name) {
			spec.Sub = append(spec.Sub, cmdSpec{
				Name:    sub.name,
				Summary: sub.summary,
				Flags:   flagsOf(e, c.run, []string{sub.name}),
				Archive: archiveTakingSubcommands[c.name+" "+sub.name],
			})
		}
		out = append(out, spec)
	}
	return out
}

// flagsOf runs a command with "-help" and collects the options it registered.
//
// prefix is the subcommand path, so "debug" plus ["get-obj"] reaches the inner FlagSet.
// The command's output goes nowhere: what is wanted is the side effect of it having
// built its flags, not its usage text.
func flagsOf(e *Env, run func(*Env, []string) int, prefix []string) []string {
	// The FlagSets are collected as they are built and walked afterwards: at the moment
	// newFlagSet hands one over it is still empty, because the command registers its
	// options on the next lines.
	var sets []*flag.FlagSet
	probe := &Env{
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		Getenv:       e.Getenv,
		captureFlags: func(fs *flag.FlagSet) { sets = append(sets, fs) },
	}
	run(probe, append(append([]string{}, prefix...), "-help"))

	var names []string
	for _, fs := range sets {
		fs.VisitAll(func(f *flag.Flag) {
			names = append(names, optionName(f.Name))
		})
	}
	sort.Strings(names)
	return dedupe(names)
}

// optionName spells a flag the way a user types it: one dash for a single character, two
// for a word. Go's flag package accepts either for both, but offering "--r" or "-repo" in
// a completion would teach the wrong habit.
func optionName(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------- bash

func writeBashCompletion(w io.Writer, spec []cmdSpec) {
	fmt.Fprint(w, `# borge bash completion. Source it, or install it as
# /usr/share/bash-completion/completions/borge
#
# Completing an archive name runs "borge repo-list", so it works best with BORGE_REPO and
# BORGE_PASSPHRASE set - otherwise the repository cannot be reached and nothing is offered.

_borge_archives() {
    # Names come from the first column, ids from --short. Both are offered because an
    # archive name is not unique and "aid:<hex>" is the way to be exact about which one.
    borge repo-list 2>/dev/null | awk 'NR>1 {print $1}'
    borge repo-list --short 2>/dev/null | sed 's/^/aid:/'
}

_borge() {
    local cur prev words cword
    _init_completion 2>/dev/null || {
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
        words=("${COMP_WORDS[@]}")
        cword=$COMP_CWORD
    }

    local commands='`)
	fmt.Fprint(w, strings.Join(commandNames(spec), " "))
	fmt.Fprint(w, `'

    # Find the command, and the subcommand if the command has any.
    local cmd='' sub='' i
    for (( i=1; i < cword; i++ )); do
        case "${words[i]}" in
            -*) continue ;;
        esac
        if [[ -z $cmd ]]; then cmd="${words[i]}"; else sub="${words[i]}"; break; fi
    done

    if [[ -z $cmd ]]; then
        COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
        return
    fi

    local opts='' subs='' archive=''
    case "$cmd" in
`)
	for _, c := range spec {
		fmt.Fprintf(w, "        %s)\n", c.Name)
		if len(c.Sub) > 0 {
			fmt.Fprintf(w, "            subs='%s'\n", strings.Join(subNames(c.Sub), " "))
			fmt.Fprint(w, "            case \"$sub\" in\n")
			for _, s := range c.Sub {
				fmt.Fprintf(w, "                %s) opts='%s'; archive='%v' ;;\n",
					s.Name, strings.Join(s.Flags, " "), s.Archive)
			}
			fmt.Fprintf(w, "                *) opts='%s' ;;\n", strings.Join(c.Flags, " "))
			fmt.Fprint(w, "            esac\n")
		} else {
			fmt.Fprintf(w, "            opts='%s'; archive='%v'\n",
				strings.Join(c.Flags, " "), c.Archive)
		}
		fmt.Fprint(w, "            ;;\n")
	}
	fmt.Fprint(w, `    esac

    if [[ -n $subs && -z $sub ]]; then
        COMPREPLY=( $(compgen -W "$subs" -- "$cur") )
        return
    fi
    if [[ $cur == -* ]]; then
        COMPREPLY=( $(compgen -W "$opts" -- "$cur") )
        return
    fi
    if [[ $archive == true ]]; then
        COMPREPLY=( $(compgen -W "$(_borge_archives)" -- "$cur") )
        # A path may follow the archive, so files are offered too.
        COMPREPLY+=( $(compgen -f -- "$cur") )
        return
    fi
    COMPREPLY=( $(compgen -f -- "$cur") )
}

complete -F _borge borge
`)
}

// ---------------------------------------------------------------- zsh

func writeZshCompletion(w io.Writer, spec []cmdSpec) {
	fmt.Fprint(w, `#compdef borge
# borge zsh completion. Install it as _borge somewhere on $fpath.
#
# Completing an archive name runs "borge repo-list", so it works best with BORGE_REPO and
# BORGE_PASSPHRASE set.

_borge_archives() {
    local -a names ids
    names=( ${(f)"$(borge repo-list 2>/dev/null | awk 'NR>1 {print $1}')"} )
    ids=( ${(f)"$(borge repo-list --short 2>/dev/null | sed 's/^/aid:/')"} )
    _describe -t archives 'archive' names
    _describe -t archive-ids 'archive id' ids
}

_borge() {
    local -a commands
    commands=(
`)
	for _, c := range spec {
		fmt.Fprintf(w, "        %s:%s\n", zshQuote(c.Name), zshQuote(c.Summary))
	}
	fmt.Fprint(w, `    )

    _arguments -C '1: :->command' '*:: :->argument' && return

    case $state in
        command) _describe -t commands 'borge command' commands ;;
        argument)
            case $words[1] in
`)
	for _, c := range spec {
		fmt.Fprintf(w, "                %s)\n", c.Name)
		if len(c.Sub) > 0 {
			fmt.Fprint(w, "                    local -a subs\n                    subs=(\n")
			for _, s := range c.Sub {
				fmt.Fprintf(w, "                        %s:%s\n", zshQuote(s.Name), zshQuote(s.Summary))
			}
			fmt.Fprint(w, "                    )\n")
			fmt.Fprint(w, "                    _arguments -C '1: :->sub' '*:: :->subargument' && return\n")
			fmt.Fprint(w, "                    case $state in\n")
			fmt.Fprint(w, "                        sub) _describe -t subcommands 'subcommand' subs ;;\n")
			fmt.Fprint(w, "                        subargument)\n")
			fmt.Fprint(w, "                            case $words[1] in\n")
			for _, s := range c.Sub {
				fmt.Fprintf(w, "                                %s) _arguments %s%s ;;\n",
					s.Name, zshOptionList(s.Flags), zshArchiveArg(s.Archive))
			}
			fmt.Fprint(w, "                            esac ;;\n")
			fmt.Fprint(w, "                    esac ;;\n")
			continue
		}
		fmt.Fprintf(w, "                    _arguments %s%s ;;\n",
			zshOptionList(c.Flags), zshArchiveArg(c.Archive))
	}
	fmt.Fprint(w, `            esac ;;
    esac
}

_borge "$@"
`)
}

func zshOptionList(flags []string) string {
	var parts []string
	for _, f := range flags {
		parts = append(parts, "'"+f+"'")
	}
	if len(parts) == 0 {
		return "'*:file:_files'"
	}
	return strings.Join(parts, " ")
}

func zshArchiveArg(archive bool) string {
	if archive {
		return " '1:archive:_borge_archives' '*:file:_files'"
	}
	return " '*:file:_files'"
}

func zshQuote(s string) string {
	// A colon separates the value from its description in a _describe entry, so one inside
	// either half has to be escaped or the entry splits in the wrong place.
	return strings.ReplaceAll(s, ":", `\:`)
}

// ---------------------------------------------------------------- fish

func writeFishCompletion(w io.Writer, spec []cmdSpec) {
	fmt.Fprint(w, `# borge fish completion. Install it as ~/.config/fish/completions/borge.fish
#
# Completing an archive name runs "borge repo-list", so it works best with BORGE_REPO and
# BORGE_PASSPHRASE set.

function __borge_archives
    borge repo-list 2>/dev/null | awk 'NR>1 {print $1}'
    borge repo-list --short 2>/dev/null | sed 's/^/aid:/'
end

function __borge_no_command
    set -l cmd (commandline -opc)
    test (count $cmd) -eq 1
end

function __borge_using
    set -l cmd (commandline -opc)
    if test (count $cmd) -lt 2
        return 1
    end
    contains -- $cmd[2] $argv
end

`)
	for _, c := range spec {
		fmt.Fprintf(w, "complete -c borge -n __borge_no_command -a %s -d %s\n",
			fishQuote(c.Name), fishQuote(c.Summary))
	}
	fmt.Fprintln(w)
	for _, c := range spec {
		for _, s := range c.Sub {
			fmt.Fprintf(w, "complete -c borge -n '__borge_using %s' -a %s -d %s\n",
				c.Name, fishQuote(s.Name), fishQuote(s.Summary))
		}
		for _, f := range c.Flags {
			fmt.Fprintf(w, "complete -c borge -n '__borge_using %s' %s\n", c.Name, fishOption(f))
		}
		if c.Archive {
			fmt.Fprintf(w, "complete -c borge -n '__borge_using %s' -a '(__borge_archives)' -d archive\n", c.Name)
		}
	}
}

// fishOption spells one option as fish's -s (short) or -l (long).
func fishOption(name string) string {
	if strings.HasPrefix(name, "--") {
		return "-l " + strings.TrimPrefix(name, "--")
	}
	return "-s " + strings.TrimPrefix(name, "-")
}

func fishQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// ---------------------------------------------------------------- shared

func commandNames(spec []cmdSpec) []string {
	var out []string
	for _, c := range spec {
		out = append(out, c.Name)
	}
	return out
}

func subNames(sub []cmdSpec) []string {
	var out []string
	for _, s := range sub {
		out = append(out, s.Name)
	}
	return out
}
