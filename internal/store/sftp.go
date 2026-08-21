// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/backends/sftp.py (borgstore 0.6.1, BSD 3-Clause,
// Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// The SFTP backend: files in directories below a base path, on someone else's machine.
//
// This is the backend most people mean by "a remote repository", and it is the one with
// the most decisions that are not borge's to make. Two matter enough to state plainly.
//
// # There is no password
//
// borgstore's sftp URL has no password field at all, and its connect call passes a key file
// and an agent and nothing else. So borge authenticates with a key or through the agent,
// full stop - a URL cannot carry a password, and there is nowhere to type one. This was
// established by measurement after asking for a password-authenticated test account and
// finding borg could not use it (PORTING_PLAN §11.5).
//
// # An unknown host is a failure, not a question
//
// The host key must already be in known_hosts. borgstore says why: "we do not deal with
// unknown hosts ... the user should make the first contact using the ssh or sftp CLI
// command and interactively verify remote host fingerprints". A backup program that
// accepted a new key on its own would be one that keeps working while an attacker stands
// in the middle, so borge refuses rather than trusting - and says which command to run.

const (
	// sftpConnectTimeout bounds the connection and authentication, so a dead network does
	// not block a backup before it has started.
	sftpConnectTimeout = 30 * time.Second
	// sftpKeepalive keeps NAT and firewall state alive and makes a dead peer noticeable.
	sftpKeepalive = 30 * time.Second
	// sftpDefaultPort is ssh's.
	sftpDefaultPort = 22
)

// sftpURL is borgstore's regex: "sftp://[user@]host[:port]/path", where the slash is a
// separator and not part of the path. So "sftp://host/repo" is relative - to the login
// directory, usually - and "sftp://host//srv/repo" is absolute.
var sftpURL = regexp.MustCompile(`^sftp://(?:([^@]+)@)?([^:/]+)(?::(\d+))?/(.+)$`)

// SFTP is a Backend that stores objects on an SFTP server.
type SFTP struct {
	user string
	host string
	port int
	base string

	ssh    *ssh.Client
	client *sftp.Client
	opened bool
}

// NewSFTP parses an sftp:// URL and returns a backend for it. Nothing is connected yet.
func NewSFTP(rawURL string) (*SFTP, error) {
	m := sftpURL.FindStringSubmatch(rawURL)
	if m == nil {
		return nil, fmt.Errorf("store: %q is not an sftp URL; the form is "+
			"sftp://[user@]host[:port]/path", rawURL)
	}
	user, err := url.PathUnescape(m[1])
	if err != nil {
		return nil, fmt.Errorf("store: the user in %q is not valid: %w", rawURL, err)
	}
	base, err := url.PathUnescape(m[4])
	if err != nil {
		return nil, fmt.Errorf("store: the path in %q is not valid: %w", rawURL, err)
	}
	port := 0
	if m[3] != "" {
		if port, err = strconv.Atoi(m[3]); err != nil {
			return nil, fmt.Errorf("store: the port in %q is not a number: %w", rawURL, err)
		}
	}
	return &SFTP{user: user, host: m[2], port: port, base: base}, nil
}

// connect opens the ssh connection and the SFTP session.
func (b *SFTP) connect() error {
	config := lookupSSHConfig(b.host)
	// What the URL says wins over what the config says, which is borgstore's order.
	user := config.User
	if b.user != "" {
		user = b.user
	}
	if user == "" {
		user = localUsername()
	}
	port := sftpDefaultPort
	if config.Port != "" {
		if parsed, err := strconv.Atoi(config.Port); err == nil {
			port = parsed
		}
	}
	if b.port != 0 {
		port = b.port
	}

	auth, err := sftpAuthMethods(config.IdentityFiles)
	if err != nil {
		return err
	}
	hostKeys, err := knownHostsCallback()
	if err != nil {
		return err
	}

	address := net.JoinHostPort(config.HostName, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKeys,
		// Only the key types known_hosts actually has for this host. Without this the
		// handshake negotiates whatever both sides prefer - often RSA or ECDSA - and the
		// check then compares that against an ed25519 entry and calls it a mismatch. The
		// message would be "the host key changed", on a host that changed nothing:
		// a false alarm of exactly the kind nobody should be trained to ignore.
		HostKeyAlgorithms: knownHostAlgorithms(hostKeys, address),
		Timeout:           sftpConnectTimeout,
	})
	if err != nil {
		return sftpConnectError(b.host, address, err)
	}
	// A keepalive request every half minute, ignored by the server but not by the kernel
	// in between: it is what keeps a NAT mapping from being dropped mid-backup.
	go sftpKeepaliveLoop(client)

	session, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return fmt.Errorf("store: could not start an SFTP session on %s: %w", b.host, err)
	}
	b.ssh, b.client = client, session
	return nil
}

// sftpConnectError explains the two failures worth explaining.
func sftpConnectError(host, address string, err error) error {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		if len(keyErr.Want) > 0 {
			return fmt.Errorf("store: the host key of %s does not match the one in "+
				"known_hosts. Either the server changed or something is impersonating it; "+
				"borge will not guess which: %w", host, err)
		}
		return fmt.Errorf("store: %s is not in known_hosts, and borge does not accept a "+
			"host key it has not seen before. Connect once with \"ssh %s\" and verify the "+
			"fingerprint, then try again: %w", host, host, err)
	}
	return fmt.Errorf("store: could not connect to %s: %w", address, err)
}

func sftpKeepaliveLoop(client *ssh.Client) {
	ticker := time.NewTicker(sftpKeepalive)
	defer ticker.Stop()
	for range ticker.C {
		// The request is meaningless by design; what matters is that bytes move. A
		// failure means the connection is gone, and the operation using it will say so.
		if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			return
		}
	}
}

// sftpAuthMethods builds the authentication borgstore uses: the configured keys, then the
// agent. No password - see the note at the top of this file.
func sftpAuthMethods(identityFiles []string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	var signers []ssh.Signer
	for _, file := range identityFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			// A key named in the config that is not there is not fatal: ssh tries the
			// next one, and so does this.
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			// An encrypted key cannot be used unattended and there is nowhere to ask for
			// its passphrase, so it is skipped rather than failing the connection - the
			// agent may have it already.
			continue
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if len(methods) == 0 {
		return nil, errors.New("store: no usable ssh key: borge authenticates with a key " +
			"or an agent and never with a password, so name one with IdentityFile in " +
			"~/.ssh/config or start an ssh agent")
	}
	return methods, nil
}

// knownHostsCallback reads the user's known_hosts. There is no fallback: a host borge has
// not seen is a host borge will not talk to.
func knownHostsCallback() (ssh.HostKeyCallback, error) {
	path := os.Getenv("BORGE_KNOWN_HOSTS")
	if path == "" {
		path = fmt.Sprintf("%s/.ssh/known_hosts", homeDir())
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("store: could not read %s, which is where borge looks for "+
			"the host keys it is willing to talk to: %w", path, err)
	}
	return callback, nil
}

// knownHostAlgorithms asks known_hosts which key types it holds for an address.
//
// There is no method for this, so the question is put the only way the package allows:
// offer a key it cannot possibly know and read the answer's list of what it wanted
// instead. An unknown host produces an empty list, which leaves the handshake unconstrained
// and lets the real check refuse it with "not in known_hosts" rather than "changed".
func knownHostAlgorithms(callback ssh.HostKeyCallback, address string) []string {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	public, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return nil
	}
	err = callback(address, &net.TCPAddr{IP: net.IPv4zero}, public)
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return nil
	}
	var algorithms []string
	for _, known := range keyErr.Want {
		algorithms = append(algorithms, hostKeyAlgorithmsFor(known.Key.Type())...)
	}
	return algorithms
}

// hostKeyAlgorithmsFor expands a stored key type into the signature algorithms that key can
// produce. An "ssh-rsa" entry covers the two SHA-2 signature algorithms as well, and
// offering only "ssh-rsa" would ask a modern server for SHA-1 signatures it may refuse.
func hostKeyAlgorithmsFor(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

func (b *SFTP) disconnect() {
	if b.client != nil {
		_ = b.client.Close()
		b.client = nil
	}
	if b.ssh != nil {
		_ = b.ssh.Close()
		b.ssh = nil
	}
}

func (b *SFTP) requireOpen() error {
	if !b.opened {
		return ErrMustBeOpen
	}
	return nil
}

// resolve turns a store name into a path on the server. Names are relative to the base
// path, which borgstore achieves with a chdir; doing it by joining is the same thing and
// survives a reconnection without having to remember to chdir again.
func (b *SFTP) resolve(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	if name == "" {
		return b.base, nil
	}
	return path.Join(b.base, name), nil
}

// Create makes the base path, which must be empty or absent.
func (b *SFTP) Create() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	if err := b.connect(); err != nil {
		return err
	}
	defer b.disconnect()

	// Parents are made too: some repository hosts give access through borg and nothing
	// else, so requiring the user to create them out of band would make those unusable.
	if err := b.client.MkdirAll(b.base); err != nil {
		return fmt.Errorf("store: could not create %s: %w", b.base, err)
	}
	entries, err := b.client.ReadDir(b.base)
	if err != nil {
		return fmt.Errorf("store: could not read %s: %w", b.base, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: the sftp base path is not empty: %s", ErrAlreadyExists, b.base)
	}
	return nil
}

// Destroy removes everything below the base path.
func (b *SFTP) Destroy() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	if err := b.connect(); err != nil {
		return err
	}
	defer b.disconnect()

	if _, err := b.client.Stat(b.base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: the sftp base path does not exist: %s", ErrDoesNotExist, b.base)
		}
		return fmt.Errorf("store: %w", err)
	}
	if err := b.removeTree(b.base); err != nil {
		return err
	}
	// The base directory itself may stay: Create accepts an existing empty directory, so
	// it may not have made this one - and on many servers a user cannot remove it anyway.
	if err := b.client.RemoveDirectory(b.base); err != nil {
		return nil
	}
	return nil
}

// removeTree deletes a directory's contents, depth first.
func (b *SFTP) removeTree(dir string) error {
	entries, err := b.client.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	for _, entry := range entries {
		child := path.Join(dir, entry.Name())
		if entry.IsDir() {
			if err := b.removeTree(child); err != nil {
				return err
			}
			if err := b.client.RemoveDirectory(child); err != nil {
				return fmt.Errorf("store: %w", err)
			}
			continue
		}
		if err := b.client.Remove(child); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// Open connects and checks that the base path is a directory.
func (b *SFTP) Open() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	if err := b.connect(); err != nil {
		return err
	}
	info, err := b.client.Stat(b.base)
	if err != nil {
		b.disconnect()
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: the sftp base path does not exist: %s", ErrDoesNotExist, b.base)
		}
		return fmt.Errorf("store: %w", err)
	}
	if !info.IsDir() {
		b.disconnect()
		return fmt.Errorf("%w: the sftp base path is not a directory: %s", ErrDoesNotExist, b.base)
	}
	b.opened = true
	return nil
}

// Close ends the session.
func (b *SFTP) Close() error {
	if !b.opened {
		return ErrMustBeOpen
	}
	b.disconnect()
	b.opened = false
	return nil
}

// Load reads an object, or a range of one.
func (b *SFTP) Load(name string, offset, size int64) ([]byte, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	target, err := b.resolve(name)
	if err != nil {
		return nil, err
	}
	file, err := b.client.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ObjectNotFoundError{Name: name}
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	defer file.Close()

	if offset != 0 {
		whence := io.SeekStart
		if offset < 0 {
			whence = io.SeekEnd
		}
		if _, err := file.Seek(offset, whence); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	if size < 0 {
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		return data, nil
	}
	data := make([]byte, size)
	n, err := io.ReadFull(file, data)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("store: %w", err)
	}
	// A short read is not an error: the pack reader asks for a header-sized slice at the
	// end of a pack and reads the short answer as a clean end of file.
	return data[:n], nil
}

// Store writes an object.
//
// To a temporary name in the same directory and then renamed over the target, so a reader
// never sees a half-written object. The rename is the POSIX one, which replaces an existing
// target atomically; SFTP's plain rename fails if the target exists, which would make an
// overwrite two operations with a gap in the middle.
func (b *SFTP) Store(name string, value []byte) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	dir := path.Dir(target)
	tmp := path.Join(dir, randomTempName())

	write := func() error {
		file, err := b.client.Create(tmp)
		if err != nil {
			return err
		}
		if _, err := file.Write(value); err != nil {
			file.Close()
			_ = b.client.Remove(tmp)
			return err
		}
		return file.Close()
	}
	if err := write(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: %w", err)
		}
		// The directory was not there. Made only now, rather than before every write:
		// each SFTP operation is a round trip, and on a link with latency that one is
		// almost always wasted.
		if err := b.client.MkdirAll(dir); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if err := write(); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	if err := b.client.PosixRename(tmp, target); err != nil {
		_ = b.client.Remove(tmp)
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// randomTempName is eight lowercase letters and the reserved suffix, as borgstore's is.
// The suffix is what makes a listing skip it: a half-written object must never be mistaken
// for a real one.
func randomTempName() string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	name := make([]byte, 8)
	for i := range name {
		name[i] = letters[mathrand.Intn(len(letters))]
	}
	return string(name) + TmpSuffix
}

// Delete removes an object permanently.
func (b *SFTP) Delete(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := b.client.Remove(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ObjectNotFoundError{Name: name}
		}
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Move renames an object, creating the destination directory if it is missing.
func (b *SFTP) Move(oldName, newName string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	from, err := b.resolve(oldName)
	if err != nil {
		return err
	}
	to, err := b.resolve(newName)
	if err != nil {
		return err
	}
	if err := b.client.PosixRename(from, to); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: %w", err)
		}
		if mkErr := b.client.MkdirAll(path.Dir(to)); mkErr != nil {
			return fmt.Errorf("store: %w", mkErr)
		}
		if err := b.client.PosixRename(from, to); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &ObjectNotFoundError{Name: oldName}
			}
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// Info reports on one name without reading it.
func (b *SFTP) Info(name string) (ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return ItemInfo{}, err
	}
	target, err := b.resolve(name)
	if err != nil {
		return ItemInfo{}, err
	}
	info, err := b.client.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ItemInfo{Name: path.Base(target)}, nil
		}
		return ItemInfo{}, fmt.Errorf("store: %w", err)
	}
	return ItemInfo{
		Name:      path.Base(target),
		Exists:    true,
		Directory: info.IsDir(),
		Size:      info.Size(),
		MTime:     info.ModTime(),
	}, nil
}

// List reports one directory's entries, sorted by name.
func (b *SFTP) List(name string) ([]ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	target, err := b.resolve(name)
	if err != nil {
		return nil, err
	}
	entries, err := b.client.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ObjectNotFoundError{Name: name}
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	out := make([]ItemInfo, 0, len(entries))
	for _, entry := range entries {
		// A name that is not one of ours is skipped: the directory may hold a temp file
		// from a write in flight, or something that never came from borge.
		if validateName(entry.Name()) != nil {
			continue
		}
		out = append(out, ItemInfo{
			Name:      entry.Name(),
			Exists:    true,
			Directory: entry.IsDir(),
			Size:      entry.Size(),
			MTime:     entry.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Mkdir creates a directory and any missing parents.
func (b *SFTP) Mkdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := b.client.MkdirAll(target); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Rmdir removes an empty directory.
func (b *SFTP) Rmdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := b.client.RemoveDirectory(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ObjectNotFoundError{Name: name}
		}
		return fmt.Errorf("store: %w", err)
	}
	return nil
}
