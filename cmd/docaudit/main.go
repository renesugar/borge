// SPDX-License-Identifier: Apache-2.0

// Command docaudit reports how borge's user-facing documentation is verified, and fails
// when an anchor promises something that is not there.
//
// It is read-only. It generates nothing and rewrites nothing: it answers "which prose is
// tied to code, by what, and how much is not tied to anything".
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/renesugar/borge/internal/cli"
	"github.com/renesugar/borge/internal/docs"
)

func main() {
	root := flag.String("root", ".", "directory to scan")
	asJSON := flag.Bool("json", false, "print the report as JSON")
	warningsFail := flag.Bool("strict", false, "exit non-zero on warnings as well as errors")
	flag.Parse()

	set, err := docs.Parse(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "docaudit: %v\n", err)
		os.Exit(1)
	}
	// A run that parsed nothing would report a clean audit, which is the most misleading
	// thing it could do.
	if set.Files == 0 {
		fmt.Fprintf(os.Stderr, "docaudit: no Go files under %s; nothing was audited\n", *root)
		os.Exit(1)
	}

	report := docs.Audit(set, cli.HelpTopicNames(), cli.EnumerationNames())

	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "docaudit: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Print(report.Format())
	}

	if len(report.Errors()) > 0 {
		os.Exit(1)
	}
	if *warningsFail && len(report.Findings) > 0 {
		os.Exit(1)
	}
}
