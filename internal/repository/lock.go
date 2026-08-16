// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/storelocking.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package repository

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/renesugar/borge/internal/store"
)

// Lock is a repository lock held as an object in the store's locks/ namespace.
//
// # Why locks live in the store
//
// A repository may be on a filesystem with no working flock, or reached through a
// backend that is not a filesystem at all. So a lock is just another stored object,
// named by the hash of its own content, and mutual exclusion comes from listing that
// namespace rather than from the operating system.
//
// The trade-off is that acquisition is not atomic: two clients can both write a lock
// and only then discover each other. That is handled by writing first and checking
// after - whoever loses removes its own lock and retries - which is safe because a lock
// object is never modified, only created and deleted.
//
// Shared locks may coexist; an exclusive lock excludes everything.
type Lock struct {
	store     *store.Store
	exclusive bool

	// id identifies this holder: host, process, and a random value standing in for the
	// thread id borg uses.
	hostID    string
	processID int
	threadID  string

	myKey       string
	lastRefresh time.Time

	timeout time.Duration
	stale   time.Duration
	sleep   time.Duration
}

// Lock errors.
var (
	// ErrLockTimeout means another client holds a conflicting lock.
	ErrLockTimeout = errors.New("repository: lock timeout")
	// ErrNotLocked means a release or refresh found no lock of ours.
	ErrNotLocked = errors.New("repository: not locked")
)

// lockRecord is the JSON stored in a lock object. The field names are borg's.
type lockRecord struct {
	Exclusive bool   `json:"exclusive"`
	HostID    string `json:"hostid"`
	ProcessID int    `json:"processid"`
	ThreadID  string `json:"threadid"`
	Time      string `json:"time"`
}

// Default lock timings, from borg.
const (
	defaultLockTimeout = 1 * time.Second
	// defaultLockStale is how long a lock may go unrefreshed before another client may
	// remove it. Thirty minutes is long enough that a slow operation is not killed, and
	// short enough that a crashed client does not block the repository indefinitely.
	defaultLockStale = 30 * time.Minute
	defaultLockSleep = 200 * time.Millisecond
)

// NewLock returns a lock for a store.
func NewLock(s *store.Store, exclusive bool) (*Lock, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	// borg uses a host id that includes more than the hostname; the important property
	// is only that two different machines do not collide, and that a lock can be
	// recognised as ours.
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, fmt.Errorf("repository: %w", err)
	}
	return &Lock{
		store:     s,
		exclusive: exclusive,
		hostID:    host,
		processID: os.Getpid(),
		threadID:  hex.EncodeToString(rnd[:]),
		timeout:   defaultLockTimeout,
		stale:     defaultLockStale,
		sleep:     defaultLockSleep,
	}, nil
}

// SetTimeout overrides how long Acquire waits for a conflicting lock to go away.
func (l *Lock) SetTimeout(d time.Duration) { l.timeout = d }

// SetStale overrides how long a lock may go unrefreshed before it is considered stale.
func (l *Lock) SetStale(d time.Duration) { l.stale = d }

func (l *Lock) lockName(key string) string { return "locks/" + key }

// create writes a lock object and returns its key.
func (l *Lock) create(exclusive bool) (string, error) {
	rec := lockRecord{
		Exclusive: exclusive,
		HostID:    l.hostID,
		ProcessID: l.processID,
		ThreadID:  l.threadID,
		Time:      time.Now().UTC().Format("2006-01-02T15:04:05.000"),
	}
	value, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(value)
	key := hex.EncodeToString(sum[:])
	if err := l.store.Store(l.lockName(key), value); err != nil {
		return "", err
	}
	return key, nil
}

func (l *Lock) deleteLock(key string, ignoreNotFound bool) error {
	err := l.store.Delete(l.lockName(key), false)
	if err != nil && ignoreNotFound && errors.Is(err, store.ErrObjectNotFound) {
		return nil
	}
	return err
}

// heldLock is one lock object read back from the store.
type heldLock struct {
	key string
	rec lockRecord
	at  time.Time
}

// getLocks reads every lock object, removing any that are stale.
//
// Removing a stale lock here rather than only failing is what keeps a crashed client
// from blocking the repository forever.
func (l *Lock) getLocks() ([]heldLock, error) {
	names, err := l.store.ListNames("locks", false)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil, nil // the namespace is created lazily
		}
		return nil, err
	}

	var out []heldLock
	for _, key := range names {
		value, err := l.store.Load(l.lockName(key), 0, -1, false)
		if err != nil {
			if errors.Is(err, store.ErrObjectNotFound) {
				continue // released under us, which is normal
			}
			return nil, err
		}
		var rec lockRecord
		if err := json.Unmarshal(value, &rec); err != nil {
			// Not a lock we understand. Leave it alone rather than deleting something
			// another tool may own.
			continue
		}
		at, err := time.Parse("2006-01-02T15:04:05.000", strings.TrimSuffix(rec.Time, "Z"))
		if err != nil {
			continue
		}
		if time.Since(at) > l.stale {
			// Stale: the holder has not refreshed it in long enough that it is presumed
			// dead. Best effort - another client may be removing it at the same moment.
			_ = l.deleteLock(key, true)
			continue
		}
		out = append(out, heldLock{key: key, rec: rec, at: at})
	}
	return out, nil
}

func (l *Lock) isOurs(h heldLock) bool {
	return h.rec.HostID == l.hostID && h.rec.ProcessID == l.processID && h.rec.ThreadID == l.threadID
}

// Acquire takes the lock, waiting up to the timeout for a conflicting one to go away.
//
// The order is: write our lock first, then look at everyone else's. Acquisition is not
// atomic, so a loser has to withdraw and retry - which is safe because lock objects are
// immutable once written.
func (l *Lock) Acquire() error {
	deadline := time.Now().Add(l.timeout)
	for {
		key, err := l.create(l.exclusive)
		if err != nil {
			return err
		}
		l.myKey = key
		l.lastRefresh = time.Now()

		locks, err := l.getLocks()
		if err != nil {
			_ = l.deleteLock(key, true)
			l.myKey = ""
			return err
		}

		conflict := false
		for _, h := range locks {
			if l.isOurs(h) {
				continue
			}
			// An exclusive lock conflicts with everything; a shared one only with an
			// exclusive.
			if l.exclusive || h.rec.Exclusive {
				conflict = true
				break
			}
		}
		if !conflict {
			return nil
		}

		// Withdraw and retry, so a conflicting holder is not blocked by our attempt.
		_ = l.deleteLock(key, true)
		l.myKey = ""
		if time.Now().After(deadline) {
			kind := "shared"
			if l.exclusive {
				kind = "exclusive"
			}
			return fmt.Errorf("%w: could not take a %s lock; another borg or borge is using this repository",
				ErrLockTimeout, kind)
		}
		time.Sleep(l.sleep)
	}
}

// Release removes our lock.
func (l *Lock) Release(ignoreNotFound bool) error {
	if l.myKey == "" {
		if ignoreNotFound {
			return nil
		}
		return ErrNotLocked
	}
	key := l.myKey
	l.myKey = ""
	return l.deleteLock(key, ignoreNotFound)
}

// Refresh renews the lock by writing a new object with a current timestamp and removing
// the old one, so a long operation is not mistaken for a crashed client.
//
// The new lock is written *before* the old one is removed, so the repository is never
// momentarily unlocked.
func (l *Lock) Refresh() error {
	if l.myKey == "" {
		return ErrNotLocked
	}
	if time.Since(l.lastRefresh) < l.stale/2 {
		return nil // not due yet
	}
	old := l.myKey
	key, err := l.create(l.exclusive)
	if err != nil {
		return err
	}
	l.myKey = key
	l.lastRefresh = time.Now()
	return l.deleteLock(old, true)
}

// Exclusive reports whether this is an exclusive lock.
func (l *Lock) Exclusive() bool { return l.exclusive }

// BreakLock removes every lock in the repository.
//
// It is the manual escape hatch for a lock left behind by a killed client, and it is
// unconditionally destructive: if another client really is running, breaking its lock
// invites two writers into one repository.
func BreakLock(s *store.Store) error {
	names, err := s.ListNames("locks", false)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil
		}
		return err
	}
	for _, key := range names {
		if err := s.Delete("locks/"+key, false); err != nil && !errors.Is(err, store.ErrObjectNotFound) {
			return err
		}
	}
	return nil
}

// randRead fills b with cryptographically random bytes. It is here rather than in
// repository.go so the crypto/rand import stays with the other security-relevant code.
func randRead(b []byte) (int, error) { return rand.Read(b) }
