// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve, from the server's side.
//
// The client half of a rest:// repository needs a built binary, so the two ends meet in
// tests/interop. What is decided here is everything the command decides before a byte of
// protocol is exchanged: which mode it is in, which repository it will serve, what a
// client is allowed to do to it - and that its stdout carries nothing but the protocol.

// serveRequest runs "borge serve" with args, feeding it one raw HTTP request, and returns
// the parsed response along with the exit code and stderr.
func serveRequest(t *testing.T, args []string, request string) (*http.Response, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := &Env{
		Stdin:  strings.NewReader(request),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) (string, bool) { return "", false },
	}
	code := Run(env, append([]string{"serve"}, args...))
	if stdout.Len() == 0 {
		return nil, stderr.String(), code
	}
	// Parsing is itself an assertion: anything printed to stdout that is not part of the
	// protocol makes this fail, which is the point of the last test in this file.
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(stdout.Bytes())), nil)
	if err != nil {
		t.Fatalf("the server's stdout is not an HTTP response: %v\n%q", err, stdout.String())
	}
	return resp, stderr.String(), code
}

// restRequest builds one raw request in the protocol's shape.
func restRequest(method, target string, body string) string {
	return fmt.Sprintf("%s %s HTTP/1.1\r\nHost: stdio-backend\r\n"+
		"Accept: application/vnd.x.borgstore.rest.v1\r\n"+
		"Content-Length: %d\r\nConnection: keep-alive\r\n\r\n%s",
		method, target, len(body), body)
}

// TestServeRefusesTheLegacyMode: "borge serve" with no --rest is borg 1.x's protocol, and
// that is a non-goal rather than a gap - the message has to say which.
func TestServeRefusesTheLegacyMode(t *testing.T) {
	_, stderr, code := serveRequest(t, nil, "")
	if code == ExitOK {
		t.Fatal("borge serve without --rest reported success")
	}
	for _, want := range []string{"borg 1.x", "§0.6", "--rest"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
}

func TestServeRestNeedsABackend(t *testing.T) {
	_, stderr, code := serveRequest(t, []string{"--rest"}, "")
	if code == ExitOK {
		t.Fatal("borge serve --rest with no --backend reported success")
	}
	if !strings.Contains(stderr, "--backend FILE:") {
		t.Errorf("the error does not say what is missing:\n%s", stderr)
	}
	// And a backend that is not a FILE: one is refused rather than half-understood.
	_, stderr, code = serveRequest(t, []string{"--rest", "--backend", "sftp://host/repo"}, "")
	if code == ExitOK || !strings.Contains(stderr, "FILE:") {
		t.Errorf("a non-FILE backend was accepted or badly explained (%d):\n%s", code, stderr)
	}
}

// TestServeRestrictions: --restrict-to-path allows a subdirectory and --restrict-to-repository
// does not, which is the whole difference between them.
//
// This is what stands between an ssh key that may run "borge serve" and every repository on
// the machine, so both halves are checked: that the allowed path is served, and that the
// one next to it is not.
func TestServeRestrictions(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	inside := filepath.Join(allowed, "deeper", "repo")
	beside := filepath.Join(base, "beside", "repo")
	for _, dir := range []string{inside, beside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	create := restRequest("POST", "/?cmd=create", "")

	for _, c := range []struct {
		name    string
		args    []string
		path    string
		allowed bool
	}{
		{"a subdirectory of --restrict-to-path", []string{"--restrict-to-path", allowed}, inside, true},
		{"outside --restrict-to-path", []string{"--restrict-to-path", allowed}, beside, false},
		{"exactly --restrict-to-repository", []string{"--restrict-to-repository", inside}, inside, true},
		{"a subdirectory of --restrict-to-repository", []string{"--restrict-to-repository", allowed}, inside, false},
		{"no restriction at all", nil, beside, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"--rest", "--backend", "FILE:" + c.path}, c.args...)
			resp, stderr, code := serveRequest(t, args, create)
			if c.allowed {
				if code != ExitOK {
					t.Fatalf("serving %s was refused: %s", c.path, stderr)
				}
				if resp == nil || resp.StatusCode != http.StatusOK {
					t.Errorf("the client got %v rather than a created repository", resp)
				}
				return
			}
			if code == ExitOK {
				t.Fatalf("serving %s was allowed", c.path)
			}
			if !strings.Contains(stderr, "not allowed") {
				t.Errorf("the refusal does not say the path is not allowed:\n%s", stderr)
			}
			if resp != nil {
				t.Errorf("a refused path still answered the client: %v", resp.StatusCode)
			}
		})
	}
}

// TestServePermissionsAreEnforced: --permissions read-only means a client cannot write,
// and the refusal reaches it as the protocol's own "forbidden" rather than as a crash.
func TestServePermissionsAreEnforced(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "config", "id"), []byte("2f6a"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reading is allowed...
	args := []string{"--rest", "--backend", "FILE:" + repo, "--permissions", "read-only"}
	resp, stderr, code := serveRequest(t, args, restRequest("GET", "/config/id", ""))
	if code != ExitOK || resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a read-only server refused a read (%d, %v):\n%s", code, resp, stderr)
	}
	// ... and writing is not.
	resp, stderr, code = serveRequest(t, args, restRequest("POST", "/config/id", "new"))
	if code != ExitOK {
		t.Fatalf("the server exited %d instead of answering:\n%s", code, stderr)
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a write to a read-only server was answered %v, want 403", resp)
	}
	// The object is unchanged, which is the fact the status code is about.
	got, err := os.ReadFile(filepath.Join(repo, "config", "id"))
	if err != nil || string(got) != "2f6a" {
		t.Errorf("the object was written anyway: %q, %v", got, err)
	}

	// An unknown mode is refused by name, with borg's own message.
	_, stderr, code = serveRequest(t, []string{"--rest", "--backend", "FILE:" + repo,
		"--permissions", "read-write"}, "")
	if code == ExitOK || !strings.Contains(stderr, "should be one of") {
		t.Errorf("an unknown permission mode was accepted or badly explained:\n%s", stderr)
	}
}

// TestServeTakesPermissionsFromTheEnvironment, as borg takes them from
// BORG_REPO_PERMISSIONS when the option is absent.
func TestServeTakesPermissionsFromTheEnvironment(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := &Env{
		Stdin:  strings.NewReader(restRequest("POST", "/config/id", "new")),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) (string, bool) {
			if name == "BORGE_REPO_PERMISSIONS" {
				return "read-only", true
			}
			return "", false
		},
	}
	if code := Run(env, []string{"serve", "--rest", "--backend", "FILE:" + repo}); code != ExitOK {
		t.Fatalf("serve exited %d:\n%s", code, stderr.String())
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(stdout.Bytes())), nil)
	if err != nil {
		t.Fatalf("the response did not parse: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("with BORGE_REPO_PERMISSIONS=read-only a write was answered %d, want 403",
			resp.StatusCode)
	}
}

// TestServeKeepsStdoutForTheProtocol: nothing but HTTP goes to stdout.
//
// A stray line - a warning, a progress message - would corrupt the stream, and the client's
// error would be a parse failure a long way from the cause. The check is that the first
// bytes on stdout are the status line and that the whole stream parses as exactly one
// response with nothing left over.
func TestServeKeepsStdoutForTheProtocol(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := &Env{
		Stdin:  strings.NewReader(restRequest("POST", "/?cmd=create", "")),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) (string, bool) { return "", false },
	}
	if code := Run(env, []string{"serve", "--rest", "--backend", "FILE:" + repo}); code != ExitOK {
		t.Fatalf("serve exited %d:\n%s", code, stderr.String())
	}
	if !bytes.HasPrefix(stdout.Bytes(), []byte("HTTP/1.1 ")) {
		t.Errorf("stdout does not begin with a status line: %q", firstLine(stdout.String()))
	}
	reader := bufio.NewReader(bytes.NewReader(stdout.Bytes()))
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("stdout is not one HTTP response: %v", err)
	}
	resp.Body.Close()
	if left, _ := reader.Peek(1); len(left) != 0 {
		t.Errorf("something followed the response on stdout: %q", stdout.String())
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
