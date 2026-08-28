// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of StdioSession and ssh_cmd in borgstore/backends/rest.py
// (borgstore 0.6.1, BSD 3-Clause, Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/placeholders"
	"github.com/renesugar/borge/internal/version"
)

// HTTP over a child process's stdin and stdout.
//
// The server is a program this process starts - "borge serve --rest" here, or an ssh that
// runs one on another machine - and the "connection" is the pair of pipes to it. There is
// no socket and no port, which is the point: the transport's security is ssh's, and a
// repository reachable this way needs no service listening anywhere.
//
// Requests are written with net/http's own request writer and replies read with its
// response reader, so the framing is the standard library's rather than this file's.

// sshAliveInterval and sshAliveCountMax are borgstore's keepalive settings, and the reason
// for them is worth repeating: without them a dead peer leaves a read blocked forever,
// because nothing at this end can tell "the server is slow" from "the network is gone".
// ssh gives up after roughly the product of the two, in seconds.
const (
	sshAliveInterval = 30
	sshAliveCountMax = 3

	// serverSaidWait bounds how long an error message waits for the server's stderr.
	serverSaidWait = 2 * time.Second
)

// stdioTransport is one running server process and the pipes to it.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	closer io.Closer

	// stderr is kept because it is where the server says why it failed: an error the
	// client sees as "the connection closed" is usually explained there. drained closes
	// when the server's stderr has reached EOF, so a message can wait for it.
	stderr  *tailBuffer
	drained chan struct{}

	// mu serialises requests. One pipe pair carries one conversation, so two callers
	// writing at once would interleave two requests into one stream.
	mu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

func newStdioTransport(command []string, extraEnv []string) (*stdioTransport, error) {
	cmd := exec.Command(command[0], command[1:]...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	// Read with a pipe of our own rather than by handing exec a writer. exec's copier is
	// joined only by Wait, so an error raised before Wait could format its message before
	// the server's last words had been copied anywhere - which is how the one message
	// that explains a failed connection came and went depending on timing. With a pipe,
	// "the stderr is complete" is a channel that can be waited on.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	stderr := &tailBuffer{limit: 10}
	// Its own process group, so that a Ctrl-C at a terminal does not tear the server down
	// underneath a request this end is still waiting for - and Pdeathsig, so a borge that
	// is killed outright does not leave it running.
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("store: could not start %q: %w", strings.Join(command, " "), err)
	}
	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		closer:  stdout,
		stderr:  stderr,
		drained: make(chan struct{}),
	}
	go func() {
		_, _ = io.Copy(stderr, stderrPipe)
		close(t.drained)
	}()
	return t, nil
}

// roundTrip writes one request and reads its response.
func (t *stdioTransport) roundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Keep-alive is not decoration here: a new connection would mean a new process, and
	// a backup makes tens of thousands of requests.
	req.Header.Set("Connection", "keep-alive")
	if err := req.Write(t.stdin); err != nil {
		return nil, fmt.Errorf("store: the connection to the repository server broke while sending: %w%s",
			err, t.serverSaid())
	}
	resp, err := http.ReadResponse(t.stdout, req)
	if err != nil {
		return nil, fmt.Errorf("store: the repository server closed the connection: %w%s",
			err, t.serverSaid())
	}
	return resp, nil
}

// serverSaid is the last few lines the server wrote to stderr, for an error message.
//
// It waits, briefly, for the stderr to be complete. A server that failed has usually
// already exited, so its pipe is at EOF and this returns at once; the bound is there for
// the case where it has not, because an error message is not worth hanging for.
func (t *stdioTransport) serverSaid() string {
	select {
	case <-t.drained:
	case <-time.After(serverSaidWait):
	}
	lines := t.stderr.lines()
	if len(lines) == 0 {
		return ""
	}
	return "\nthe server said:\n" + strings.Join(lines, "\n")
}

// Close ends the session: the server sees EOF on its stdin, finishes, and exits.
func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		closeErr := t.stdin.Close()
		done := make(chan error, 1)
		go func() {
			// The stderr pipe has to be read to the end before Wait, which closes it.
			<-t.drained
			done <- t.cmd.Wait()
		}()
		select {
		case err := <-done:
			if err != nil {
				t.closeErr = fmt.Errorf("store: the repository server exited with an error: %w%s",
					err, t.serverSaid())
			}
		case <-time.After(restTimeout):
			_ = t.cmd.Process.Kill()
			<-done
			t.closeErr = fmt.Errorf("store: the repository server did not exit%s", t.serverSaid())
		}
		if t.closeErr == nil && closeErr != nil {
			t.closeErr = fmt.Errorf("store: %w", closeErr)
		}
		_ = t.closer.Close()
	})
	return t.closeErr
}

// tailBuffer keeps the last few lines written to it and throws the rest away.
//
// The server's stderr has to be read continuously or it fills its pipe and blocks the
// server, but a backup can produce a lot of it, and only the end is useful in an error.
type tailBuffer struct {
	mu      sync.Mutex
	limit   int
	partial []byte
	kept    []string
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	for {
		index := bytes.IndexByte(b.partial, '\n')
		if index < 0 {
			break
		}
		b.add(strings.TrimRight(string(b.partial[:index]), "\r"))
		b.partial = b.partial[index+1:]
	}
	return len(p), nil
}

func (b *tailBuffer) add(line string) {
	if line == "" {
		return
	}
	b.kept = append(b.kept, line)
	if len(b.kept) > b.limit {
		b.kept = b.kept[len(b.kept)-b.limit:]
	}
}

func (b *tailBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]string(nil), b.kept...)
	if len(b.partial) > 0 {
		out = append(out, string(b.partial))
	}
	return out
}

// helpText marks a declaration that exists only to carry user-facing documentation.
//
// The doc comment above such a declaration is help text and nothing else: docgen renders
// it into "borge help", so a maintainer's note in it would be printed at a user. Notes
// belong in the code below it.
const helpText = "user-facing help text"

// Several variables here are not borge's own and are read under their own names, because the
// tools on the far end and the libraries in between already use them: BORGSTORE_RSH is
// honoured before BORGE_RSH and BORG_RSH, so a remote shell configured for borg works
// unchanged; BORGSTORE_REST_USERNAME and BORGSTORE_REST_PASSWORD authenticate an http(s)://
// repository whose URL carries no credentials; and RCLONE_BINARY names the rclone to run for
// an rclone: repository.
//
// An s3: or b2: repository takes its credentials from the URL, or from AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN, or from the profile named by AWS_PROFILE in
// AWS_SHARED_CREDENTIALS_FILE (~/.aws/credentials by default) - the order boto3 uses, so a
// machine set up for borg needs no second setup. AWS_REGION and AWS_DEFAULT_REGION choose the
// region, which is part of the signature rather than only an address: signing for the wrong
// one is refused rather than redirected.
//
//borge:doc user
//borge:help environment/remote-not-ours
var _ = helpText

// SSHCommand builds the ssh prefix borgstore builds, honouring the same environment.
//
// BORGSTORE_RSH replaces the whole command, options included, because a user who sets it
// is taking control of how the connection is made. Without it, ssh gets the keepalive
// options and the port.
func SSHCommand(user, host string, port int) []string {
	// borg propagates BORG_RSH into BORGSTORE_RSH before handing over, so a user who set
	// only borg's variable still gets their remote shell; borge honours its own first.
	if rsh := firstEnv("BORGSTORE_RSH", "BORGE_RSH", "BORG_RSH"); rsh != "" {
		args := strings.Fields(rsh)
		return append(args, sshTarget(user, host))
	}
	args := []string{"ssh",
		"-o", "ServerAliveInterval=" + strconv.Itoa(sshAliveInterval),
		"-o", "ServerAliveCountMax=" + strconv.Itoa(sshAliveCountMax),
	}
	if port != 0 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	return append(args, sshTarget(user, host))
}

func sshTarget(user, host string) string {
	if user != "" {
		return user + "@" + host
	}
	return host
}

// RESTServeCommand builds the command that serves a rest:// location.
//
// borg builds the same thing in repository.py's rest_serve_command, and the two must agree
// on the shape: a client that ran "serve" with different arguments would be talking to a
// server it configured differently from the one the user's other tool starts.
//
// Locally that is this very binary - the client is also the server, which is what makes a
// "rest:///path" repository work with nothing installed anywhere. With a host it is ssh,
// then the program named by BORGE_REMOTE_PATH (or BORG_REMOTE_PATH, or "borge").
func RESTServeCommand(loc *location.Location) []string {
	backend := "FILE:" + loc.Path
	if loc.HostName() == "" {
		return append(localServeCommand(), "serve", "--rest", "--backend", backend)
	}
	remote := firstEnv("BORGE_REMOTE_PATH", "BORG_REMOTE_PATH")
	if remote == "" {
		remote = "borge"
	}
	// borg expands placeholders here, so a fleet can point one BORG_REMOTE_PATH at a
	// per-host location. An unexpandable value is left alone rather than failing the
	// command: this is a program name, and the error belongs to exec.
	if expanded, err := placeholders.Default(version.Version).Expand(remote); err == nil {
		remote = expanded
	}
	command := SSHCommand(loc.User, loc.HostName(), loc.Port)
	return append(command, remote, "serve", "--rest", "--backend", backend)
}

// localServeCommand is this executable, so that a locally served repository does not
// depend on borge being installed on the PATH under any particular name.
func localServeCommand() []string {
	if self, err := os.Executable(); err == nil {
		return []string{self}
	}
	return []string{"borge"}
}

// restCredentials finds the username and password for an http(s) repository: in the URL,
// or in the environment, which is borgstore's order and its variable names.
func restCredentials(loc *location.Location) (string, string) {
	if parsed, err := url.Parse(loc.Processed); err == nil && parsed.User != nil {
		password, _ := parsed.User.Password()
		if parsed.User.Username() != "" && password != "" {
			return parsed.User.Username(), password
		}
	}
	return firstEnv("BORGSTORE_REST_USERNAME"), firstEnv("BORGSTORE_REST_PASSWORD")
}

// withoutCredentials strips a user:password@ from a URL, because they travel in the
// Authorization header rather than in the request line.
func withoutCredentials(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User == nil {
		return rawURL
	}
	parsed.User = nil
	return parsed.String()
}

// firstEnv returns the first of these variables that is set and not empty.
func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
