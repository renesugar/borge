// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The rclone backend, tested against a real rclone against a local remote.
//
// "rclone:/tmp/somewhere" is a complete rclone remote, so all of this runs with no network,
// no credentials and no service - which is why this is the backend that answers whether
// store.Backend is the right interface (PORTING_PLAN §11.5) at the lowest cost.

func requireRclone(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the rclone backend tests in short mode")
	}
	if _, err := exec.LookPath(rcloneBinary()); err != nil {
		t.Skipf("rclone is not installed, so the rclone backend cannot be tested: %v", err)
	}
}

// newRcloneForTest returns an opened rclone backend over a fresh local remote, and the
// directory that remote is.
func newRcloneForTest(t *testing.T) (Backend, planter) {
	t.Helper()
	requireRclone(t)
	dir := filepath.Join(t.TempDir(), "remote")
	b, err := NewRclone(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, localPlanter(dir)
}

func newPosixFSForTest(t *testing.T) (Backend, planter) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote")
	b, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Create(); err != nil {
		t.Fatal(err)
	}
	if err := b.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, localPlanter(dir)
}

// localPlanter writes a file straight into the directory a store is kept in.
func localPlanter(dir string) planter {
	return func(t *testing.T, name string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not borge's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBackendConformance runs one suite over every backend borge has.
//
// The REST backend appears twice because it has two transports and only one protocol: over
// a socket, and over the stdin and stdout of a server process, which is the one borg uses.
func TestBackendConformance(t *testing.T) {
	t.Run("posixfs", func(t *testing.T) { runBackendConformance(t, newPosixFSForTest) })
	t.Run("rclone", func(t *testing.T) { runBackendConformance(t, newRcloneForTest) })
	t.Run("rest-http", func(t *testing.T) { runBackendConformance(t, newRESTForTest) })
	t.Run("rest-stdio", func(t *testing.T) { runBackendConformance(t, newRESTStdioForTest) })
	t.Run("sftp", func(t *testing.T) { runBackendConformance(t, newSFTPForTest) })
}

// TestRcloneWritesWhatPosixFSWould is the interop claim in its smallest form: a store
// written through rclone to a local remote is byte-for-byte a store the local backend
// reads, which is what lets borg open what borge wrote through rclone and the other way
// round.
func TestRcloneWritesWhatPosixFSWould(t *testing.T) {
	requireRclone(t)
	dir := filepath.Join(t.TempDir(), "remote")

	through, err := NewRclone(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := through.Create(); err != nil {
		t.Fatal(err)
	}
	if err := through.Open(); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("borge"), 1000)
	for name, value := range map[string][]byte{
		"config/id":      []byte("2f6a"),
		"config/version": []byte("4"),
		"packs/ab/cdef":  body,
	} {
		if err := through.Store(name, value); err != nil {
			t.Fatalf("Store(%q): %v", name, err)
		}
	}
	if err := through.Close(); err != nil {
		t.Fatal(err)
	}

	direct, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Open(); err != nil {
		t.Fatalf("the local backend cannot open what rclone wrote: %v", err)
	}
	defer direct.Close()

	got, err := direct.Load("packs/ab/cdef", 0, -1)
	if err != nil {
		t.Fatalf("Load through the local backend: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("the object read back through the filesystem is %d bytes, want %d", len(got), len(body))
	}
	entries, err := direct.List("config")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "id,version" {
		t.Errorf("the local backend lists %v under config/", names)
	}
}

// TestRcloneLoadsATailThatNeedsTheSize covers the branch the small cases do not: when the
// part of the tail being thrown away is larger than the threshold, the backend asks rclone
// for the object's size so it can send an absolute range instead.
func TestRcloneLoadsATailThatNeedsTheSize(t *testing.T) {
	b, _ := newRcloneForTest(t)
	body := make([]byte, 4000)
	for i := range body {
		body[i] = byte(i)
	}
	if err := b.Store("packs/ab/cdef", body); err != nil {
		t.Fatal(err)
	}
	// 2000 bytes back from the end, 4 of them: 1996 bytes would be thrown away, which is
	// past the threshold, so this goes the other way round.
	got, err := b.Load("packs/ab/cdef", -2000, 4)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, body[2000:2004]) {
		t.Errorf("Load(-2000, 4) = %v, want %v", got, body[2000:2004])
	}
}

// TestRcloneStopsItsServer: the child process is borge's responsibility, and a backup that
// ran and left an rclone behind holding the user's storage credentials would be a leak that
// nothing else cleans up.
func TestRcloneStopsItsServer(t *testing.T) {
	requireRclone(t)
	b, err := NewRclone(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Create(); err != nil {
		t.Fatal(err)
	}
	if err := b.Open(); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(b.baseURL, "http://"), "/")
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("the server is not listening while the backend is open: %v", err)
	}
	conn.Close()

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("the rclone server is still listening after Close")
	}
	// And the lifecycle is the same one every backend has.
	if err := b.Close(); !errors.Is(err, ErrMustBeOpen) {
		t.Errorf("closing twice returned %v, want ErrMustBeOpen", err)
	}
	if _, err := b.Load("config/id", 0, -1); !errors.Is(err, ErrMustBeOpen) {
		t.Errorf("Load on a closed backend returned %v, want ErrMustBeOpen", err)
	}
}

// TestRcloneCreateRefusesANonEmptyRemote, as every backend does: a repository must not be
// created on top of files that are already there.
func TestRcloneCreateRefusesANonEmptyRemote(t *testing.T) {
	requireRclone(t)
	dir := filepath.Join(t.TempDir(), "remote")

	first, err := NewRclone(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Create(); err != nil {
		t.Fatal(err)
	}
	if err := first.Open(); err != nil {
		t.Fatal(err)
	}
	if err := first.Store("config/id", []byte("2f6a")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := NewRclone(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Create(); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("Create over an existing store returned %v, want ErrAlreadyExists", err)
	}
}

// TestRcloneNeedsRclone: the error when the binary is missing names the tool, rather than
// saying the repository does not exist.
func TestRcloneNeedsRclone(t *testing.T) {
	t.Setenv("RCLONE_BINARY", "rclone-that-is-not-installed")
	_, err := NewRclone("/tmp/whatever")
	if err == nil {
		t.Fatal("NewRclone succeeded with no rclone binary")
	}
	if !strings.Contains(err.Error(), "rclone") {
		t.Errorf("the error does not name rclone: %v", err)
	}
	if errors.Is(err, ErrDoesNotExist) {
		t.Error("a missing rclone was reported as a missing repository")
	}
}
