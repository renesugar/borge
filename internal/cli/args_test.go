// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The permutation this file tests exists because of docs/DIVERGENCES.md #20: Go's flag
// package stops reading options at the first positional, so an --exclude written after the
// paths silently archived what it was meant to leave out. See args.go.

// testFlagSet has one option of each kind that matters to the permutation: a string that
// takes a value, a bool that does not, an int that takes a negative one, and a repeatable
// Value.
func testFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	fs.String("r", "", "repository")
	fs.Bool("v", false, "verbose")
	fs.Int("keep-daily", 0, "keep")
	fs.Var(&stringList{}, "exclude", "exclude")
	return fs
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// stringList is the repeatable-option type from serve.go; this test uses it because a
// test type with the same shape would be a second implementation to keep in step.

func TestPermute(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{{
		name: "the divergence 20 case: an option after the positionals",
		in:   []string{"-r", "REPO", "archive", "/home/me", "--exclude", "sh:**/.cache"},
		want: []string{"-r", "REPO", "--exclude", "sh:**/.cache", "--", "archive", "/home/me"},
	}, {
		name: "already in order, and still terminated so a dash-leading path is safe",
		in:   []string{"-r", "REPO", "--exclude", "x", "archive", "/home/me"},
		want: []string{"-r", "REPO", "--exclude", "x", "--", "archive", "/home/me"},
	}, {
		name: "no positionals: nothing to terminate",
		in:   []string{"-r", "REPO", "-v"},
		want: []string{"-r", "REPO", "-v"},
	}, {
		name: "a bool does not swallow the argument after it",
		in:   []string{"archive", "-v", "/home/me"},
		want: []string{"-v", "--", "archive", "/home/me"},
	}, {
		name: "an option carrying its own value",
		in:   []string{"archive", "-r=REPO"},
		want: []string{"-r=REPO", "--", "archive"},
	}, {
		name: "a negative value stays with its option",
		in:   []string{"archive", "--keep-daily", "-1"},
		want: []string{"--keep-daily", "-1", "--", "archive"},
	}, {
		name: "everything after -- is positional, however it looks",
		in:   []string{"-v", "--", "archive", "--exclude", "-weird"},
		want: []string{"-v", "--", "archive", "--exclude", "-weird"},
	}, {
		name: "a lone dash is a filename",
		in:   []string{"-", "-v"},
		want: []string{"-v", "--", "-"},
	}, {
		name: "an undefined option keeps its place in the options so flag rejects it",
		in:   []string{"archive", "--nope", "/home/me"},
		want: []string{"--nope", "--", "archive", "/home/me"},
	}, {
		name: "an undefined option does not swallow what follows it",
		in:   []string{"--nope", "archive"},
		want: []string{"--nope", "--", "archive"},
	}, {
		name: "a repeatable option keeps every occurrence and its order",
		in:   []string{"archive", "--exclude", "a", "--exclude", "b"},
		want: []string{"--exclude", "a", "--exclude", "b", "--", "archive"},
	}, {
		name: "an option at the very end with nothing to take",
		in:   []string{"archive", "-r"},
		want: []string{"-r", "--", "archive"},
	}, {
		name: "nothing at all",
		in:   nil,
		want: nil,
	}}

	// A permutation that never moved anything would pass every "already in order" case
	// above and prove nothing.
	moved := 0
	for _, c := range cases {
		got := permute(testFlagSet(), c.in)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("%s\n  in   %q\n  got  %q\n  want %q", c.name, c.in, got, c.want)
		}
		if strings.Join(got, "\x00") != strings.Join(c.in, "\x00") {
			moved++
		}
	}
	if moved < 5 {
		t.Errorf("only %d of %d cases changed their arguments; the table is not "+
			"exercising the permutation", moved, len(cases))
	}
}

// TestPermutedArgumentsParse: the permuted form is one flag.Parse accepts, and the values
// arrive where they belong. Checking the permutation's output as a list of strings does not
// prove that.
func TestPermutedArgumentsParse(t *testing.T) {
	fs := testFlagSet()
	repo := fs.Lookup("r")
	excludes := fs.Lookup("exclude").Value.(*stringList)

	args := []string{"archive", "/home/me", "--exclude", "sh:**/.cache", "-r", "REPO", "-v"}
	if err := fs.Parse(permute(fs, args)); err != nil {
		t.Fatalf("the permuted arguments do not parse: %v", err)
	}
	if repo.Value.String() != "REPO" {
		t.Errorf("-r is %q", repo.Value.String())
	}
	if got := strings.Join(*excludes, ","); got != "sh:**/.cache" {
		t.Errorf("--exclude is %q", got)
	}
	if fs.Lookup("v").Value.String() != "true" {
		t.Errorf("-v did not take")
	}
	if got := strings.Join(fs.Args(), ","); got != "archive,/home/me" {
		t.Errorf("the positionals are %q", got)
	}
}

// TestExcludeAfterPositionalsMatchesBorg is the divergence itself, closed.
//
// The assertion is about the archive rather than about borge's own reporting of it: borg
// and borge are given the same command, with the option in the position borg has always
// accepted, and have to produce the same contents.
func TestExcludeAfterPositionalsMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"notes.txt":    "notes",
		".cache/junk":  "junk",
		"sub/deep.txt": "deep",
	} {
		p := filepath.Join(src, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The option comes last, which is what used to make it a path.
	r.mustRun("create", "-r", r.path, "byborg", src, "--exclude", "sh:**/.cache")
	if _, stderr, code := r.borge(t, "create", "byborge", src, "--exclude", "sh:**/.cache"); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}

	// Sorted on both sides: this test is about *which* paths were archived, and the two
	// tools store them in different orders by design - borge sorts each directory,
	// borg keeps readdir order (docs/DIVERGENCES.md #23). Comparing sequences here would
	// fail for a reason that has nothing to do with argument parsing, which is exactly
	// what it did first time round.
	borgPaths := sortedItemPaths(t, r.mustRun("list", "-r", r.path, "byborg", "--json-lines"))
	stdout, stderr, code := r.borge(t, "list", "--json-lines", "byborge")
	if code != ExitOK {
		t.Fatalf("borge list exited %d\n%s", code, stderr)
	}
	borgePaths := sortedItemPaths(t, stdout)

	// Both tools excluding everything would compare equal and mean nothing.
	if len(borgPaths) < 3 {
		t.Fatalf("borg archived %d paths; the fixture is not what this test needs: %v",
			len(borgPaths), borgPaths)
	}
	if strings.Join(borgPaths, "\n") != strings.Join(borgePaths, "\n") {
		t.Errorf("borg archived\n  %s\nborge archived\n  %s",
			strings.Join(borgPaths, "\n  "), strings.Join(borgePaths, "\n  "))
	}
	for _, p := range borgePaths {
		if strings.Contains(p, "/.cache") {
			t.Errorf("--exclude after the positionals did not exclude %q", p)
		}
	}
}

// TestWithLockDoesNotPermute: with-lock runs another program, and that program's options
// are not borge's. Permuting them would pull "-c" out of "sh -c ..." and borge would
// reject its own command line.
func TestWithLockDoesNotPermute(t *testing.T) {
	e := &Env{Stdout: nopWriter{}, Stderr: nopWriter{}}
	if newFlagSet(e, "probe").passthrough {
		t.Fatal("newFlagSet is passing arguments through; nothing would be permuted")
	}
	if !newPassthroughFlagSet(e, "probe").passthrough {
		t.Fatal("newPassthroughFlagSet does not pass arguments through")
	}
	// And the command that needs it still asks for it. Checked here rather than left to
	// TestWithLockRunsTheCommandAndPassesItsExitCode, which would fail for a reason that
	// does not name the cause.
	if !strings.Contains(readSource(t, "lock.go"), `newPassthroughFlagSet(e, "with-lock")`) {
		t.Error("with-lock no longer opts out of permutation; \"borge with-lock sh -c ...\" " +
			"will fail because -c is not one of borge's options")
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
