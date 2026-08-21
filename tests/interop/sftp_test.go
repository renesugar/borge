// SPDX-License-Identifier: Apache-2.0

//go:build linux

package interop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sftp backend, both directions, against a real SFTP server.
//
// The server is sftpgo on localhost, prepared by tests/remote/setup.sh, and reached through
// a Host block in ~/.ssh/config - which is how an sftp:// repository is normally addressed
// and therefore what has to work. borg reaches it through paramiko and borge through
// x/crypto/ssh, so this is two independent SSH implementations writing one repository.

const sftpAlias = "borge-sftp-test"

// requireSFTPServer skips when there is no test server here, and fails when there is one
// that cannot be reached.
//
// The difference matters more than it looks. A skip that fires by accident is a row that
// reports success while testing nothing, and this exact test skipped every row for a while
// because its probe ran "ssh host true" against a server that offers only the SFTP
// subsystem. So: a Host block for the alias means somebody set this machine up for these
// tests, and anything failing after that is a fault rather than an absence.
func requireSFTPServer(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the sftp interop rows in short mode")
	}
	configured, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".ssh", "config"))
	if err != nil || !strings.Contains(string(configured), "Host "+sftpAlias) {
		t.Skipf("no %q block in ~/.ssh/config; run tests/remote/setup.sh to enable the "+
			"sftp rows", sftpAlias)
	}
	// One connection with the command-line client, which is neither of the two
	// implementations under test and so cannot hide a fault in either. It has to be the
	// *sftp* client: an sftp-only server refuses "ssh host true" with "exec request
	// failed", so probing that way skips every row on a server that is working perfectly.
	cmd := exec.Command("sftp", "-b", "-", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", sftpAlias)
	cmd.Stdin = strings.NewReader("pwd\nbye\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%q is configured in ~/.ssh/config but the server is not reachable "+
			"(%v: %s). That is a broken test environment, not a missing one, so these rows "+
			"fail rather than skip: start sftpgo, or remove the Host block.",
			sftpAlias, err, strings.TrimSpace(string(out)))
	}
}

// sftpRepoURL is a repository of this test's own on the server.
func sftpRepoURL(t *testing.T, suffix string) string {
	t.Helper()
	name := fmt.Sprintf("borge-interop-%s-%d", suffix, os.Getpid())
	return "sftp://" + sftpAlias + "/" + name
}

func TestSftpBothDirections(t *testing.T) {
	requireSFTPServer(t)
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)

	for i, c := range []struct {
		name           string
		writer, reader string
	}{
		{"borg writes, borge reads", tl.borg, tl.borge},
		{"borge writes, borg reads", tl.borge, tl.borg},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := sftpRepoURL(t, fmt.Sprintf("both-%d", i))
			// Removed with the tool that did not write it, which exercises one more
			// crossing and leaves the server clean either way.
			t.Cleanup(func() { _, _ = tl.run(c.reader, "", "repo-delete", "-r", url, "--force") })

			if out, err := tl.run(c.writer, "", "repo-create", "-r", url, "-e", "aes256-ocb"); err != nil {
				t.Fatalf("repo-create over sftp: %v\n%s", err, out)
			}
			if out, err := tl.run(c.writer, "", "create", "-r", url, "one", src); err != nil {
				t.Fatalf("create over sftp: %v\n%s", err, out)
			}

			out, err := tl.run(c.reader, "", "repo-list", "-r", url, "--format", "{archive}{NL}")
			if err != nil {
				t.Fatalf("repo-list over sftp: %v\n%s", err, out)
			}
			if lastLine(out) != "one" {
				t.Errorf("the other tool lists %q over sftp", lastLine(out))
			}

			dest := t.TempDir()
			if c.reader == tl.borg {
				out, err = tl.run(c.reader, dest, "extract", "-r", url, "one")
			} else {
				out, err = tl.run(c.reader, "", "extract", "-r", url, "-C", dest, "one")
			}
			if err != nil {
				t.Fatalf("extract over sftp: %v\n%s", err, out)
			}
			checkTrees(t, src, filepath.Join(dest,
				filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)
		})
	}
}

// TestSftpSharedRepository is the row that matters most: both tools writing into one
// repository over sftp, and each reading what the other wrote.
//
// Two archives, one from each tool, then a check by each: this is where a difference in how
// the two write - a name, a nesting level, a lock - shows up as something the other cannot
// read, which the one-writer rows above would not catch.
func TestSftpSharedRepository(t *testing.T) {
	requireSFTPServer(t)
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)

	url := sftpRepoURL(t, "shared")
	t.Cleanup(func() { _, _ = tl.run(tl.borge, "", "repo-delete", "-r", url, "--force") })

	if out, err := tl.run(tl.borg, "", "repo-create", "-r", url, "-e", "aes256-ocb"); err != nil {
		t.Fatalf("repo-create: %v\n%s", err, out)
	}
	if out, err := tl.run(tl.borg, "", "create", "-r", url, "by-borg", src); err != nil {
		t.Fatalf("borg create over sftp: %v\n%s", err, out)
	}
	if out, err := tl.run(tl.borge, "", "create", "-r", url, "by-borge", src); err != nil {
		t.Fatalf("borge create over sftp: %v\n%s", err, out)
	}

	for _, reader := range []struct {
		name string
		bin  string
	}{{"borg", tl.borg}, {"borge", tl.borge}} {
		out, err := tl.run(reader.bin, "", "repo-list", "-r", url, "--format", "{archive}{NL}")
		if err != nil {
			t.Fatalf("%s repo-list: %v\n%s", reader.name, err, out)
		}
		for _, want := range []string{"by-borg", "by-borge"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not see the archive %q in the shared repository:\n%s",
					reader.name, want, out)
			}
		}
		// The deduplication is the point of sharing a repository: the second archive of
		// the same tree must have cost almost nothing, and a check reads it all back.
		if out, err := tl.run(reader.bin, "", "check", "-r", url, "--verify-data"); err != nil {
			t.Fatalf("%s check over sftp: %v\n%s", reader.name, err, out)
		}
	}

	// Each tool extracts the other's archive, which is the assertion the listing only
	// suggests.
	for _, c := range []struct{ reader, archive string }{
		{tl.borg, "by-borge"},
		{tl.borge, "by-borg"},
	} {
		dest := t.TempDir()
		var err error
		var out string
		if c.reader == tl.borg {
			out, err = tl.run(c.reader, dest, "extract", "-r", url, c.archive)
		} else {
			out, err = tl.run(c.reader, "", "extract", "-r", url, "-C", dest, c.archive)
		}
		if err != nil {
			t.Fatalf("extracting %s: %v\n%s", c.archive, err, out)
		}
		checkTrees(t, src, filepath.Join(dest,
			filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)
	}
}
