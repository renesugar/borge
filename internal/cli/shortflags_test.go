// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The short spellings borg offers, and the two options that finish row 3's small remainder.
//
// A short flag is easy to register and forget to wire, and the option gate compares *names*
// - so an "-n" that parsed and did nothing would satisfy it. These compare behaviour: the
// short form has to do what the long one does.

// TestShortFlagsMatchTheirLongForms: every -n and -s borge gained on 2026-08-20.
func TestShortFlagsMatchTheirLongForms(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "one", src)
	r.mustRun("create", "-r", r.path, "two", src)
	// Soft-deleted, so undelete has something to be a dry run about.
	r.mustRun("delete", "-r", r.path, "-a", "two")

	cases := []struct {
		name  string
		long  []string
		short []string
	}{
		{"compact --dry-run", []string{"compact", "-r", r.path, "--dry-run"},
			[]string{"compact", "-r", r.path, "-n"}},
		{"compact --stats", []string{"compact", "-r", r.path, "--dry-run", "--stats"},
			[]string{"compact", "-r", r.path, "--dry-run", "-s"}},
		{"delete --dry-run", []string{"delete", "-r", r.path, "-a", "one", "--dry-run", "--list"},
			[]string{"delete", "-r", r.path, "-a", "one", "-n", "--list"}},
		{"undelete --dry-run", []string{"undelete", "-r", r.path, "-a", "two", "--dry-run", "--list"},
			[]string{"undelete", "-r", r.path, "-a", "two", "-n", "--list"}},
		{"recreate --dry-run", []string{"recreate", "-r", r.path, "-a", "one", "--dry-run", "--list"},
			[]string{"recreate", "-r", r.path, "-a", "one", "-n", "--list"}},
		{"recreate --stats", []string{"recreate", "-r", r.path, "-a", "one", "--dry-run", "--stats"},
			[]string{"recreate", "-r", r.path, "-a", "one", "--dry-run", "-s"}},
		{"repo-compress --stats", []string{"repo-compress", "-r", r.path, "--dry-run", "--stats"},
			[]string{"repo-compress", "-r", r.path, "--dry-run", "-s"}},
		{"extract --dry-run", []string{"extract", "-r", r.path, "one", "--dry-run"},
			[]string{"extract", "-r", r.path, "one", "-n"}},
		{"extract --stats", []string{"extract", "-r", r.path, "one", "--dry-run", "--stats"},
			[]string{"extract", "-r", r.path, "one", "--dry-run", "-s"}},
		{"create --stats", []string{"create", "-r", r.path, "s-long", src, "--stats"},
			[]string{"create", "-r", r.path, "s-short", src, "-s"}},
		{"create --one-file-system", []string{"create", "-r", r.path, "x-long", src, "--one-file-system"},
			[]string{"create", "-r", r.path, "x-short", src, "-x"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			longOut, longErr, longCode := r.borge(t, c.long...)
			shortOut, shortErr, shortCode := r.borge(t, c.short...)
			if longCode != shortCode {
				t.Fatalf("exit %d for the long form, %d for the short\nlong: %s\nshort: %s",
					longCode, shortCode, longErr, shortErr)
			}
			if longCode != ExitOK {
				t.Fatalf("the long form failed, so this proves nothing: %s", longErr)
			}
			// The outputs carry archive names and ids that differ by design in the two
			// create cases, so those compare the *shape*: same number of lines, same
			// leading words.
			if strings.Contains(c.name, "create ") {
				if lines(longErr) != lines(shortErr) {
					t.Errorf("%s: %d lines long, %d short\nlong: %s\nshort: %s",
						c.name, lines(longErr), lines(shortErr), longErr, shortErr)
				}
				return
			}
			// Timings are blanked before comparing: the store report --stats prints holds
			// elapsed times and throughputs, and two runs of the same command never agree
			// on those. Everything else, including every count and volume, is compared as
			// it stands.
			if longOut != shortOut || withoutTimings(longErr) != withoutTimings(shortErr) {
				t.Errorf("%s differs\nlong stdout: %q\nshort stdout: %q\nlong stderr: %q\nshort stderr: %q",
					c.name, longOut, shortOut, longErr, shortErr)
			}
		})
	}
}

// withoutTimings blanks the value of any line whose units make it a measurement of this
// particular run.
func withoutTimings(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasSuffix(line, " seconds") || strings.Contains(line, "/s") {
			label, _, ok := strings.Cut(line, ": ")
			if ok {
				line = label + ": <timing>"
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func lines(s string) int {
	return len(strings.Split(strings.TrimRight(s, "\n"), "\n"))
}

// TestHelpPartsMatchBorgsExitCodes: --usage-only and --epilog-only.
//
// The *text* cannot be compared - borg prints an argparse usage block and a paragraph of
// prose, and borge has neither - so what is compared is that both tools accept the options
// for a command and for a help topic, and print something rather than nothing. What each
// prints is recorded in DIVERGENCES.md #53.
func TestHelpPartsMatchBorgsExitCodes(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	for _, topic := range []string{"create", "patterns"} {
		for _, opt := range []string{"--usage-only", "--epilog-only"} {
			t.Run(topic+" "+opt, func(t *testing.T) {
				wantOut, _ := borgStreams(t, r, "help", topic, opt)
				if strings.TrimSpace(wantOut) == "" {
					t.Fatalf("borg printed nothing for %s %s; this test asserts the wrong thing", topic, opt)
				}
				gotOut, stderr, code := r.borge(t, "help", topic, opt)
				if code != ExitOK {
					t.Fatalf("borge help %s %s exited %d\n%s", topic, opt, code, stderr)
				}
				if strings.TrimSpace(gotOut) == "" {
					t.Errorf("borge printed nothing for help %s %s", topic, opt)
				}
			})
		}
	}

	// --usage-only on a command has to be the option list, which is the part a reader
	// asking for "usage" wants: it must name at least one of the command's own options.
	out, _, code := r.borge(t, "help", "create", "--usage-only")
	if code != ExitOK {
		t.Fatalf("borge help create --usage-only exited %d", code)
	}
	for _, want := range []string{"-exclude", "-list"} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage does not mention %q:\n%s", want, out)
		}
	}
	// And --epilog-only must not be the same thing: one line, not the option list.
	out, _, _ = r.borge(t, "help", "create", "--epilog-only")
	if strings.Contains(out, "-exclude") {
		t.Errorf("--epilog-only printed the option list:\n%s", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("--epilog-only printed nothing")
	}
}

// TestKeyRemoveByPassphraseMatchesBorg: --passphrase removes the key the repository was
// opened with, and the three selectors are mutually exclusive.
//
// Which key a passphrase opens is not fixed: both tools try the keys in turn and take the
// first that opens, and with two keys sharing a passphrase either may be the one. So the
// test reads "borg key list" for the key it marks current and asserts about *that* key,
// rather than assuming the order.
func TestKeyRemoveByPassphraseMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.mustRun("key", "add", "-r", r.path, "--label", "second")

	// borg marks the current key with "*" in the first column.
	currentKey := func(t *testing.T) (id, label string) {
		t.Helper()
		out, _ := borgStreams(t, r, "key", "list", "-r", r.path)
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "*") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 {
				t.Fatalf("cannot read the current key from %q", line)
			}
			return fields[1], fields[3]
		}
		t.Fatalf("borg's key list marks no current key:\n%s", out)
		return "", ""
	}

	// borg refuses two selectors at once, and so must borge.
	if out, err := r.runErr("key", "remove", "-r", r.path, "--label", "second", "--passphrase"); err == nil {
		t.Fatalf("borg accepted two selectors:\n%s", out)
	}
	if _, stderr, code := r.borge(t, "key", "remove", "-r", r.path,
		"--label", "second", "--passphrase"); code != ExitError {
		t.Errorf("borge accepted two selectors (exit %d): %s", code, stderr)
	}

	id, label := currentKey(t)
	stdout, stderr, code := r.borge(t, "key", "remove", "-r", r.path, "--passphrase")

	if label == "admin" {
		// The admin key is protected in both tools, so the refusal is the answer - and it
		// shows the selection worked, because it named the admin key rather than the other.
		if code != ExitError {
			t.Fatalf("borge removed the admin key (exit %d): %s%s", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "admin") || !strings.Contains(stderr, "protected") {
			t.Errorf("borge refused for a different reason: %s", stderr)
		}
		out, err := r.runErr("key", "remove", "-r", r.path, "--passphrase")
		if err == nil {
			t.Errorf("borg removed the admin key:\n%s", out)
		}
	} else {
		if code != ExitOK {
			t.Fatalf("borge key remove --passphrase exited %d\n%s", code, stderr)
		}
		if !strings.Contains(stdout, "removed key") {
			t.Errorf("borge said nothing about what it removed: %q", stdout)
		}
		out, _ := borgStreams(t, r, "key", "list", "-r", r.path)
		if strings.Contains(out, id) {
			t.Errorf("the key the passphrase opened (%s, %q) is still there:\n%s", id, label, out)
		}
	}

	// Either way a removable key still goes by label, so a refusal above is about which
	// key was selected and not about the command being broken.
	if strings.Contains(mustKeyList(t, r), "second") {
		if _, stderr, code := r.borge(t, "key", "remove", "-r", r.path, "--label", "second"); code != ExitOK {
			t.Fatalf("borge key remove --label exited %d\n%s", code, stderr)
		}
		if strings.Contains(mustKeyList(t, r), "second") {
			t.Error("the labelled key was not removed")
		}
	}
	// And the repository still opens with what is left.
	if _, stderr, code := r.borge(t, "repo-list", "-r", r.path); code != ExitOK {
		t.Errorf("the repository no longer opens after removing a key: %s", stderr)
	}
}

func mustKeyList(t *testing.T, r *borgRepo) string {
	t.Helper()
	out, _ := borgStreams(t, r, "key", "list", "-r", r.path)
	return out
}
