// SPDX-License-Identifier: Apache-2.0

// Command borge is a deduplicating backup program with compression and
// authenticated encryption. It is a Go port of BorgBackup; see the README and
// docs/LICENSING.md for provenance and license obligations.
//
// Repositories are read and written in borg 2's own format, and the interoperability
// gate (docs/PORTING_PLAN.md stage 7) checks both tools against each other on real
// corpora. The remote backends - sftp, rest, s3, rclone - are not implemented yet, so a
// repository has to be reachable as a local path.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
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

A repository may be a local path, an sftp:, s3:, b2: or rclone: remote, or a
rest:// URL served by "borge serve --rest" (started by the client, over ssh
if the URL names a host). "borge help environment" lists what each needs.
`

func main() {
	stop := startProfiling(os.Stderr)
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// startProfiling turns on Go's profilers when the TESTONLY variables ask for it, and
// returns the function that writes them out.
//
// Stage 9 (docs/PORTING_PLAN.md §12.5 step 1) says to profile before changing anything,
// and profiling the real binary over the real corpus is worth more than profiling a
// benchmark that approximates it. The alternative - a --cpuprofile flag - would be CLI
// surface borg does not have, so this uses the environment instead, under the TESTONLY
// prefix the KDF weakener already established.
//
// os.Exit does not run deferred functions, which is why main calls stop() explicitly
// rather than deferring it: a profile that is never flushed is an empty file, and an empty
// profile looks like a program that did nothing.
func startProfiling(stderr *os.File) func() {
	cpuPath := os.Getenv("BORGE_TESTONLY_CPUPROFILE")
	memPath := os.Getenv("BORGE_TESTONLY_MEMPROFILE")
	if cpuPath == "" && memPath == "" {
		return func() {}
	}
	var cpuFile *os.File
	if cpuPath != "" {
		f, err := os.Create(cpuPath)
		if err != nil {
			fmt.Fprintf(stderr, "borge: cpu profile: %v\n", err)
		} else if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(stderr, "borge: cpu profile: %v\n", err)
			f.Close()
		} else {
			cpuFile = f
		}
	}
	return func() {
		if cpuFile != nil {
			pprof.StopCPUProfile()
			cpuFile.Close()
		}
		if memPath == "" {
			return
		}
		f, err := os.Create(memPath)
		if err != nil {
			fmt.Fprintf(stderr, "borge: memory profile: %v\n", err)
			return
		}
		defer f.Close()
		// The heap profile is a snapshot, so it is taken after the work rather than
		// before, and a GC first makes it a profile of what is live rather than of what
		// has not been collected yet.
		runtime.GC()
		if err := pprof.WriteHeapProfile(f); err != nil {
			fmt.Fprintf(stderr, "borge: memory profile: %v\n", err)
		}
	}
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
