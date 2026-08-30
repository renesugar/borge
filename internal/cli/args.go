// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"flag"
	"strings"
)

// Argument permutation.
//
// borg parses with Python's argparse, which accepts options anywhere on the command line.
// Go's flag stops reading options at the first non-option argument, so
//
//	borge create -r REPO archive ~ --exclude 'sh:**/.cache'
//
// archived the directory it had been told to exclude: "--exclude" and the pattern after it
// became two more paths to back up. borge warned that they did not exist, exited 1, and
// stored the .cache tree anyway. That was docs/DIVERGENCES.md #20, and it is why this file
// exists - not a different answer to a question, but the same command meaning something
// else, silently, in the direction of keeping data the user tried to leave out.
//
// So options are moved ahead of the positionals before flag.Parse sees them, the way GNU
// getopt does. Three rules make that safe:
//
//   - An option that takes a value carries the next argument with it. "-r REPO" has to
//     move as a pair, and "--keep-daily -1" must not lose its argument to the positionals.
//     Whether an option takes a value is asked of the FlagSet, not guessed.
//   - "--" ends the options: everything after it is positional however it looks. A "--" is
//     re-emitted ahead of the positionals, so a path that begins with a dash still reaches
//     the command as a path.
//   - A command whose trailing arguments belong to something else opts out entirely. That
//     is "with-lock", which runs another program: permuting its arguments would steal that
//     program's options.
//
// An argument that looks like an option and is not one is left in the option run rather
// than quietly demoted to a positional, so flag.Parse still rejects it by name. Before
// this, a mistyped option became a filename and the only sign was a warning.

// flagSet is a flag.FlagSet that permutes.
type flagSet struct {
	*flag.FlagSet
	// passthrough leaves the arguments as typed: what follows the first positional
	// belongs to a program borge is about to run, not to borge.
	passthrough bool

	// env and name are what --log-json needs once parsing succeeds; see Parse.
	env  *Env
	name string
	// logJSON is bound to the --log-json option newFlagSet registers.
	logJSON bool
}

// wasSet reports whether an option appeared on the command line, as against holding its
// default value.
//
// Needed wherever borg distinguishes "not given" from "given an empty or extreme value",
// which argparse does for free with a default of None. An empty --sort-by is an error in
// borg ("unsupported sort field: empty spec") while omitting it means do not sort, and
// "--depth -1" lists nothing while omitting it lists everything. Comparing against the zero value
// cannot tell those apart, and quietly treating one as the other is the kind of difference
// that only shows up in somebody's script.
func (fs *flagSet) wasSet(name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// Parse permutes the arguments and parses them.
func (fs *flagSet) Parse(args []string) error {
	if !fs.passthrough {
		args = permute(fs.FlagSet, args)
	}
	err := fs.FlagSet.Parse(args)
	// Only after a successful parse, which is borg's rule and not an accident of where
	// this sits: "JSON logging requires successful argument parsing. Even with --log-json
	// specified, a parsing error will be printed in plain text, because logging set-up
	// happens after all arguments are parsed" (frontends.rst). A frontend therefore has
	// to be ready for a plain-text usage error, from either tool.
	if err == nil && fs.logJSON && fs.env != nil {
		fs.env.enableJSONLog(fs.name)
	}
	return err
}

// Options may come before or after the paths; both of these exclude the same thing:
//
//	borge create -r REPO --exclude 'sh:**/.cache' archive ~
//	borge create -r REPO archive ~ --exclude 'sh:**/.cache'
//
// An argument that begins with a dash and is not one of the command's options is an error,
// not a filename. A path that really does begin with a dash needs "--" to end the options:
//
//	borge create -r REPO archive -- ~/-weird-name
//
//borge:doc user
//borge:help patterns/option-order
//borge:claim patterns/option-order
//borge:about permute
var _ = helpText

// permute returns args with the options ahead of the positionals.
func permute(fs *flag.FlagSet, args []string) []string {
	var opts, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		// A lone "-" is a filename by convention, and Go's flag treats it as one too.
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		opts = append(opts, arg)
		name, ok := parseOptionName(arg)
		if !ok {
			continue // malformed; flag.Parse will say so
		}
		if _, _, written := strings.Cut(name, "="); written {
			continue // "-r=REPO" carries its own value
		}
		if i+1 < len(args) && optionTakesValue(fs, name) {
			i++
			opts = append(opts, args[i])
		}
	}
	if len(positional) == 0 {
		return opts
	}
	return append(append(opts, "--"), positional...)
}

// parseOptionName strips the leading dashes, the way flag.Parse does: one or two, and
// never a third. It reports false for something flag will reject, so that nothing after it
// is swallowed as its value. (completion.go's optionName is the opposite direction: how a
// flag should be spelled back to the user.)
func parseOptionName(arg string) (string, bool) {
	name := arg[1:]
	if name[0] == '-' {
		name = name[1:]
	}
	if name == "" || name[0] == '-' || name[0] == '=' {
		return "", false
	}
	return name, true
}

// optionTakesValue reports whether this option consumes the argument after it.
//
// Asked of the FlagSet rather than guessed from the spelling, because the answer differs
// per command: "-e" is create's --exclude and repo-create's --encryption, and both take a
// value, while "-v" takes none. An option the command does not define takes nothing: what
// follows it stays a positional, and flag.Parse rejects the option itself by name.
func optionTakesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}
