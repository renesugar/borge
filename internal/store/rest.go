// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/backends/rest.py (borgstore 0.6.1, BSD 3-Clause,
// Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The client side of a rest:// repository, and of an http:// or https:// one.
//
// Two transports, one protocol. A "rest://" repository is served by a process this client
// starts - "borge serve --rest" locally, or over ssh when the URL names a host - and the
// HTTP goes over that process's stdin and stdout. An "http(s)://" repository is an
// ordinary web request to a server somebody else is running. Everything above the
// transport is identical, which is why they are one type here as they are one class in
// borgstore.
//
// # What this client will find on the other end
//
// borg's own rest:// client starts "borg serve --rest", so a repository borge reaches this
// way may be served by borg, by borge, or by borgstore's own server. All three are tested
// against (DIVERGENCES #59); the protocol is borgstore's, and none of the three is
// entitled to be lenient about it.

// restTailThreshold is the same optimisation the rclone backend has: a tail read whose
// discarded part is small is cheaper as one over-large request than as a size lookup plus
// an exact one.
const restTailThreshold = 1024

// REST is a Backend that speaks the borgstore REST protocol.
type REST struct {
	// baseURL has no trailing slash; names are appended with one.
	baseURL string

	// transport is how a request gets to the server: a child process over stdio, or the
	// network. It is nil until Open, which is what makes the lifecycle errors work.
	transport restTransport

	// newTransport builds a fresh transport. Open uses it; so do Create and Destroy,
	// which run with the backend closed and need a session of their own.
	newTransport func() (restTransport, error)

	username, password string

	// commandEnv is added to the environment of a stdio server this client starts. It
	// exists for the tests, which run the server as a second copy of the test binary;
	// nothing in borge sets it.
	commandEnv []string
}

// restTransport carries one request and returns the response.
type restTransport interface {
	roundTrip(req *http.Request) (*http.Response, error)
	Close() error
}

// NewRESTOverStdio returns a client that talks to a process it starts.
//
// command is the whole command line, including any ssh prefix: borg builds the same thing
// in repository.py's rest_serve_command, and the two have to agree, because a user may
// point either tool at the same rest:// URL.
func NewRESTOverStdio(command []string) (*REST, error) {
	if len(command) == 0 {
		return nil, errors.New("store: a rest:// repository needs a command to serve it")
	}
	b := &REST{baseURL: "http://stdio-backend"}
	b.newTransport = func() (restTransport, error) { return newStdioTransport(command, b.commandEnv) }
	return b, nil
}

// NewRESTOverHTTP returns a client for a server somebody else is running.
func NewRESTOverHTTP(baseURL, username, password string) (*REST, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("store: %q is not a URL: %w", baseURL, err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("store: %q names no host", baseURL)
	}
	b := &REST{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		username: username,
		password: password,
	}
	b.newTransport = func() (restTransport, error) {
		return &httpTransport{client: &http.Client{Timeout: restTimeout}}, nil
	}
	return b, nil
}

// restTimeout bounds a single request. A backup is long; one request is not.
const restTimeout = 30 * time.Second

func (b *REST) requireOpen() error {
	if b.transport == nil {
		return ErrMustBeOpen
	}
	return nil
}

// Open starts a session: a child process for stdio, an HTTP client otherwise.
func (b *REST) Open() error {
	if b.transport != nil {
		return ErrMustNotBeOpen
	}
	transport, err := b.newTransport()
	if err != nil {
		return err
	}
	b.transport = transport
	return nil
}

// Close ends the session, which for stdio means waiting for the server to exit.
func (b *REST) Close() error {
	if b.transport == nil {
		return ErrMustBeOpen
	}
	transport := b.transport
	b.transport = nil
	return transport.Close()
}

// request sends one request, using the open session or a session of its own.
//
// The second case is Create and Destroy: borgstore calls them with the backend closed, so
// they cannot use the session - but they still need a server, and for stdio that means
// starting one for the single request.
func (b *REST) request(method, target string, query url.Values, body []byte, headers map[string]string) (
	int, []byte, http.Header, error) {

	transport := b.transport
	if transport == nil {
		fresh, err := b.newTransport()
		if err != nil {
			return 0, nil, nil, err
		}
		defer fresh.Close()
		transport = fresh
	}

	full := b.baseURL + "/" + strings.TrimPrefix(target, "/")
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, full, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("store: %w", err)
	}
	// Always explicit, never chunked: the server on the other end reads Content-Length
	// and does not speak chunked transfer encoding.
	req.ContentLength = int64(len(body))
	req.Header.Set("Accept", restContentType)
	if b.username != "" && b.password != "" {
		req.SetBasicAuth(b.username, b.password)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := transport.roundTrip(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("store: reading the reply: %w", err)
	}
	return resp.StatusCode, data, resp.Header, nil
}

// statusError turns a response status into the error the rest of borge knows.
//
// This is the mirror of the server's mapping, and the pair is the protocol: a code that
// one side means differently from the other is a failure reported as the wrong kind.
func statusError(status int, body []byte, name string) error {
	message := strings.TrimSpace(string(body))
	switch status {
	case http.StatusOK, http.StatusPartialContent:
		return nil
	case http.StatusNotFound:
		return &ObjectNotFoundError{Name: name}
	case http.StatusGone:
		return fmt.Errorf("%w: %s", ErrDoesNotExist, message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrAlreadyExists, message)
	case http.StatusPreconditionFailed:
		// borgstore's client decides which state error this is by looking for the words
		// in the message, so borge does the same rather than inventing a header.
		if strings.Contains(message, "must not be open") {
			return ErrMustNotBeOpen
		}
		if strings.Contains(message, "must be open") {
			return ErrMustBeOpen
		}
		return fmt.Errorf("store: %s", message)
	case http.StatusForbidden:
		return &PermissionDeniedError{Name: name}
	case http.StatusInsufficientStorage:
		return fmt.Errorf("store: the repository is over its quota: %s", message)
	case http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("%w: %s", ErrReadRange, message)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("store: the server did not accept %s: %s", name, message)
	case http.StatusNotAcceptable:
		return fmt.Errorf("store: the server does not speak this protocol version (%s): %s",
			restContentType, message)
	case http.StatusUnauthorized:
		return fmt.Errorf("store: the server refused these credentials: %s", message)
	default:
		return fmt.Errorf("store: %s: %d: %s", name, status, message)
	}
}

// Create initialises the repository. The backend is closed while this runs, as borgstore
// requires: creating is not something to do inside a session.
func (b *REST) Create() error {
	if b.transport != nil {
		return ErrMustNotBeOpen
	}
	status, body, _, err := b.request(http.MethodPost, "", url.Values{"cmd": {"create"}}, nil, nil)
	if err != nil {
		return err
	}
	return statusError(status, body, "the repository")
}

// Destroy removes the repository and everything in it.
func (b *REST) Destroy() error {
	if b.transport != nil {
		return ErrMustNotBeOpen
	}
	status, body, _, err := b.request(http.MethodDelete, "", url.Values{"cmd": {"destroy"}}, nil, nil)
	if err != nil {
		return err
	}
	return statusError(status, body, "the repository")
}

// Load reads an object, or a range of one.
func (b *REST) Load(name string, offset, size int64) ([]byte, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}

	var truncateTo int64 = -1
	rangeHeader := ""
	switch {
	case offset < 0 && size >= 0:
		if -offset-size <= restTailThreshold {
			rangeHeader = makeRangeHeader(offset, -1, -1)
			truncateTo = size
		} else {
			info, err := b.Info(name)
			if err != nil {
				return nil, err
			}
			if !info.Exists {
				return nil, &ObjectNotFoundError{Name: name}
			}
			rangeHeader = makeRangeHeader(offset, size, info.Size)
		}
	default:
		rangeHeader = makeRangeHeader(offset, size, -1)
	}
	headers := map[string]string{}
	if rangeHeader != "" {
		headers["Range"] = rangeHeader
	}

	status, body, _, err := b.request(http.MethodGet, name, nil, nil, headers)
	if err != nil {
		return nil, err
	}
	if err := statusError(status, body, name); err != nil {
		return nil, err
	}
	if truncateTo >= 0 && truncateTo < int64(len(body)) {
		body = body[:truncateTo]
	}
	return body, nil
}

// Store writes an object, with the hash of what was sent.
//
// The hash is the client's own check on the transport: the server recomputes it and
// refuses the object if the two disagree, so a corrupted transfer is caught where it can
// still be retried rather than at the next restore.
func (b *REST) Store(name string, value []byte) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	sum := sha256.Sum256(value)
	headers := map[string]string{"X-Content-hash-sha256": hex.EncodeToString(sum[:])}
	status, body, _, err := b.request(http.MethodPost, name, nil, value, headers)
	if err != nil {
		return err
	}
	return statusError(status, body, name)
}

// Delete removes an object permanently.
func (b *REST) Delete(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	status, body, _, err := b.request(http.MethodDelete, name, nil, nil, nil)
	if err != nil {
		return err
	}
	return statusError(status, body, name)
}

// Move renames an object.
func (b *REST) Move(oldName, newName string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(oldName); err != nil {
		return err
	}
	if err := validateName(newName); err != nil {
		return err
	}
	query := url.Values{"cmd": {"move"}, "current": {oldName}, "new": {newName}}
	status, body, _, err := b.request(http.MethodPost, "", query, nil, nil)
	if err != nil {
		return err
	}
	return statusError(status, body, oldName)
}

// Info reports on one name without reading it.
func (b *REST) Info(name string) (ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return ItemInfo{}, err
	}
	if err := validateName(name); err != nil {
		return ItemInfo{}, err
	}
	status, body, headers, err := b.request(http.MethodHead, name, nil, nil, nil)
	if err != nil {
		return ItemInfo{}, err
	}
	if status == http.StatusNotFound {
		// Not an error: a name that is not there is a fact about the store, and every
		// backend reports it the same way.
		return ItemInfo{Name: path.Base(name)}, nil
	}
	if err := statusError(status, body, name); err != nil {
		return ItemInfo{}, err
	}
	size, _ := strconv.ParseInt(headers.Get("Content-Length"), 10, 64)
	return ItemInfo{
		Name:      path.Base(name),
		Exists:    true,
		Directory: headers.Get("X-BorgStore-Is-Directory") == "true",
		Size:      size,
		ATime:     epochTime(headers.Get("X-BorgStore-Atime")),
		MTime:     epochTime(headers.Get("X-BorgStore-Mtime")),
	}, nil
}

// List reports one directory's entries, sorted by name.
//
// The trailing slash is what makes this a listing rather than a read; it is the protocol's
// only overloaded path.
func (b *REST) List(name string) ([]ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	status, body, _, err := b.request(http.MethodGet, name+"/", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := statusError(status, body, name); err != nil {
		return nil, err
	}
	var entries []struct {
		Name      string  `json:"name"`
		Size      int64   `json:"size"`
		Directory bool    `json:"directory"`
		ATime     float64 `json:"atime"`
		MTime     float64 `json:"mtime"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("store: the listing of %q was not JSON: %w", name, err)
	}
	out := make([]ItemInfo, 0, len(entries))
	for _, e := range entries {
		if validateName(e.Name) != nil {
			continue
		}
		out = append(out, ItemInfo{
			Name:      e.Name,
			Exists:    true,
			Directory: e.Directory,
			Size:      e.Size,
			ATime:     epochSeconds(e.ATime),
			MTime:     epochSeconds(e.MTime),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Mkdir creates a directory.
func (b *REST) Mkdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	status, body, _, err := b.request(http.MethodPost, name, url.Values{"cmd": {"mkdir"}}, nil, nil)
	if err != nil {
		return err
	}
	return statusError(status, body, name)
}

// Rmdir removes an empty directory.
func (b *REST) Rmdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	status, body, _, err := b.request(http.MethodDelete, name, url.Values{"cmd": {"rmdir"}}, nil, nil)
	if err != nil {
		return err
	}
	return statusError(status, body, name)
}

// epochTime reads one of the server's float-seconds headers.
func epochTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}
	}
	return epochSeconds(seconds)
}

func epochSeconds(seconds float64) time.Time {
	if seconds == 0 {
		// Zero means "the server does not know", not 1970.
		return time.Time{}
	}
	return time.Unix(0, int64(seconds*1e9))
}

// httpTransport is the plain-network transport: an ordinary HTTP client.
type httpTransport struct{ client *http.Client }

func (t *httpTransport) roundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return resp, nil
}

func (t *httpTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}
