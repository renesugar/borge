// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the key commands in borg's
// src/borg/archiver/key_cmds.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/repository"
)

// The key commands.
//
// # Why this arrived last
//
// The library underneath was finished and gated at docs/PORTING_PLAN.md §1.3, paper key
// included, and the gate text there says "borge key export / borg key import cross-check
// in both directions" - which was true of a *library* test and of nothing a user could
// run. The command was simply never written, and nothing noticed until
// tests/evidence/command-coverage.sh compared borge's command list against borg's. That
// is the whole argument for having such a gate.
//
// # Passphrases
//
// The environment is tried first and a terminal is asked only when that fails; see
// passphrase.go for why that order rather than prompting up front.
//
// The two commands that need a *second* passphrase read BORGE_NEW_PASSPHRASE, falling back
// to BORG_NEW_PASSPHRASE, which is the variable borg uses for the same purpose. With
// neither set and no terminal to ask at they say so and stop, rather than proceeding with
// an empty passphrase - silently protecting a key with "" would be a security failure that
// looks like success.

func keyCommands() []command {
	return []command{
		{"list", "list the repository's keys", cmdKeyList},
		{"export", "write a key out for safekeeping", cmdKeyExport},
		{"import", "restore a key from a backup", cmdKeyImport},
		{"change-passphrase", "change a key's passphrase", cmdKeyChangePassphrase},
		{"change-location", "move a key between repokey and keyfile storage", cmdKeyChangeLocation},
		{"add", "add a key with an independent passphrase", cmdKeyAdd},
		{"remove", "remove a key", cmdKeyRemove},
	}
}

func cmdKey(e *Env, args []string) int {
	// --log-json may be given to the group as well as to the subcommand; borg
	// accepts it in both places. See takeParentLogJSON.
	args = e.takeParentLogJSON("key", args)
	if len(args) == 0 {
		printKeyUsage(e.Stdout)
		return ExitOK
	}
	name := args[0]
	for _, c := range keyCommands() {
		if c.name == name {
			return c.run(e, args[1:])
		}
	}
	e.errorf("unknown key command %q", name)
	printKeyUsage(e.Stderr)
	return ExitError
}

func printKeyUsage(w io.Writer) {
	var b strings.Builder
	for _, c := range keyCommands() {
		fmt.Fprintf(&b, "  %-18s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "usage: borge key <command> [options]\n\ncommands:\n%s", b.String())
}

// openKeys opens a repository far enough to manage its keys: no manifest, no lock.
//
// No lock, because borg takes none either for export and import, and because a user
// locked out of a repository by a lost passphrase still has to be able to import the key
// that gets them back in. The commands that write pass exclusive=true.
func (e *Env) openKeys(path string, exclusive bool) (*repository.Repository, *key.Manager, error) {
	repo, err := repository.Open(path, repository.Options{Exclusive: exclusive, NoLock: !exclusive})
	if err != nil {
		return nil, nil, err
	}
	mgr, err := repo.KeyManager()
	if err != nil {
		repo.Close()
		return nil, nil, err
	}
	return repo, mgr, nil
}

// unlockOrFail opens the current key, prompting when the environment did not supply a
// working passphrase and there is a terminal to ask at.
func (e *Env) unlockOrFail(mgr *key.Manager, repoPath string) (*key.Unlocked, error) {
	return e.unlockKeyManagerWithPrompt(mgr, repoPath)
}

// ---------------------------------------------------------------- list

func cmdKeyList(e *Env, args []string) int {
	fs := newFlagSet(e, "key list")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, mgr, err := e.openKeys(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	blobs, err := mgr.List()
	if err != nil {
		return e.fail(err)
	}
	if len(blobs) == 0 {
		fmt.Fprintf(e.Stdout, "this repository has no keys "+
			"(the none-* modes have no key material to store)\n")
		return ExitOK
	}

	// The current key is the one this passphrase opens, marked with an asterisk as borg
	// does. A wrong or absent passphrase is not an error here: listing what keys exist is
	// useful precisely when you are not sure which passphrase you hold.
	current := ""
	if u, err := mgr.Unlock(e.passphrase()); err == nil {
		current = u.Blob.ID
	}

	fmt.Fprintf(e.Stdout, "%-1s %-12s %-8s %-24s %s\n", "", "KEY ID", "MODE", "LABEL", "ALGORITHM")
	for _, b := range blobs {
		marker := ""
		if b.ID == current {
			marker = "*"
		}
		label, algorithm := b.Label, b.Algorithm
		if label == "" {
			label = "-"
		}
		if algorithm == "" {
			algorithm = "-"
		}
		if b.Corrupt {
			// A key that cannot be parsed is still listed. Hiding it would leave the user
			// unable to see - or remove - the thing that is wrong.
			algorithm = "UNREADABLE"
		}
		fmt.Fprintf(e.Stdout, "%-1s %-12s %-8s %-24s %s\n",
			marker, shortKeyID(b.ID), b.Storage, label, algorithm)
	}
	return ExitOK
}

// shortKeyID is the first twelve characters, which is what borg prints and enough to name
// a key among the handful a repository has.
func shortKeyID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---------------------------------------------------------------- export

func cmdKeyExport(e *Env, args []string) int {
	fs := newFlagSet(e, "key export")
	var common commonFlags
	common.register(fs)
	paper := fs.Bool("paper", false, "print a human-readable key for writing down")
	qrHTML := fs.Bool("qr-html", false, "write a printable HTML page with a QR code")
	label := fs.String("label", "", "export the key with this label")
	keyID := fs.String("key", "", "export the key with this id prefix")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *paper && *qrHTML {
		e.errorf("--paper and --qr-html are two different formats; pick one")
		return ExitError
	}
	target := ""
	if fs.NArg() > 1 {
		e.errorf("key export takes at most one output path")
		return ExitError
	}
	if fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	selector := *label
	if selector == "" {
		selector = *keyID
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, mgr, err := e.openKeys(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	blob, err := mgr.Export(selector)
	if err != nil {
		return e.fail(err)
	}

	var out []byte
	switch {
	case *paper:
		text, err := key.ExportPaperKey(blob, mgr.RepoIDHex())
		if err != nil {
			return e.fail(err)
		}
		out = []byte(text)
	case *qrHTML:
		out, err = key.ExportPaperKeyHTML(blob)
		if err != nil {
			return e.fail(err)
		}
	default:
		out = blob.Text
	}

	// A directory where a file was meant is worth its own message: os.WriteFile's error
	// says "is a directory" without saying which argument was wrong.
	if target != "" && target != "-" {
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			e.errorf("%s is a directory; key export needs a file to write", target)
			return ExitError
		}
		if err := os.WriteFile(target, out, 0o600); err != nil {
			return e.fail(err)
		}
	} else {
		if _, err := e.Stdout.Write(out); err != nil {
			return e.fail(err)
		}
	}

	name := blob.Label
	if name == "" {
		name = "unlabelled"
	}
	e.warnf("exported key %s (label %q) - this file is as good as the repository's "+
		"contents to whoever holds the passphrase", shortKeyID(blob.ID), name)
	return ExitOK
}

// ---------------------------------------------------------------- import

func cmdKeyImport(e *Env, args []string) int {
	fs := newFlagSet(e, "key import")
	var common commonFlags
	common.register(fs)
	paper := fs.Bool("paper", false, "read a paper key instead of an exported file")
	location := fs.String("key-location", "", "where to store it: repokey or keyfile "+
		"(default: where this repository's keys already live)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() > 1 {
		e.errorf("key import takes at most one input path")
		return ExitError
	}
	source := ""
	if fs.NArg() == 1 {
		source = fs.Arg(0)
	}
	if source == "" && !*paper {
		e.errorf("key import needs a file to read (or - for stdin)")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	// Exclusive: importing writes a key, and two imports racing could each believe they
	// stored the only copy.
	repo, mgr, err := e.openKeys(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	text, err := e.readKeyInput(source)
	if err != nil {
		return e.fail(err)
	}
	if *paper {
		text, err = key.ImportPaperKey(string(text), mgr.RepoIDHex())
		if err != nil {
			return e.fail(err)
		}
	}

	storage, err := keyStorage(*location, mgr)
	if err != nil {
		return e.fail(err)
	}
	// A key for the wrong repository is refused by Import, and its error names both ids -
	// the most common mistake with several repositories, and unhelpful without them.
	blob, err := mgr.Import(text, storage)
	if err != nil {
		return e.fail(err)
	}
	where := blob.Path
	if where == "" {
		where = "the repository's keys/ namespace"
	}
	fmt.Fprintf(e.Stdout, "imported key %s into %s\n", shortKeyID(blob.ID), where)
	return ExitOK
}

func (e *Env) readKeyInput(source string) ([]byte, error) {
	if source == "" || source == "-" {
		if e.Stdin == nil {
			return nil, errors.New("no standard input to read the key from")
		}
		return io.ReadAll(e.Stdin)
	}
	return os.ReadFile(source)
}

// keyStorage resolves --key-location, defaulting to wherever this repository's keys
// already are.
//
// Defaulting to the existing location rather than to repokey matters: importing a key into
// a repository whose keys are deliberately kept outside it would quietly undo that choice
// and put the key next to the data it protects.
func keyStorage(given string, mgr *key.Manager) (key.Storage, error) {
	switch given {
	case "repokey":
		return key.StorageRepo, nil
	case "keyfile":
		return key.StorageKeyfile, nil
	case "":
	default:
		return "", fmt.Errorf("--key-location must be repokey or keyfile, not %q", given)
	}

	blobs, err := mgr.List()
	if err != nil {
		return "", err
	}
	for _, b := range blobs {
		if b.Storage == key.StorageKeyfile || b.Storage == key.StorageRepo {
			return b.Storage, nil
		}
	}
	return key.StorageRepo, nil
}

// ---------------------------------------------------------------- change-passphrase

func cmdKeyChangePassphrase(e *Env, args []string) int {
	fs := newFlagSet(e, "key change-passphrase")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, mgr, err := e.openKeys(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	u, err := e.unlockOrFail(mgr, path)
	if err != nil {
		return e.fail(err)
	}
	// Asked for after the current one is accepted, so the prompts come in the order the
	// user thinks about them: prove you hold the key, then choose its new secret.
	newPass, err := e.newPassphraseWithPrompt("this repository's key")
	if err != nil {
		return e.fail(err)
	}
	blob, err := mgr.ChangePassphrase(u, newPass)
	if err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stdout, "passphrase changed for key %s\n", shortKeyID(blob.ID))
	if blob.Path != "" {
		// Where the key now lives, so that backing it up is possible without hunting.
		fmt.Fprintf(e.Stdout, "key location: %s\n", blob.Path)
	}
	return ExitOK
}

// ---------------------------------------------------------------- change-location

func cmdKeyChangeLocation(e *Env, args []string) int {
	fs := newFlagSet(e, "key change-location")
	var common commonFlags
	common.register(fs)
	keep := fs.Bool("keep", false, "leave the key in its old location as well")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("key change-location needs a location: repokey or keyfile")
		return ExitError
	}
	var want key.Storage
	switch fs.Arg(0) {
	case "repokey":
		want = key.StorageRepo
	case "keyfile":
		want = key.StorageKeyfile
	default:
		e.errorf("the location must be repokey or keyfile, not %q", fs.Arg(0))
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, mgr, err := e.openKeys(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	u, err := e.unlockOrFail(mgr, path)
	if err != nil {
		return e.fail(err)
	}
	if u.Blob.Storage == want {
		fmt.Fprintf(e.Stdout, "the key is already stored as %s, nothing to do\n", want)
		return ExitOK
	}

	// Written before the old one is removed, so an interruption leaves two working keys
	// rather than none - the same order ChangePassphrase uses, and for the same reason.
	created, err := mgr.Save(u.Material, e.passphrase(), key.SaveOptions{
		Storage: want, Label: u.Blob.Label,
	})
	if err != nil {
		return e.fail(err)
	}
	if *keep {
		fmt.Fprintf(e.Stdout, "key copied to %s\n", keyLocationOf(created))
		return ExitOK
	}
	if err := mgr.Delete(u.Blob); err != nil {
		e.errorf("the key was copied to %s but the old one could not be removed: %v",
			keyLocationOf(created), err)
		return ExitError
	}
	fmt.Fprintf(e.Stdout, "key moved to %s\n", keyLocationOf(created))
	return ExitOK
}

func keyLocationOf(b key.Blob) string {
	if b.Path != "" {
		return b.Path
	}
	return "the repository's keys/ namespace"
}

// ---------------------------------------------------------------- add and remove

func cmdKeyAdd(e *Env, args []string) int {
	fs := newFlagSet(e, "key add")
	var common commonFlags
	common.register(fs)
	label := fs.String("label", "", "the new key's label (required)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *label == "" {
		e.errorf("key add needs --label: a second key has to be nameable to be removable")
		return ExitError
	}
	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, mgr, err := e.openKeys(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	u, err := e.unlockOrFail(mgr, path)
	if err != nil {
		return e.fail(err)
	}
	newPass, err := e.newPassphraseWithPrompt("the new key " + *label)
	if err != nil {
		return e.fail(err)
	}
	blob, err := mgr.AddKey(u, newPass, *label)
	if err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stdout, "added key %s (label %q) at %s\n",
		shortKeyID(blob.ID), *label, keyLocationOf(blob))
	return ExitOK
}

func cmdKeyRemove(e *Env, args []string) int {
	fs := newFlagSet(e, "key remove")
	var common commonFlags
	common.register(fs)
	label := fs.String("label", "", "remove the key with this label")
	keyID := fs.String("key", "", "remove the key with this id prefix")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	selector := *label
	if selector == "" {
		selector = *keyID
	}
	if selector == "" && fs.NArg() == 1 {
		selector = fs.Arg(0)
	}
	if selector == "" {
		e.errorf("key remove needs --label or --key: removing an unnamed key is not " +
			"something to do by default")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	repo, mgr, err := e.openKeys(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	blob, err := mgr.RemoveKey(selector)
	if err != nil {
		return e.fail(err)
	}
	label2 := blob.Label
	if label2 == "" {
		label2 = "unlabelled"
	}
	fmt.Fprintf(e.Stdout, "removed key %s (label %q)\n", shortKeyID(blob.ID), label2)
	return ExitOK
}
