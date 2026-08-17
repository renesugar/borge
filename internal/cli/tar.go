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
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
)

// cmdExportTar writes an archive as a tar stream.
func cmdExportTar(e *Env, args []string) int {
	fs := newFlagSet(e, "export-tar")
	var common commonFlags
	var pf patternFlags
	common.register(fs)
	pf.register(fs)
	format := fs.String("tar-format", "PAX", "PAX (carries xattrs, ACLs, sub-second times) or GNU")
	strip := fs.Int("strip-components", 0, "remove this many leading path components")
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
	default:
		e.errorf("--tar-format must be PAX or GNU, not %q", *format)
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
