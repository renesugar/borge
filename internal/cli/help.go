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
	"fmt"
	"io"
	"sort"
	"strings"
)

// The help topics.
//
// # Why these are not borg's text
//
// It is tempting to copy the topics across: the licence allows it and they are good
// documentation. But every one of them would then be wrong somewhere. borge reads
// BORGE_* variables, refuses three of borg's chunkers, has four zstd levels rather than
// twenty-two (docs/DIVERGENCES.md #16), and does not implement `mount` or `transfer`. A
// help topic that describes a tool's behaviour has to describe *this* tool's behaviour,
// and a nearly-right one is worse than none: it is believed.
//
// So each topic is written here and pinned by a test that checks it against the code -
// TestHelpTopicsCoverTheCode fails when a variable, a pattern style or a codec exists in
// the source and not in the text.

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

// helpTopicNames is the sorted topic list, for the completions.
func helpTopicNames() []string {
	var out []string
	for _, t := range helpTopics() {
		out = append(out, t.name)
	}
	sort.Strings(out)
	return out
}

const helpPatterns = `borge help patterns

Which files a command acts on is decided by patterns. They appear as --exclude and
--pattern options, in a --patterns-from or --exclude-from file, and as the positional
PATH arguments of list, extract, diff, export-tar and find.

STYLES

A pattern may carry a two-letter style prefix. Without one, the style depends on where
the pattern appears: --exclude and --pattern default to "fm", and a positional PATH
defaults to "pp".

  fm:PATTERN   fnmatch. * matches anything including /, ? matches one character,
               [abc] matches a character class. This is the default for --exclude.

  sh:PATTERN   shell style. * stops at a directory separator, ** crosses them, ? matches
               one character, and {a,b} alternates.

  re:PATTERN   a regular expression, matched against the whole path (Go's regexp syntax,
               which is RE2 - no backreferences and no lookaround).

  pp:PATH      path prefix. Matches PATH and everything under it. This is the default
               for a positional PATH argument.

  pf:PATH      path full. Matches that one path exactly and nothing under it.

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

const helpMatchArchives = `borge help match-archives

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

EXAMPLES

  borge repo-list -a 'sh:daily-*' --last 7
  borge delete -a 'tags:temporary'
  borge info -a aid:4a9cd8a3
  borge prune --keep-daily 7 -a 'host:laptop'
`

const helpPlaceholders = `borge help placeholders

An archive name, a comment, an archive selector (-a) and the repository path may contain
placeholders, which borge substitutes before using them.

  {now}            the current local time, as YYYY-MM-DDTHH:MM:SS
  {utcnow}         the same instant in UTC
  {now:FORMAT}     the current local time in a chosen format
  {utcnow:FORMAT}  the same in UTC
  {unixtime}       seconds since the epoch
  {hostname}       this machine's hostname (BORGE_HOSTNAME overrides it)
  {fqdn}           its fully qualified name
  {reverse-fqdn}   the same with the components reversed
  {user}           the current user (BORGE_USERNAME overrides it)
  {pid}            this process's id
  {uuid4}          a random UUID
  {borgversion}    borge's version, and {borgmajor} {borgminor} {borgpatch} its parts

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

const helpCompression = `borge help compression

Compression applies to each chunk as it is stored, and is chosen with -C or
--compression. A chunk's id is the hash of its *plaintext*, so compression sits below
deduplication: changing it does not change which chunks exist, only how large they are.
That is why "borge recreate --compression" cannot work and "borge repo-compress" exists.

SPECIFICATIONS

  none             store the chunk as it is
  lz4              very fast, modest ratio. borge's default.
  zstd[,LEVEL]     level -128 to 22, default 3
  zlib[,LEVEL]     level 0 to 9, default 6
  lzma[,LEVEL]     level 0 to 9, default 6
  auto,SPEC        try lz4 first, and use SPEC only if it compresses meaningfully better
  obfuscate,N,SPEC compress with SPEC, then pad the result to hide its true size

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

const helpEnvironment = `borge help environment

Every variable is read as BORGE_<NAME> first and BORG_<NAME> second, so an existing borg
setup works unchanged and a machine running both can tell them apart.

REPOSITORY AND KEYS

  BORGE_REPO                 the repository to act on, when -r is not given
  BORGE_PASSPHRASE           the passphrase that unlocks the key
  BORGE_NEW_PASSPHRASE       the passphrase to set, for "key change-passphrase" and
                             "key add"
  BORGE_KEYS_DIR             where keyfiles are kept. Set, it pins the search to that
                             one directory: a user who says where the keys are is not
                             asking for a search.
  BORGE_KEY_FILE             one specific keyfile, overriding the search entirely
  BORGE_CONFIG_DIR           a configuration directory; its keys/ subdirectory joins the
                             keyfile search path
  BORGE_BASE_DIR             a home-like directory; its .config/borge/keys joins the
                             search path

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

  BORGE_CACHE_DIR            where the files cache lives
  BORGE_FILES_CACHE_TTL      how many backups an unused cache entry survives

BEHAVIOUR

  BORGE_REPO_LIST_FORMAT     the default --format for "repo-list", when it is not given
  BORGE_UNITS                si (default), iec or raw, for how sizes are printed
  BORGE_HOST_ID              this machine's lock identity, when the derived one is wrong
  BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING
                             set to YES to answer repo-delete's confirmation
  BORGE_ASSERT_ID            where chunk ids are verified: a comma-separated subset of
                             read, repair, transfer, rechunk. The default is
                             repair,transfer,rechunk - verifying on the read path costs
                             a full extra hash pass over everything restored.

TUNING

  BORGE_PACK_MAX_COUNT       how many objects one pack file holds
  BORGE_PACK_MAX_SIZE        how large one pack file may become
  BORGE_PACK_ASYNC           set to "no" to write packs on the calling goroutine

TESTING

  BORGE_TESTONLY_WEAKEN_KDF  weakens the passphrase KDF so tests are fast. Never set
                             this for a real repository: it makes the passphrase
                             cheap to attack.

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
	for _, line := range strings.Split(helpEnvironment, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if name, ok := strings.CutPrefix(fields[0], "BORGE_"); ok {
			// An examples line starts with an assignment: BORGE_UNITS=iec borge ...
			name, _, _ = strings.Cut(name, "=")
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
