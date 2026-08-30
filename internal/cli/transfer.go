// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/transfer_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/chunker"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/manifest"
)

// cmdTransfer copies archives from one repository into another.
//
// See internal/archive/transfer.go for what it does and why the destination has to be a
// related repository. This file is the command: the options, the guards and the reporting.
func cmdTransfer(e *Env, args []string) int {
	fs := newFlagSet(e, "transfer")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	sel.register(fs, selectorExtras{})
	otherRepo := fs.String("other-repo", "", "transfer archives from this repository")
	dryRun := fs.Bool("dry-run", false, "read and report what would be transferred, write nothing")
	fs.BoolVar(dryRun, "n", false, "read and report what would be transferred, write nothing")
	compression := fs.String("C", compress.Default, "compression for the metadata, and for content with --recompress always")
	fs.StringVar(compression, "compression", compress.Default, "compression spec")
	recompress := fs.String("recompress", "never", "recompress content chunks: never or always")
	chunkerParams := fs.String("chunker-params", "", "re-chunk the content with these parameters")
	fromBorg1 := fs.Bool("from-borg1", false, "the source is a borg 1.x repository (not supported)")
	upgrader := fs.String("upgrader", "NoOp", "conversion to apply: NoOp only")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	// The two borg 1.x doors, refused by name rather than as unknown options. Reading a
	// borg 1.x repository needs the borg 1 reader, which is a §0.6 non-goal - and a
	// transfer that silently did nothing about the format would produce a borg 2 archive
	// full of borg 1 metadata.
	if *fromBorg1 {
		e.errorf("--from-borg1 is not supported: borge does not read borg 1.x repositories " +
			"(plans/PORTING_PLAN.md §0.6). Transfer with borg itself, then use borge on the result.")
		return ExitError
	}
	if *upgrader != "NoOp" {
		e.errorf("--upgrader=%s is not supported: borge implements borg's NoOp upgrader only, "+
			"because the others convert borg 1.x data (plans/PORTING_PLAN.md §0.6)", *upgrader)
		return ExitError
	}

	mode := archive.RecompressMode(*recompress)
	if mode != archive.RecompressNever && mode != archive.RecompressAlways {
		e.errorf("--recompress must be never or always, not %q", *recompress)
		return ExitError
	}
	compressor, err := compress.FromSpec(*compression)
	if err != nil {
		return e.fail(err)
	}
	var params *chunker.Params
	if *chunkerParams != "" {
		p, err := chunker.ParseParams(*chunkerParams)
		if err != nil {
			return e.fail(err)
		}
		params = &p
	}

	dstPath, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	srcPath, err := e.resolveOtherRepo(*otherRepo)
	if err != nil {
		return e.fail(err)
	}
	if srcPath == dstPath {
		e.errorf("the source and destination are the same repository")
		return ExitError
	}

	dst, err := e.openRepo(dstPath, true, manifest.OpWrite)
	if err != nil {
		return e.fail(err)
	}
	defer dst.Close()

	src, err := e.openOtherRepo(srcPath)
	if err != nil {
		return e.fail(err)
	}
	defer src.Close()

	if err := checkRelated(src, dst, params != nil); err != nil {
		return e.fail(err)
	}

	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
	}
	infos, err := src.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}
	if len(infos) == 0 {
		return ExitOK
	}

	// Every name and comment is validated before the first archive is written, as borg
	// does: failing half way through a long transfer over something that could have been
	// seen at the start is a bad way to spend an hour.
	if err := validateTransferNames(src, infos); err != nil {
		return e.fail(err)
	}

	transferOpts := archive.TransferOptions{
		Recompress:    mode,
		Compressor:    compressor,
		ChunkerParams: params,
		DryRun:        *dryRun,
	}
	// The destination's chunk seed drives any re-chunking, because the new chunks belong
	// to the destination.
	if seed, ok := chunkSeedOf(dst); ok {
		transferOpts.ChunkSeed = seed
	}

	for _, info := range infos {
		ts := isoTime(info.Time)
		// Already there? borg checks name+timestamp AND name+id, because borg 2 allows
		// duplicate archive names, so neither identifies an archive on its own. This is
		// what makes a transfer resumable: run it again and it finishes what was left.
		if !*dryRun {
			if existing, why := alreadyTransferred(dst.manifest, info); existing {
				// borg prints transfer's per-archive lines with print(), so they are on
				// stdout, not stderr like most of its progress output. Measured, because
				// it decides whether "borge transfer | tee log" records anything.
				fmt.Fprintf(e.Stdout, "%s %s: archive is already present in destination repo, skipping.\n",
					info.Name, why)
				continue
			}
			fmt.Fprintf(e.Stdout, "%s %s %s: copying archive to destination repo...\n",
				info.Name, ts, hex.EncodeToString(info.ID))
		}

		res, _, err := archive.Transfer(dst.manifest, src.manifest, info.ID, transferOpts)
		if err != nil {
			return e.fail(fmt.Errorf("%s: %w", info.Name, err))
		}
		fmt.Fprintln(e.Stdout, archive.TransferReport(info.Name, info.ID, ts, res, *dryRun,
			func(n int64) string { return formatBytesIn(n, e.sizeUnits()) }))
	}

	if !*dryRun {
		if err := dst.manifest.Write(); err != nil {
			return e.fail(err)
		}
	}
	return ExitOK
}

// alreadyTransferred reports whether the destination already holds this archive.
func alreadyTransferred(m *manifest.Manifest, info manifest.Info) (bool, string) {
	all, err := m.Archives.List(manifest.ListOptions{})
	if err != nil {
		return false, ""
	}
	// Two passes rather than one, because borg asks both questions of the whole
	// repository and answers with the timestamp when either could answer: with duplicate
	// names allowed, the archive matching by name+timestamp and the one matching by
	// name+id need not be the same archive.
	for _, have := range all {
		if have.Name == info.Name && have.Time.Equal(info.Time) {
			return true, isoTime(info.Time)
		}
	}
	for _, have := range all {
		if have.Name == info.Name && string(have.ID) == string(info.ID) {
			return true, hex.EncodeToString(info.ID)
		}
	}
	return false, ""
}

// validateTransferNames checks every archive name and comment before anything is written.
//
// borg validates in two passes, names first and comments second, and reports every bad one
// rather than stopping at the first: the point is to hand the user the whole list so that
// they can rename or edit in one go, instead of discovering the next offender on the next
// attempt. Both are checked before a single object is copied, so a transfer that would end
// in a rejection never starts.
//
// The archives being checked were written by an older borg, or by another tool: the rules
// have tightened over time (leading blanks, for instance), so a name that was legal when it
// was created may not be legal to write now.
func validateTransferNames(src *opened, infos []manifest.Info) error {
	var bad []string
	for _, info := range infos {
		if err := validateArchiveName(info.Name); err != nil {
			bad = append(bad, err.Error())
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("Invalid archive names detected, please rename them before transfer:\n%s",
			strings.Join(bad, "\n"))
	}

	// Comments cost an archive metadata read each, which is why borg does them in a second
	// pass: with a bad name there is nothing to read them for.
	bad = nil
	for _, info := range infos {
		a, err := archive.Open(src.manifest, info.ID)
		if err != nil {
			return err
		}
		comment := ""
		if a.Meta.Comment != nil {
			comment = *a.Meta.Comment
		}
		if err := validateComment(comment); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %s", info.Name, err))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("Invalid archive comments detected, please fix them before transfer:\n%s",
			strings.Join(bad, "\n"))
	}
	return nil
}

// checkRelated is borg's pair of guards, refused before a single object is written.
//
// The failure they prevent looks like success: an unrelated destination stores every chunk
// again under a new id, so the transfer completes, takes as long as a fresh backup and
// deduplicates nothing.
func checkRelated(src, dst *opened, rechunking bool) error {
	srcMode := src.key.Name()
	dstMode := dst.key.Name()
	if !usesSameIDHash(srcMode, dstMode) && !rechunking {
		return fmt.Errorf("You must either keep the same ID hash or use --chunker-params.")
	}
	if rechunking {
		// Re-chunked content is hashed afresh, so neither guard applies: that is borg's
		// escape hatch from both.
		return nil
	}
	srcSeed, srcOK := seedOf(src)
	dstSeed, dstOK := seedOf(dst)
	if srcOK != dstOK || (srcOK && srcSeed != dstSeed) {
		return fmt.Errorf("You must use the same chunker secret or deduplication will break. " +
			"Use a related repository!")
	}
	return nil
}

// seedOf reads a repository's chunk seed, which is the chunker secret.
//
// The unencrypted modes have no key material and therefore no seed; borg treats their seed
// as 0, so two of them are related to each other and to nothing else. That is what the
// second return value distinguishes.
func seedOf(o *opened) (int64, bool) {
	if o.unlocked == nil || o.unlocked.Material == nil || o.unlocked.Material.ChunkSeed == nil {
		return 0, false
	}
	return *o.unlocked.Material.ChunkSeed, true
}

func chunkSeedOf(o *opened) (uint32, bool) {
	if o.unlocked == nil || o.unlocked.Material == nil {
		return 0, false
	}
	return uint32(key.ChunkSeed(o.unlocked.Material)), true
}
