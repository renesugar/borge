// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the export-tar command in borg's
// src/borg/archiver/tar_cmds.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"bufio"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/cache"
	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// cmdImportTar creates an archive from a tar stream.
//
// It is the inverse of export-tar, and the pair is how an archive moves between
// repositories that cannot talk to each other - or into borge from anything at all that
// can produce a tar.
//
// The round trip is only exact with --tar-format=BORG. PAX carries what tar has a concept
// of; BORG carries the item itself. See internal/archive/tar_import.go.
func cmdImportTar(e *Env, args []string) int {
	fs := newFlagSet(e, "import-tar")
	var common commonFlags
	common.register(fs)
	common.registerJSON(fs, "output stats as JSON (implies --stats)")
	comment := fs.String("comment", "", "a comment to store with the archive")
	compression := fs.String("C", "lz4", "compression spec, e.g. zstd,3")
	fs.StringVar(compression, "compression", "lz4", "compression spec")
	chunkerParams := fs.String("chunker-params", "", "chunker parameters, e.g. fastcdc,19,23,21,2")
	ignoreZeros := fs.Bool("ignore-zeros", false,
		"keep reading past the end-of-archive marker, for concatenated tars")
	list := fs.Bool("list", false, "print each item as it is imported")
	stats := fs.Bool("stats", false, "print statistics when finished")
	var timestamp timestampFlag
	timestamp.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	// borg: "output stats as JSON (implies --stats)".
	if common.json {
		*stats = true
	}
	if fs.NArg() < 2 {
		e.errorf("import-tar needs an archive name and an input file (or - for stdin)")
		return ExitError
	}
	source := fs.Arg(1)
	name, err := e.expand(fs.Arg(0))
	if err != nil {
		return e.fail(err)
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

	// The reader stack, innermost last: tar <- optional gzip <- buffer <- file.
	var in io.Reader = e.Stdin
	if source == "-" && in == nil {
		e.errorf("import-tar was given - for the input, but this process has no stdin")
		return ExitError
	}
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return e.fail(err)
		}
		defer file.Close()
		in = file
	}
	in = bufio.NewReaderSize(in, 1<<20)
	if strings.HasSuffix(source, ".gz") || strings.HasSuffix(source, ".tgz") {
		gz, err := gzip.NewReader(in)
		if err != nil {
			return e.fail(err)
		}
		defer gz.Close()
		in = gz
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}

	// Exclusive, for the same reason create is: this writes packs, the archive directory
	// and the manifest.
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

	cwd, _ := os.Getwd()
	status := ExitOK
	st, id, err := archive.ImportTar(m, in, archive.ImportTarOptions{
		Name:          name,
		Comment:       comm,
		ChunkerParams: params,
		ChunkSeed:     chunkSeed,
		Compressor:    compressor,
		IgnoreZeros:   *ignoreZeros,
		Timestamp:     timestamp.value(),
		CommandLine:   "borge import-tar " + strings.Join(args, " "),
		CWD:           cwd,
		OnItem: func(s byte, p string) {
			if *list {
				fmt.Fprintf(e.Stdout, "%c %s\n", s, p)
			}
		},
		OnWarning: func(p, reason string) {
			e.warnf("%s: %s", p, reason)
			status = ExitWarning
		},
	})
	if err != nil {
		return e.fail(err)
	}
	if err := m.Write(); err != nil {
		return e.fail(err)
	}

	if common.json {
		cacheDir, err := cache.Dir(repo.ID())
		if err != nil {
			return e.fail(err)
		}
		doc := archiveCreatedJSON(repo, k, m, path, cacheDir, st.Meta, id,
			createStatsJSON(int64(st.Files), st.Bytes, st.FileStatus))
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(doc); err != nil {
			return e.fail(err)
		}
		return status
	}

	if *stats || common.verbose {
		fmt.Fprintf(e.Stdout, "imported %d item(s), %d file(s), %d hard link(s), %d bytes; "+
			"%d skipped\narchive %s: %s\n",
			st.Items, st.Files, st.Hardlinks, st.Bytes, st.Skipped,
			name, hex.EncodeToString(id))
	}
	return status
}

// cmdExportTar writes an archive as a tar stream.
func cmdExportTar(e *Env, args []string) int {
	fs := newFlagSet(e, "export-tar")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	format := fs.String("tar-format", "PAX",
		"PAX (xattrs, ACLs, sub-second times), BORG (PAX plus the whole item) or GNU")
	strip := fs.Int("strip-components", 0, "remove this many leading path components")
	list := fs.Bool("list", false, "print each item as it is exported")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 2 {
		e.errorf("export-tar needs an archive and an output file (or - for stdout)")
		return ExitError
	}
	archiveName, target := fs.Arg(0), fs.Arg(1)

	var tarFormat archive.TarFormat
	switch strings.ToUpper(*format) {
	case "PAX", "":
		tarFormat = archive.TarPAX
	case "GNU":
		tarFormat = archive.TarGNU
	case "BORG":
		tarFormat = archive.TarBORG
	default:
		e.errorf("--tar-format must be PAX, BORG or GNU, not %q", *format)
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false, manifest.OpRead)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, archiveName)
	if err != nil {
		return e.fail(err)
	}
	matcher, err := pf.matcher(fs.Args()[2:])
	if err != nil {
		return e.fail(err)
	}

	// The writer stack, outermost last: tar -> optional gzip -> buffer -> file.
	var file *os.File
	if target != "-" {
		file, err = os.Create(target)
		if err != nil {
			return e.fail(err)
		}
		defer file.Close()
	}
	var sink io.Writer = e.Stdout
	if file != nil {
		sink = file
	}
	buffered := bufio.NewWriterSize(sink, 1<<20)

	var out io.Writer = buffered
	var gz *gzip.Writer
	// A ".gz" name means the user wants it compressed. Deciding from the name rather than
	// a flag is what every other tar tool does.
	if strings.HasSuffix(target, ".gz") || strings.HasSuffix(target, ".tgz") {
		gz = gzip.NewWriter(buffered)
		out = gz
	}

	status := ExitOK
	stats, err := a.ExportTar(out, archive.TarOptions{
		Format:          tarFormat,
		StripComponents: *strip,
		Filter:          func(it *item.Item) bool { return matcher.Match(it.Path) },
		OnItem: func(path string) {
			if *list {
				// stderr, not stdout: "export-tar --list ARCHIVE -" writes the tar to
				// stdout, and a listing mixed into it produces a corrupt archive. borg
				// puts every listing on stderr for this reason. Measured: borge wrote the
				// paths into the tar and "tar -t" rejected the result.
				fmt.Fprintln(e.Stderr, path)
			}
		},
		OnWarning: func(p, reason string) {
			e.warnf("%s: %s", p, reason)
			status = ExitWarning
		},
	})
	if err != nil {
		return e.fail(err)
	}

	// Closed innermost first: gzip's trailer has to reach the buffer before the buffer is
	// flushed, or the output is a truncated stream that looks complete.
	if gz != nil {
		if err := gz.Close(); err != nil {
			return e.fail(err)
		}
	}
	if err := buffered.Flush(); err != nil {
		return e.fail(err)
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return e.fail(err)
		}
	}

	if common.verbose {
		fmt.Fprintf(e.Stdout, "exported %d item(s), %d file(s), %d hard link(s), %d bytes; %d skipped\n",
			stats.Items, stats.Files, stats.Hardlinks, stats.Bytes, stats.Skipped)
	}
	return status
}
