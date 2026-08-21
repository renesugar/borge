// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// The SFTP backend, against a real SFTP server.
//
// The server is sftpgo on localhost, prepared by tests/remote/setup.sh: a user, a key, a
// Host block in ~/.ssh/config and the host key in known_hosts. That is not incidental
// scaffolding - it is the configuration path being tested. An sftp:// URL is usually an
// alias, and everything that decides where the connection goes comes out of the ssh config.
//
// These skip when the server is not there, and say so rather than passing quietly.

const sftpTestAlias = "borge-sftp-test"

func requireSFTP(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the sftp backend tests in short mode")
	}
	// A Host block for the alias means this machine was set up for these tests, so from
	// there on a failure is a fault rather than an absence and the tests say so instead
	// of skipping. A skip that fires by accident is a test reporting success while
	// measuring nothing, which is what these backends are most at risk of.
	config := lookupSSHConfig(sftpTestAlias)
	if config.HostName == sftpTestAlias {
		t.Skipf("no %q block in ~/.ssh/config; run tests/remote/setup.sh", sftpTestAlias)
	}
	backend, err := NewSFTP("sftp://" + sftpTestAlias + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.connect(); err != nil {
		t.Fatalf("%q is configured in ~/.ssh/config but the server is not reachable: %v. "+
			"Start sftpgo (tests/remote/setup.sh lists what it needs), or remove the Host "+
			"block if this machine is not meant to run these tests.", sftpTestAlias, err)
	}
	backend.disconnect()
}

// sftpTestPath is a directory of this test's own on the server, removed afterwards.
func sftpTestPath(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("borge-test-%s-%d", strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '-'
		}
		return r
	}, t.Name()), os.Getpid())
	return name
}

// newSFTPForTest returns an opened backend over a fresh directory on the server, and a way
// to plant a file the backend would refuse to write.
func newSFTPForTest(t *testing.T) (Backend, planter) {
	t.Helper()
	requireSFTP(t)
	path := sftpTestPath(t)
	backend, err := NewSFTP("sftp://" + sftpTestAlias + "/" + path)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		fresh, err := NewSFTP("sftp://" + sftpTestAlias + "/" + path)
		if err == nil {
			_ = fresh.Destroy()
		}
	})
	if err := backend.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, sftpPlanter(backend)
}

// sftpPlanter writes a file over the backend's own connection, bypassing the name rules.
//
// The store is on a server, so there is no local directory to write into - and reaching
// around to sftpgo's data directory would test this machine's filesystem layout rather than
// the backend. The connection is the one thing that is certainly there.
func sftpPlanter(b *SFTP) planter {
	return func(t *testing.T, name string) {
		t.Helper()
		full := path.Join(b.base, name)
		if err := b.client.MkdirAll(path.Dir(full)); err != nil {
			t.Fatal(err)
		}
		file, err := b.client.Create(full)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("not borge's")); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSFTPOverwriteLeavesOneObject: a write goes to a temporary name and is renamed over
// the target, so a reader never sees a partial object and an overwrite leaves no debris.
//
// The rename is the POSIX one, which replaces an existing target in a single step; SFTP's
// plain rename fails when the target exists, which would make an overwrite a delete
// followed by a write, with a window in between where the object does not exist at all.
func TestSFTPOverwriteLeavesOneObject(t *testing.T) {
	backend, _ := newSFTPForTest(t)

	if err := backend.Store("config/id", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := backend.Store("config/id", []byte("second-and-longer")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	entries, err := backend.List("config")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "id" {
		t.Errorf("after an overwrite the directory holds %+v", entries)
	}
	got, err := backend.Load("config/id", 0, -1)
	if err != nil || string(got) != "second-and-longer" {
		t.Errorf("after an overwrite the object is %q, %v", got, err)
	}
}

// TestSFTPWritesIntoADirectoryThatIsNotThere: the parent is created only after a write
// fails for the want of it.
//
// Not an optimisation for its own sake: every SFTP operation is a round trip, and on a link
// with latency a mkdir before each of tens of thousands of writes is most of the time.
func TestSFTPWritesIntoADirectoryThatIsNotThere(t *testing.T) {
	backend, _ := newSFTPForTest(t)

	if err := backend.Store("packs/ab/cdef0123", []byte("payload")); err != nil {
		t.Fatalf("Store into a missing directory: %v", err)
	}
	got, err := backend.Load("packs/ab/cdef0123", 0, -1)
	if err != nil || string(got) != "payload" {
		t.Errorf("the object is %q, %v", got, err)
	}
	// And a move into one that is not there, which has the same retry.
	if err := backend.Move("packs/ab/cdef0123", "packs/ff/cdef0123"); err != nil {
		t.Fatalf("Move into a missing directory: %v", err)
	}
	if got, err := backend.Load("packs/ff/cdef0123", 0, -1); err != nil || string(got) != "payload" {
		t.Errorf("the moved object is %q, %v", got, err)
	}
}

// TestSFTPCreateRefusesANonEmptyDirectory, as every backend does.
func TestSFTPCreateRefusesANonEmptyDirectory(t *testing.T) {
	backend, _ := newSFTPForTest(t)
	if err := backend.Store("config/id", []byte("x")); err != nil {
		t.Fatal(err)
	}
	again, err := NewSFTP("sftp://" + sftpTestAlias + "/" + sftpTestPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Create(); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("Create over an existing store gave %v, want ErrAlreadyExists", err)
	}
}

// TestSFTPRefusesAnUnknownHost: the host key must already be known.
//
// borgstore says why, and borge repeats it: a backup program that accepted a new host key
// on its own would keep working while an attacker stood in the middle. The message has to
// say which of the two failures it is - never seen, or changed - because they need
// different answers from the user.
func TestSFTPRefusesAnUnknownHost(t *testing.T) {
	requireSFTP(t)

	empty := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BORGE_KNOWN_HOSTS", empty)

	backend, err := NewSFTP("sftp://" + sftpTestAlias + "/whatever")
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Open()
	if err == nil {
		backend.Close()
		t.Fatal("borge connected to a host with no key in known_hosts")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("the refusal does not mention known_hosts: %v", err)
	}
	if !strings.Contains(err.Error(), "ssh "+sftpTestAlias) {
		t.Errorf("the refusal does not say how to establish the key: %v", err)
	}
}

// TestSFTPNeverAsksForAPassword: there is no password in an sftp URL and no prompt for one.
//
// Measured rather than assumed, when a password-authenticated test account turned out to be
// unusable by borg: borgstore's URL has no password field and its connect call passes a key
// file and an agent. A borge that accepted one would be accepting a URL borg rejects.
func TestSFTPNeverAsksForAPassword(t *testing.T) {
	// The regex is the contract: user@host, no colon-password.
	backend, err := NewSFTP("sftp://someone:secret@host/path")
	if err != nil {
		t.Fatalf("the URL did not parse at all: %v", err)
	}
	if backend.user != "someone:secret" {
		// borgstore's username group is [^@]+, so everything before the "@" is the user
		// name - a colon in it is part of the name, not a separator.
		t.Errorf("the user was parsed as %q; a password would mean the URL carries one", backend.user)
	}
	source := readSourceFile(t, "sftp.go")
	for _, forbidden := range []string{"ssh.Password(", "PasswordCallback", "KeyboardInteractive"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("the sftp backend uses %s; authentication is by key or agent only", forbidden)
		}
	}
}

func TestSFTPURLParsing(t *testing.T) {
	for _, c := range []struct {
		url        string
		user, host string
		port       int
		base       string
	}{
		{"sftp://host/repo", "", "host", 0, "repo"},
		{"sftp://user@host/repo", "user", "host", 0, "repo"},
		{"sftp://user@host:2222/repo", "user", "host", 2222, "repo"},
		// The slash after the host is a separator, so one slash is a relative path - to
		// the login directory - and two make it absolute. Same shape as rest:// (#59).
		{"sftp://host//srv/repo", "", "host", 0, "/srv/repo"},
		// Percent-escapes are decoded, which is how a user name with an "@" in it fits.
		{"sftp://user%40example.com@host/repo", "user@example.com", "host", 0, "repo"},
	} {
		got, err := NewSFTP(c.url)
		if err != nil {
			t.Errorf("%q: %v", c.url, err)
			continue
		}
		if got.user != c.user || got.host != c.host || got.port != c.port || got.base != c.base {
			t.Errorf("%q parsed as user=%q host=%q port=%d path=%q, want %q/%q/%d/%q",
				c.url, got.user, got.host, got.port, got.base, c.user, c.host, c.port, c.base)
		}
	}
	for _, bad := range []string{"sftp://host", "sftp:///path", "sftp://host/", "http://host/x"} {
		if _, err := NewSFTP(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// readSourceFile reads one of this package's files, for the tests that assert on how
// something is done rather than on what it produces.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
