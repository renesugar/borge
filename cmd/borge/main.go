// SPDX-License-Identifier: Apache-2.0

// Command borge is a deduplicating backup program with compression and
// authenticated encryption. It is a Go port of BorgBackup; see the README and
// docs/LICENSING.md for provenance and license obligations.
//
// The read-only commands are implemented (docs/PORTING_PLAN.md stage 5). The commands
// that write to a repository arrive with stage 6; until then borge deliberately refuses
// to do anything to a repository rather than doing it wrong.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/renesugar/borge"
	"github.com/renesugar/borge/internal/cli"
	"github.com/renesugar/borge/internal/version"
)

// Exit codes follow borg's convention: 0 success, 1 warning, 2 error.
const (
	exitOK    = cli.ExitOK
	exitError = cli.ExitError
)

const usage = `borge - deduplicating backup with compression and authenticated encryption

usage: borge [options] <command> [...]

options:
  --version    print the version and what this build interoperates with
  --license    print borge's license, its NOTICE, and the upstream license texts
  -h, --help   print this message

commands:
%s

borge is a port of BorgBackup (https://github.com/borgbackup/borg) and is not
produced, sponsored or endorsed by the Borg Collective. Run "borge --license"
for the full attribution.

The commands that write to a repository are not implemented yet; see
docs/PORTING_PLAN.md for the staged plan.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("borge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText()) }

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
		fmt.Fprint(stdout, usageText())
		return exitOK
	}
	return cli.Run(&cli.Env{Stdout: stdout, Stderr: stderr, Stdin: os.Stdin}, fs.Args())
}

// usageText fills the command list into the usage message, so adding a command in
// internal/cli cannot leave the help behind.
func usageText() string {
	return fmt.Sprintf(usage, strings.Join(cli.Commands(), "\n"))
}
