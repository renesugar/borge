// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file corresponds to the help topics in borg's src/borg/archiver/help_cmd.py.
// The topics are the same five borg has, and some of the wording follows borg's, but the
// content is written for borge: the environment variables, the chunkers, the compression
// levels and the pattern behaviour all differ in ways that would make a copy wrong.
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
)

// The help topics.
//
// # Why these are not borg's text
//
// It is tempting to copy the topics across: the licence allows it and they are good
// documentation. But every one of them would then be wrong somewhere. borge reads
// BORGE_* variables, refuses three of borg's chunkers, has four zstd levels rather than
// twenty-two (docs/DIVERGENCES.md #16), and does not implement `mount`. A
// help topic that describes a tool's behaviour has to describe *this* tool's behaviour,
// and a nearly-right one is worse than none: it is believed.
//
// # Why they are not written here either
//
// They were, until 2026-08-27, and that is how two of them went false during stage 8: the
// sentence lived here and the behaviour lived in another file, so changing one did not put
// the other in the diff. Each paragraph now lives on the declaration that implements it -
// the prompting paragraph on unlockWithPrompt, the pattern styles on ParsePattern - and
// docgen assembles them. What remains here is the arrangement, and the fragments that
// describe nothing in particular: introductions and examples.

// The topics as printed. They are generated: see helptemplate.go for their shape,
// help_generated.go for the text, and "make docgen" for how it gets there.
var (
	helpPatterns      = helpGeneratedTopics["patterns"]
	helpMatchArchives = helpGeneratedTopics["match-archives"]
	helpPlaceholders  = helpGeneratedTopics["placeholders"]
	helpCompression   = helpGeneratedTopics["compression"]
	helpEnvironment   = helpGeneratedTopics["environment"]
)

// helpText marks a declaration that exists only to carry user-facing documentation.
//
// The doc comment above such a declaration is help text and nothing else: docgen renders
// it into "borge help", so a maintainer's note in it would be printed at a user. Notes
// belong in the code below it.
//
// Most fragments live beside what they describe, in the package that implements it. These
// are the ones that describe nothing in particular - a topic's introduction, its examples
// - and they live here, next to the templates that arrange them.
const helpText = "user-facing help text"

// borge create -r REPO --exclude 'sh:**/.cache' archive ~
// borge extract ARCHIVE 'sh:home/me/**/*.txt'
// borge list ARCHIVE 're:\.(jpg|png)$'
// borge find 'sh:**/invoice-*.pdf'
//
//borge:doc user
//borge:help patterns/examples
//borge:claim patterns/examples
var _ = helpText

// borge repo-list -a 'sh:daily-*' --last 7
// borge delete -a 'tags:temporary'
// borge info -a aid:4a9cd8a3
// borge prune --keep-daily 7 -a 'host:laptop'
//
//borge:doc user
//borge:help match-archives/examples
//borge:claim match-archives/examples
var _ = helpText

// borge create -r REPO '{hostname}-{now:%Y-%m-%d}' ~
// borge create -r REPO 'daily-{utcnow:%Y%m%dT%H%M%S}' /srv
// borge delete -a 'sh:{hostname}-*'
// borge repo-list -r /backups/{hostname}
//
//borge:doc user
//borge:help placeholders/examples
//borge:claim placeholders/examples
var _ = helpText

// borge create -r REPO -C zstd,10 archive ~
// borge create -r REPO -C auto,zstd,3 archive ~
// borge repo-compress -r REPO -C zstd,3
//
//borge:doc user
//borge:help compression/examples
//borge:claim compression/examples
var _ = helpText

// BORGE_REPO=/backups/{hostname} borge repo-list
// BORGE_UNITS=iec borge analyze
//
//borge:doc user
//borge:help environment/examples
//borge:claim environment/examples
var _ = helpText

// helpTopic is one topic of "borge help".
type helpTopic struct {
	name    string
	summary string
	body    string
}

func helpTopics() []helpTopic {
	var out []helpTopic
	for _, t := range helpTemplates() {
		out = append(out, helpTopic{t.name, t.summary, helpGeneratedTopics[t.name]})
	}
	return out
}

func cmdHelp(e *Env, args []string) int {
	fs := newFlagSet(e, "help")
	// borg's two, which select one part of what "help TOPIC" prints. For a help *topic*
	// there is only one part - the text - and borg prints it under either option, so both
	// are no-ops there rather than errors.
	usageOnly := fs.Bool("usage-only", false, "print only the command's usage")
	epilogOnly := fs.Bool("epilog-only", false, "print only the command's description")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	topics := helpTopics()

	if fs.NArg() == 0 {
		printHelpIndex(e.Stdout, topics)
		return ExitOK
	}
	if fs.NArg() > 1 {
		e.errorf("help takes one topic at a time")
		return ExitError
	}
	name := fs.Arg(0)

	for _, t := range topics {
		if t.name == name {
			fmt.Fprint(e.Stdout, t.body)
			return ExitOK
		}
	}
	// A command name is a reasonable thing to type here, so it gets a useful answer
	// rather than "unknown topic".
	for _, c := range commands() {
		if c.name == name {
			switch {
			case *usageOnly:
				// borg prints its argparse usage block. borge's equivalent is the option
				// list Go's flag package builds from the FlagSet the command registered -
				// the same information in a different shape, and the only "usage" borge
				// has.
				//
				// Printed by capturing the command's FlagSet rather than by running it
				// with "-help": that path ends in flag.ErrHelp and exit 2, and asking for
				// help is not an error. completion.go captures the same way.
				return printCommandUsage(e, c)
			case *epilogOnly:
				// borg's epilog is the long description under the option list. borge has
				// one line of description per command - the summary in the dispatch table
				// - so that is what this prints. Recorded in DIVERGENCES.md #53 rather
				// than left to be discovered by a reader expecting borg's prose.
				fmt.Fprintf(e.Stdout, "%s\n", c.summary)
				return ExitOK
			}
			fmt.Fprintf(e.Stdout, "%s: %s\n\nRun \"borge %s -help\" for its options.\n",
				c.name, c.summary, c.name)
			return ExitOK
		}
	}
	e.errorf("no help topic or command called %q", name)
	printHelpIndex(e.Stderr, topics)
	return ExitError
}

func printHelpIndex(w io.Writer, topics []helpTopic) {
	fmt.Fprintf(w, "usage: borge help <topic>\n\ntopics:\n")
	for _, t := range topics {
		fmt.Fprintf(w, "  %-16s %s\n", t.name, t.summary)
	}
	fmt.Fprintf(w, "\nFor a command's options, run \"borge <command> -help\".\n"+
		"For the list of commands, run \"borge\".\n")
}

// HelpTopicNames is the sorted topic list.
//
// Exported for the documentation tooling, which must ask the code which topics exist
// rather than keep a list of its own - a second list is a second thing to disagree with
// the first, which is the bug the whole doc-anchor mechanism exists to remove.
func HelpTopicNames() []string { return helpTopicNames() }

// helpTopicNames is the sorted topic list, for the completions.
func helpTopicNames() []string {
	var out []string
	for _, t := range helpTopics() {
		out = append(out, t.name)
	}
	sort.Strings(out)
	return out
}

// helpEnvVarNames lists the variables the environment topic documents, so a test can
// compare them against the ones the code actually reads.
func helpEnvVarNames() []string {
	var out []string
	for _, v := range envVars() {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out
}

// printCommandUsage writes a command's option list to stdout.
//
// The command is run with "-help" against a throwaway Env whose captureFlags hook keeps the
// FlagSet it builds; nothing else of that run is used. The output goes to the real Env, so
// "borge help create --usage-only > file" writes what the reader asked for and the
// throwaway run's own output is discarded.
func printCommandUsage(e *Env, c command) int {
	var sets []*flag.FlagSet
	probe := &Env{
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		Getenv:       e.Getenv,
		captureFlags: func(fs *flag.FlagSet) { sets = append(sets, fs) },
	}
	c.run(probe, []string{"-help"})
	if len(sets) == 0 {
		// A command that builds no FlagSet has no options to print - the three command
		// groups are like that. Its summary is the whole of what there is to say.
		fmt.Fprintf(e.Stdout, "%s: %s\n", c.name, c.summary)
		return ExitOK
	}
	fmt.Fprintf(e.Stdout, "Usage of borge %s:\n", c.name)
	for _, fs := range sets {
		fs.SetOutput(e.Stdout)
		fs.PrintDefaults()
	}
	return ExitOK
}
