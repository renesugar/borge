// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/repo_create_cmd.py and
// create_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// cmdRepoCreate makes a new repository.
func cmdRepoCreate(e *Env, args []string) int {
	fs := newFlagSet(e, "repo-create")
	var common commonFlags
	common.register(fs)
	encryption := fs.String("e", "", "encryption mode (required)")
	fs.StringVar(encryption, "encryption", "", "encryption mode (required)")
	idHash := fs.String("i", "", "id hash for the encrypted modes: sha256 or blake3")
	fs.StringVar(idHash, "id-hash", "", "id hash for the encrypted modes")
	location := fs.String("key-location", "repokey", "where to keep the key: repokey or keyfile")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *encryption == "" {
		e.errorf("repo-create needs --encryption; see 'borge repo-create -h'")
		return ExitError
	}

	mode, err := resolveEncryptionMode(*encryption, *idHash)
	if err != nil {
		return e.fail(err)
	}
	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}

	repo, err := repository.Create(path, repository.Options{Exclusive: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	var k key.Key
	if key.RequiresKeyMaterial(modeTypeByte(mode)) {
		storage := key.StorageRepo
		switch *location {
		case "repokey":
		case "keyfile":
			storage = key.StorageKeyfile
		default:
			return e.fail(fmt.Errorf("--key-location must be repokey or keyfile, not %q", *location))
		}

		material, err := key.NewMaterial(repo.ID())
		if err != nil {
			return e.fail(err)
		}
		m, err := repo.KeyManager()
		if err != nil {
			return e.fail(err)
		}
		blob, err := m.Save(material, e.passphrase(), key.SaveOptions{
			Storage: storage,
			Label:   key.AdminLabel,
			Create:  true,
		})
		if err != nil {
			return e.fail(err)
		}
		k, err = key.FromMaterial(mode, material)
		if err != nil {
			return e.fail(err)
		}
		if storage == key.StorageKeyfile {
			fmt.Fprintf(e.Stdout, "Key stored in %s\n", blob.Path)
			fmt.Fprintln(e.Stdout, "Keep this file safe. Your data will be inaccessible without it.")
		}
	} else {
		k, err = key.ByName(mode, nil, nil)
		if err != nil {
			return e.fail(err)
		}
		fmt.Fprintln(e.Stdout, "Encryption is NOT enabled: this repository authenticates nothing.")
	}

	// A new repository still needs a manifest: it is what every later command reads
	// first, and it is what identifies the encryption mode.
	m, err := manifest.Create(repo, k)
	if err != nil {
		return e.fail(err)
	}
	if err := m.Write(); err != nil {
		return e.fail(err)
	}

	fmt.Fprintf(e.Stdout, "Repository created: %s\n", path)
	fmt.Fprintf(e.Stdout, "Encryption: %s\n", k.Name())
	return ExitOK
}

// resolveEncryptionMode turns borg's (--encryption, --id-hash) pair into borge's single
// mode name.
//
// borg's --encryption is not unique on its own: "aes256-ocb" is two different key classes
// depending on --id-hash. The modes that do not encrypt name their hash themselves, and
// giving --id-hash as well is only accepted if the two agree.
func resolveEncryptionMode(encryption, idHash string) (string, error) {
	switch encryption {
	case "aes256-ocb", "chacha20-poly1305":
		switch idHash {
		case "", "sha256":
			return encryption, nil
		case "blake3":
			return "blake3-" + encryption, nil
		default:
			return "", fmt.Errorf("--id-hash must be sha256 or blake3, not %q", idHash)
		}
	case "none-sha256", "authenticated-sha256":
		if idHash != "" && idHash != "sha256" {
			return "", fmt.Errorf("%q always uses sha256, so --id-hash %q cannot be used with it",
				encryption, idHash)
		}
		return encryption, nil
	case "none-blake3", "authenticated-blake3":
		if idHash != "" && idHash != "blake3" {
			return "", fmt.Errorf("%q always uses blake3, so --id-hash %q cannot be used with it",
				encryption, idHash)
		}
		return encryption, nil
	default:
		return "", fmt.Errorf("unknown encryption mode %q", encryption)
	}
}

// modeTypeByte maps a mode name to its key type byte, for the "does this need a stored
// key" question.
func modeTypeByte(mode string) byte {
	switch mode {
	case "none-sha256":
		return key.TypeSHA256None
	case "none-blake3":
		return key.TypeBlake3None
	case "authenticated-sha256":
		return key.TypeSHA256Authenticated
	case "authenticated-blake3":
		return key.TypeBlake3Authenticated
	case "aes256-ocb":
		return key.TypeAESOCB
	case "chacha20-poly1305":
		return key.TypeCHPO
	case "blake3-aes256-ocb":
		return key.TypeBlake3AESOCB
	default:
		return key.TypeBlake3CHPO
	}
}

// cmdCreate backs up paths into a new archive.
func cmdCreate(e *Env, args []string) int {
	fs := newFlagSet(e, "create")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	comment := fs.String("comment", "", "a comment to store with the archive")
	compression := fs.String("C", "lz4", "compression spec, e.g. zstd,3")
	fs.StringVar(compression, "compression", "lz4", "compression spec")
	chunkerParams := fs.String("chunker-params", "", "chunker parameters, e.g. fastcdc,19,23,21,2")
	oneFileSystem := fs.Bool("one-file-system", false, "do not cross mount points")
	numericIDs := fs.Bool("numeric-ids", false, "store numeric uid/gid only")
	noXAttrs := fs.Bool("noxattrs", false, "do not store extended attributes")
	noACLs := fs.Bool("noacls", false, "do not store ACLs")
	noFlags := fs.Bool("noflags", false, "do not store file flags")
	readSpecial := fs.Bool("read-special", false, "read the contents of fifos and devices")
	list := fs.Bool("list", false, "print each item as it is archived")
	stats := fs.Bool("stats", false, "print statistics when finished")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 2 {
		e.errorf("create needs an archive name and at least one path")
		return ExitError
	}
	name := fs.Arg(0)
	paths := fs.Args()[1:]

	compressor, err := compress.FromSpec(*compression)
	if err != nil {
		return e.fail(err)
	}
	params := chunker.DefaultParams()
	if *chunkerParams != "" {
		params, err = chunker.ParseParams(*chunkerParams)
		if err != nil {
			return e.fail(err)
		}
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}

	// Exclusive: creating an archive writes packs, the archive directory and the
	// manifest, and two writers doing that at once would interleave badly.
	repo, err := repository.Open(path, repository.Options{Exclusive: true})
	if err != nil {
		return e.fail(err)
	}
	defer repo.Close()

	k, unlocked, err := repo.Unlock(e.passphrase())
	if err != nil {
		if errors.Is(err, key.ErrPassphraseWrong) {
			return e.fail(fmt.Errorf("%w (set BORGE_PASSPHRASE or BORG_PASSPHRASE)", err))
		}
		return e.fail(err)
	}
	m, err := manifest.Load(repo, k, manifest.OpWrite)
	if err != nil {
		return e.fail(err)
	}

	var chunkSeed uint32
	if unlocked != nil {
		chunkSeed = uint32(key.ChunkSeed(unlocked.Material))
	}

	b, err := archive.NewBuilder(m, archive.BuilderOptions{
		ChunkerParams: params,
		ChunkSeed:     chunkSeed,
		Compressor:    compressor,
	})
	if err != nil {
		return e.fail(err)
	}

	matcher, err := pf.matcher(nil)
	if err != nil {
		return e.fail(err)
	}

	status := ExitOK
	opts := archive.CreateOptions{
		Paths:         paths,
		Matcher:       matcher,
		OneFileSystem: *oneFileSystem,
		NumericIDs:    *numericIDs,
		NoXAttrs:      *noXAttrs,
		NoACLs:        *noACLs,
		NoFlags:       *noFlags,
		ReadSpecial:   *readSpecial,
		OnError: func(p string, err error) error {
			// One unreadable file does not abandon the backup: the rest is still worth
			// having, and the exit code says something was missed.
			e.warnf("%s: %v", p, err)
			status = ExitWarning
			return nil
		},
	}
	if *list {
		opts.OnItem = func(st byte, p string) { fmt.Fprintf(e.Stdout, "%c %s\n", st, p) }
	}

	created, err := b.Create(opts)
	if err != nil {
		return e.fail(err)
	}

	cwd, _ := os.Getwd()
	meta, id, err := b.Save(archive.SaveOptions{
		Name:        name,
		Comment:     *comment,
		CommandLine: commandLine(args),
		CWD:         cwd,
	})
	if err != nil {
		return e.fail(err)
	}
	_ = meta

	if *stats || common.verbose {
		s := created.Stats
		fmt.Fprintf(e.Stdout, "Archive name: %s\n", name)
		fmt.Fprintf(e.Stdout, "Archive fingerprint: %x\n", id)
		fmt.Fprintf(e.Stdout, "Number of files: %d\n", s.NFiles)
		fmt.Fprintf(e.Stdout, "Original size: %d\n", s.OriginalSize)
		fmt.Fprintf(e.Stdout, "Deduplicated size: %d\n", s.DedupedSize)
		fmt.Fprintf(e.Stdout, "Chunks: %d (%d new)\n", s.Chunks, s.NewChunks)
		if created.Errors > 0 {
			fmt.Fprintf(e.Stdout, "Errors: %d\n", created.Errors)
		}
		if created.Skipped > 0 {
			fmt.Fprintf(e.Stdout, "Skipped: %d\n", created.Skipped)
		}
	}
	return status
}

// commandLine renders what produced the archive, for the metadata.
//
// It is "borge" plus the arguments as given, not os.Args: the point is a line a person
// can read and rerun, and the absolute path of the binary is noise.
func commandLine(args []string) string {
	return "borge create " + strings.Join(args, " ")
}
