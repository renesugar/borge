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
// So each topic is written here and pinned by a test that checks it against the code -
// TestHelpTopicsCoverTheCode fails when a variable, a pattern style or a codec exists in
// the source and not in the text.

// The rendered topics: the templates above with their generated lists filled in.
//
// Rendering at package initialisation rather than per call means a template naming a list
// that does not exist stops the binary at startup instead of printing a topic with a
// marker in it. TestTopicsRenderTheirLists is the test that never lets it get that far.
var (
	helpPatterns      = mustRenderTopic(helpPatternsTemplate)
	helpMatchArchives = mustRenderTopic(helpMatchArchivesTemplate)
	helpPlaceholders  = mustRenderTopic(helpPlaceholdersTemplate)
	helpCompression   = mustRenderTopic(helpCompressionTemplate)
	helpEnvironment   = mustRenderTopic(helpEnvironmentTemplate)
)

// mustRenderTopic fills in a topic's generated lists, or refuses to start.
//
// A help topic that cannot be rendered is a programming mistake, not a runtime condition:
// there is no input that causes it and nothing a user could do about it.
func mustRenderTopic(body string) string {
	out, err := renderEnumerations(body)
	if err != nil {
		panic(err)
	}
	return out
}

// helpTopic is one topic of "borge help".
type helpTopic struct {
	name    string
	summary string
	body    string
}

func helpTopics() []helpTopic {
	return []helpTopic{
		{"patterns", "selecting paths to include or exclude", helpPatterns},
		{"match-archives", "selecting archives", helpMatchArchives},
		{"placeholders", "substitutions in archive names", helpPlaceholders},
		{"compression", "compression specifications", helpCompression},
		{"environment", "environment variables borge reads", helpEnvironment},
	}
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

// helpPatterns is the text of "borge help patterns".
//
// Anchored so the audit can see it. The claims below name the behaviour this text
// asserts; each has a registered check, and docaudit fails if one loses it.
//
//borge:doc user
//borge:help patterns
//borge:claim patterns/examples
//borge:enumerates pattern-styles
const helpPatternsTemplate = `borge help patterns

Which files a command acts on is decided by patterns. They appear as --exclude and
--pattern options, in a --patterns-from or --exclude-from file, and as the positional
PATH arguments of list, extract, diff, export-tar and find.

STYLES

A pattern may carry a two-letter style prefix. Without one, the style depends on where
the pattern appears: --exclude and --pattern default to "fm", and a positional PATH
defaults to "pp".

{{enum:pattern-styles}}

The prefix is recognised on positional PATH arguments too, not only on --pattern. borge
got this wrong once - a positional "sh:**/*.txt" was read as a literal path beginning
with the characters "sh:" and quietly matched nothing at all - so it is worth stating
plainly: "borge list ARCHIVE sh:**/*.txt" works.

PATHS INSIDE AN ARCHIVE

Paths are stored without a leading slash, so an archive of /home/me holds "home/me/...".
A pattern is matched against that stored form.

OPTIONS AND PATHS

Options may come before or after the paths; both of these exclude the same thing:

  borge create -r REPO --exclude 'sh:**/.cache' archive ~
  borge create -r REPO archive ~ --exclude 'sh:**/.cache'

An argument that begins with a dash and is not one of the command's options is an error,
not a filename. A path that really does begin with a dash needs "--" to end the options:

  borge create -r REPO archive -- ~/-weird-name

ORDER

Patterns are applied in the order given, and the first one that matches decides. In a
--patterns-from file each line is one pattern, prefixed with its action:

  + PATTERN    include
  - PATTERN    exclude
  ! PATTERN    exclude and do not descend into it at all

Blank lines and lines starting with # are ignored.

EXAMPLES

  borge create -r REPO --exclude 'sh:**/.cache' archive ~
  borge extract ARCHIVE 'sh:home/me/**/*.txt'
  borge list ARCHIVE 're:\.(jpg|png)$'
  borge find 'sh:**/invoice-*.pdf'
`

// helpMatchArchives is the text of "borge help match-archives".
//
// Anchored so the audit can see it. The claims below name the behaviour this text
// asserts; each has a registered check, and docaudit fails if one loses it.
//
//borge:doc user
//borge:help match-archives
//borge:claim match-archives/examples
const helpMatchArchivesTemplate = `borge help match-archives

Commands that act on archives take a selector, as -a or --match-archives, or as a
positional archive name. A selector is one of:

  NAME             an exact archive name. Names are not unique: several archives may
                   share one, and the most recent match wins.

  name:NAME        the same thing, written explicitly.

  aid:HEX          an archive id, or a unique prefix of one. This is the only selector
                   that is guaranteed to name exactly one archive, which is why
                   "borge repo-list --short" prints ids.

  sh:PATTERN       a shell-style pattern over the name: * crosses nothing, ** crosses
                   everything, ? is one character, {a,b} alternates.

  re:PATTERN       a regular expression over the name.

  tags:A,B         archives carrying all of these tags.

  user:NAME        archives created by this user.

  host:NAME        archives created on this host.

FILTERS

Selectors combine with the ordering and limiting options:

  --sort-by KEYS   comma-separated: timestamp, name, id, host, user, tags
  --first N        the N oldest of what matched
  --last N         the N newest
  --reverse        reverse the order
  --deleted        act on soft-deleted archives instead of live ones

THE PROTECTED TAG

An archive tagged @PROT is never pruned and never counts against a retention rule's
quota. Tag one with "borge tag --add @PROT ARCHIVE".

It does not appear in prune's listing either: it is set aside before the rules run, so
"--list" shows only the archives the policy actually decided about.

EXAMPLES

  borge repo-list -a 'sh:daily-*' --last 7
  borge delete -a 'tags:temporary'
  borge info -a aid:4a9cd8a3
  borge prune --keep-daily 7 -a 'host:laptop'
`

// helpPlaceholders is the text of "borge help placeholders".
//
// Anchored so the audit can see it. The claims below name the behaviour this text
// asserts; each has a registered check, and docaudit fails if one loses it.
//
//borge:doc user
//borge:help placeholders
//borge:claim placeholders/examples
//borge:claim placeholders/substituted
//borge:enumerates placeholders
const helpPlaceholdersTemplate = `borge help placeholders

An archive name, a comment, an archive selector (-a) and the repository path may contain
placeholders, which borge substitutes before using them.

{{enum:placeholders}}

Every placeholder in one command sees the same instant, so a name built from {now} and
{unixtime} cannot straddle a second boundary.

FORMATS

{now} and {utcnow} take strftime directives: %Y year, %m month, %d day, %H hour,
%M minute, %S second, %f microseconds, %j day of year, %U and %W week number, %V and %G
the ISO week and its year, %a %A %b %B the day and month names, %p AM/PM, %z and %Z the
zone, %s the epoch seconds. %F, %T, %D and %R are the usual composites. A literal percent
is %%.

%c, %x and %X are refused. They format a date the way the machine's locale prefers, and an
archive name is an identifier: it is matched by scripts and sorted by retention rules, so
it must not change with LC_TIME. For the same reason the day and month names are always
English here, where borg follows the locale.

An unknown directive is an error rather than being copied through, so a typo in a crontab
is caught the first time rather than baked into a year of archive names.

LITERAL BRACES

Write {{ and }} for a literal { and }. An unknown placeholder is an error, not a literal:
"{hostnmae}" fails rather than quietly becoming part of the name.

EXAMPLES

  borge create -r REPO '{hostname}-{now:%Y-%m-%d}' ~
  borge create -r REPO 'daily-{utcnow:%Y%m%dT%H%M%S}' /srv
  borge delete -a 'sh:{hostname}-*'
  borge repo-list -r /backups/{hostname}
`

// helpCompression is the text of "borge help compression".
//
// Anchored so the audit can see it. The claims below name the behaviour this text
// asserts; each has a registered check, and docaudit fails if one loses it.
//
//borge:doc user
//borge:help compression
//borge:claim compression/examples
//borge:enumerates compression-specs
const helpCompressionTemplate = `borge help compression

Compression applies to each chunk as it is stored, and is chosen with -C or
--compression. A chunk's id is the hash of its *plaintext*, so compression sits below
deduplication: changing it does not change which chunks exist, only how large they are.
That is why "borge recreate --compression" cannot work and "borge repo-compress" exists.

SPECIFICATIONS

{{enum:compression-specs}}

A chunk that does not get smaller is stored uncompressed whatever the setting, so a
high level costs time and never costs space.

LEVELS ARE COARSER THAN BORG'S

borge compresses with a pure-Go zstd implementation whose encoder has four levels where
libzstd has twenty-two. Levels are mapped onto those four, so "zstd,16" and "zstd,22"
produce identical output, as do "lzma,0", "lzma,6" and "lzma,9".

This costs no interoperability - the level is metadata, borg reads borge's chunks and
the stored level records what was asked for - but a high level does not compress as hard
as borg's would. "borge benchmark cpu --compressing" shows the ratio each level actually
achieves on this machine. See docs/DIVERGENCES.md #16.

EXAMPLES

  borge create -r REPO -C zstd,10 archive ~
  borge create -r REPO -C auto,zstd,3 archive ~
  borge repo-compress -r REPO -C zstd,3
`

// helpEnvironment is the text of "borge help environment".
//
// Anchored so the audit can see it. The claims below name the behaviour this text
// asserts; each has a registered check, and docaudit fails if one loses it.
//
//borge:doc user
//borge:help environment
//borge:claim environment/examples
//borge:claim environment/prefix-fallback
//borge:claim environment/passphrase-prompt
//borge:claim environment/other-passphrase-no-fallback
//borge:enumerates environment-variables
const helpEnvironmentTemplate = `borge help environment

Every variable is read as BORGE_<NAME> first and BORG_<NAME> second, so an existing borg
setup works unchanged and a machine running both can tell them apart.

REPOSITORY AND KEYS

{{enum:environment-variables:repository and keys}}

With none of those set, keyfiles are looked for in the user configuration directory,
under borge/keys and then borg/keys - so a borg installation's keys are found without
being moved.

When the environment does not supply a working passphrase, borge asks at the terminal,
with echo off, up to three times. It asks only after the environment has been tried and
only for a repository that actually has a passphrase, so the unencrypted modes never
prompt - and a wrong BORGE_PASSPHRASE gets a prompt rather than a bare refusal.

With no terminal - a cron job, a pipeline - there is nothing to ask at, so the command
says which variable to set and stops rather than hanging. The prompt is written to stderr,
so redirecting a command's output still captures only its output.

CACHE

{{enum:environment-variables:cache}}

BEHAVIOUR

{{enum:environment-variables:behaviour}}

REMOTE REPOSITORIES

{{enum:environment-variables:remote repositories}}

Several variables here are not borge's own and are read under their own names, because the
tools on the far end and the libraries in between already use them: BORGSTORE_RSH is
honoured before BORGE_RSH and BORG_RSH, so a remote shell configured for borg works
unchanged; BORGSTORE_REST_USERNAME and BORGSTORE_REST_PASSWORD authenticate an http(s)://
repository whose URL carries no credentials; and RCLONE_BINARY names the rclone to run for
an rclone: repository.

An s3: or b2: repository takes its credentials from the URL, or from AWS_ACCESS_KEY_ID,
AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN, or from the profile named by AWS_PROFILE in
AWS_SHARED_CREDENTIALS_FILE (~/.aws/credentials by default) - the order boto3 uses, so a
machine set up for borg needs no second setup. AWS_REGION and AWS_DEFAULT_REGION choose the
region, which is part of the signature rather than only an address: signing for the wrong
one is refused rather than redirected.

TUNING

{{enum:environment-variables:tuning}}

TESTING

{{enum:environment-variables:testing}}

NOT BORGE'S

SSH_ORIGINAL_COMMAND is read by "borge debug info" only, to report what a remote
invocation was asked to run.

EXAMPLES

  BORGE_REPO=/backups/{hostname} borge repo-list
  BORGE_UNITS=iec borge analyze
`

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
