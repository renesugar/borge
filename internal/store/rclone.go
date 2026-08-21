// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/backends/rclone.py (borgstore 0.6.1, BSD 3-Clause,
// Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The rclone backend, which is not a protocol borge speaks but a program borge drives.
//
// There is no borge on the far end of an "rclone:" repository and no borg either: the
// client starts "rclone rcd" as a child process, talks to it over its remote-control HTTP
// API on a loopback port, and rclone reaches the actual storage - S3, Backblaze, Google
// Drive, a WebDAV server, seventy others. That is the whole appeal, and it is why this is
// the first remote backend borge ports: it needs no new dependency, no credentials and no
// network, because "rclone:/tmp/somewhere" is a perfectly good rclone remote.
//
// # What the client and the server agree on
//
// Nothing that borg defines. The wire is rclone's own rc API, so the parts that have to
// match borgstore exactly are the ones a *repository* depends on: which operation each
// store method maps to, how a missing object is recognised, and how a range read is asked
// for. Those are pinned by TestRcloneMatchesBorgstore and by the interop tests, which have
// borg read what borge wrote through the same remote.
//
// # The port is random and the password is fresh
//
// borgstore binds a random free port, invents a 32-byte password and passes it to rclone in
// the environment rather than on the command line, where it would be readable in the
// process list by every user on the machine. borge does the same, and adds one thing:
// Pdeathsig, so an rclone left behind by a borge that died is not left behind at all.

const (
	// rcloneHost is loopback and only loopback: the rc API has no transport security, and
	// its password is the only thing between a caller and the user's storage credentials.
	rcloneHost = "127.0.0.1"

	// rcloneTries is borgstore's TRIES: a 500 from rclone is a backend, protocol or
	// network error, and rclone has already retried internally for everything except a
	// streaming transfer. So this retries the transfers and nothing else.
	rcloneTries = 3

	// rcloneStartAttempts bounds the port race. borgstore loops until rclone stays up,
	// which never ends if rclone is broken rather than unlucky.
	rcloneStartAttempts = 10

	// rcloneStopGrace is how long a terminated rclone gets to exit before it is killed.
	rcloneStopGrace = 10 * time.Second

	// rcloneTailThreshold is borgstore's optimisation, kept because it changes the number
	// of round trips: reading the last N bytes of an object needs the object's size to
	// turn a negative offset into a range - unless the part being thrown away is small
	// enough that fetching it costs less than the extra request.
	rcloneTailThreshold = 1024
)

// rcloneMinVersion is borgstore's floor. Below it the rc API lacks operations this backend
// uses, and the failure would arrive as a puzzling 404 rather than as a version error.
var rcloneMinVersion = []int{1, 57, 0}

// Rclone is a Backend that stores objects through an rclone remote.
type Rclone struct {
	// fs is the rclone remote spec, ending in ":" or "/" - "remote:", "remote:path/",
	// "/local/path/". Every operation names it, because rclone has no notion of a
	// connection to one remote.
	fs string

	user     string
	password string

	proc    *rcloneProcess
	baseURL string
	client  *http.Client
}

// rcloneBinary is the program to run. borgstore reads the same variable, so a user who has
// rclone somewhere unusual configures both tools once.
func rcloneBinary() string {
	if v := os.Getenv("RCLONE_BINARY"); v != "" {
		return v
	}
	return "rclone"
}

// NewRclone returns a backend for an rclone remote spec - the part of the URL after
// "rclone:", passed through exactly as the user wrote it.
//
// borge does not parse the spec, and must not: "remote:path", "remote:", ":s3,provider=…:"
// and a bare local path are all rclone syntax, and rclone is the only thing that knows what
// this year's version accepts. borgstore takes the same position (its comment: "no
// URL-unquote here, we just pass through the rclone remote spec as is").
func NewRclone(spec string) (*Rclone, error) {
	if spec == "" {
		return nil, fmt.Errorf("store: an rclone remote is required, as in rclone:remote:path")
	}
	// rclone treats "remote:path" and "remote:path/" alike but borgstore appends the
	// slash so that a name is joined to it without one, and the joined form is what
	// reaches the rc API.
	if !strings.HasSuffix(spec, ":") && !strings.HasSuffix(spec, "/") {
		spec += "/"
	}
	if err := checkRcloneVersion(); err != nil {
		return nil, err
	}
	password, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &Rclone{
		fs:       spec,
		user:     "borg",
		password: password,
		client:   &http.Client{},
	}, nil
}

// checkRcloneVersion runs rclone against itself to find out whether it is there and new
// enough. "rc --loopback" runs an rc command inside the rclone process rather than sending
// it to a server, so this costs no port and no daemon.
//
// Not reported as "the repository does not exist", which is how borgstore classifies it: a
// missing rclone is a missing *tool*, and answering "does not exist" about the repository
// would send a user looking in the wrong place.
func checkRcloneVersion() error {
	out, err := exec.Command(rcloneBinary(), "rc", "--loopback", "core/version").Output()
	if err != nil {
		return fmt.Errorf("store: rclone is needed for an rclone: repository, and %q "+
			"could not be run (%v); set RCLONE_BINARY if it is installed elsewhere",
			rcloneBinary(), err)
	}
	var version struct {
		Version    string `json:"version"`
		Decomposed []int  `json:"decomposed"`
	}
	if err := json.Unmarshal(out, &version); err != nil {
		return fmt.Errorf("store: %q did not report a version this can read: %w", rcloneBinary(), err)
	}
	for i, min := range rcloneMinVersion {
		if i >= len(version.Decomposed) {
			break
		}
		if version.Decomposed[i] > min {
			return nil
		}
		if version.Decomposed[i] < min {
			return fmt.Errorf("store: rclone %s is too old for an rclone: repository; "+
				"1.57.0 or newer is needed", version.Version)
		}
	}
	return nil
}

// isNotFound reports whether an error is the "no such object" one, whichever layer made
// it: rclone answers a missing name with 404 in most operations and with a null item in
// one, and both have to reach callers as the same thing.
func isNotFound(err error) bool { return errors.Is(err, ErrObjectNotFound) }

// randomToken is the rc password: 32 random bytes, base64url, never written to a file and
// never passed as an argument.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("store: could not generate an rclone password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Open starts the rclone server and waits until it answers.
func (b *Rclone) Open() error {
	if b.proc != nil {
		return ErrMustNotBeOpen
	}
	var lastErr error
	for attempt := 0; attempt < rcloneStartAttempts; attempt++ {
		port, err := freeLoopbackPort()
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		// The password goes in the environment, not in the argument list, so it does not
		// appear in "ps" for every user on the machine. --rc-serve is what makes the
		// object readable over plain GET, which is how ranged reads are done; without it
		// there is no way to ask for part of an object.
		proc, err := startProcess(rcloneBinary(), []string{
			"rcd",
			"--rc-user", b.user,
			"--rc-addr", net.JoinHostPort(rcloneHost, strconv.Itoa(port)),
			"--rc-serve",
			"--use-server-modtime",
		}, "RCLONE_RC_PASS="+b.password)
		if err != nil {
			return fmt.Errorf("store: could not start rclone: %w", err)
		}

		if !proc.waitForPort(net.JoinHostPort(rcloneHost, strconv.Itoa(port))) {
			// It exited before it listened, which usually means something else took the
			// port between finding it free and rclone binding it. Try another one.
			lastErr = proc.wait()
			continue
		}
		b.proc = proc
		b.baseURL = "http://" + net.JoinHostPort(rcloneHost, strconv.Itoa(port)) + "/"
		// One call before anything else, so that a server which is listening but not
		// working is found here rather than in the middle of a backup.
		if err := b.rpc("rc/noop", map[string]any{"value": "noop"}, nil, 1); err != nil {
			b.Close()
			return fmt.Errorf("store: rclone started but did not answer: %w", err)
		}
		return nil
	}
	return fmt.Errorf("store: rclone would not stay running after %d attempts: %v",
		rcloneStartAttempts, lastErr)
}

// Close stops the rclone server.
func (b *Rclone) Close() error {
	if b.proc == nil {
		return ErrMustBeOpen
	}
	err := b.proc.stop(rcloneStopGrace)
	b.proc, b.baseURL = nil, ""
	return err
}

func (b *Rclone) requireOpen() error {
	if b.proc == nil {
		return ErrMustBeOpen
	}
	return nil
}

// freeLoopbackPort asks the kernel for a port and immediately gives it back, which is what
// borgstore does. There is a race between letting go and rclone binding it; Open handles
// that by trying again rather than by pretending it cannot happen.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(rcloneHost, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// rpc posts one rc command and decodes its reply into out, which may be nil.
func (b *Rclone) rpc(command string, input any, out any, tries int) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	var body []byte
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		body = encoded
	}
	resp, err := b.do(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, b.baseURL+command, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}, command, tries)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("store: rclone's answer to %s was not JSON: %w", command, err)
	}
	return nil
}

// do runs a request, retrying a 500 and translating the status codes rclone uses.
//
// The retry is borgstore's and its reason is worth keeping: rclone retries everything
// internally except an operation that streams data, so a 500 seen here is usually one of
// those - an upload or a download that failed part-way and is worth attempting again.
func (b *Rclone) do(build func() (*http.Request, error), what string, tries int) (*http.Response, error) {
	if tries < 1 {
		tries = 1
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		req, err := build()
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		req.SetBasicAuth(b.user, b.password)
		resp, err := b.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("store: rclone %s: %w", what, err)
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
			return resp, nil
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return nil, &ObjectNotFoundError{Name: what}
		default:
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("store: rclone %s failed: %d: %s",
				what, resp.StatusCode, strings.TrimSpace(string(message)))
			if resp.StatusCode != http.StatusInternalServerError {
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

// Create initialises the remote: it must be empty or absent, as everywhere else in borg.
func (b *Rclone) Create() error {
	if b.proc != nil {
		return ErrMustNotBeOpen
	}
	if err := b.Open(); err != nil {
		return err
	}
	defer b.Close()

	entries, err := b.List("")
	switch {
	case err == nil && len(entries) > 0:
		return fmt.Errorf("%w: the rclone remote %s is not empty", ErrAlreadyExists, b.fs)
	case err != nil && !isNotFound(err):
		return err
	}
	return b.Mkdir("")
}

// Destroy removes everything under the remote's base path.
func (b *Rclone) Destroy() error {
	if b.proc != nil {
		return ErrMustNotBeOpen
	}
	if err := b.Open(); err != nil {
		return err
	}
	defer b.Close()

	info, err := b.Info("")
	if err != nil {
		return err
	}
	if !info.Exists {
		return fmt.Errorf("%w: %s", ErrDoesNotExist, b.fs)
	}
	return b.rpc("operations/purge", map[string]any{"fs": b.fs, "remote": ""}, nil, 1)
}

// Load reads an object, or a range of one, over rclone's --rc-serve HTTP endpoint.
func (b *Rclone) Load(name string, offset, size int64) ([]byte, error) {
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
		if -offset-size <= rcloneTailThreshold {
			// Ask for the whole tail and cut it here: one request instead of two,
			// because the size that a proper range needs would have to be fetched.
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

	target := b.baseURL + "[" + b.fs + "]/" + name
	resp, err := b.do(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		return req, nil
	}, name, rcloneTries)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", name, err)
	}
	if truncateTo >= 0 && truncateTo < int64(len(data)) {
		data = data[:truncateTo]
	}
	return data, nil
}

// makeRangeHeader builds the Range header borgstore builds, including the negative offsets
// borge's Load allows. An empty result means no header: the whole object.
func makeRangeHeader(offset, size, totalSize int64) string {
	if offset < 0 {
		if size < 0 {
			return "bytes=" + strconv.FormatInt(offset, 10)
		}
		start := totalSize + offset
		return fmt.Sprintf("bytes=%d-%d", start, start+size-1)
	}
	if size < 0 {
		if offset > 0 {
			return fmt.Sprintf("bytes=%d-", offset)
		}
		return ""
	}
	return fmt.Sprintf("bytes=%d-%d", offset, offset+size-1)
}

// Store writes an object, as a multipart upload to operations/uploadfile.
func (b *Rclone) Store(name string, value []byte) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", path.Base(name))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := part.Write(value); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	contentType := writer.FormDataContentType()
	encoded := body.Bytes()

	query := url.Values{"fs": {b.fs}, "remote": {parentName(name)}}
	resp, err := b.do(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost,
			b.baseURL+"operations/uploadfile?"+query.Encode(), bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, "uploadfile "+name, rcloneTries)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// parentName is the directory part of a store name, in the form rclone wants: "" for a
// name at the top rather than Go's ".".
func parentName(name string) string {
	dir := path.Dir(name)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// Delete removes an object permanently.
func (b *Rclone) Delete(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	err := b.rpc("operations/deletefile", map[string]any{"fs": b.fs, "remote": name}, nil, 1)
	if isNotFound(err) {
		return &ObjectNotFoundError{Name: name}
	}
	return err
}

// Move renames an object.
//
// Not atomic on every remote - on an object store it is a copy followed by a delete - and
// borg lives with that, because it is how a soft delete is done and the alternative is not
// having one. Same property in borg itself.
func (b *Rclone) Move(oldName, newName string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(oldName); err != nil {
		return err
	}
	if err := validateName(newName); err != nil {
		return err
	}
	err := b.rpc("operations/movefile", map[string]any{
		"srcFs": b.fs, "srcRemote": oldName,
		"dstFs": b.fs, "dstRemote": newName,
	}, nil, 1)
	if isNotFound(err) {
		return &ObjectNotFoundError{Name: oldName}
	}
	return err
}

// rcloneItem is one entry as rclone's rc API reports it.
type rcloneItem struct {
	Path  string `json:"Path"`
	Name  string `json:"Name"`
	Size  int64  `json:"Size"`
	IsDir bool   `json:"IsDir"`
}

// Info reports on one name. A missing name is not an error.
func (b *Rclone) Info(name string) (ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return ItemInfo{}, err
	}
	if err := validateName(name); err != nil {
		return ItemInfo{}, err
	}
	var result struct {
		Item *rcloneItem `json:"item"`
	}
	err := b.rpc("operations/stat", map[string]any{
		"fs": b.fs, "remote": name,
		"opt": map[string]any{"recurse": false, "noModTime": true, "noMimeType": true},
	}, &result, 1)
	if err != nil {
		if isNotFound(err) {
			return ItemInfo{Name: path.Base(name)}, nil
		}
		return ItemInfo{}, err
	}
	// rclone answers a missing name with 200 and a null item rather than with 404, so
	// both have to mean the same thing here. Measured against rclone 1.75.
	if result.Item == nil {
		return ItemInfo{Name: path.Base(name)}, nil
	}
	return itemInfoFromRclone(*result.Item), nil
}

// List reports one directory's entries, sorted by name.
func (b *Rclone) List(name string) ([]ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	var result struct {
		List []rcloneItem `json:"list"`
	}
	if err := b.rpc("operations/list", map[string]any{
		"fs": b.fs, "remote": name,
		"opt": map[string]any{"recurse": false, "noModTime": true, "noMimeType": true},
	}, &result, 1); err != nil {
		if isNotFound(err) {
			return nil, &ObjectNotFoundError{Name: name}
		}
		return nil, err
	}
	out := make([]ItemInfo, 0, len(result.List))
	for _, item := range result.List {
		// A name that is not one of ours is skipped rather than reported: the remote may
		// hold something that never came from borge, or an upload still in flight.
		if validateName(item.Name) != nil {
			continue
		}
		out = append(out, itemInfoFromRclone(item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// itemInfoFromRclone converts one rclone entry.
//
// No times. rclone's ModTime is the *client-side* mtime carried across an upload rather
// than a stamp made by the storage, so reporting it as ItemInfo.MTime would be inventing a
// fact; borgstore says the same in its own comment and asks rclone not to fetch it, which
// on several remotes saves a metadata read per object during a listing.
func itemInfoFromRclone(item rcloneItem) ItemInfo {
	return ItemInfo{
		Name:      item.Name,
		Exists:    true,
		Directory: item.IsDir,
		Size:      item.Size,
	}
}

// Mkdir creates a directory. rclone makes parents as needed.
func (b *Rclone) Mkdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	return b.rpc("operations/mkdir", map[string]any{"fs": b.fs, "remote": name}, nil, 1)
}

// Rmdir removes an empty directory.
func (b *Rclone) Rmdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	return b.rpc("operations/rmdir", map[string]any{"fs": b.fs, "remote": name}, nil, 1)
}
