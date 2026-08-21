// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/server/rest.py (borgstore 0.6.1, BSD 3-Clause,
// Copyright (C) 2026 Thomas Waldmann), which is the server "borg serve --rest" runs.
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/crypto"
)

// The server side of a rest:// repository.
//
// borg 2 has no repository protocol of its own, so this is not borg's: it is borgstore's
// REST protocol, and "borg serve --rest" is a thin wrapper that hands borgstore's server a
// FILE: backend. borge's serve is the same shape - what makes it useful is that the client
// on the other end may be borg.
//
// # HTTP over a pipe, with the standard library doing the work
//
// The transport is stdin and stdout, not a socket: a client starts this process (directly
// or through ssh) and writes HTTP/1.1 requests into its stdin. That does not need a
// hand-written parser. net/http serves any net.Conn, so a two-file pipe wrapped as one
// makes http.Server frame requests, handle keep-alive and honour HEAD unaided - which was
// worth measuring, because PORTING_PLAN §11.5 assumed the opposite.
//
// # One thing the standard library gets wrong for this protocol
//
// Go decides between Content-Length and chunked transfer encoding by itself, and picks
// chunked for anything it cannot buffer (over about 4 KB). The client on the other end
// reads exactly Content-Length bytes and knows nothing about chunked encoding, so every
// response here sets Content-Length explicitly. Getting this wrong would work perfectly
// for a small object and hang on a pack.

// restContentType is the protocol's version marker. A request without it is refused with
// 406, which is how a client of another version finds out rather than misbehaving.
const restContentType = "application/vnd.x.borgstore.rest.v1"

// ErrReadRange is a range read that came back short. It is separate from a missing object
// because it means the object is there and truncated - which is a damaged repository, not
// an absent one.
var ErrReadRange = errors.New("store: range read returned less than was asked for")

// RESTServer serves a Backend over the borgstore REST protocol.
type RESTServer struct {
	backend Backend

	// username and password, when set, require HTTP basic authentication. borg's
	// "serve --rest" sets neither: over stdio the client already had to be able to start
	// this process, and over ssh the authentication happened there.
	username, password string
}

// NewRESTServer returns a server for a backend.
func NewRESTServer(backend Backend) *RESTServer { return &RESTServer{backend: backend} }

// SetBasicAuth requires these credentials on every request.
func (s *RESTServer) SetBasicAuth(username, password string) {
	s.username, s.password = username, password
}

// ServeStdio serves requests read from in, answering on out, until in reaches EOF.
//
// This reads and answers requests itself rather than handing the pipe to http.Server, and
// the reason is a client borge has to work with: borg's sends **no Host header**. borgstore
// writes the headers that "requests" prepared, and Host is normally added further down by
// urllib3, which is bypassed here - so the request that arrives is HTTP/1.1 without a Host.
// RFC 7230 requires one and http.Server enforces it, answering "400 Bad Request: missing
// required Host header" to everything borg sends; borgstore's own Python server has no such
// check and accepts it. Being right about the RFC would mean being useless as a server for
// borg, so this uses http.ReadRequest, which is lenient, and writes the responses.
//
// Measured, not deduced: borge talking to borge worked perfectly, because Go's client always
// sends Host. Only the cross-tool test found it (DIVERGENCES #59).
func (s *RESTServer) ServeStdio(in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// The client closed the pipe, which is how a session ends.
				return nil
			}
			return fmt.Errorf("store: could not read a request: %w", err)
		}

		response := &bufferedResponse{header: http.Header{}}
		s.ServeHTTP(response, req)

		// Anything the handler did not read has to go, or the next request would be read
		// out of the middle of this one's body.
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()

		if err := response.writeTo(out, req.Method); err != nil {
			return fmt.Errorf("store: could not answer: %w", err)
		}
		if req.Close {
			return nil
		}
	}
}

// bufferedResponse collects a handler's answer so that it can be written with a correct
// Content-Length and without a body for HEAD.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponse) writeTo(out io.Writer, method string) error {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if w.header.Get("Content-Length") == "" {
		w.header.Set("Content-Length", strconv.Itoa(w.body.Len()))
	}

	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	// Sorted, so that two runs of the same request produce the same bytes - which is what
	// makes a captured exchange comparable.
	names := make([]string, 0, len(w.header))
	for name := range w.header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range w.header[name] {
			fmt.Fprintf(&head, "%s: %s\r\n", name, value)
		}
	}
	head.WriteString("\r\n")
	if _, err := out.Write(head.Bytes()); err != nil {
		return err
	}
	if method == http.MethodHead || w.body.Len() == 0 {
		// HEAD carries the headers of the response it would have had, and no body.
		return flush(out)
	}
	if _, err := out.Write(w.body.Bytes()); err != nil {
		return err
	}
	return flush(out)
}

// flush pushes the answer out, because the client is waiting for it before it sends
// anything else - a buffered response is a deadlock.
func flush(out io.Writer) error {
	if flusher, ok := out.(interface{ Sync() error }); ok {
		// os.Stdout: Sync is the closest thing to a flush, and a failure on a pipe is
		// not interesting.
		_ = flusher.Sync()
	}
	if flusher, ok := out.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}
	return nil
}

// ServeHTTP dispatches one request.
func (s *RESTServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") != restContentType {
		// Not a client of this protocol version - or not a client of this protocol.
		s.sendError(w, http.StatusNotAcceptable,
			"Not Acceptable: unsupported or missing Accept header")
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="BorgStore REST Server"`)
		s.respond(w, http.StatusUnauthorized, []byte("Unauthorized"), "")
		return
	}

	name := strings.Trim(r.URL.Path, "/")
	query := r.URL.Query()
	switch r.Method {
	case http.MethodPost:
		s.post(w, r, name, query)
	case http.MethodDelete:
		s.delete(w, r, name, query)
	case http.MethodHead:
		s.head(w, name)
	case http.MethodGet:
		s.get(w, r, name)
	default:
		s.sendError(w, http.StatusBadRequest, "Bad Request")
	}
}

func (s *RESTServer) authorized(r *http.Request) bool {
	if s.username == "" || s.password == "" {
		return true
	}
	user, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	// Constant time, as borgstore does with secrets.compare_digest: the comparison is
	// against a secret and a timing difference is a way to learn it.
	sameUser := subtle.ConstantTimeCompare([]byte(user), []byte(s.username)) == 1
	samePassword := subtle.ConstantTimeCompare([]byte(password), []byte(s.password)) == 1
	return sameUser && samePassword
}

// withBackend opens the backend for one operation and closes it afterwards.
//
// Per request, as borgstore's server does ("with self.server.backend"). It is the state
// machine the protocol implies: the client's own open and close bracket a session, not a
// connection, so the server must not hold the backend open between requests.
func (s *RESTServer) withBackend(fn func(b Backend) error) error {
	if err := s.backend.Open(); err != nil {
		return err
	}
	defer s.backend.Close()
	return fn(s.backend)
}

// do runs one operation with the backend open and answers OK, or maps the failure.
func (s *RESTServer) do(w http.ResponseWriter, fn func(b Backend) error) {
	if err := s.withBackend(fn); err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, http.StatusOK, nil, "")
}

func (s *RESTServer) post(w http.ResponseWriter, r *http.Request, name string, query map[string][]string) {
	switch first(query, "cmd") {
	case "create":
		// create and destroy are the two that must run with the backend *closed*.
		if err := s.backend.Create(); err != nil {
			s.fail(w, err)
			return
		}
		s.respond(w, http.StatusOK, nil, "")
	case "move":
		current, next := first(query, "current"), first(query, "new")
		if current == "" || next == "" {
			s.sendError(w, http.StatusBadRequest, "Missing current or new name for move")
			return
		}
		s.do(w, func(b Backend) error { return b.Move(current, next) })
	case "mkdir":
		s.do(w, func(b Backend) error { return b.Mkdir(name) })
	case "hash":
		if name == "" {
			s.sendError(w, http.StatusBadRequest, "Missing name for hash")
			return
		}
		algorithm := first(query, "algorithm")
		if algorithm == "" {
			algorithm = "sha256"
		}
		var digest string
		if err := s.withBackend(func(b Backend) error {
			var err error
			digest, err = hashObject(b, name, algorithm)
			return err
		}); err != nil {
			s.fail(w, err)
			return
		}
		s.respond(w, http.StatusOK, []byte(digest), "text/plain")
	case "quota":
		// borge tracks no quota, and borgstore's answer for a backend that does not is
		// this one: -1 for both, meaning "not set / not tracked".
		body, err := json.Marshal(map[string]int64{"limit": -1, "usage": -1})
		if err != nil {
			s.fail(w, err)
			return
		}
		s.respond(w, http.StatusOK, body, "application/json")
	case "defrag":
		s.defrag(w, r, query)
	default:
		if name == "" {
			s.sendError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		s.store(w, r, name)
	}
}

// store writes an object, checking the content hash the client sent with it.
//
// The check is the client's own: it hashes what it sent, and a mismatch means the bytes
// changed on the way. 422 tells the client to send it again, which is worth having on a
// link that a backup runs over for hours.
func (s *RESTServer) store(w http.ResponseWriter, r *http.Request, name string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	if expected := r.Header.Get("X-Content-hash-sha256"); expected != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != expected {
			s.respond(w, http.StatusUnprocessableEntity,
				[]byte("Content hash verification failed, please retry"), "")
			return
		}
	}
	s.do(w, func(b Backend) error { return b.Store(name, data) })
}

func (s *RESTServer) delete(w http.ResponseWriter, r *http.Request, name string, query map[string][]string) {
	switch first(query, "cmd") {
	case "rmdir":
		s.do(w, func(b Backend) error { return b.Rmdir(name) })
	case "destroy":
		if err := s.backend.Destroy(); err != nil {
			s.fail(w, err)
			return
		}
		s.respond(w, http.StatusOK, nil, "")
	default:
		if name == "" {
			s.sendError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		s.do(w, func(b Backend) error { return b.Delete(name) })
	}
}

// head answers with an object's metadata and no body.
func (s *RESTServer) head(w http.ResponseWriter, name string) {
	var info ItemInfo
	if err := s.withBackend(func(b Backend) error {
		var err error
		info, err = b.Info(name)
		return err
	}); err != nil {
		s.fail(w, err)
		return
	}
	if !info.Exists {
		// borgstore's server raises ObjectNotFound here rather than answering 200 with
		// "exists: false", and the client reads the status rather than a header.
		s.sendError(w, http.StatusNotFound, "object not found: "+name)
		return
	}
	w.Header().Set("X-BorgStore-Is-Directory", boolText(info.Directory))
	w.Header().Set("X-BorgStore-Atime", epochFloat(info.ATime))
	w.Header().Set("X-BorgStore-Mtime", epochFloat(info.MTime))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.WriteHeader(http.StatusOK)
}

// get is both "read this object" and "list this directory": the trailing slash decides,
// which is borgstore's convention rather than a choice made here.
func (s *RESTServer) get(w http.ResponseWriter, r *http.Request, name string) {
	if strings.HasSuffix(r.URL.Path, "/") {
		var entries []ItemInfo
		if err := s.withBackend(func(b Backend) error {
			var err error
			entries, err = b.List(name)
			return err
		}); err != nil {
			s.fail(w, err)
			return
		}
		listing := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			listing = append(listing, map[string]any{
				"name":      e.Name,
				"size":      e.Size,
				"directory": e.Directory,
				"atime":     epochNumber(e.ATime),
				"mtime":     epochNumber(e.MTime),
			})
		}
		body, err := json.MarshalIndent(listing, "", "  ")
		if err != nil {
			s.fail(w, err)
			return
		}
		s.respond(w, http.StatusOK, body, "application/json")
		return
	}

	if name == "" {
		s.sendError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	offset, size := int64(0), int64(-1)
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		offset, size = parseRangeHeader(rangeHeader)
	}
	var data []byte
	if err := s.withBackend(func(b Backend) error {
		var err error
		data, err = b.Load(name, offset, size)
		return err
	}); err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if rangeHeader != "" {
		status = http.StatusPartialContent
	}
	s.respond(w, status, data, "application/octet-stream")
}

// defrag builds one object out of byte ranges of others.
//
// This is what borg's compaction uses over a remote backend: rather than downloading a
// pack, cutting the dead objects out of it and uploading it again, it names the spans to
// keep and the server does the copying. So a borge that could not do this would be a
// server borg can back up to but not compact.
func (s *RESTServer) defrag(w http.ResponseWriter, r *http.Request, query map[string][]string) {
	target := first(query, "target")
	algorithm := first(query, "algorithm")
	namespace := first(query, "namespace")
	levels := 0
	if v := first(query, "levels"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			s.sendError(w, http.StatusBadRequest, "levels must be a number: "+v)
			return
		}
		levels = parsed
	}
	if target == "" && algorithm == "" {
		s.sendError(w, http.StatusBadRequest, "Missing target or algorithm for defrag")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	sources, err := parseDefragSources(body)
	if err != nil {
		s.sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	err = s.withBackend(func(b Backend) error {
		var data []byte
		for _, source := range sources {
			part, err := b.Load(source.Name, source.Offset, source.Size)
			if err != nil {
				return err
			}
			if int64(len(part)) != source.Size {
				// The pack is shorter than the index believes. Reported as its own
				// error, because "the object is truncated" and "the object is missing"
				// need different answers from the client.
				return fmt.Errorf("%w: %s (asked for %d bytes at offset %d, got %d)",
					ErrReadRange, source.Name, source.Size, source.Offset, len(part))
			}
			data = append(data, part...)
		}
		if target == "" {
			digest, err := hashBytes(data, algorithm)
			if err != nil {
				return err
			}
			target = digest
			if namespace != "" {
				target = strings.TrimSuffix(namespace, "/") + "/" + target
			}
			if levels > 0 {
				target = Nest(target, levels, "")
			}
		}
		return b.Store(target, data)
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, http.StatusOK, []byte(target), "text/plain")
}

// defragSource is one span to copy: a name and a byte range within it.
type defragSource struct {
	Name   string
	Offset int64
	Size   int64
}

// parseDefragSources reads the client's list of [name, offset, size] triples.
func parseDefragSources(body []byte) ([]defragSource, error) {
	// The name is a string and the other two are numbers, so a triple cannot be decoded
	// into one Go type directly; decode loosely and check the shape.
	var loose [][]any
	if err := json.Unmarshal(body, &loose); err != nil {
		return nil, fmt.Errorf("defrag sources are not a JSON list: %w", err)
	}
	out := make([]defragSource, 0, len(loose))
	for i, entry := range loose {
		if len(entry) != 3 {
			return nil, fmt.Errorf("defrag source %d has %d fields, want name, offset and size", i, len(entry))
		}
		name, ok := entry[0].(string)
		if !ok {
			return nil, fmt.Errorf("defrag source %d does not start with a name", i)
		}
		offset, ok1 := entry[1].(float64)
		size, ok2 := entry[2].(float64)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("defrag source %d has a non-numeric offset or size", i)
		}
		out = append(out, defragSource{Name: name, Offset: int64(offset), Size: int64(size)})
	}
	return out, nil
}

// hashObject is borgstore's default hash implementation: read the object and hash it.
//
// borg uses it to check a pack without downloading it - the pack's name *is* the hash of
// its contents, so "is this pack intact" is one request whose answer is a hex string.
func hashObject(b Backend, name, algorithm string) (string, error) {
	data, err := b.Load(name, 0, -1)
	if err != nil {
		return "", err
	}
	return hashBytes(data, algorithm)
}

func hashBytes(data []byte, algorithm string) (string, error) {
	switch algorithm {
	case "", "sha256":
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]), nil
	case "blake3":
		return hex.EncodeToString(crypto.Blake3Unkeyed(data)), nil
	default:
		// Not "unsupported by this server": borgstore accepts any hashlib name, so a
		// client asking for one borge does not have needs to know which it was.
		return "", fmt.Errorf("store: unsupported hash algorithm %q", algorithm)
	}
}

// respond writes one complete response, always with an explicit Content-Length.
func (s *RESTServer) respond(w http.ResponseWriter, status int, data []byte, contentType string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if len(data) > 0 {
		_, _ = w.Write(data)
	}
}

// sendError answers with a status and a plain-text message.
func (s *RESTServer) sendError(w http.ResponseWriter, status int, message string) {
	s.respond(w, status, []byte(message), "text/plain")
}

// fail maps a backend error onto the status code the protocol gives it.
//
// The mapping is the protocol: the client turns these back into its own errors, so a
// wrong code here becomes a wrong error there - a missing object reported as 500 would
// stop a backup that should have carried on.
func (s *RESTServer) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrObjectNotFound):
		s.sendError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrDoesNotExist):
		s.sendError(w, http.StatusGone, err.Error())
	case errors.Is(err, ErrAlreadyExists):
		s.sendError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrMustBeOpen):
		// The words matter: the client decides which of the two state errors this is by
		// looking for them in the message, so borge's own wording is not enough.
		s.sendError(w, http.StatusPreconditionFailed, "backend must be open: "+err.Error())
	case errors.Is(err, ErrMustNotBeOpen):
		s.sendError(w, http.StatusPreconditionFailed, "backend must not be open: "+err.Error())
	case errors.Is(err, ErrPermissionDenied):
		s.sendError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrReadRange):
		s.sendError(w, http.StatusRequestedRangeNotSatisfiable, err.Error())
	default:
		s.sendError(w, http.StatusInternalServerError, err.Error())
	}
}

func first(query map[string][]string, key string) string {
	if values := query[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// epochFloat formats a time the way Python's str(float) does for these headers. A zero
// time is "0", which is what borgstore's client reads as "not known".
func epochFloat(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 6, 64)
}

func epochNumber(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// parseRangeHeader is borgstore's parse_range_header: it understands one byte range, and
// anything it does not understand means "the whole object" rather than an error.
func parseRangeHeader(header string) (offset, size int64) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, -1
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.HasPrefix(spec, "-") {
		// bytes=-N: the last N bytes, which this store spells as a negative offset.
		value, err := strconv.ParseInt(spec, 10, 64)
		if err != nil {
			return 0, -1
		}
		return value, -1
	}
	start, end, found := strings.Cut(spec, "-")
	if !found {
		return 0, -1
	}
	from, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return 0, -1
	}
	if end == "" {
		return from, -1
	}
	to, err := strconv.ParseInt(end, 10, 64)
	if err != nil {
		return 0, -1
	}
	return from, to - from + 1
}
