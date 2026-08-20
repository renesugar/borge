// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// borg has three format key sets and borge had two. check formats with the archive set -
// the one repo-list and prune already use - and diff needs a third, whose records are
// changes rather than paths or archives. See DIVERGENCES.md #47.

// outputLines splits a listing, keeping the order it came in.
//
// It used to sort, because borg's diff did not sort and borge's did - "a difference real
// and separate from what this test is about". That difference is gone (DIVERGENCES #48),
// and with it the reason to look away from the order: sorting here would hide a return of
// exactly the bug that was fixed.
func outputLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestDiffFormatMatchesBorg: the default rendering and each key.
func TestDiffFormatMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("v1")

	// A change of every kind the two tools can both produce.
	write(t, filepath.Join(r.src, "added.txt"), "brand new content here")
	if err := os.Remove(filepath.Join(r.src, "file1.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.src, "file0.txt"), "rewritten, and a different length")
	if err := os.Chmod(filepath.Join(r.src, "file2.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.src, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "v2", r.src)

	formats := []string{
		"", // the default
		"{change} {path}{NL}",
		"[{content}|{mode}|{type}|{link}|{mtime}|{ctime}] {path}{NL}",
		"[{owner}|{user}|{group}|{directory}|{fifo}] {path}{NL}",
		"{isomtime} {isoctime} {path}{NL}",
		"{content:>40} {path}{NL}",
	}
	for _, f := range formats {
		name := f
		if name == "" {
			name = "(default)"
		}
		t.Run(name, func(t *testing.T) {
			borgArgs := []string{"diff", "-r", r.path, "v1", "v2"}
			borgeArgs := []string{"diff", "v1", "v2"}
			if f != "" {
				borgArgs = append(borgArgs, "--format", f)
				borgeArgs = append(borgeArgs, "--format", f)
			}
			borgOut, _ := r.runSplit(borgArgs...)
			borgeOut, stderr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge diff exited %d\n%s", code, stderr)
			}
			want, got := outputLines(borgOut), outputLines(borgeOut)
			if len(want) == 0 {
				t.Fatalf("borg reported no differences; the comparison would pass on anything")
			}
			if strings.Join(want, "\n") != strings.Join(got, "\n") {
				t.Errorf("output differs\nborg :\n%s\nborge:\n%s",
					strings.Join(want, "\n"), strings.Join(got, "\n"))
			}
		})
	}

	// A directory that appeared must be reported under {directory} and not {content}:
	// borg files a presence change by the *kind* of thing, and only a regular file goes
	// to content with a size.
	borgOut, _ := r.runSplit("diff", "-r", r.path, "v1", "v2", "--format", "{directory}|{path}{NL}")
	if !strings.Contains(borgOut, "added directory") {
		t.Fatalf("borg did not report an added directory; the case is not being exercised:\n%s", borgOut)
	}
}

// TestCheckFormatMatchesBorg: check names each archive, and --format says how.
func TestCheckFormatMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("first", "second")

	analyzing := func(out string) []string {
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "Analyzing archive ") {
				lines = append(lines, line)
			}
		}
		sort.Strings(lines)
		return lines
	}

	for _, f := range []string{"", "{archive} [{id:.8}]", "{archive}"} {
		name := f
		if name == "" {
			name = "(default)"
		}
		t.Run(name, func(t *testing.T) {
			borgArgs := []string{"check", "-r", r.path, "-v"}
			borgeArgs := []string{"check", "-v"}
			if f != "" {
				borgArgs = append(borgArgs, "--format", f)
				borgeArgs = append(borgeArgs, "--format", f)
			}
			_, borgErr := r.runSplit(borgArgs...)
			_, borgeErr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge check exited %d\n%s", code, borgeErr)
			}
			want, got := analyzing(borgErr), analyzing(borgeErr)
			if len(want) != 2 {
				t.Fatalf("borg announced %d archives, want 2:\n%s", len(want), borgErr)
			}
			if len(got) != len(want) {
				t.Fatalf("borge announced %d archives, borg %d\nborge:\n%s", len(got), len(want), borgeErr)
			}
			// The ids differ between the two repositories' archives only if the archives
			// differ; here both tools read the same repository, so the lines must match.
			if strings.Join(want, "\n") != strings.Join(got, "\n") {
				t.Errorf("the Analyzing lines differ\nborg :\n%s\nborge:\n%s",
					strings.Join(want, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}

// TestCheckFormatRejectsAnUnknownKey: a bad key is refused before the repository is read,
// not after a long check has already run.
func TestCheckFormatRejectsAnUnknownKey(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.makeArchives("only")

	_, stderr, code := r.borge(t, "check", "--format", "{nosuchkey}")
	if code == ExitOK {
		t.Fatal("check accepted a format naming a key that does not exist")
	}
	if !strings.Contains(stderr, "nosuchkey") {
		t.Errorf("the error does not name the bad key:\n%s", stderr)
	}
}
