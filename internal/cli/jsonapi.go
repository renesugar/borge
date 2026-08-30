// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// The shapes here are borg's, from src/borg/helpers/parseformat.py (BaseFormatter and
// friends) and the commands that emit them.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/formatter"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// borg's JSON output is its API. Its documentation is explicit that there is no other:
// "Borg does not have a public API on the Python level [...] Borg provides an API on a
// command-line level" (docs/internals/frontends.rst). So the JSON shape is the interface
// a frontend is written against, and a key borge spells differently is a frontend that
// does not work - the same class of breakage as a wire format that does not match.
//
// Every JSON-producing command wraps its own payload in the same two blocks, so they live
// here rather than being spelled out per command: borge got them wrong five separate ways
// before this file existed, which is what having five copies does.

// isoTime is the timestamp spelling in every borg JSON document: local time, microsecond
// precision, explicit offset. Python's datetime.isoformat() with a timezone-aware value.
func isoTime(t time.Time) string {
	return t.Local().Format("2006-01-02T15:04:05.000000-07:00")
}

// repositoryJSON is borg's "repository" block.
//
// Note it is not what borge's repo-info used to print under the same name: that had
// "version" and "archive_count" and no "last_modified", so a frontend reading
// repository.last_modified found nothing and one reading repository.version found
// something borg never puts there.
type repositoryJSON struct {
	ID           string `json:"id"`
	LastModified string `json:"last_modified"`
	Location     string `json:"location"`
}

// encryptionJSON is borg's "encryption" block.
//
// The two fields are not independent, and neither is simply borge's mode name. borg names
// the cipher in "encryption" and the chunk-id hash in "id_hash", so its aes256-ocb repos
// come in two kinds that share a name and differ in id_hash. borge names the pair in one
// string ("blake3-aes256-ocb"), which is the clearer spelling but not the one on the wire,
// so it is split here.
type encryptionJSON struct {
	Encryption string `json:"encryption"`
	IDHash     string `json:"id_hash"`
}

// splitEncryptionMode turns borge's mode name into borg's (encryption, id_hash) pair.
func splitEncryptionMode(mode string) encryptionJSON {
	hash := "sha256"
	name := mode
	switch {
	case strings.HasPrefix(mode, "blake3-"):
		// blake3-aes256-ocb is borg's Blake3AESOCBKey, whose ENC_NAME is plain
		// "aes256-ocb"; only id_hash tells the two apart.
		hash = "blake3"
		name = strings.TrimPrefix(mode, "blake3-")
	case strings.HasSuffix(mode, "-blake3"):
		// authenticated-blake3 and none-blake3 keep their full name as ENC_NAME.
		hash = "blake3"
	}
	return encryptionJSON{Encryption: name, IDHash: hash}
}

// envelope is what every JSON-producing repository command has around its payload.
func (o *opened) envelope(loc *location.Location) (repositoryJSON, encryptionJSON) {
	return envelopeFor(o.repo, o.key, o.manifest, loc)
}

// envelopeFor is envelope for the commands that hold the three pieces separately rather
// than as an "opened" - create and import-tar open the repository themselves.
func envelopeFor(
	repo *repository.Repository, k key.Key, m *manifest.Manifest, loc *location.Location,
) (repositoryJSON, encryptionJSON) {
	// The canonical form, which is borg's: parseformat.py publishes
	// _location.canonical_path() here, so two spellings of one repository produce one
	// string and an S3 location does not publish its secret key in a JSON log.
	out := repositoryJSON{ID: repo.IDString(), Location: loc.Canonical()}
	// A repository whose timestamp cannot be read still has an id and a location worth
	// reporting, so a bad timestamp leaves the field empty rather than failing the
	// command. borg has no such case: it always has a manifest timestamp by here.
	if ts, err := m.LastTimestamp(); err == nil {
		out.LastModified = isoTime(ts)
	}
	return out, splitEncryptionMode(k.Name())
}

// archiveJSONData is borg's archive-level JSON object.
//
// The key set is not fixed. borg builds it from the effective --format: "The form of
// --format is ignored, but keys used in it are added to the JSON output. Some keys are
// always present." Four are always there - name, archive, id, time - and the other nine
// appear only when the format names them.
//
// borge emitted all thirteen unconditionally, which happens to match borg for repo-list's
// default format and matches nothing else: "prune --json" gained four keys borg does not
// send, and "repo-list --format '{archive}' --json" gained eight. A frontend that reads
// what it asked for is fine either way; one that iterates the object is not.
//
// command_line is absent here because borge does not read it from the archive metadata
// yet; see plans/PORTING_PLAN.md section 11.4. It is the one key of borg's thirteen that
// borge cannot supply, and leaving it out is the honest form: a frontend asking for it via
// --format gets an unknown-key error rather than an empty string that looks like an
// archive created by an empty command line.
func archiveJSONData(info manifest.Info, template string) (map[string]any, error) {
	out := map[string]any{
		"name":    info.Name,
		"archive": info.Name,
		"id":      hex.EncodeToString(info.ID),
		"time":    isoTime(info.Time),
	}
	keys, err := formatter.Keys(template)
	if err != nil {
		return nil, err
	}
	optional := map[string]any{
		"hostname": info.Host,
		"username": info.User,
		"comment":  info.Comment,
		"size":     info.Size,
		"nfiles":   info.NFiles,
		"start":    isoTime(info.Start),
		"end":      isoTime(info.End),
		"tags":     strings.Join(info.Tags, ","),
	}
	for _, k := range keys {
		if v, ok := optional[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// createStatsJSON is the "stats" block of the document create and import-tar print.
//
// borg sends six keys here. borge sends three, and the omissions are deliberate:
//
//   - chunking_time and hashing_time are instrumentation borge does not collect.
//   - store_stats is a per-backend call/volume/latency report from borg's Store layer.
//
// Sending them as zeros would be the worse choice by some distance: a frontend charting
// hashing_time would draw a flat line and believe it, where a missing key is a question
// it can answer by asking the version. See plans/PORTING_PLAN.md section 11.4.
func createStatsJSON(nfiles, originalSize int64, fileStatus map[string]int64,
	timings archive.Stats, storeStats map[string]any) map[string]any {

	// files_stats is always an object, never null: a backup that stored nothing has an
	// empty count, and null would read as "not measured".
	counts := map[string]int64{}
	for k, v := range fileStatus {
		counts[k] = v
	}
	return map[string]any{
		"nfiles":        nfiles,
		"original_size": originalSize,
		"files_stats":   counts,
		// The three keys borge used to leave out because it measured nothing. It measures
		// them now (DIVERGENCES #51), and they are seconds as floats, which is what borg
		// sends - not a formatted duration, which is the text form's business.
		"hashing_time":  timings.HashingTime.Seconds(),
		"chunking_time": timings.ChunkingTime.Seconds(),
		"store_stats":   storeStats,
	}
}

// archiveCreatedJSON is the document "create --json" and "import-tar --json" print.
//
// The four blocks are borg's. "cache" carries the path of the files cache, which borge
// keeps per repository exactly as borg does.
func archiveCreatedJSON(
	repo *repository.Repository, k key.Key, m *manifest.Manifest,
	loc *location.Location, cachePath string, meta *item.ArchiveItem, id []byte, stats map[string]any,
) map[string]any {
	archiveBlock := map[string]any{
		"name":  meta.Name,
		"id":    hex.EncodeToString(id),
		"stats": stats,
	}
	start, startOK := metaTime(meta.Start)
	end, endOK := metaTime(meta.End)
	if nominal, ok := metaTime(meta.Time); ok {
		archiveBlock["time"] = isoTime(nominal)
	}
	if startOK {
		archiveBlock["start"] = isoTime(start)
	}
	if endOK {
		archiveBlock["end"] = isoTime(end)
	}
	if startOK && endOK {
		// Seconds with a fraction, as borg reports it.
		archiveBlock["duration"] = end.Sub(start).Seconds()
	}
	if meta.CommandLine != nil {
		archiveBlock["command_line"] = *meta.CommandLine
	}

	repoBlock, encBlock := envelopeFor(repo, k, m, loc)
	return map[string]any{
		"archive":    archiveBlock,
		"cache":      map[string]any{"path": cachePath},
		"repository": repoBlock,
		"encryption": encBlock,
	}
}

// metaTime parses one of the archive metadata's stored timestamps.
func metaTime(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	t, err := manifest.ParseTimestamp(*s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// infoArchiveJSON is one archive as "borg info --json" describes it.
//
// Unlike the archive objects in repo-list and prune, this set is fixed rather than driven
// by --format: info takes no --format at all, and borg sends the same fourteen keys every
// time. borge sent nine of them, and four of the five missing ones - command_line, cwd,
// chunker_params and duration - it was storing in the archive metadata all along and
// simply not reading back. See docs/DIVERGENCES.md #42.
func infoArchiveJSON(a *archive.Archive) map[string]any {
	out := map[string]any{
		"name":     a.Info.Name,
		"id":       hex.EncodeToString(a.ID),
		"hostname": a.Info.Host,
		"username": a.Info.User,
		"comment":  a.Info.Comment,
		"start":    isoTime(a.Info.Start),
		"end":      isoTime(a.Info.End),
		"time":     isoTime(a.Info.Time),
		// An archive with no tags is an empty list, not null: borg sends [], and null
		// reads as "unknown" to anything that iterates it.
		"tags":     append([]string{}, a.Info.Tags...),
		"duration": a.Info.End.Sub(a.Info.Start).Seconds(),
		"stats": map[string]any{
			"nfiles":        a.Info.NFiles,
			"original_size": a.Info.Size,
			// borg sends these three here as well, and in info they are always empty:
			// nothing in the archive records them, so its Statistics object is fresh. They
			// are emitted for that reason and no other - unlike create and import-tar,
			// where borg's are real measurements and borge's would be invented (#36).
			"files_stats":   map[string]any{},
			"chunking_time": 0.0,
			"hashing_time":  0.0,
			"store_stats":   map[string]any{},
		},
	}
	if a.Meta != nil {
		if a.Meta.CommandLine != nil {
			out["command_line"] = *a.Meta.CommandLine
		}
		if a.Meta.CWD != nil {
			out["cwd"] = *a.Meta.CWD
		}
		if a.Meta.ChunkerParams != nil {
			out["chunker_params"] = a.Meta.ChunkerParams
		}
	}
	return out
}

// # Text that is not valid unicode
//
// A path on Linux is bytes, not text, and JSON is text. borg's rule (frontends.rst,
// "Dealing with non-unicode byte sequences") is that a value which does not decode cleanly
// gets *two* keys: the named one holding an approximation with each bad byte shown as "?",
// and "<key>_b64" holding base64 of the original bytes. A frontend that needs precision
// decodes the second; one that only displays uses the first.
//
// borge emitted neither. Go's encoder replaces invalid bytes with U+FFFD, so the path came
// out mangled and with no way to recover it - lossy output that looks fine.
//
// Note this is *not* the representation borge already implements in pydump.go. borg uses
// two different ones and so does borge: "debug dump-*" and "diff --json-lines" write
// Python's surrogate escapes (\udcff), while the item and archive objects use ? plus _b64.
// The plan called for unifying them, which would have been wrong - measuring borg shows
// they genuinely differ. What they share is the question, not the answer.
func putText(m map[string]any, key, value string) {
	if utf8.ValidString(value) {
		m[key] = value
		return
	}
	m[key] = approximateText(value)
	m[key+"_b64"] = base64.StdEncoding.EncodeToString([]byte(value))
}

// approximateText replaces every byte that is not part of a valid UTF-8 sequence with "?".
//
// One "?" per bad byte, which is what Python's str.encode(errors="replace") produces for
// the surrogate escapes borg is carrying at that point: two bad bytes give "??". A U+FFFD
// that was genuinely in the name is left alone - it is valid text, and turning it into a
// question mark would report damage that is not there.
func approximateText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('?')
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// itemJSONData is borg's per-item JSON object, for "list" and "find".
//
// Eleven keys are always present and the rest appear only when the effective --format names
// them, exactly as for the archive-level object: "the form of --format is ignored, but keys
// used in it are added". borge sent thirteen every time, which matches borg for list's
// default format and nothing else - "list --json-lines --format '{path}'" gave borg eleven
// keys and borge thirteen.
func itemJSONData(it *item.Item, template, archiveName, archiveID string) (map[string]any, error) {
	mode := it.ModeOr(0)
	out := map[string]any{
		"type":  item.TypeChar(mode),
		"mode":  item.FormatMode(mode),
		"uid":   nullableInt(it.UID),
		"gid":   nullableInt(it.GID),
		"hlid":  hex.EncodeToString(it.HLID),
		"flags": nullableInt(it.BSDFlags),
		"inode": nullableUint(it.Inode),
	}
	putText(out, "path", it.Path)
	target := ""
	if it.Target != nil {
		target = *it.Target
	}
	putText(out, "target", target)
	// borg falls back to the numeric id when the name was not stored, and renders it as
	// text either way.
	putText(out, "user", nameOrID(it.User, it.UID))
	putText(out, "group", nameOrID(it.Group, it.GID))

	keys, err := formatter.Keys(template)
	if err != nil {
		return nil, err
	}
	iso := func(ts *int64) string {
		// borg's format_time falls back to mtime for a missing or zero timestamp; see the
		// note in itemValues.
		if ts == nil || *ts == 0 {
			ts = it.MTime
		}
		if ts == nil {
			return ""
		}
		return isoTime(time.Unix(0, *ts))
	}
	optional := map[string]func() any{
		"size":       func() any { return itemSize(it) },
		"num_chunks": func() any { return int64(len(it.Chunks)) },
		"mtime":      func() any { return iso(it.MTime) },
		"ctime":      func() any { return iso(it.CTime) },
		"atime":      func() any { return iso(it.ATime) },
		"isomtime":   func() any { return iso(it.MTime) },
		"isoctime":   func() any { return iso(it.CTime) },
		"isoatime":   func() any { return iso(it.ATime) },
		"archivename": func() any {
			return archiveName
		},
		"archiveid": func() any { return archiveID },
	}
	for _, k := range keys {
		if f, ok := optional[k]; ok {
			out[k] = f()
		}
	}
	return out, nil
}

// nullableInt is borg's JSON for a number an item may not carry: null, not the four
// letters "None" the *text* form prints (DIVERGENCES.md #33). JSON has a null and Python's
// encoder uses it.
func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableUint(v *uint64) any {
	if v == nil {
		return nil
	}
	return *v
}
