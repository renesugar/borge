// SPDX-License-Identifier: Apache-2.0

// Package version reports what this borge build is and what it is compatible with.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is the borge release version, set at build time with
// -ldflags "-X github.com/renesugar/borge/internal/version.Version=...".
// "dev" means an unreleased build.
var Version = "dev"

// The upstream borg commit this port targets. See plans/PORTING_PLAN.md section 0.1:
// the commit is pinned so the interoperability gate has a fixed meaning. Changing it
// is a deliberate, reviewed rebase, never an incidental edit.
const (
	BorgUpstreamCommit = "114bd1e944c4ade6e512be20b36bcdd6398ad78e"
	BorgUpstreamDate   = "2026-08-16"
	BorgSeries         = "2.x"
)

// RepositoryVersion is the borg repository format version borge reads and writes.
// borg 2's Repository.acceptable_repo_versions is (4,).
const RepositoryVersion = 4

// Revision returns the VCS revision this binary was built from, or "" if the build
// carried no VCS stamp (as with "go run", or a build from a dirty tree).
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev != "" && modified == "true" {
		return rev + "+dirty"
	}
	return rev
}

// String is the one-line version banner.
func String() string {
	s := "borge " + Version
	if rev := Revision(); rev != "" {
		s += " (" + shorten(rev) + ")"
	}
	return s
}

// Long is the multi-line version report, including what this build interoperates with.
func Long() string {
	return fmt.Sprintf(
		"%s\n"+
			"  go:                %s %s/%s\n"+
			"  borg upstream:     %s @ %s (%s)\n"+
			"  repository format: version %d\n",
		String(), runtime.Version(), runtime.GOOS, runtime.GOARCH,
		BorgSeries, shorten(BorgUpstreamCommit), BorgUpstreamDate,
		RepositoryVersion,
	)
}

func shorten(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
