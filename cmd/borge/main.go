// SPDX-License-Identifier: Apache-2.0

// Command borge is a deduplicating backup program with compression and
// authenticated encryption. It is a Go port of BorgBackup; see the README and
// docs/LICENSING.md for provenance and license obligations.
//
// Only --version and --license are implemented so far. The subcommands arrive with
// the stages in docs/PORTING_PLAN.md; until then borge deliberately refuses to do
// anything to a repository rather than doing it wrong.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/renesugar/borge"
	"github.com/renesugar/borge/internal/version"
)

// Exit codes follow borg's convention: 0 success, 1 warning, 2 error.
const (
	exitOK    = 0
	exitError = 2
)

const usage = `borge - deduplicating backup with compression and authenticated encryption

usage: borge [options] <command> [...]

options:
  --version    print the version and what this build interoperates with
  --license    print borge's license, its NOTICE, and the upstream license texts
  -h, --help   print this message

borge is a port of BorgBackup (https://github.com/borgbackup/borg) and is not
produced, sponsored or endorsed by the Borg Collective. Run "borge --license"
for the full attribution.

No subcommands are implemented yet; see docs/PORTING_PLAN.md for the staged plan.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("borge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	showVersion := fs.Bool("version", false, "print version information")
	showLicense := fs.Bool("license", false, "print license information")

	if err := fs.Parse(args); err != nil {
		return exitError
	}

	switch {
	case *showVersion:
		fmt.Fprint(stdout, version.Long())
		return exitOK
	case *showLicense:
		if err := borge.WriteAll(stdout); err != nil {
			fmt.Fprintf(stderr, "borge: %v\n", err)
			return exitError
		}
		return exitOK
	}

	if fs.NArg() == 0 {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "borge: unknown command %q - no subcommands are implemented yet.\n"+
		"See docs/PORTING_PLAN.md for what is being built and in what order.\n", fs.Arg(0))
	return exitError
}
