// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file corresponds to the passphrase handling in borg's src/borg/helpers/passphrase.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/repository"
)

// Passphrase prompting.
//
// # Why the prompt is a retry rather than a first step
//
// A repository's key type is not known until its manifest has been read, and the none-*
// and authenticated-* modes have no passphrase at all. Asking before opening would mean
// prompting for repositories that have nothing to unlock.
//
// So the environment's passphrase is tried first, and the prompt happens only when that
// fails with ErrPassphraseWrong - which is a failure the unkeyed modes cannot produce.
// The pleasant side effect is that a *wrong* BORGE_PASSPHRASE also gets a prompt instead
// of a bare refusal.
//
// # Why it writes to stderr
//
// So that "borge list ARCHIVE > files.txt" still writes only the listing to the file. The
// passphrase itself is read from the terminal with echo off, not from Stdin: a command
// reading a tar stream on standard input still has a terminal to ask at.

// passphraseTries is how many times a prompt is offered before giving up. borg's number.
const passphraseTries = 3

// promptPassphrase reads a passphrase from the terminal with echo disabled.
//
// ok is false when there is no terminal to ask at - a cron job, a pipeline, a test - in
// which case the caller must fail with a message about the environment variable rather
// than hang.
func (e *Env) promptPassphrase(prompt string) (pass string, ok bool, err error) {
	fd, isTTY := e.terminalFD()
	if !isTTY {
		return "", false, nil
	}
	fmt.Fprint(e.Stderr, prompt)
	line, err := term.ReadPassword(fd)
	// ReadPassword leaves the cursor after the (invisible) input, so the newline the user
	// typed has to be echoed or the next output continues on the prompt's line.
	fmt.Fprintln(e.Stderr)
	if err != nil {
		return "", true, fmt.Errorf("cannot read the passphrase: %w", err)
	}
	return string(line), true, nil
}

// terminalFD reports the file descriptor to prompt on.
//
// The process's standard input is used rather than Env.Stdin: Env.Stdin is an io.Reader
// so that the whole CLI is testable, and in a test it is a buffer with no terminal behind
// it - which is exactly the "no terminal, do not prompt" answer wanted there. When Env is
// wired to the real process, the two are the same file.
func (e *Env) terminalFD() (int, bool) {
	f, isFile := e.Stdin.(*os.File)
	if !isFile {
		return 0, false
	}
	fd := int(f.Fd())
	return fd, term.IsTerminal(fd)
}

// unlockWithPrompt opens a repository's key, asking for the passphrase if the environment
// did not supply a working one.
func (e *Env) unlockWithPrompt(repo *repository.Repository) (key.Key, *key.Unlocked, error) {
	k, u, err := repo.Unlock(e.passphrase())
	if !errors.Is(err, key.ErrPassphraseWrong) {
		return k, u, err
	}

	for try := 0; try < passphraseTries; try++ {
		pass, ok, promptErr := e.promptPassphrase("Enter passphrase for " + repo.Location().Canonical() + ": ")
		if promptErr != nil {
			return nil, nil, promptErr
		}
		if !ok {
			// No terminal: the original error, with the way out.
			return nil, nil, fmt.Errorf("%w (set BORGE_PASSPHRASE or BORG_PASSPHRASE)", err)
		}
		k, u, err = repo.Unlock(pass)
		if err == nil {
			// Remembered for the rest of this command, so a second unlock - key
			// change-location does two - does not ask twice.
			e.rememberPassphrase(pass)
			return k, u, nil
		}
		if !errors.Is(err, key.ErrPassphraseWrong) {
			return nil, nil, err
		}
		if try < passphraseTries-1 {
			e.errorf("wrong passphrase, try again")
		}
	}
	return nil, nil, fmt.Errorf("%w after %d attempts", key.ErrPassphraseWrong, passphraseTries)
}

// unlockKeyManagerWithPrompt is unlockWithPrompt for the key commands, which work through
// the key manager rather than the repository.
func (e *Env) unlockKeyManagerWithPrompt(mgr *key.Manager, repoPath string) (*key.Unlocked, error) {
	u, err := mgr.Unlock(e.passphrase())
	if !errors.Is(err, key.ErrPassphraseWrong) {
		return u, err
	}

	for try := 0; try < passphraseTries; try++ {
		pass, ok, promptErr := e.promptPassphrase("Enter passphrase for " + repoPath + ": ")
		if promptErr != nil {
			return nil, promptErr
		}
		if !ok {
			return nil, fmt.Errorf("%w (set BORGE_PASSPHRASE or BORG_PASSPHRASE)", err)
		}
		u, err = mgr.Unlock(pass)
		if err == nil {
			e.rememberPassphrase(pass)
			return u, nil
		}
		if !errors.Is(err, key.ErrPassphraseWrong) {
			return nil, err
		}
		if try < passphraseTries-1 {
			e.errorf("wrong passphrase, try again")
		}
	}
	return nil, fmt.Errorf("%w after %d attempts", key.ErrPassphraseWrong, passphraseTries)
}

// rememberPassphrase keeps a prompted passphrase for the rest of this command.
//
// It is not written anywhere and does not outlive the process. Without it, a command that
// unlocks twice would ask twice for the same secret, which trains people to type
// passphrases at any prompt that appears.
func (e *Env) rememberPassphrase(pass string) {
	e.prompted = &pass
}

// newPassphraseWithPrompt reads the passphrase a key is being *given*.
//
// It is asked for twice, because there is no way to check it afterwards: the passphrase
// that protects a key is the only thing that opens it, and a typo made once here is a
// repository nobody can read. borg does the same.
func (e *Env) newPassphraseWithPrompt(what string) (string, error) {
	if v, ok := e.lookupBorg("NEW_PASSPHRASE"); ok {
		return v, nil
	}

	first, ok, err := e.promptPassphrase("Enter a new passphrase for " + what + ": ")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("no new passphrase given and no terminal to ask at; " +
			"set BORGE_NEW_PASSPHRASE")
	}
	again, _, err := e.promptPassphrase("Enter it again: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the two passphrases do not match; nothing was changed")
	}
	if first == "" {
		// An empty passphrase is technically allowed and almost never meant. borg warns;
		// borge refuses at a prompt, where it is certainly a mistake, and still allows it
		// through BORGE_NEW_PASSPHRASE for the scripts that genuinely want it.
		return "", errors.New("an empty passphrase would leave the key unprotected; " +
			"set BORGE_NEW_PASSPHRASE= explicitly if that is really what you want")
	}
	return first, nil
}
