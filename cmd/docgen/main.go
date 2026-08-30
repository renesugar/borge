// SPDX-License-Identifier: Apache-2.0

// Command docgen writes the help topics from the anchored documentation in the source.
//
// It is the generator half of the doc-anchor mechanism: docaudit reports what is anchored,
// docgen renders it. Run it with "make docgen"; TestDocsAreCurrent fails when the checked-in
// file no longer matches what this would produce.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/renesugar/borge/internal/cli"
)

func main() {
	root := flag.String("root", ".", "directory to read anchors from")
	out := flag.String("out", "internal/cli/help_generated.go", "file to write")
	check := flag.Bool("check", false, "report whether the file is current, write nothing")
	flag.Parse()

	source, orphans, err := cli.GenerateHelpFile(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
	// A fragment nothing renders is prose its author believes is published. That is worse
	// than a missing one, which fails loudly above, so it is reported every run.
	for _, orphan := range orphans {
		fmt.Fprintf(os.Stderr, "docgen: //borge:help %s is anchored but no topic template asks for it\n", orphan)
	}

	if *check {
		existing, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
			os.Exit(1)
		}
		if string(existing) != source {
			fmt.Fprintf(os.Stderr, "docgen: %s is out of date; run \"make docgen\"\n", *out)
			os.Exit(1)
		}
		fmt.Printf("docgen: %s is current\n", *out)
		return
	}

	if err := os.WriteFile(*out, []byte(source), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("docgen: wrote %s\n", *out)
	if len(orphans) > 0 {
		os.Exit(1)
	}
}
