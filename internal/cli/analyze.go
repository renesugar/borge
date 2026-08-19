// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of ArchiveAnalyzer in borg's src/borg/archiver/analyze_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/hashindex"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
)

// Analysing where a repository's space actually goes.
//
// # The question it answers
//
// "The repository is 400 GB. What would I have to delete to make it smaller?" is not
// answerable from an archive listing, because deduplication means the archives do not add
// up: a chunk shared by twenty archives is stored once, and deleting nineteen of them
// frees nothing at all.
//
// So every figure here is about *chunks*, not archives, and each chunk is counted exactly
// once. The two modes ask it differently:
//
//   - the default takes a set of archives and reports what the set costs, and what would
//     actually be freed by deleting the whole set (chunks nothing else references);
//   - --by-name decomposes the entire repository, so each archive name gets the size that
//     is exclusive to it, with everything shared between names in its own row.
//
// # Two sizes, and why both
//
// "source" is uncompressed plaintext, and is what a restore would write. "stored" is what
// the repository holds after compression. Reporting only the first overstates what
// deleting would recover; reporting only the second cannot be compared with the size of
// the directory that was backed up.
//
// A chunk's plaintext size is only recorded in the archives that reference it - the
// repository index stores 0 - so unreferenced chunks have no known source size, and their
// row says "n/a" rather than a misleading zero.
//
// # Cost
//
// borg reads a per-archive references cache written by compact, so unchanged archives are
// never opened. borge has no such cache yet, so this reads and decrypts every archive's
// item stream every time. The answer is the same; the wait is longer.
func cmdAnalyze(e *Env, args []string) int {
	fs := newFlagSet(e, "analyze")
	var common commonFlags
	var sel listSelectors
	common.register(fs)
	common.registerJSON(fs, "")
	sel.register(fs)
	byName := fs.Bool("by-name", false, "decompose the whole repository by archive name")
	hotspots := fs.Int("hotspots", 20, "show this many busiest directories (0 for none)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	opts, err := sel.options(e)
	if err != nil {
		return e.fail(err)
	}
	if *byName && !opts.IsZero() {
		// The decomposition is inherently repository-wide: "shared" and "unreferenced" can
		// only be decided by looking at every archive, so a filter would silently change
		// what those words mean.
		e.errorf("--by-name analyses the whole repository and cannot be combined with " +
			"archive filters")
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

	chunks, err := o.repo.Chunks()
	if err != nil {
		return e.fail(err)
	}

	if *byName {
		return analyzeByName(e, o, chunks, common)
	}
	return analyzeSet(e, o, chunks, opts, *hotspots, common)
}

// reference is what one archive walk records about a chunk: its plaintext size, which the
// repository index does not have.
type reference struct {
	size uint32
	// owner is the 1-based index of the first archive name to reference this chunk, or 0
	// for "not yet seen". Only --by-name uses it.
	owner uint32
	multi bool
	// considered and rest are the two membership bits of the default mode.
	considered bool
	inRest     bool
}

// walkArchive collects every chunk id an archive references, with its plaintext size.
//
// The archive object and its item-pointer blocks count too. They are ordinary chunks in
// ordinary packs, and leaving them out would report an archive's own metadata as
// unreferenced - which compact would then offer to delete.
func walkArchive(m *manifest.Manifest, id []byte, fn func(id []byte, size uint32)) error {
	a, err := archive.Open(m, id)
	if err != nil {
		return err
	}
	fn(id, 0)
	for _, ptr := range a.Meta.ItemPtrs {
		fn(ptr, 0)
	}
	streamIDs, err := a.ItemStreamIDs()
	if err != nil {
		return err
	}
	for _, sid := range streamIDs {
		fn(sid, 0)
	}
	return a.Items(func(it *item.Item) error {
		for _, c := range it.Chunks {
			fn(c.ID, uint32(c.Size))
		}
		return nil
	})
}

// analyzeSet reports what a set of archives costs and what deleting it would free.
func analyzeSet(e *Env, o *opened, chunks *hashindex.ChunkIndex, opts manifest.ListOptions,
	hotspotCount int, common commonFlags,
) int {
	considered, err := o.manifest.Archives.List(opts)
	if err != nil {
		return e.fail(err)
	}
	if len(considered) == 0 {
		e.errorf("no archives match the given selection")
		return ExitError
	}
	all, err := o.manifest.Archives.List(manifest.ListOptions{})
	if err != nil {
		return e.fail(err)
	}

	inSet := map[string]bool{}
	for _, info := range considered {
		inSet[hex.EncodeToString(info.ID)] = true
	}
	var rest []manifest.Info
	for _, info := range all {
		if !inSet[hex.EncodeToString(info.ID)] {
			rest = append(rest, info)
		}
	}

	refs := map[string]*reference{}
	mark := func(infos []manifest.Info, set func(*reference)) (int, error) {
		missing := 0
		for i, info := range infos {
			if !info.Exists {
				e.warnf("archive %s: %s (not analysed)", hex.EncodeToString(info.ID)[:8], info.Problem)
				continue
			}
			if common.verbose {
				e.warnf("analysing %s (%d/%d)", info.Name, i+1, len(infos))
			}
			err := walkArchive(o.manifest, info.ID, func(id []byte, size uint32) {
				k := string(id)
				r := refs[k]
				if r == nil {
					if _, ok := chunks.Get(id); !ok {
						missing++
						return
					}
					r = &reference{}
					refs[k] = r
				}
				if size > r.size {
					r.size = size
				}
				set(r)
			})
			if err != nil {
				return missing, fmt.Errorf("archive %s: %w", info.Name, err)
			}
		}
		return missing, nil
	}

	missing, err := mark(considered, func(r *reference) { r.considered = true })
	if err != nil {
		return e.fail(err)
	}
	m2, err := mark(rest, func(r *reference) { r.inRest = true })
	if err != nil {
		return e.fail(err)
	}
	missing += m2

	var setSource, setStored, exclSource, exclStored int64
	var unrefStored int64
	var unrefCount, totalCount int
	chunks.Iterate(func(id []byte, entry hashindex.Entry) bool {
		totalCount++
		r := refs[string(id)]
		switch {
		case r == nil || (!r.considered && !r.inRest):
			// Referenced by no non-deleted archive: what compact could free right now.
			unrefCount++
			unrefStored += int64(entry.ObjSize)
		case r.considered:
			setSource += int64(r.size)
			setStored += int64(entry.ObjSize)
			if !r.inRest {
				exclSource += int64(r.size)
				exclStored += int64(entry.ObjSize)
			}
		}
		return true
	})

	if missing > 0 {
		e.warnf("%d chunk reference(s) are missing from the repository index; "+
			"run 'borge check' - the figures below are incomplete", missing)
	}

	// With nothing left over, the set is the whole repository and every referenced chunk
	// is trivially exclusive to it - so that line would just repeat the line above.
	wholeRepo := len(rest) == 0

	data := analyzeSetJSON{
		ConsideredArchives: len(considered),
		TotalArchives:      len(all),
		WholeRepository:    wholeRepo,
		Deduplicated:       sizePair{setSource, setStored},
		Unreferenced:       unrefPair{unrefStored, unrefCount},
		TotalChunks:        totalCount,
		MissingChunks:      missing,
	}
	if !wholeRepo {
		data.Exclusive = &sizePair{exclSource, exclStored}
	}

	if hotspotCount > 0 && len(considered) >= 2 {
		spots, err := analyzeHotspots(o, considered, hotspotCount)
		if err != nil {
			return e.fail(err)
		}
		data.Hotspots = spots
	}

	if common.json {
		// borg's shape: the numbers under "dedup_size", the hot spots beside them rather
		// than inside, and the envelope every JSON command carries. borge emitted the
		// numbers bare, which is the same information under a different document.
		//
		// hotspots is null rather than absent when it was not computed - borg's own
		// comment for that value is "not computed, as opposed to computed and empty", and
		// the distinction is one a frontend can act on: fewer than two archives matched.
		spots := data.Hotspots
		data.Hotspots = nil
		repoBlock, encBlock := o.envelope(o.repo.Path())
		doc := map[string]any{
			"dedup_size": data,
			"hotspots":   nil,
			"encryption": encBlock,
			"repository": repoBlock,
		}
		if spots != nil {
			doc["hotspots"] = spots
		}
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}
	reportSet(e, data, len(considered) < 2 && hotspotCount > 0)
	return ExitOK
}

// analyzeByName decomposes the whole repository by archive name.
func analyzeByName(e *Env, o *opened, chunks *hashindex.ChunkIndex, common commonFlags) int {
	all, err := o.manifest.Archives.List(manifest.ListOptions{})
	if err != nil {
		return e.fail(err)
	}
	if len(all) == 0 {
		e.errorf("the repository contains no archives")
		return ExitError
	}

	perName := map[string]int{}
	for _, info := range all {
		perName[info.Name]++
	}
	names := make([]string, 0, len(perName))
	for name := range perName {
		names = append(names, name)
	}
	sort.Strings(names)
	ownerOf := map[string]uint32{}
	for i, name := range names {
		ownerOf[name] = uint32(i) + 1 // 0 means "referenced by nothing"
	}

	refs := map[string]*reference{}
	missing := 0
	for i, info := range all {
		if !info.Exists {
			e.warnf("archive %s: %s (not analysed)", hex.EncodeToString(info.ID)[:8], info.Problem)
			continue
		}
		if common.verbose {
			e.warnf("analysing %s (%d/%d)", info.Name, i+1, len(all))
		}
		owner := ownerOf[info.Name]
		err := walkArchive(o.manifest, info.ID, func(id []byte, size uint32) {
			k := string(id)
			r := refs[k]
			if r == nil {
				if _, ok := chunks.Get(id); !ok {
					missing++
					return
				}
				r = &reference{}
				refs[k] = r
			}
			if size > r.size {
				r.size = size
			}
			switch {
			case r.owner == 0:
				r.owner = owner
			case r.owner != owner:
				// A second, different name: this chunk belongs to no single one of them.
				r.multi = true
			}
		})
		if err != nil {
			return e.fail(fmt.Errorf("archive %s: %w", info.Name, err))
		}
	}

	exclusive := map[string]*sizePair{}
	for _, name := range names {
		exclusive[name] = &sizePair{}
	}
	var shared sizePair
	var unrefStored int64
	var unrefCount, totalCount int
	chunks.Iterate(func(id []byte, entry hashindex.Entry) bool {
		totalCount++
		r := refs[string(id)]
		if r == nil || r.owner == 0 {
			unrefCount++
			unrefStored += int64(entry.ObjSize)
			return true
		}
		bucket := &shared
		if !r.multi {
			bucket = exclusive[names[r.owner-1]]
		}
		bucket.SourceSize += int64(r.size)
		bucket.StoredSize += int64(entry.ObjSize)
		return true
	})

	if missing > 0 {
		e.warnf("%d chunk reference(s) are missing from the repository index; "+
			"run 'borge check' - the figures below are incomplete", missing)
	}

	rows := make([]nameRow, 0, len(names))
	var totalSource, totalStored int64
	for _, name := range names {
		rows = append(rows, nameRow{
			Name: name, Archives: perName[name],
			SourceSize: exclusive[name].SourceSize, StoredSize: exclusive[name].StoredSize,
		})
		totalSource += exclusive[name].SourceSize
		totalStored += exclusive[name].StoredSize
	}
	totalSource += shared.SourceSize
	totalStored += shared.StoredSize
	// Biggest exclusive consumer first: that is the row somebody would act on.
	sort.Slice(rows, func(i, j int) bool { return rows[i].StoredSize > rows[j].StoredSize })

	data := analyzeByNameJSON{
		Archives:      len(all),
		Names:         rows,
		Shared:        shared,
		Unreferenced:  unrefPair{unrefStored, unrefCount},
		Total:         totalRow{len(rows), totalSource, totalStored},
		TotalChunks:   totalCount,
		MissingChunks: missing,
	}

	if common.json {
		repoBlock, encBlock := o.envelope(o.repo.Path())
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"by_name":    data,
			"encryption": encBlock,
			"repository": repoBlock,
		}); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}
	reportByName(e, data)
	return ExitOK
}

// analyzeHotspots finds the directories whose contents churn most between consecutive
// archives.
//
// It answers a different question from the size figures: not "what is big" but "what keeps
// changing", which is what makes a repository grow over time even when nothing gets
// bigger. A directory of daily-rewritten database files is invisible in a size report and
// obvious here.
func analyzeHotspots(o *opened, infos []manifest.Info, limit int) ([]hotspot, error) {
	// Oldest first, so consecutive pairs are consecutive in time whichever order the
	// selectors produced.
	ordered := make([]manifest.Info, len(infos))
	copy(ordered, infos)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Time.Before(ordered[j].Time) })

	churn := map[string]int64{}
	var base map[string]map[string]uint32
	for _, info := range ordered {
		if !info.Exists {
			continue
		}
		next, err := chunksByDirectory(o.manifest, info.ID)
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", info.Name, err)
		}
		if base != nil {
			accumulateChurn(churn, base, next)
		}
		base = next
	}

	spots := make([]hotspot, 0, len(churn))
	for p, size := range churn {
		spots = append(spots, hotspot{Path: p, Size: size})
	}
	sort.Slice(spots, func(i, j int) bool {
		if spots[i].Size != spots[j].Size {
			return spots[i].Size > spots[j].Size
		}
		return spots[i].Path < spots[j].Path // stable, so two runs agree
	})
	if len(spots) > limit {
		spots = spots[:limit]
	}
	return spots, nil
}

// chunksByDirectory maps each directory to the chunks its files are made of.
func chunksByDirectory(m *manifest.Manifest, id []byte) (map[string]map[string]uint32, error) {
	a, err := archive.Open(m, id)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]uint32{}
	err = a.Items(func(it *item.Item) error {
		if len(it.Chunks) == 0 {
			return nil
		}
		dir := path.Dir(it.Path)
		byID := out[dir]
		if byID == nil {
			byID = map[string]uint32{}
			out[dir] = byID
		}
		for _, c := range it.Chunks {
			byID[string(c.ID)] = uint32(c.Size)
		}
		return nil
	})
	return out, err
}

// accumulateChurn adds the sizes of chunks that appeared or vanished between two archives.
//
// Both directions count. A directory that only ever loses data churns just as much work
// for the repository as one that only gains it, and reporting only additions would make a
// large deletion look like nothing happened.
func accumulateChurn(churn map[string]int64, base, next map[string]map[string]uint32) {
	for dir, baseChunks := range base {
		nextChunks := next[dir]
		for id, size := range baseChunks {
			if _, still := nextChunks[id]; !still {
				churn[dir] += int64(size)
			}
		}
		for id, size := range nextChunks {
			if _, had := baseChunks[id]; !had {
				churn[dir] += int64(size)
			}
		}
	}
	for dir, nextChunks := range next {
		if _, seen := base[dir]; seen {
			continue // already handled above
		}
		for _, size := range nextChunks {
			churn[dir] += int64(size)
		}
	}
}

// ---------------------------------------------------------------- output

type sizePair struct {
	SourceSize int64 `json:"source_size"`
	StoredSize int64 `json:"stored_size"`
}

type unrefPair struct {
	StoredSize int64 `json:"stored_size"`
	Chunks     int   `json:"chunks"`
}

type nameRow struct {
	Name       string `json:"name"`
	Archives   int    `json:"archives"`
	SourceSize int64  `json:"source_size"`
	StoredSize int64  `json:"stored_size"`
}

type hotspot struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type analyzeSetJSON struct {
	ConsideredArchives int       `json:"considered_archives"`
	TotalArchives      int       `json:"total_archives"`
	WholeRepository    bool      `json:"whole_repository"`
	Deduplicated       sizePair  `json:"deduplicated"`
	Exclusive          *sizePair `json:"exclusive,omitempty"`
	Unreferenced       unrefPair `json:"unreferenced"`
	TotalChunks        int       `json:"total_chunks"`
	MissingChunks      int       `json:"missing_chunks"`
	Hotspots           []hotspot `json:"hotspots,omitempty"`
}

// totalRow is borg's "total" object in the by-name report: a sizePair with the archive
// count beside it. borge sent the pair alone, so a frontend reading total.archives found
// nothing where borg puts the number the row is a total *of*.
type totalRow struct {
	Archives   int   `json:"archives"`
	SourceSize int64 `json:"source_size"`
	StoredSize int64 `json:"stored_size"`
}

type analyzeByNameJSON struct {
	Archives      int       `json:"archives"`
	Names         []nameRow `json:"names"`
	Shared        sizePair  `json:"shared"`
	Unreferenced  unrefPair `json:"unreferenced"`
	Total         totalRow  `json:"total"`
	TotalChunks   int       `json:"total_chunks"`
	MissingChunks int       `json:"missing_chunks"`
}

// compressionFactor is stored/source. There is no answer when the source size is unknown,
// and printing 0.00 there would read as "compresses perfectly".
func compressionFactor(stored, source int64) string {
	if source == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(stored)/float64(source))
}

func reportSet(e *Env, d analyzeSetJSON, tooFewForHotspots bool) {
	row := func(label string, source, stored int64, sourceKnown bool) {
		src, factor := "n/a", "n/a"
		if sourceKnown {
			src = e.fmtBytes(source)
			factor = compressionFactor(stored, source)
		}
		fmt.Fprintf(e.Stdout, "%-26s%14s%14s%13s\n", label, src, e.fmtBytes(stored), factor)
	}

	fmt.Fprintln(e.Stdout)
	if d.WholeRepository {
		fmt.Fprintf(e.Stdout, "All %d archive(s) in the repository\n", d.ConsideredArchives)
	} else {
		fmt.Fprintf(e.Stdout, "%d of %d archive(s)\n", d.ConsideredArchives, d.TotalArchives)
	}
	fmt.Fprintf(e.Stdout, "%-26s%14s%14s%13s\n", "", "source", "stored", "compression")
	row("Deduplicated size of set:", d.Deduplicated.SourceSize, d.Deduplicated.StoredSize, true)
	if d.Exclusive != nil {
		row("Exclusive size of set:", d.Exclusive.SourceSize, d.Exclusive.StoredSize, true)
	}
	row("Unreferenced chunks:", 0, d.Unreferenced.StoredSize, false)
	fmt.Fprintln(e.Stdout)
	fmt.Fprintf(e.Stdout, "Unreferenced: %d of %d chunks in the repository index.\n",
		d.Unreferenced.Chunks, d.TotalChunks)
	fmt.Fprintln(e.Stdout)
	if d.WholeRepository {
		fmt.Fprintln(e.Stdout, "source       = uncompressed source data size (each chunk counted once)")
	} else {
		fmt.Fprintln(e.Stdout, "source       = uncompressed source data size (chunks shared within the set counted once)")
	}
	fmt.Fprintln(e.Stdout, "stored       = deduplicated size as stored in the repository (compressed)")
	fmt.Fprintln(e.Stdout, "compression  = stored / source (1.00 = not compressible)")
	if d.Exclusive != nil {
		fmt.Fprintln(e.Stdout, "exclusive    = chunks referenced only by this set; deleting the set would free them")
	}
	fmt.Fprintln(e.Stdout, "unreferenced = referenced by no non-deleted archive; 'borge compact' could free")
	fmt.Fprintln(e.Stdout, "               these. Their source size is not known: it is recorded only in")
	fmt.Fprintln(e.Stdout, "               the archives referencing a chunk. Soft-deleted archives' chunks")
	fmt.Fprintln(e.Stdout, "               count as unreferenced.")

	if len(d.Hotspots) > 0 {
		fmt.Fprintln(e.Stdout)
		fmt.Fprintln(e.Stdout, "chunks added or removed between consecutive archives, by directory")
		for _, h := range d.Hotspots {
			fmt.Fprintf(e.Stdout, "%14s  %s\n", e.fmtBytes(h.Size), h.Path)
		}
	} else if tooFewForHotspots {
		fmt.Fprintln(e.Stdout)
		fmt.Fprintln(e.Stdout, "(no hot-spot analysis: it compares consecutive archives and needs at least two)")
	}
}

func reportByName(e *Env, d analyzeByNameJSON) {
	width := 30
	for _, r := range d.Names {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	if width > 60 {
		width = 60
	}

	row := func(label string, archives string, source, stored int64, sourceKnown bool) {
		src := "n/a"
		factor := "n/a"
		if sourceKnown {
			src = e.fmtBytes(source)
			factor = compressionFactor(stored, source)
		}
		fmt.Fprintf(e.Stdout, "%-*s%10s%14s%14s%13s\n", width, label, archives, src, e.fmtBytes(stored), factor)
	}

	fmt.Fprintln(e.Stdout)
	fmt.Fprintln(e.Stdout, "Repository decomposition by archive name")
	fmt.Fprintf(e.Stdout, "%d archive(s) with %d distinct name(s)\n\n", d.Archives, len(d.Names))
	fmt.Fprintf(e.Stdout, "%-*s%10s%14s%14s%13s\n", width, "name", "archives", "source", "stored", "compression")
	for _, r := range d.Names {
		row(r.Name, fmt.Sprint(r.Archives), r.SourceSize, r.StoredSize, true)
	}
	row("(shared by 2+ names)", "", d.Shared.SourceSize, d.Shared.StoredSize, true)
	row("(unreferenced)", "", 0, d.Unreferenced.StoredSize, false)
	row("total (deduplicated)", fmt.Sprint(d.Archives), d.Total.SourceSize, d.Total.StoredSize, true)
	fmt.Fprintln(e.Stdout)
	fmt.Fprintf(e.Stdout, "Unreferenced: %d of %d chunks in the repository index.\n",
		d.Unreferenced.Chunks, d.TotalChunks)
	fmt.Fprintln(e.Stdout)
	fmt.Fprintln(e.Stdout, "Each chunk is counted in exactly one row, so the rows add up to the total.")
	fmt.Fprintln(e.Stdout, "A name's row is what is exclusive to it: no archive of another name references")
	fmt.Fprintln(e.Stdout, "those chunks, so deleting every archive of that name would free them. Chunks")
	fmt.Fprintln(e.Stdout, "used by several names are counted in the shared row, not against any name.")
}
