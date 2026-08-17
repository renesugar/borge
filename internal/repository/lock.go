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
	"net"
	"os"
	"strings"
	"sync"
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

	// id identifies this holder. The three fields are borg's identity tuple, and their
	// types are load-bearing: borg asserts that processid and threadid are integers, so a
	// lock carrying anything else makes borg abort rather than merely ignore it.
	//
	// threadID is always zero, as it is in borg. The consequence is that two locks taken
	// by the same process do not conflict with each other - which is borg's behaviour
	// too, and is what lets a single process hold a lock across nested operations.
	hostID    string
	processID int
	threadID  threadID

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

// lockTimeLayout is how a lock's timestamp is written.
//
// It has to be **timezone-aware**, matching Python's
// datetime.now(UTC).isoformat(timespec="milliseconds") - which produces a "+00:00"
// suffix, not a bare local-looking time and not a "Z".
//
// This is not cosmetic. borg parses the field with datetime.fromisoformat and then
// compares it against an aware "now": a naive timestamp makes that comparison raise
// TypeError, so a lock written by borge would crash borg on every subsequent repository
// access. It was found by the stage 4 gate, where borg had to open a repository borge
// still held.
const lockTimeLayout = "2006-01-02T15:04:05.000-07:00"

// parseLockTime reads a lock timestamp.
//
// Both spellings are accepted on the way in - with an offset, and the bare form borge
// itself wrote before this was fixed - because refusing to parse a lock means silently
// ignoring it, and ignoring a lock is how two writers end up in one repository. A lock
// that cannot be read is more dangerous than one that is slightly the wrong shape.
func parseLockTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02T15:04:05.000", strings.TrimSuffix(s, "Z"))
}

// lockRecord is the JSON stored in a lock object. The field names are borg's.
type lockRecord struct {
	Exclusive bool     `json:"exclusive"`
	HostID    string   `json:"hostid"`
	ProcessID int      `json:"processid"`
	ThreadID  threadID `json:"threadid"`
	Time      string   `json:"time"`
}

// threadID decodes borg's integer thread id, and also tolerates a string.
//
// borg always writes an integer and asserts on anything else. borge is lenient on the way
// *in* for the same reason it accepts both timestamp spellings: a lock it refuses to
// parse is a lock it might act on wrongly, and being generous about a field nobody uses
// costs nothing.
type threadID int

func (t *threadID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		// The value is only ever compared for equality, so a non-numeric string just
		// becomes "not zero" - which is what it means.
		if s == "0" || s == "" {
			*t = 0
		} else {
			*t = -1
		}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*t = threadID(n)
	return nil
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
	return &Lock{
		store:     s,
		exclusive: exclusive,
		hostID:    HostID(),
		processID: os.Getpid(),
		threadID:  0, // always, as in borg; see the Lock struct
		timeout:   defaultLockTimeout,
		stale:     defaultLockStale,
		sleep:     defaultLockSleep,
	}, nil
}

// hostIDOnce caches the host id, which involves a network interface lookup.
var (
	hostIDOnce  sync.Once
	hostIDValue string
)

// HostID identifies this machine in a lock record, in borg's spelling: "<fqdn>@<node>",
// where node is a MAC address as a decimal integer (Python's uuid.getnode()).
//
// # Why the spelling matters
//
// borg compares this against its own host id to decide whether it may check the recorded
// pid for liveness. If the two do not match, borg assumes the lock's owner is alive - the
// conservative answer - and falls back on the staleness timeout. So a mismatch is safe
// but costs half an hour of waiting after a crash, which is worth avoiding.
//
// It cannot be guaranteed to match: Python and Go may pick different interfaces on a
// multi-homed machine, and getfqdn's behaviour depends on the resolver. BORG_HOST_ID
// exists for exactly that case and is honoured here, so the two can always be aligned
// explicitly.
func HostID() string {
	hostIDOnce.Do(func() {
		if v, ok := lookupEnv("HOST_ID"); ok && v != "" {
			hostIDValue = v
			return
		}
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		hostIDValue = fmt.Sprintf("%s@%d", host, nodeID())
	})
	return hostIDValue
}

// nodeID is Python's uuid.getnode(): the first usable MAC address as a 48-bit integer.
//
// Python falls back to a random value when it finds none, and so does this - a random
// value cannot collide with another machine's real one, which is the property that
// matters. A zero MAC is skipped for the reason borg documents (borg #3968): some
// virtualised environments hand out an all-zero address to everybody.
func nodeID() uint64 {
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) != 6 {
				continue
			}
			var v uint64
			var nonZero bool
			for _, b := range iface.HardwareAddr {
				v = v<<8 | uint64(b)
				if b != 0 {
					nonZero = true
				}
			}
			if nonZero {
				return v
			}
		}
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return 0
	}
	var v uint64
	for _, b := range rnd {
		v = v<<8 | uint64(b)
	}
	// Set the multicast bit, as Python does, to mark the value as not a real address.
	return v | (1 << 40)
}

// setHolder overrides the identity this lock is taken under.
//
// It exists so tests can act as a second client without starting a second process. The
// identity is otherwise fixed for the lifetime of the process, which is the point:
// pretending to be somebody else is not something a real caller should be able to do.
func (l *Lock) setHolder(host string, pid, thread int) {
	l.hostID = host
	l.processID = pid
	l.threadID = threadID(thread)
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
		Time:      time.Now().UTC().Format(lockTimeLayout),
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
			// A lock that cannot be read must not be treated as a lock that is not there.
			// Ignoring it would let a second writer into a repository somebody is using,
			// which is the one failure this whole mechanism exists to prevent - so refuse
			// to proceed and say which object is the problem. borg fails here too.
			return nil, fmt.Errorf("repository: locks/%s could not be read (%w); "+
				"if no borg or borge is running, remove the stale lock", key, err)
		}
		at, err := parseLockTime(rec.Time)
		if err != nil {
			return nil, fmt.Errorf("repository: locks/%s has an unreadable timestamp %q; "+
				"if no borg or borge is running, remove the stale lock", key, rec.Time)
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

// HeldLock describes a lock currently in the repository, for reporting.
type HeldLock struct {
	// Key is the lock object's name within locks/.
	Key string
	// Exclusive distinguishes a writer's lock from a reader's.
	Exclusive bool
	// Host and PID identify the holder. Host is the "<hostname>@<nodeid>" form borg uses.
	Host string
	PID  int
	// Time is when the lock was last written or refreshed.
	Time time.Time
	// Stale reports that the holder has not refreshed within the stale period, so the
	// lock would be removed by the next client that tried to acquire one.
	Stale bool
}

// ListLocks reports the locks a repository currently holds, without removing any.
//
// Acquiring a lock removes stale ones as a side effect, because a crashed client must not
// block the repository forever. Reporting must not: "break-lock told me there were no
// locks" is a very different claim from "break-lock silently removed two". So this reads
// and classifies, and leaves removal to BreakLock.
func ListLocks(s *store.Store) ([]HeldLock, error) {
	names, err := s.ListNames("locks", false)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			return nil, nil // the namespace is created lazily
		}
		return nil, err
	}

	var out []HeldLock
	for _, key := range names {
		value, err := s.Load("locks/"+key, 0, -1, false)
		if err != nil {
			if errors.Is(err, store.ErrObjectNotFound) {
				continue // released under us, which is normal
			}
			return nil, err
		}
		var rec lockRecord
		if err := json.Unmarshal(value, &rec); err != nil {
			return nil, fmt.Errorf("repository: locks/%s could not be read (%w); "+
				"if no borg or borge is running, remove the stale lock", key, err)
		}
		at, err := parseLockTime(rec.Time)
		if err != nil {
			return nil, fmt.Errorf("repository: locks/%s has an unreadable timestamp %q; "+
				"if no borg or borge is running, remove the stale lock", key, rec.Time)
		}
		out = append(out, HeldLock{
			Key: key, Exclusive: rec.Exclusive, Host: rec.HostID, PID: rec.ProcessID,
			Time: at, Stale: time.Since(at) > defaultLockStale,
		})
	}
	return out, nil
}

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
