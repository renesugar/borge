// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/location"
)

// The REST client and the REST server, against each other and against the trap in the
// middle of them.
//
// Both ends are borge's here. That is deliberately not the whole story - the point of this
// protocol is that the other end may be borg or borgstore, and the interop tests cover
// that - but it is what makes the failures legible: a mismatch found here is in one of two
// files, not in three programs.

// newRESTForTest returns a REST client talking to a REST server over a real HTTP
// connection, and the directory the server is serving.
func newRESTForTest(t *testing.T) (Backend, planter) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote")
	served, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := served.Create(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewRESTServer(served))
	t.Cleanup(server.Close)

	client, err := NewRESTOverHTTP(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, localPlanter(dir)
}

// TestRESTResponsesCarryContentLength: every response says how long its body is, and none
// of them uses chunked transfer encoding.
//
// net/http decides between the two by itself and picks chunked for anything it cannot
// buffer - about 4 KB. borge's own client would not care, because net/http reads chunked
// responses too; the client that would is borgstore's, which reads exactly Content-Length
// bytes and has never heard of chunked encoding. So this is a requirement borge cannot
// verify by talking to itself, and asserting it on the round trip proves nothing: the
// assertion has to be on the bytes on the wire. The interop rows then check the same thing
// from the other side, with borg doing the reading.
func TestRESTResponsesCarryContentLength(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	served, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := served.Create(); err != nil {
		t.Fatal(err)
	}
	if err := served.Open(); err != nil {
		t.Fatal(err)
	}
	// A megabyte, which is far past anything net/http will buffer.
	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i * 31)
	}
	if err := served.Store("packs/ab/cdef", body); err != nil {
		t.Fatal(err)
	}
	if err := served.Close(); err != nil {
		t.Fatal(err)
	}

	toServer, fromTest := io.Pipe()
	toTest, fromServer := io.Pipe()
	go func() {
		_ = NewRESTServer(served).ServeStdio(toServer, fromServer)
		_ = fromServer.Close()
	}()

	request := "GET /packs/ab/cdef HTTP/1.1\r\nHost: stdio-backend\r\n" +
		"Accept: " + restContentType + "\r\nConnection: keep-alive\r\n\r\n"
	if _, err := io.WriteString(fromTest, request); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(toTest)
	var head []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the response head: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		head = append(head, line)
	}
	joined := strings.Join(head, "\n")
	if strings.Contains(strings.ToLower(joined), "transfer-encoding: chunked") {
		t.Errorf("a megabyte was sent with chunked transfer encoding, which borgstore's "+
			"client cannot read:\n%s", joined)
	}
	var length int
	for _, line := range head {
		if value, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			length, _ = strconv.Atoi(value)
		}
	}
	if length != len(body) {
		t.Errorf("Content-Length is %d for a body of %d bytes:\n%s", length, len(body), joined)
	}
	// And the body really is that long, read by count as the other client reads it.
	got := make([]byte, length)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("reading %d bytes of body: %v", length, err)
	}
	if !bytes.Equal(got, body) {
		t.Error("the body read by Content-Length is not what was stored")
	}
	_ = fromTest.Close()
}

// TestRESTLargeObjectRoundTrips over the client, including the range read a pack reader
// actually does.
func TestRESTLargeObjectRoundTrips(t *testing.T) {
	client, _ := newRESTForTest(t)

	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i * 31)
	}
	if err := client.Store("packs/ab/cdef", body); err != nil {
		t.Fatalf("Store of a megabyte: %v", err)
	}
	got, err := client.Load("packs/ab/cdef", 0, -1)
	if err != nil {
		t.Fatalf("Load of a megabyte: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("a megabyte came back as %d bytes", len(got))
	}
	got, err = client.Load("packs/ab/cdef", 1000, 49)
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	if !bytes.Equal(got, body[1000:1049]) {
		t.Errorf("the range read returned the wrong 49 bytes")
	}
}

// TestRESTContentHashIsChecked: the client sends the hash of what it sent, and the server
// refuses an object whose bytes changed on the way.
func TestRESTContentHashIsChecked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	served, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := served.Create(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewRESTServer(served))
	defer server.Close()

	client, err := NewRESTOverHTTP(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Open(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// A correct hash goes through; a wrong one is refused. The second half is what the
	// check is for, and it needs the header to be wrong rather than the body.
	if err := client.Store("config/id", []byte("2f6a")); err != nil {
		t.Fatalf("Store with a correct hash: %v", err)
	}
	status, body, _, err := client.request("POST", "config/id", nil, []byte("2f6a"),
		map[string]string{"X-Content-hash-sha256": strings.Repeat("00", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if status != 422 {
		t.Errorf("a wrong content hash was answered with %d, want 422", status)
	}
	if !strings.Contains(string(body), "retry") {
		t.Errorf("the refusal does not suggest a retry: %q", body)
	}
}

// TestRESTErrorsMapBackToBorgesOwn: every status the protocol uses has to arrive as the
// error the rest of borge branches on, or a missing object becomes a failed backup.
func TestRESTErrorsMapBackToBorgesOwn(t *testing.T) {
	backend, _ := newRESTForTest(t)
	client := backend.(*REST)

	if _, err := client.Load("config/absent", 0, -1); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Load of a missing object gave %v, want ErrObjectNotFound", err)
	}
	if _, err := client.List("archives"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("List of a missing directory gave %v, want ErrObjectNotFound", err)
	}
	// Creating over an existing store: the server answers 409 and the client has to turn
	// that back into ErrAlreadyExists rather than a generic failure.
	if err := client.Store("config/id", []byte("x")); err != nil {
		t.Fatal(err)
	}
	closed := client
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.Create(); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("Create over an existing store gave %v, want ErrAlreadyExists", err)
	}
	// And the lifecycle errors, which travel as 412 with the words the client looks for.
	if _, err := closed.Load("config/id", 0, -1); !errors.Is(err, ErrMustBeOpen) {
		t.Errorf("Load on a closed client gave %v, want ErrMustBeOpen", err)
	}
}

// TestRESTRefusesAnotherProtocol: a server that does not send the version marker is not
// this protocol, and saying so beats failing on the first parse.
func TestRESTRefusesAnotherProtocol(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	served, _ := NewPosixFS(dir, nil)
	if err := served.Create(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewRESTServer(served))
	defer server.Close()

	client, err := NewRESTOverHTTP(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Open(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The client always sends Accept; strip it and the server must refuse.
	status, _, _, err := client.request("GET", "config/id", nil, nil, map[string]string{"Accept": "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if status != 406 {
		t.Errorf("a request without the protocol's Accept header was answered %d, want 406", status)
	}
}

// ---------------------------------------------------------------- stdio

// The stdio transport is the one borg uses, so it is tested as a process rather than as a
// function: this test binary re-executes itself as the server, which exercises the pipes,
// the framing and the keep-alive across many requests on one connection.

const helperEnv = "BORGE_TEST_REST_SERVER_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(helperEnv); dir != "" {
		backend, err := NewPosixFS(dir, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := NewRESTServer(backend).ServeStdio(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newRESTStdioForTest returns a client talking to this test binary, running as a server.
func newRESTStdioForTest(t *testing.T) (Backend, planter) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote")
	served, err := NewPosixFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := served.Create(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewRESTOverStdio([]string{self})
	if err != nil {
		t.Fatal(err)
	}
	// The child is this binary with the helper variable set, which is how it knows to be
	// a server instead of a test run.
	client.commandEnv = []string{helperEnv + "=" + dir}
	_ = served
	if err := client.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, localPlanter(dir)
}

// TestRESTStdioCarriesManyRequestsOnOneConnection: the transport has to be keep-alive, or
// every object would cost a process.
func TestRESTStdioCarriesManyRequestsOnOneConnection(t *testing.T) {
	client, _ := newRESTStdioForTest(t)
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("archives/%02x", i)
		if err := client.Store(name, []byte(name)); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}
	entries, err := client.List("archives")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 50 {
		t.Errorf("50 objects stored over one connection, %d listed", len(entries))
	}
	got, err := client.Load("archives/00", 0, -1)
	if err != nil || string(got) != "archives/00" {
		t.Errorf("Load after 50 requests: %q, %v", got, err)
	}
}

// TestRESTStdioReportsWhatTheServerSaid: when the server cannot start, the client's error
// has to carry the server's own message, or the user is left with "the connection closed".
func TestRESTStdioReportsWhatTheServerSaid(t *testing.T) {
	client, err := NewRESTOverStdio([]string{"sh", "-c", "echo 'no such repository' >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, loadErr := client.Load("config/id", 0, -1)
	if loadErr == nil {
		t.Fatal("a server that exits immediately reported no error")
	}
	if !strings.Contains(loadErr.Error(), "no such repository") {
		t.Errorf("the error does not carry what the server said: %v", loadErr)
	}
	_ = client.Close()
}

// TestRESTServeCommandMatchesBorgs: the command borge builds for a rest:// location has
// the shape borg builds, because a user may point either tool at the same URL.
func TestRESTServeCommandMatchesBorgs(t *testing.T) {
	local := mustParse(t, "rest:////srv/repo")
	command := RESTServeCommand(local)
	if len(command) != 5 || command[1] != "serve" || command[2] != "--rest" ||
		command[3] != "--backend" || command[4] != "FILE:/srv/repo" {
		t.Errorf("the local serve command is %v", command)
	}

	t.Setenv("BORGSTORE_RSH", "")
	t.Setenv("BORGE_REMOTE_PATH", "/opt/borge/bin/borge")
	remote := mustParse(t, "rest://backup@host:2222/srv/repo")
	command = RESTServeCommand(remote)
	joined := strings.Join(command, " ")
	for _, want := range []string{"ssh", "-p 2222", "backup@host",
		"/opt/borge/bin/borge serve --rest --backend FILE:srv/repo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the remote serve command %q does not contain %q", joined, want)
		}
	}
	// And a custom remote shell replaces ssh entirely, options included.
	t.Setenv("BORGSTORE_RSH", "my-rsh -q")
	command = RESTServeCommand(remote)
	if command[0] != "my-rsh" || command[1] != "-q" {
		t.Errorf("BORGSTORE_RSH did not replace the remote shell: %v", command)
	}
}

func TestRESTNeedsSomethingToTalkTo(t *testing.T) {
	if _, err := NewRESTOverStdio(nil); err == nil {
		t.Error("a rest:// client was built with no command to run")
	}
	if _, err := NewRESTOverHTTP("http:///nohost", "", ""); err == nil {
		t.Error("an http:// client was built with no host")
	}
}

// mustParse parses a repository location or fails the test.
func mustParse(t *testing.T, text string) *location.Location {
	t.Helper()
	loc, err := location.Parse(text)
	if err != nil {
		t.Fatalf("parsing %q: %v", text, err)
	}
	return loc
}

// TestRESTDefragBuildsAnObjectFromRanges: the operation borg's compaction runs, checked
// against what it promises - the named spans, in order, under a name that is their hash.
//
// borge's client does not call defrag (nothing above it does), so this drives the request
// the way borg's client builds it. What it is really testing is the server, which is the
// half a borg client depends on.
func TestRESTDefragBuildsAnObjectFromRanges(t *testing.T) {
	backend, _ := newRESTForTest(t)
	client := backend.(*REST)

	pack := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if err := client.Store("packs/ab/cdef", pack); err != nil {
		t.Fatal(err)
	}

	// Keep the first ten bytes and six from the middle, which is the shape compaction
	// produces: the spans between the objects it is dropping.
	sources := [][]any{
		{"packs/ab/cdef", 0, 10},
		{"packs/ab/cdef", 20, 6},
	}
	body, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{
		"cmd":       {"defrag"},
		"algorithm": {"sha256"},
		"namespace": {"packs"},
		"levels":    {"1"},
	}
	status, out, _, err := client.request("POST", "", query, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 {
		t.Fatalf("defrag was answered %d: %s", status, out)
	}

	want := append(append([]byte{}, pack[0:10]...), pack[20:26]...)
	sum := sha256.Sum256(want)
	digest := hex.EncodeToString(sum[:])
	// The name is the hash, inside the namespace, nested one level - all three decided by
	// the client's parameters, because the client has to be able to predict the name.
	wantName := "packs/" + digest[:2] + "/" + digest
	if string(out) != wantName {
		t.Errorf("defrag returned the name %q, want %q", out, wantName)
	}
	got, err := client.Load(wantName, 0, -1)
	if err != nil {
		t.Fatalf("loading what defrag wrote: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("defrag wrote %q, want %q", got, want)
	}
}

// TestRESTDefragRefusesAShortRead: a span that runs past the end of the object means the
// pack is truncated. That has to be its own answer - the client turns 416 into "run borg
// check", where a 500 or a silently short object would produce a corrupt new pack.
func TestRESTDefragRefusesAShortRead(t *testing.T) {
	backend, _ := newRESTForTest(t)
	client := backend.(*REST)

	if err := client.Store("packs/ab/cdef", []byte("short")); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([][]any{{"packs/ab/cdef", 0, 1000}})
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"cmd": {"defrag"}, "algorithm": {"sha256"}, "namespace": {"packs"}}
	status, out, _, err := client.request("POST", "", query, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != 416 {
		t.Errorf("a span past the end of the object was answered %d, want 416: %s", status, out)
	}
	if err := statusError(status, out, "defrag"); !errors.Is(err, ErrReadRange) {
		t.Errorf("the client turns that answer into %v, want ErrReadRange", err)
	}
}
