// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Running the examples in the help text.
//
// For an API, a code example carries the information. For a command-line tool the
// equivalent is a command line, and it has the property prose does not: it can be
// executed. Nothing executed these until 2026-08-17, when running the fifteen of them by
// hand found three wrong - and one of the three was not a documentation bug at all but a
// defect that silently stored data the user had asked to exclude (docs/DIVERGENCES.md
// #20). See docs/PORTING_PLAN.md §2.1.2.
//
// So: every "borge ..." in every help topic is extracted, substituted against a scratch
// repository built for the purpose, run, and checked - not only for its exit status but
// for what it did. Exit status alone is not enough. "borge list ARCHIVE 're:...'" exits 0
// whether it matches the right files, the wrong files or nothing at all, and a test that
// accepts all three is a test that cannot fail.
//
// Commands that appear in prose as names rather than invocations ("that is why
// 'borge repo-compress' exists") are in the table too, marked notRunnable with the
// reason. They are listed rather than skipped so that the table is a complete inventory
// of every command-shaped thing in the help text: a new example added to a topic fails
// this test until somebody says what it should do.

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

// helpExample is one command found in a help topic.
type helpExample struct {
	topic string
	line  int
	cmd   string // exactly as documented, before substitution
	note  string // the trailing annotation, if any: "correct", "WRONG"
	prose bool   // quoted inside a sentence rather than set out as an example
}

// quotedCommand finds `"borge ..."` inside a sentence.
var quotedCommand = regexp.MustCompile(`"(borge [^"]*)"`)

// runsOfSpaces splits an example line from its trailing annotation.
var runsOfSpaces = regexp.MustCompile(`   +`)

// helpExamplesFromTopics reads every topic and returns every command in it.
func helpExamplesFromTopics(t *testing.T) []helpExample {
	t.Helper()
	var out []helpExample
	for _, topic := range helpTopics() {
		for i, line := range strings.Split(topic.body, "\n") {
			lineNo := i + 1
			// An example is indented and starts with an optional run of
			// NAME=VALUE assignments followed by "borge".
			if strings.HasPrefix(line, "  ") {
				body := strings.TrimSpace(line)
				parts := runsOfSpaces.Split(body, 2)
				note := ""
				if len(parts) == 2 {
					body, note = parts[0], parts[1]
				}
				if isCommandLine(body) {
					out = append(out, helpExample{topic.name, lineNo, body, note, false})
					continue
				}
			}
			for _, m := range quotedCommand.FindAllStringSubmatch(line, -1) {
				out = append(out, helpExample{topic.name, lineNo, m[1], "", true})
			}
		}
	}
	return out
}

// isCommandLine reports whether s is an invocation: environment assignments, then
// "borge".
func isCommandLine(s string) bool {
	for _, field := range strings.Fields(s) {
		if field == "borge" {
			return true
		}
		if !envAssignment.MatchString(field) {
			return false
		}
	}
	return false
}

var envAssignment = regexp.MustCompile(`^[A-Z][A-Z0-9_]*=`)

// splitDocCommand turns a documented command into its tokens. Only single quotes are
// understood, because that is all the topics use; anything else is an error rather than
// a silent misreading, so an example written with shell syntax this cannot run fails
// loudly.
func splitDocCommand(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote, started := false, false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			started = true
		case r == '"' && !inQuote:
			return nil, fmt.Errorf("double quotes are not understood: %s", s)
		case (r == ' ' || r == '\t') && !inQuote:
			if started {
				tokens = append(tokens, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unbalanced quote: %s", s)
	}
	if started {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

// ---------------------------------------------------------------------------
// The scratch repository
// ---------------------------------------------------------------------------

// helpFixture is the world the examples run in. It is built so that every example has
// something real to act on: an archive named ARCHIVE because the topics say ARCHIVE, an
// archive matching "sh:daily-*", one tagged "temporary", one named after this host. An
// example that matched nothing would exit 0 and prove nothing.
type helpFixture struct {
	t         *testing.T
	root      string // the scratch root
	repo      string // root/REPO, the repository the topics call REPO
	backups   string // root/backups/<hostname>, for the /backups/{hostname} examples
	work      string // the working directory examples run in
	hostname  string
	archiveID string // ARCHIVE's id, for the "aid:" example
	// stored is root without its leading slash: the prefix every archived path carries,
	// because an archive stores an absolute path with the slash removed.
	stored string
}

func newHelpFixture(t *testing.T) *helpFixture {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &helpFixture{
		t:        t,
		root:     root,
		repo:     filepath.Join(root, "REPO"),
		hostname: host,
		work:     filepath.Join(root, "work"),
		stored:   strings.TrimPrefix(filepath.ToSlash(root), "/"),
	}
	f.backups = filepath.Join(root, "backups", host)

	// The source tree. The names are the ones the topics name: a .cache to exclude, .txt
	// files to extract, a .jpg and a .png to match with a regular expression, an
	// invoice-*.pdf to find, and a .tiff that none of those patterns may match.
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("home/me/notes.txt", "notes\n")
	write("home/me/docs/report.txt", "report\n")
	write("home/me/pics/photo.jpg", "jpeg data\n")
	write("home/me/pics/icon.png", "png data\n")
	write("home/me/pics/raw.tiff", "tiff data\n")
	write("home/me/invoice-2026-01.pdf", "invoice\n")
	write("home/me/.cache/junk.bin", "junk\n")
	// A path that begins with a dash, for the "--" example in the patterns topic.
	write("home/me/-weird-name", "weird\n")
	write("srv/data.txt", "server data\n")
	// Large enough that "borge analyze" prints a size with a unit prefix, which is what
	// the BORGE_UNITS example checks. Incompressible so the stored size stays large.
	big := make([]byte, 400*1024)
	for i := range big {
		big[i] = byte(i*2654435761 + i/7)
	}
	if err := os.WriteFile(filepath.Join(root, "home", "me", "blob.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		t.Fatal(err)
	}

	// none-sha256 keeps the fixture fast and needs no passphrase; none of the examples
	// is about encryption.
	f.must("repo-create", "-r", f.repo, "-e", "none-sha256")
	f.must("repo-create", "-r", f.backups, "-e", "none-sha256")
	f.must("create", "-r", f.repo, "ARCHIVE", filepath.Join(root, "home", "me"))
	f.must("create", "-r", f.repo, "daily-2026-01-01", filepath.Join(root, "home", "me"))
	f.must("create", "-r", f.repo, "tagged", filepath.Join(root, "srv"))
	f.must("tag", "-r", f.repo, "-add", "temporary", "tagged")
	f.must("create", "-r", f.repo, host+"-example", filepath.Join(root, "srv"))
	f.must("create", "-r", f.backups, "other", filepath.Join(root, "srv"))

	for _, id := range strings.Fields(f.must("repo-list", "-r", f.repo, "--short", "-a", "ARCHIVE")) {
		f.archiveID = id
	}
	if len(f.archiveID) != 64 {
		t.Fatalf("expected a 64-character archive id for ARCHIVE, got %q", f.archiveID)
	}
	return f
}

// run executes borge in this fixture, with extra environment on top of its own.
func (f *helpFixture) run(extra map[string]string, args ...string) (string, string, int) {
	f.t.Helper()
	vars := map[string]string{
		"BORGE_REPO":      f.repo,
		"BORGE_CACHE_DIR": filepath.Join(f.root, "cache"),
		"BORGE_KEYS_DIR":  filepath.Join(f.root, "keys"),
	}
	for k, v := range extra {
		vars[k] = v
	}
	var stdout, stderr bytes.Buffer
	env := &Env{
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		},
	}
	// Run first: the buffers have to be read after it, not in the same return
	// expression.
	code := Run(env, args)
	return stdout.String(), stderr.String(), code
}

func (f *helpFixture) must(args ...string) string {
	f.t.Helper()
	stdout, stderr, code := f.run(nil, args...)
	if code != ExitOK {
		f.t.Fatalf("building the fixture: borge %s exited %d\n%s%s",
			strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// archivePaths is what an archive holds, as stored paths.
func (f *helpFixture) archivePaths(name string) []string {
	f.t.Helper()
	var out []string
	for _, line := range strings.Split(f.must("list", "-r", f.repo, "--short", name), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// archiveNames is what the repository holds. It reads the JSON rather than the columns,
// because the columns are padded to widths that depend on the data.
func (f *helpFixture) archiveNames() []string {
	f.t.Helper()
	var doc struct {
		Archives []struct {
			Name string `json:"name"`
		} `json:"archives"`
	}
	if err := json.Unmarshal([]byte(f.must("repo-list", "-r", f.repo, "--json")), &doc); err != nil {
		f.t.Fatalf("repo-list --json does not parse: %v", err)
	}
	var out []string
	for _, a := range doc.Archives {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Substitution
// ---------------------------------------------------------------------------

// substitution turns a documented placeholder into something that exists here. Each one
// carries its reason, because every substitution is a small step away from running what
// the user reads, and a step nobody can justify is a step that hides a bug.
// A whole substitution replaces a token only when the token is exactly doc; the rest
// replace anywhere they appear. The distinction is not cosmetic: replacing "REPO"
// wherever it occurred turned BORGE_REPO=... into nonsense.
type substitution struct {
	doc, real, why string
	whole          bool
}

func (f *helpFixture) substitutions() []substitution {
	return []substitution{
		{doc: "REPO", real: f.repo, whole: true,
			why: "the topics write REPO for a repository path, and borge requires an absolute one"},
		{doc: "~", real: filepath.Join(f.root, "home", "me"),
			why: "the test must not archive the real home directory"},
		{doc: "/srv", real: filepath.Join(f.root, "srv"), whole: true,
			why: "the test must not archive the real /srv"},
		{doc: "aid:4a9cd8a3", real: "aid:" + f.archiveID[:8], whole: true,
			why: "the documented archive id is fictional"},
		{doc: "host:laptop", real: "host:" + f.hostname, whole: true,
			why: "the fixture's archives were made on this machine"},
		{doc: "/backups", real: filepath.Join(f.root, "backups"),
			why: "the test must not write to the real /backups"},
		{doc: "sh:home/me/", real: "sh:" + f.stored + "/home/me/",
			why: "an archive stores an absolute path without its leading slash, so a " +
				"pattern written against /home/me carries the scratch root here"},
	}
}

func (f *helpFixture) substitute(token string) string {
	// In NAME=VALUE only the value is a path or a selector.
	if envAssignment.MatchString(token) {
		name, value, _ := strings.Cut(token, "=")
		return name + "=" + f.substitute(value)
	}
	for _, s := range f.substitutions() {
		if s.whole {
			if token == s.doc {
				return s.real
			}
			continue
		}
		token = strings.ReplaceAll(token, s.doc, s.real)
	}
	return token
}

// ---------------------------------------------------------------------------
// What each example is supposed to do
// ---------------------------------------------------------------------------

type exampleCheck struct {
	// notRunnable holds the reason when the command appears in prose as a name rather
	// than as something to type.
	notRunnable string
	// env is added to the environment for this example only.
	env      map[string]string
	wantExit int
	// check inspects what the command did. Exit status is not enough: a pattern that
	// matches nothing exits 0.
	check func(t *testing.T, f *helpFixture, stdout, stderr string)
}

// The inventory, one group per topic. Every command in every topic has an entry, and
// every entry has to match a command - so a topic edited without the test being updated
// fails, in both directions.

var patternsExamples = map[string]exampleCheck{
	`borge create -r REPO --exclude 'sh:**/.cache' archive ~`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			paths := f.archivePaths("archive")
			if len(paths) < 6 {
				t.Fatalf("the archive holds %d paths; the fixture cannot have been "+
					"archived and this check proves nothing: %v", len(paths), paths)
			}
			for _, p := range paths {
				if strings.Contains(p, "/.cache") {
					t.Errorf("--exclude 'sh:**/.cache' did not exclude %q", p)
				}
			}
			mustHaveSuffix(t, paths, "/home/me/notes.txt")
		},
	},

	// The same command with the option after the paths. It used to archive the .cache
	// tree it was told to leave out (docs/DIVERGENCES.md #20, fixed); it is kept as a
	// separate entry from the one above because the topic now promises the two forms
	// mean the same thing, and only running both proves it.
	`borge create -r REPO archive ~ --exclude 'sh:**/.cache'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			paths := f.archivePaths("archive")
			if len(paths) < 6 {
				t.Fatalf("the archive holds %d paths; the fixture cannot have been "+
					"archived and this check proves nothing: %v", len(paths), paths)
			}
			for _, p := range paths {
				if strings.Contains(p, "/.cache") {
					t.Errorf("an --exclude written after the paths did not exclude %q", p)
				}
			}
			mustHaveSuffix(t, paths, "/home/me/notes.txt")
		},
	},

	// A path that begins with a dash, reached through "--".
	`borge create -r REPO archive -- ~/-weird-name`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			paths := f.archivePaths("archive")
			if len(paths) != 1 {
				t.Fatalf("expected the one dash-leading path, got %v", paths)
			}
			mustHaveSuffix(t, paths, "/home/me/-weird-name")
		},
	},

	`borge extract ARCHIVE 'sh:home/me/**/*.txt'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			got := regularFilesUnder(t, f.work)
			want := []string{
				f.stored + "/home/me/docs/report.txt",
				f.stored + "/home/me/notes.txt",
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("extracted\n  %s\nwanted\n  %s",
					strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		},
	},

	`borge list ARCHIVE 're:\.(jpg|png)$'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			lines := nonEmptyLines(stdout)
			if len(lines) != 2 {
				t.Fatalf("expected the .jpg and the .png, got %d lines:\n%s", len(lines), stdout)
			}
			for _, want := range []string{"photo.jpg", "icon.png"} {
				if !strings.Contains(stdout, want) {
					t.Errorf("the regular expression did not match %s:\n%s", want, stdout)
				}
			}
			for _, unwanted := range []string{"raw.tiff", "notes.txt"} {
				if strings.Contains(stdout, unwanted) {
					t.Errorf("the regular expression matched %s, which it must not:\n%s", unwanted, stdout)
				}
			}
		},
	},

	`borge find 'sh:**/invoice-*.pdf'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			lines := nonEmptyLines(stdout)
			// The invoice is in ARCHIVE and in daily-2026-01-01, and find searches
			// every archive.
			if len(lines) != 2 {
				t.Fatalf("expected the invoice in two archives, got %d lines:\n%s", len(lines), stdout)
			}
			for _, line := range lines {
				if !strings.Contains(line, "invoice-2026-01.pdf") {
					t.Errorf("find returned something that is not an invoice: %s", line)
				}
			}
		},
	},

	// From the sentence "a bare pattern is a pattern, so 'borge list ARCHIVE
	// sh:**/*.txt' works".
	`borge list ARCHIVE sh:**/*.txt`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			lines := nonEmptyLines(stdout)
			if len(lines) != 2 {
				t.Fatalf("expected the two .txt files, got %d lines:\n%s", len(lines), stdout)
			}
			for _, want := range []string{"notes.txt", "report.txt"} {
				if !strings.Contains(stdout, want) {
					t.Errorf("a positional pattern did not match %s:\n%s", want, stdout)
				}
			}
		},
	},
}

var matchArchivesExamples = map[string]exampleCheck{

	`borge repo-list -a 'sh:daily-*' --last 7`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			lines := nonEmptyLines(stdout)
			if len(lines) != 1 || !strings.Contains(stdout, "daily-2026-01-01") {
				t.Fatalf("expected the one daily-* archive:\n%s", stdout)
			}
			if strings.Contains(stdout, "ARCHIVE") {
				t.Errorf("the selector matched an archive it should not have:\n%s", stdout)
			}
		},
	},

	`borge delete -a 'tags:temporary'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			names := f.archiveNames()
			if contains(names, "tagged") {
				t.Errorf("the tagged archive is still there: %v", names)
			}
			if !contains(names, "ARCHIVE") {
				t.Errorf("the selector deleted more than the tagged archive: %v", names)
			}
		},
	},

	`borge info -a aid:4a9cd8a3`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			if !strings.Contains(stdout, "Archive name: ARCHIVE") {
				t.Errorf("an 8-character aid: prefix did not select ARCHIVE:\n%s", stdout)
			}
		},
	},

	`borge prune --keep-daily 7 -a 'host:laptop'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			// stderr: prune's listing and summary are progress, and borg puts progress
			// there so that a command's data has stdout to itself.
			if !strings.Contains(stderr, "pruned") {
				t.Errorf("prune said nothing about what it pruned:\n%s", stderr)
			}
			// Every archive in the fixture was made on this host, in the same second, so
			// one daily period holds all of them: the daily rule keeps the newest of that
			// period, and - since it is the only rule given and its quota of 7 is nowhere
			// near spent - it also keeps the OLDEST archive, which is borg's behaviour for
			// the last active rule (DIVERGENCES.md #50). Two survive, not one.
			//
			// Verified against borg on three archives sharing a timestamp:
			//   Keeping archive (rule: daily #1):            two
			//   Would prune:                                 three
			//   Keeping archive (rule: daily[oldest] #2):    one
			names := f.archiveNames()
			if len(names) != 2 {
				t.Errorf("expected two archives to survive --keep-daily 7 - the newest of "+
					"the day and the oldest of all - got %v", names)
			}
		},
	},

	// From "\"borge repo-list --short\" prints ids".
	`borge repo-list --short`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			lines := nonEmptyLines(stdout)
			if len(lines) < 4 {
				t.Fatalf("expected the fixture's archives, got %d lines:\n%s", len(lines), stdout)
			}
			for _, line := range lines {
				if !archiveIDLine.MatchString(line) {
					t.Errorf("--short printed something that is not an id: %q", line)
				}
			}
		},
	},

	// From "tag one with \"borge tag --add @PROT ARCHIVE\"".
	`borge tag --add @PROT ARCHIVE`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			if !strings.Contains(f.must("repo-list", "-r", f.repo), "@PROT") {
				t.Errorf("the tag was not set")
			}
		},
	},
}

var placeholdersExamples = map[string]exampleCheck{

	`borge create -r REPO '{hostname}-{now:%Y-%m-%d}' ~`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			want := regexp.MustCompile(`^` + regexp.QuoteMeta(f.hostname) + `-\d{4}-\d{2}-\d{2}$`)
			if !matchesAny(f.archiveNames(), want) {
				t.Errorf("no archive is named <hostname>-<date>: %v", f.archiveNames())
			}
		},
	},

	`borge create -r REPO 'daily-{utcnow:%Y%m%dT%H%M%S}' /srv`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			want := regexp.MustCompile(`^daily-\d{8}T\d{6}$`)
			if !matchesAny(f.archiveNames(), want) {
				t.Errorf("no archive is named daily-<timestamp>: %v", f.archiveNames())
			}
		},
	},

	`borge delete -a 'sh:{hostname}-*'`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			names := f.archiveNames()
			if contains(names, f.hostname+"-example") {
				t.Errorf("the placeholder was not substituted before matching: %v", names)
			}
			if !contains(names, "ARCHIVE") {
				t.Errorf("the selector deleted more than it should have: %v", names)
			}
		},
	},

	`borge repo-list -r /backups/{hostname}`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			if !strings.Contains(stdout, "other") {
				t.Errorf("a placeholder in a repository path did not resolve:\n%s", stdout)
			}
		},
	},
}

var compressionExamples = map[string]exampleCheck{

	`borge create -r REPO -C zstd,10 archive ~`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			// The point of the example is that a level borge does not have as its own
			// encoder setting is still accepted and recorded; see DIVERGENCES.md #16.
			mustHaveSuffix(t, f.archivePaths("archive"), "/home/me/notes.txt")
		},
	},

	`borge create -r REPO -C auto,zstd,3 archive ~`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			mustHaveSuffix(t, f.archivePaths("archive"), "/home/me/notes.txt")
		},
	},

	`borge repo-compress -r REPO -C zstd,3`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			// Recompressing rewrites every chunk in the repository. What matters is
			// that the archives still read back.
			paths := f.archivePaths("ARCHIVE")
			mustHaveSuffix(t, paths, "/home/me/notes.txt")
			mustHaveSuffix(t, paths, "/home/me/pics/photo.jpg")
			if _, _, code := f.run(nil, "check", "-r", f.repo); code != ExitOK {
				t.Errorf("the repository does not check out after repo-compress")
			}
		},
	},

	`borge recreate --compression`: {
		notRunnable: "a fragment: the sentence names the option that cannot work, not an invocation",
	},

	`borge repo-compress`: {
		notRunnable: "the sentence names the command; the invocation is the example below it",
	},

	`borge benchmark cpu --compressing`: {
		// The benchmark runs over a much smaller buffer in test mode; without this it
		// would spend a minute measuring compression the test does not care about.
		env:      map[string]string{"_BORGE_BENCHMARK_CPU_TEST": "1"},
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			// The sentence promises this shows the ratio each level achieves, so both
			// the levels and the ratios have to be in the output.
			if !strings.Contains(stdout, "zstd,3") {
				t.Errorf("the compression benchmark does not name the levels:\n%s", stdout)
			}
			if !compressionRatio.MatchString(stdout) {
				t.Errorf("the compression benchmark does not report a ratio:\n%s", stdout)
			}
		},
	},
}

var environmentExamples = map[string]exampleCheck{

	`BORGE_REPO=/backups/{hostname} borge repo-list`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			if !strings.Contains(stdout, "other") {
				t.Errorf("BORGE_REPO did not select the repository:\n%s", stdout)
			}
			if strings.Contains(stdout, "ARCHIVE") {
				t.Errorf("BORGE_REPO did not override the one already set:\n%s", stdout)
			}
		},
	},

	`BORGE_UNITS=iec borge analyze`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			if !strings.Contains(stdout, "KiB") && !strings.Contains(stdout, "MiB") {
				t.Errorf("BORGE_UNITS=iec did not produce binary units:\n%s", stdout)
			}
			if strings.Contains(stdout, " kB") || strings.Contains(stdout, " MB") {
				t.Errorf("BORGE_UNITS=iec still printed SI units:\n%s", stdout)
			}
		},
	},

	// From "SSH_ORIGINAL_COMMAND is read by \"borge debug info\" only".
	`borge debug info`: {
		wantExit: ExitOK,
		check: func(t *testing.T, f *helpFixture, stdout, stderr string) {
			if !strings.Contains(stdout, "SSH_ORIGINAL_COMMAND") {
				t.Errorf("debug info does not report SSH_ORIGINAL_COMMAND:\n%s", stdout)
			}
		},
	},
}

// helpExampleChecks merges the groups, refusing a command claimed twice.
func helpExampleChecks(t *testing.T) map[string]exampleCheck {
	t.Helper()
	out := map[string]exampleCheck{}
	for _, group := range []map[string]exampleCheck{
		patternsExamples, matchArchivesExamples, placeholdersExamples,
		compressionExamples, environmentExamples,
	} {
		for cmd, check := range group {
			if _, dup := out[cmd]; dup {
				t.Fatalf("two groups claim %q", cmd)
			}
			out[cmd] = check
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// TestHelpExamplesRun executes every command in every help topic.
//
// Each one gets a repository of its own, rebuilt from scratch, so the order the examples
// run in cannot matter and a destructive example - delete, prune - can be run for real
// rather than with --dry-run, which would test something other than what is documented.
//
// Every topic's examples are executed here, which is the "executed" grade in the doc
// audit: the one a user actually relies on, because they copy the example.
//
//borge:checks patterns/examples
//borge:checks match-archives/examples
//borge:checks placeholders/examples
//borge:checks compression/examples
//borge:checks environment/examples
func TestHelpExamplesRun(t *testing.T) {
	found := helpExamplesFromTopics(t)
	checks := helpExampleChecks(t)

	// The extractor scanning nothing would make every assertion below vacuous.
	if len(found) < 20 {
		t.Fatalf("found only %d commands in the help topics; the extractor is broken "+
			"and this test would pass on anything", len(found))
	}
	perTopic := map[string]int{}
	for _, ex := range found {
		perTopic[ex.topic]++
	}
	for _, topic := range helpTopics() {
		if perTopic[topic.name] == 0 {
			t.Errorf("\"borge help %s\" contains no command a reader can copy; "+
				"prose that carries no example is the grade users do not rely on "+
				"(docs/PORTING_PLAN.md §2.1.2)", topic.name)
		}
	}

	// Every command needs an entry, and every entry needs a command.
	claimed := map[string]bool{}
	distinct := map[string]helpExample{}
	for _, ex := range found {
		if _, ok := checks[ex.cmd]; !ok {
			t.Errorf("%s topic, line %d: no entry in this test says what %q should do",
				ex.topic, ex.line, ex.cmd)
			continue
		}
		claimed[ex.cmd] = true
		if _, seen := distinct[ex.cmd]; !seen {
			distinct[ex.cmd] = ex
		}

		// The annotations in the topic and the expectations here have to agree. A line
		// the reader is told is WRONG that this test expects to succeed means one of the
		// two is lying, and the reader cannot tell which.
		check := checks[ex.cmd]
		switch ex.note {
		case "WRONG":
			if check.wantExit == ExitOK {
				t.Errorf("%s topic, line %d: the topic labels %q WRONG and this test "+
					"expects it to succeed", ex.topic, ex.line, ex.cmd)
			}
		case "correct":
			if check.wantExit != ExitOK {
				t.Errorf("%s topic, line %d: the topic labels %q correct and this test "+
					"expects it to fail", ex.topic, ex.line, ex.cmd)
			}
		}
		// An entry that neither declines to run nor says what running it should
		// produce is an entry that checks nothing.
		if check.notRunnable == "" && check.check == nil {
			t.Errorf("%s topic, line %d: the entry for %q runs the command and then "+
				"asserts nothing about what it did", ex.topic, ex.line, ex.cmd)
		}
		// Declining to run something is only defensible for a command quoted inside a
		// sentence. An indented example is there to be copied.
		if check.notRunnable != "" && !ex.prose {
			t.Errorf("%s topic, line %d: %q is set out as an example to copy, and this "+
				"test declines to run it: %s", ex.topic, ex.line, ex.cmd, check.notRunnable)
		}
	}
	for cmd := range checks {
		if !claimed[cmd] {
			t.Errorf("this test expects %q in a help topic, and no topic contains it", cmd)
		}
	}
	if t.Failed() {
		t.Fatal("the inventory and the help topics disagree; that has to be settled " +
			"before running anything, because the disagreement is the finding")
	}

	var names []string
	for cmd := range distinct {
		names = append(names, cmd)
	}
	sort.Strings(names)

	for _, cmd := range names {
		ex, check := distinct[cmd], checks[cmd]
		// The command is part of the subtest name: line numbers move whenever a topic
		// is edited, and a failure that names only a line is one nobody can find.
		t.Run(fmt.Sprintf("%s/%d/%s", ex.topic, ex.line, safeName(cmd)), func(t *testing.T) {
			if check.notRunnable != "" {
				t.Skipf("%q is not run: %s", cmd, check.notRunnable)
			}
			f := newHelpFixture(t)

			tokens, err := splitDocCommand(cmd)
			if err != nil {
				t.Fatalf("%q: %v", cmd, err)
			}
			env := map[string]string{}
			for k, v := range check.env {
				env[k] = v
			}
			var args []string
			for i, tok := range tokens {
				tok = f.substitute(tok)
				switch {
				case args == nil && envAssignment.MatchString(tok):
					name, value, _ := strings.Cut(tok, "=")
					env[name] = value
				case tok == "borge":
					args = []string{}
				default:
					if args == nil {
						t.Fatalf("token %d of %q comes before \"borge\"", i, cmd)
					}
					args = append(args, tok)
				}
			}

			// extract writes into the working directory, so the examples run in one of
			// their own.
			t.Chdir(f.work)

			stdout, stderr, code := f.run(env, args...)
			if code != check.wantExit {
				t.Fatalf("%q\nexited %d, expected %d\nstdout:\n%s\nstderr:\n%s",
					cmd, code, check.wantExit, stdout, stderr)
			}
			check.check(t, f, stdout, stderr)
		})
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// safeName makes a subtest name out of a command. Everything unusual becomes an
// underscore, because t.TempDir() puts the test's name in a path and a name holding "/"
// or "{" produces a directory borge then cannot parse as a repository.
func safeName(cmd string) string {
	return unsafeInName.ReplaceAllString(cmd, "_")
}

var (
	unsafeInName     = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	archiveIDLine    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	compressionRatio = regexp.MustCompile(`\(\d+\.\dx\)`)
)

func matchesAny(list []string, re *regexp.Regexp) bool {
	for _, s := range list {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func mustHaveSuffix(t *testing.T, list []string, suffix string) {
	t.Helper()
	for _, s := range list {
		if strings.HasSuffix(s, suffix) {
			return
		}
	}
	t.Errorf("nothing ends in %q: %v", suffix, list)
}

// regularFilesUnder lists the regular files an extraction produced, as stored paths.
func regularFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
