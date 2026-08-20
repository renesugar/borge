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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/cache"
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
			fmt.Fprintf(e.Stderr, "Key stored in %s\n", blob.Path)
			fmt.Fprintln(e.Stderr, "Keep this file safe. Your data will be inaccessible without it.")
		}
	} else {
		k, err = key.ByName(mode, nil, nil)
		if err != nil {
			return e.fail(err)
		}
		fmt.Fprintln(e.Stderr, "Encryption is NOT enabled: this repository authenticates nothing.")
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

	fmt.Fprintf(e.Stderr, "Repository created: %s\n", path)
	fmt.Fprintf(e.Stderr, "Encryption: %s\n", k.Name())
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
	common.registerJSON(fs, "output stats as JSON (implies --stats)")
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
	excludeCaches := fs.Bool("exclude-caches", false,
		"skip directories holding a CACHEDIR.TAG")
	var excludeIfPresent multiFlag
	fs.Var(&excludeIfPresent, "exclude-if-present", "skip directories holding this file (repeatable)")
	keepExcludeTags := fs.Bool("keep-exclude-tags", false,
		"archive the excluded directory and the tag files that excluded it")
	var timestamp timestampFlag
	timestamp.register(fs)
	var pathsFrom pathsFromFlags
	pathsFrom.register(fs)
	var stdin stdinFlags
	stdin.register(fs)
	storeATime := fs.Bool("atime", false, "store each item's access time")
	noCTime := fs.Bool("noctime", false, "do not store the inode change time")
	noBirthTime := fs.Bool("nobirthtime", false, "do not store the creation time")
	dryRun := fs.Bool("dry-run", false, "say what would be archived, archive nothing")
	fs.BoolVar(dryRun, "n", false, "say what would be archived, archive nothing")
	filesCache := fs.String("files-cache", "", "files cache mode, e.g. ctime,size,inode or disabled")
	list := fs.Bool("list", false, "print each item as it is archived")
	stats := fs.Bool("stats", false, "print statistics when finished")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	// borg's help says so outright: "output stats as JSON. Implies --stats." The
	// implication matters because the text summary is what --stats gates, and a caller
	// asking for the numbers in JSON is asking for the numbers.
	if common.json {
		*stats = true
	}
	// "R PATH" lines in a patterns file are paths to back up, so they count towards
	// having something to do: borg accepts a create whose only root came from a patterns
	// file, and borge used to refuse it (docs/DIVERGENCES.md #25).
	roots, err := pf.roots()
	if err != nil {
		return e.fail(err)
	}
	if fs.NArg() < 1 {
		e.errorf("create needs an archive name")
		return ExitError
	}
	if err := pathsFrom.check(fs.Args()[1:]); err != nil {
		return e.fail(err)
	}
	streamOpts, err := stdin.options()
	if err != nil {
		return e.fail(err)
	}
	if stdin.fromCmd {
		if pathsFrom.any() {
			e.errorf("--content-from-command reads the content of one file from a " +
				"command; it cannot also take its list of paths from one")
			return ExitError
		}
		if fs.NArg() < 2 {
			e.errorf("no command given; the arguments after the archive name are the " +
				"command whose output becomes the file's content")
			return ExitError
		}
	}
	if !pathsFrom.any() && fs.NArg() < 2 && len(roots) == 0 {
		e.errorf("create needs at least one path, on the command line or as an " +
			"\"R PATH\" line in a --patterns-from file")
		return ExitError
	}
	// borg accepts both of these silently and ignores them, which leaves a user believing
	// a filter applied. Saying so costs a line on stderr; see PORTING_PLAN.md §2.3.
	if pathsFrom.any() && pf.any() {
		e.warnf("the include/exclude options do not apply to paths read from a list: " +
			"the list is taken as given")
	}
	if pathsFrom.delimiterSet && !pathsFrom.any() {
		e.warnf("--paths-delimiter does nothing without --paths-from-stdin, " +
			"--paths-from-command or --paths-from-shell-command")
	}
	name, err := e.expand(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	// Roots first, then the command line, as borg orders them.
	paths := append(append([]string{}, roots...), fs.Args()[1:]...)
	if pathsFrom.any() {
		// The positionals were the command, not paths; the list replaces them entirely.
		paths, err = pathsFrom.read(e, fs.Args()[1:])
		if err != nil {
			return e.fail(err)
		}
	}

	comm, err := e.expand(*comment)
	if err != nil {
		return e.fail(err)
	}

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

	// The files cache spares unchanged files from being read. It is per archive *name*,
	// so a nightly "daily" backup reuses last night's; a differently named archive is a
	// different working set and would only evict it.
	cacheMode := cache.DefaultMode()
	if *filesCache != "" {
		cacheMode, err = cache.ParseMode(*filesCache)
		if err != nil {
			return e.fail(err)
		}
	}
	cachePath, err := cache.Path(repo.ID(), name)
	if err != nil {
		return e.fail(err)
	}
	files, err := cache.Read(cachePath, cacheMode, b.Start().UnixNano())
	if err != nil {
		return e.fail(err)
	}

	status := ExitOK
	// A "-" among the paths is standard input's *content*, not a path to walk, and
	// --content-from-command replaces the paths entirely with a command to run. Both
	// become one item; borg handles them in the same loop that walks the rest, which is
	// why they can be mixed with real paths.
	streams, paths, err := splitStreams(e, paths, stdin.fromCmd, fs.Args()[1:])
	if err != nil {
		return e.fail(err)
	}

	opts := archive.CreateOptions{
		Paths:         paths,
		Matcher:       matcher,
		OneFileSystem: *oneFileSystem,
		NumericIDs:    *numericIDs,
		NoXAttrs:      *noXAttrs,
		NoACLs:        *noACLs,
		NoFlags:       *noFlags,
		ReadSpecial:   *readSpecial,
		DryRun:        *dryRun,
		PathsOnly:     pathsFrom.any(),
		StoreATime:    *storeATime,
		NoCTime:       *noCTime,
		NoBirthTime:   *noBirthTime,
		ExcludeCaches: *excludeCaches,
		// A copy: the option value outlives this call in the caller's flag set, and a
		// walker holding the same slice would see a later change.
		ExcludeIfPresent: append([]string(nil), excludeIfPresent...),
		KeepExcludeTags:  *keepExcludeTags,
		Files:            files,
		OnError: func(p string, err error) error {
			// One unreadable file does not abandon the backup: the rest is still worth
			// having, and the exit code says something was missed.
			e.warnf("%s: %v", p, err)
			status = ExitWarning
			return nil
		},
	}
	if *list {
		// On stderr, where borg puts it. Measured rather than assumed: "borg create
		// --list --stats" writes nothing whatever to stdout - the listing and the summary
		// are both log output. borge wrote both to stdout until 2026-08-18, which was
		// merely different until --json arrived and made it corrupting: a listing line
		// ahead of the document is a parse error for the frontend reading it.
		opts.OnItem = func(st byte, p string) { e.logFileStatus(st, p) }
	}

	for _, stream := range streams {
		if *dryRun {
			// Nothing is read: a dry run must not drain a pipe it is not going to store,
			// because the data would be gone for the real run.
			fmt.Fprintf(e.Stderr, "+ %s\n", streamOpts.Name)
			continue
		}
		r, cleanup, err := stream(e)
		if err != nil {
			return e.fail(err)
		}
		_, err = b.AddStream(r, streamOpts)
		if cleanupErr := cleanup(); err == nil {
			err = cleanupErr
		}
		if err != nil {
			return e.fail(err)
		}
	}

	// With only a stream to archive there is nothing to walk, and Create refuses an empty
	// path list - rightly, since for every other caller that means a mistake.
	created := &archive.CreateStats{}
	if len(paths) > 0 {
		created, err = b.Create(opts)
		if err != nil {
			return e.fail(err)
		}
	} else {
		created.Stats = b.Stats()
	}

	// A dry run stops here. Nothing was read and nothing was stored, so there is no
	// archive to save, no manifest to rewrite and no files cache to update - a cache
	// recording a backup that did not happen would make the *next* real run skip the
	// files it claims are already there.
	if *dryRun {
		if common.json {
			// borg's dry-run document is a different shape from its real one: no
			// "archive" and no "cache" - neither exists - and a "dry_run": true that
			// says why. A frontend seeing "archive" absent has an explicit reason for
			// it rather than a missing key to guess about.
			repoBlock, encBlock := envelopeFor(repo, k, m, path)
			enc := json.NewEncoder(e.Stdout)
			enc.SetIndent("", "    ")
			if err := enc.Encode(map[string]any{
				"dry_run": true,
				"stats": map[string]any{
					"nfiles":        created.Stats.NFiles,
					"original_size": created.Stats.OriginalSize,
				},
				"repository": repoBlock,
				"encryption": encBlock,
			}); err != nil {
				return e.fail(err)
			}
			return status
		}
		if *stats || common.verbose {
			s := created.Stats
			fmt.Fprintf(e.Stderr, "Number of files: %d\n", s.NFiles)
			fmt.Fprintf(e.Stderr, "Original size: %d\n", s.OriginalSize)
		}
		return status
	}

	// The cache is saved only after the archive is complete: an interrupted backup must
	// not leave a cache claiming files were stored when they were not.
	if err := files.Save(cachePath); err != nil {
		e.warnf("could not save the files cache: %v", err)
		status = ExitWarning
	}

	cwd, _ := os.Getwd()
	meta, id, err := b.Save(archive.SaveOptions{
		// Zero means "use the start time", which is borg's default: an archive is dated
		// when it began, not when it finished.
		Timestamp:   timestamp.value(),
		Name:        name,
		Comment:     comm,
		CommandLine: commandLine(args),
		CWD:         cwd,
	})
	if err != nil {
		return e.fail(err)
	}
	// The counts are re-read from the builder here, after Save, because that is where
	// borg reads its own: the archive's "size" is sampled mid-save (after the item
	// pointers, before the archive object) and what create reports is that plus the
	// archive object. Reading the snapshot Create() returned would report the state
	// before any of the three were written - and worse, would report a *different* rule
	// for a large backup than a small one, since a long walk flushes the item stream
	// part-way and a short one does not. See docs/DIVERGENCES.md #36.
	created.Stats = b.Stats()

	if common.json {
		// The cache block names the directory, not the per-archive file: borg reports
		// Cache.path, which is the repository's cache directory.
		cacheDir, err := cache.Dir(repo.ID())
		if err != nil {
			return e.fail(err)
		}
		doc := archiveCreatedJSON(repo, k, m, path, cacheDir, meta, id,
			createStatsJSON(created.Stats.NFiles, created.Stats.OriginalSize, created.FileStatus))
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(doc); err != nil {
			return e.fail(err)
		}
		return status
	}

	if *stats || common.verbose {
		s := created.Stats
		fmt.Fprintf(e.Stderr, "Archive name: %s\n", name)
		fmt.Fprintf(e.Stderr, "Archive fingerprint: %x\n", id)
		fmt.Fprintf(e.Stderr, "Number of files: %d\n", s.NFiles)
		fmt.Fprintf(e.Stderr, "Original size: %d\n", s.OriginalSize)
		fmt.Fprintf(e.Stderr, "Deduplicated size: %d\n", s.DedupedSize)
		fmt.Fprintf(e.Stderr, "Chunks: %d (%d new)\n", s.Chunks, s.NewChunks)
		hits, misses := files.Stats()
		fmt.Fprintf(e.Stderr, "Files cache: %s, %d unchanged, %d read (%d hits, %d misses)\n",
			cacheMode, created.Unchanged, s.NFiles-int64(created.Unchanged), hits, misses)
		if created.Errors > 0 {
			fmt.Fprintf(e.Stderr, "Errors: %d\n", created.Errors)
		}
		if created.Skipped > 0 {
			fmt.Fprintf(e.Stderr, "Skipped: %d\n", created.Skipped)
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
