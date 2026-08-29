// SPDX-License-Identifier: Apache-2.0

// Package harness holds helpers shared by the test suites under tests/.
//
// It is ordinary (non-_test) code so that both tests/interop and tests/bench can import
// it; Go will not let one test package import another's test files.
package harness

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// skipDirs are directories with no source that decides what the binary does. evidence and
// .venv-borg2 hold other people's artifacts; bin holds the binary being checked.
var skipDirs = map[string]bool{
	".git": true, "bin": true, "vendor": true, "testdata": true,
	".venv-borg2": true, "evidence": true, "plans": true,
}

// NewestSource returns the most recently modified Go source in the tree, and its path.
//
// A zero time means the tree could not be walked, which callers must treat as "cannot
// tell" rather than as "nothing is newer".
func NewestSource(root string) (time.Time, string) {
	var newest time.Time
	var newestPath string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not this check's business
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && d.Name() != "go.mod" && d.Name() != "go.sum" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest, newestPath = info.ModTime(), path
		}
		return nil
	})
	if err != nil {
		return time.Time{}, ""
	}
	return newest, newestPath
}

// StaleBinary reports why a built binary is out of date, or "" when it is current.
//
// The suites under tests/ run bin/borge rather than compiling it, so nothing ties their
// results to the tree. On 2026-08-28 a full suite reported "ok tests/interop (cached)"
// against a binary seven days old: Go's cache was right, its input was stale, and the gate
// that protects borg-2 format compatibility passed without testing the change in front of
// it. Missing was already handled; out of date is the case that reports success.
//
// The same hazard is worse in tests/bench, where a stale binary yields not a false pass but
// a false number - and numbers get quoted.
func StaleBinary(root string, built time.Time) string {
	newest, path := NewestSource(root)
	if newest.IsZero() || !newest.After(built) {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return fmt.Sprintf("bin/borge was built %s but %s changed %s: these suites run the "+
		"binary rather than compiling it, so this would be measuring code that is not in "+
		"the tree. Run 'make build' (or 'make test', which depends on it).",
		built.Format(time.RFC3339), rel, newest.Format(time.RFC3339))
}
