// SPDX-License-Identifier: Apache-2.0

//go:build linux

package interop

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rest:// protocol, with each tool on each end.
//
// This is the only backend where borge is not just a client: "borge serve --rest" is the
// server side of a rest:// repository, and the client that starts it may be borg. So there
// are two claims to check and they fail differently:
//
//   - borge's client can drive borg's server (the protocol as borge speaks it is one borg
//     understands), and
//   - borg's client can drive borge's server (the protocol as borge answers it is one borg
//     understands).
//
// A single-tool test would pass with both halves wrong in the same way.
//
// # How each tool is made to start the other
//
// Neither tool has an option for "serve with that program". Both build the command the
// same way, though: for a rest:// URL with a *host*, they run a remote shell, then the
// program named by BORG_REMOTE_PATH (borge also reads BORGE_REMOTE_PATH), then "serve
// --rest --backend FILE:...". Both take the remote shell from BORGSTORE_RSH. So a two-line
// script that drops the hostname and runs the rest locally turns "over ssh to that host"
// into "as that program, here" - no network, no sshd, and the command line each tool builds
// is exercised exactly as it would be in earnest.

// fakeRSH writes a remote shell that runs its command locally, ignoring the host.
func fakeRSH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-rsh")
	script := "#!/bin/sh\n" +
		"# Drop the hostname the caller put in front of the command, then run the rest here.\n" +
		"shift\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// restEnv is the harness environment plus what makes one tool start the other.
func (tl *tools) restEnv(rsh, remotePath string) []string {
	return append(tl.env(),
		"BORGSTORE_RSH="+rsh,
		"BORG_REMOTE_PATH="+remotePath,
		"BORGE_REMOTE_PATH="+remotePath,
	)
}

func (tl *tools) runWithEnv(bin string, env []string, args ...string) (string, error) {
	tl.t.Helper()
	return tl.runWithEnvIn(bin, env, "", args...)
}

// runWithEnvIn is runWithEnv in a given working directory, which borg's extract needs.
func (tl *tools) runWithEnvIn(bin string, env []string, dir string, args ...string) (string, error) {
	tl.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// TestRestBothDirections: each tool's client against the other tool's server.
func TestRestBothDirections(t *testing.T) {
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)
	rsh := fakeRSH(t)

	for _, c := range []struct {
		name           string
		client, server string
	}{
		{"borge client, borg server", tl.borge, tl.borg},
		{"borg client, borge server", tl.borg, tl.borge},
		{"borge on both ends", tl.borge, tl.borge},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A host makes both tools build the ssh form, which the fake remote shell
			// then runs locally; the double slash keeps the path absolute.
			repo := filepath.Join(t.TempDir(), "repo")
			url := "rest://borge-rest-test/" + repo
			env := tl.restEnv(rsh, c.server)

			if out, err := tl.runWithEnv(c.client, env, "repo-create", "-r", url, "-e", "aes256-ocb"); err != nil {
				t.Fatalf("repo-create over rest://: %v\n%s", err, out)
			}
			if out, err := tl.runWithEnv(c.client, env, "create", "-r", url, "one", src); err != nil {
				t.Fatalf("create over rest://: %v\n%s", err, out)
			}

			// The archive is readable over the protocol...
			out, err := tl.runWithEnv(c.client, env, "repo-list", "-r", url, "--format", "{archive}{NL}")
			if err != nil {
				t.Fatalf("repo-list over rest://: %v\n%s", err, out)
			}
			if lastLine(out) != "one" {
				t.Errorf("repo-list over rest:// gave %q", lastLine(out))
			}

			// ... and the repository underneath is an ordinary one, which is what says
			// the protocol carried the format rather than a version of it.
			out, err = tl.run(tl.borg, "", "repo-list", "-r", repo, "--format", "{archive}{NL}")
			if err != nil {
				t.Fatalf("borg could not read the served repository as a directory: %v\n%s", err, out)
			}
			if lastLine(out) != "one" {
				t.Errorf("read as a directory, the repository lists %q", lastLine(out))
			}

			// And the whole tree comes back, extracted by the client that wrote it.
			dest := t.TempDir()
			if c.client == tl.borg {
				out, err = tl.runWithEnvIn(c.client, env, dest, "extract", "-r", url, "one")
			} else {
				out, err = tl.runWithEnv(c.client, env, "extract", "-r", url, "-C", dest, "one")
			}
			if err != nil {
				t.Fatalf("extract over rest://: %v\n%s", err, out)
			}
			checkTrees(t, src, filepath.Join(dest,
				filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)
		})
	}
}

// TestRestCompactionUsesDefrag: borg's compaction rewrites a pack through the protocol's
// defrag operation - it names the byte ranges to keep and the server does the copying,
// rather than the pack being downloaded and uploaded again. A server without it is one borg
// can back up to but not maintain, and nothing would say so until a repository needed
// compacting.
//
// The test asserts that defrag was actually used, by capturing the request stream: an
// earlier version of it ran a compaction that had nothing to reclaim and passed while
// exercising none of this.
func TestRestCompactionUsesDefrag(t *testing.T) {
	tl := newTools(t, "aes256-ocb")
	rsh := fakeRSH(t)

	// Twenty files of random bytes, so that packs hold many objects and dropping some of
	// them leaves a pack worth rewriting rather than one worth deleting.
	src := t.TempDir()
	for i := 0; i < 20; i++ {
		body := make([]byte, 100000)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d.bin", i)), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	captured := filepath.Join(t.TempDir(), "requests.bin")
	server := capturingServer(t, tl.borge, captured)

	repo := filepath.Join(t.TempDir(), "repo")
	url := "rest://borge-rest-test/" + repo
	env := tl.restEnv(rsh, server)

	run := func(args ...string) string {
		t.Helper()
		out, err := tl.runWithEnv(tl.borg, env, args...)
		if err != nil {
			t.Fatalf("borg %s over rest://: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	run("repo-create", "-r", url, "-e", "aes256-ocb")
	run("create", "-r", url, "one", src)
	// Remove two files, archive again, and delete the first archive: what is left is a
	// pack that keeps most of its objects and loses a few.
	for _, name := range []string{"f00.bin", "f01.bin"} {
		if err := os.Remove(filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	run("create", "-r", url, "two", src)
	run("delete", "-r", url, "-a", "one")

	if err := os.Truncate(captured, 0); err != nil {
		t.Fatal(err)
	}
	out := run("compact", "-r", url, "-v")
	if !strings.Contains(out, "unused objects") {
		t.Errorf("compaction reported nothing to do, so this test measures nothing:\n%s", out)
	}

	requests, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(requests, []byte("cmd=defrag")) {
		t.Fatalf("compaction did not use defrag, so the server's implementation of it was "+
			"not exercised; the requests were:\n%s", requestKinds(requests))
	}

	// The repository still checks out, which is what compaction must not break - and the
	// check reads every chunk, so it reads the pack defrag rewrote.
	run("check", "-r", url, "--verify-data")

	// And the surviving archive still restores through the same server.
	dest := t.TempDir()
	if out, err := tl.runWithEnvIn(tl.borg, env, dest, "extract", "-r", url, "two"); err != nil {
		t.Fatalf("extract after compaction: %v\n%s", err, out)
	}
	checkTrees(t, src, filepath.Join(dest,
		filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)
}

// capturingServer writes a wrapper that runs the real server with the request stream teed
// to a file, so a test can assert on what the client asked for.
func capturingServer(t *testing.T, serve, captureTo string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capturing-serve")
	script := fmt.Sprintf("#!/bin/sh\nexec tee -a %q | %q \"$@\"\n", captureTo, serve)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(captureTo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// requestKinds summarises a captured stream for an error message.
func requestKinds(requests []byte) string {
	var kinds []string
	for _, line := range strings.Split(string(requests), "\r\n") {
		if strings.HasPrefix(line, "GET ") || strings.HasPrefix(line, "POST ") ||
			strings.HasPrefix(line, "HEAD ") || strings.HasPrefix(line, "DELETE ") {
			kinds = append(kinds, line)
		}
	}
	if len(kinds) > 12 {
		kinds = append(kinds[:12], fmt.Sprintf("... and %d more", len(kinds)-12))
	}
	return strings.Join(kinds, "\n")
}
