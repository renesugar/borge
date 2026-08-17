// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the debug commands in borg's
// src/borg/archiver/debug_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/renesugar/borge/internal/archive"
	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/msgpackx"
	"github.com/renesugar/borge/internal/repoobj"
	"github.com/renesugar/borge/internal/repository"
	"github.com/renesugar/borge/internal/version"
)

// The debug commands are the layer below every other command: they read and write the
// repository's raw objects with no interpretation, and they are how a format question gets
// answered when the ordinary commands disagree.
//
// # Why they belong in a port
//
// They are the port's own debugging tools. When `borge list` and `borg list` disagree about
// an archive, the way to find out which one is wrong is to dump the same object with both
// and diff - which only works if the dumps are comparable, so these commands go to some
// length to match borg's output exactly (see pydump.go).
//
// # Why they are dangerous
//
// put-obj and delete-obj write to the repository with no consistency checking whatsoever.
// delete-obj will happily remove a chunk an archive still references, and put-obj will
// store an object under an id that does not match its contents. Both are useful for
// exactly that reason - it is how the corruption corpus for `check --repair` is built -
// and neither should ever appear in a script that is not deliberately breaking things.

// debugCommands is the dispatch table for "borge debug <name>".
//
// A function rather than a package variable, to match benchmarkCommands and so that
// completion.go can treat every command group the same way.
func debugCommands() []command {
	return []command{
		{"info", "show system information for a bug report", cmdDebugInfo},
		{"dump-archive-items", "write each item metadata stream chunk to a file", cmdDebugDumpArchiveItems},
		{"dump-archive", "write an archive's metadata and items as JSON", cmdDebugDumpArchive},
		{"dump-manifest", "write the repository manifest as JSON", cmdDebugDumpManifest},
		{"dump-repo-objs", "write every repository object's plaintext to a file", cmdDebugDumpRepoObjs},
		{"search-repo-objs", "search the repository objects for a byte sequence", cmdDebugSearchRepoObjs},
		{"get-obj", "write one repository object, as stored, to a file", cmdDebugGetObj},
		{"put-obj", "store a file as a repository object under a given id", cmdDebugPutObj},
		{"delete-obj", "remove objects from the repository", cmdDebugDeleteObj},
		{"id-hash", "compute the chunk id of a file's contents", cmdDebugIDHash},
		{"parse-obj", "split an object file into its metadata and its plaintext", cmdDebugParseObj},
		{"format-obj", "build an object file from metadata and a plaintext", cmdDebugFormatObj},
	}
}

func cmdDebug(e *Env, args []string) int {
	if len(args) == 0 {
		printDebugUsage(e.Stdout)
		return ExitOK
	}
	name := args[0]
	for _, c := range debugCommands() {
		if c.name == name {
			return c.run(e, args[1:])
		}
	}
	e.errorf("unknown debug command %q", name)
	printDebugUsage(e.Stderr)
	return ExitError
}

func printDebugUsage(w io.Writer) {
	var b strings.Builder
	for _, c := range debugCommands() {
		fmt.Fprintf(&b, "  %-20s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "usage: borge debug <command> [options]\n\n"+
		"These commands read and write raw repository objects. put-obj and delete-obj\n"+
		"make no consistency checks at all and can destroy a repository.\n\ncommands:\n%s", b.String())
}

// ---------------------------------------------------------------- opening

// openedRaw is a repository and its key without a manifest.
//
// The commands that walk every object cannot require a manifest: a repository whose
// manifest is the damaged part is precisely when they are needed. borg gets the key by
// reading any object's header; borge unlocks the stored key blob, which needs no object at
// all and works on an empty repository too.
type openedRaw struct {
	repo *repository.Repository
	key  key.Key
	ro   *repoobj.RepoObj
}

func (o *openedRaw) Close() error {
	if o.repo == nil {
		return nil
	}
	return o.repo.Close()
}

func (e *Env) openRepoRaw(path string, exclusive bool) (*openedRaw, error) {
	repo, err := repository.Open(path, repository.Options{Exclusive: exclusive})
	if err != nil {
		return nil, err
	}
	k, _, err := repo.Unlock(e.passphrase())
	if err != nil {
		repo.Close()
		if errors.Is(err, key.ErrPassphraseWrong) {
			return nil, fmt.Errorf("%w (set BORGE_PASSPHRASE or BORG_PASSPHRASE)", err)
		}
		return nil, err
	}
	ro, err := repoobj.New(k)
	if err != nil {
		repo.Close()
		return nil, err
	}
	return &openedRaw{repo: repo, key: k, ro: ro}, nil
}

// ---------------------------------------------------------------- info

// cmdDebugInfo prints what a bug report needs.
//
// borg prints its Python, msgpack and FUSE versions here because those are the things that
// differ between installations of the same borg. borge is a static binary, so the
// equivalent facts are the Go version it was built with and the upstream borg commit it
// was ported from - the latter being the one that decides whether a repository written by
// some borg is readable at all.
func cmdDebugInfo(e *Env, args []string) int {
	fs := newFlagSet(e, "debug info")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "unknown"
	}

	fmt.Fprintf(e.Stdout, "Platform: %s/%s %s\n", runtime.GOOS, runtime.GOARCH, host)
	fmt.Fprintf(e.Stdout, "Borge: %s  Go: %s\n", version.String(), runtime.Version())
	fmt.Fprintf(e.Stdout, "Ported from borg %s @ %s, repository format version %d\n",
		version.BorgSeries, version.BorgUpstreamCommit, version.RepositoryVersion)
	fmt.Fprintf(e.Stdout, "PID: %d  CWD: %s\n", os.Getpid(), cwd)
	fmt.Fprintf(e.Stdout, "os.Args: %v\n", os.Args)
	if v, ok := e.lookup("SSH_ORIGINAL_COMMAND"); ok {
		fmt.Fprintf(e.Stdout, "SSH_ORIGINAL_COMMAND: %s\n", v)
	} else {
		fmt.Fprintf(e.Stdout, "SSH_ORIGINAL_COMMAND: none\n")
	}
	fmt.Fprintf(e.Stdout, "\nProcess ID: (%s, %d, 0)\n", repository.HostID(), os.Getpid())
	return ExitOK
}

// ---------------------------------------------------------------- dumps

// cmdDebugDumpArchiveItems writes each of an archive's item metadata stream chunks to a
// file in the current directory, decrypted and decompressed but still msgpack.
//
// The files are the stream exactly as stored, which is what makes them useful: they are
// what a msgpack decoder has to be pointed at when the question is whether borge wrote the
// stream borg expects.
func cmdDebugDumpArchiveItems(e *Env, args []string) int {
	fs := newFlagSet(e, "debug dump-archive-items")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("debug dump-archive-items needs an archive name")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	ids, err := a.ItemStreamIDs()
	if err != nil {
		return e.fail(err)
	}
	for i, id := range ids {
		obj, err := o.repo.Get(id)
		if err != nil {
			return e.fail(err)
		}
		_, data, err := o.manifest.RepoObj().Parse(id, obj, repoobj.TypeArchiveStream, repoobj.ParseOptions{})
		if err != nil {
			return e.fail(err)
		}
		name := fmt.Sprintf("%06d_%s.items", i, hex.EncodeToString(id))
		fmt.Fprintf(e.Stdout, "Dumping %s\n", name)
		if err := os.WriteFile(name, data, 0o600); err != nil {
			return e.fail(err)
		}
	}
	fmt.Fprintln(e.Stdout, "Done.")
	return ExitOK
}

// cmdDebugDumpArchive writes an archive's metadata and every item as JSON.
//
// The output is written incrementally rather than built and marshalled, because a modest
// archive is megabytes of items and a large one is gigabytes: holding the whole document
// in memory to pretty-print it would put a size limit on the command that is doing the
// diagnosing.
func cmdDebugDumpArchive(e *Env, args []string) int {
	fs := newFlagSet(e, "debug dump-archive")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 2 {
		e.errorf("debug dump-archive needs an archive name and an output file (or - for stdout)")
		return ExitError
	}
	name, target := fs.Arg(0), fs.Arg(1)

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	a, err := openArchive(o.manifest, name)
	if err != nil {
		return e.fail(err)
	}

	out, closeOut, err := e.createOut(target)
	if err != nil {
		return e.fail(err)
	}
	defer closeOut()

	if err := dumpArchive(out, o, a, name); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// dumpArchive writes the document. The shape is borg's, down to the leading underscores
// on the three keys, which mark them as borg's own framing rather than stored fields.
func dumpArchive(w io.Writer, o *opened, a *archive.Archive, name string) error {
	put := func(s string) error {
		_, err := io.WriteString(w, s)
		return err
	}

	if err := put("{\n"); err != nil {
		return err
	}
	var nameJSON strings.Builder
	writeDumpString(&nameJSON, name)
	if err := put(`    "_name": ` + nameJSON.String() + ",\n"); err != nil {
		return err
	}

	if err := put("    \"_manifest_entry\":\n"); err != nil {
		return err
	}
	if err := writeDumpBlock(w, manifestEntryDump(a), ",\n"); err != nil {
		return err
	}

	if err := put("    \"_meta\":\n"); err != nil {
		return err
	}
	meta, err := rawArchiveMeta(o, a)
	if err != nil {
		return err
	}
	if err := writeDumpBlock(w, meta, ",\n"); err != nil {
		return err
	}

	if err := put("    \"_items\": [\n"); err != nil {
		return err
	}
	first := true
	err = a.RawItems(func(v any) error {
		prepared, err := prepareDump(v)
		if err != nil {
			return err
		}
		if !first {
			if err := put(",\n"); err != nil {
				return err
			}
		}
		first = false
		return writeDumpBlock(w, prepared, "")
	})
	if err != nil {
		return err
	}
	return put("\n    ]\n}\n")
}

// dumpBlockIndent is how far each sub-document of a dump-archive dump is indented: borg
// writes the key on its own line and then the value as a block below it.
const dumpBlockIndent = 4

// writeDumpBlock writes one indented sub-document, followed by tail.
func writeDumpBlock(w io.Writer, v any, tail string) error {
	var b strings.Builder
	if err := writeDumpJSON(&b, v, dumpBlockIndent); err != nil {
		return err
	}
	pad := strings.Repeat(" ", dumpBlockIndent)
	var out bytes.Buffer
	for i, line := range strings.Split(b.String(), "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(pad + line)
	}
	out.WriteString(tail)
	_, err := w.Write(out.Bytes())
	return err
}

// manifestEntryDump is the archive's directory entry, in borg's field order.
//
// The order is not alphabetical and is not sorted here, because borg builds this dict
// itself rather than decoding it from msgpack: it is the only object in these dumps whose
// key order is a choice rather than a consequence.
func manifestEntryDump(a *archive.Archive) *dumpObject {
	info := a.Info
	tags := make([]any, 0, len(info.Tags))
	for _, t := range info.Tags {
		tags = append(tags, t)
	}
	timeString := info.TimeString
	if timeString == "" {
		timeString = "1970-01-01T00:00:00.000000"
	}
	o := newDumpObject()
	o.set("id", dumpBytes(a.ID))
	o.set("name", info.Name)
	o.set("time", timeString)
	o.set("exists", info.Exists)
	o.set("username", info.User)
	o.set("hostname", info.Host)
	o.set("size", info.Size)
	o.set("nfiles", info.NFiles)
	o.set("comment", info.Comment)
	o.set("tags", tags)
	return o
}

// rawArchiveMeta decodes the archive metadata object without going through ArchiveItem,
// for the same reason RawItems exists: a field borge does not model must still show up.
func rawArchiveMeta(o *opened, a *archive.Archive) (any, error) {
	obj, err := o.repo.Get(a.ID)
	if err != nil {
		return nil, err
	}
	_, data, err := o.manifest.RepoObj().Parse(a.ID, obj, repoobj.TypeArchiveMeta, repoobj.ParseOptions{})
	if err != nil {
		return nil, err
	}
	v, err := msgpackx.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("archive metadata is not msgpack: %w", err)
	}
	return prepareDump(v)
}

// cmdDebugDumpManifest writes the repository manifest as JSON.
func cmdDebugDumpManifest(e *Env, args []string) int {
	fs := newFlagSet(e, "debug dump-manifest")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("debug dump-manifest needs an output file (or - for stdout)")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepo(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	obj, err := o.repo.Manifest()
	if err != nil {
		return e.fail(err)
	}
	_, data, err := o.manifest.RepoObj().Parse(key.ManifestID, obj, repoobj.TypeManifest, repoobj.ParseOptions{})
	if err != nil {
		return e.fail(err)
	}
	v, err := msgpackx.Unmarshal(data)
	if err != nil {
		return e.fail(fmt.Errorf("the manifest is not msgpack: %w", err))
	}
	prepared, err := prepareDump(v)
	if err != nil {
		return e.fail(err)
	}

	out, closeOut, err := e.createOut(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	defer closeOut()
	// No trailing newline: json.dump writes none, and these dumps get compared byte for
	// byte against borg's.
	if err := writeDumpJSON(out, prepared, 4); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// cmdDebugDumpRepoObjs writes every repository object's plaintext to a file in the current
// directory, named "<chunk id>.obj".
func cmdDebugDumpRepoObjs(e *Env, args []string) int {
	fs := newFlagSet(e, "debug dump-repo-objs")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	entries, err := o.repo.List(0, nil)
	if err != nil {
		return e.fail(err)
	}
	for _, entry := range entries {
		obj, err := o.repo.Get(entry.ChunkID)
		if err != nil {
			return e.fail(err)
		}
		_, data, err := o.ro.Parse(entry.ChunkID, obj, repoobj.TypeDontCare, repoobj.ParseOptions{})
		if err != nil {
			return e.fail(err)
		}
		name := hex.EncodeToString(entry.ChunkID) + ".obj"
		fmt.Fprintf(e.Stdout, "Dumping %s\n", name)
		if err := os.WriteFile(name, data, 0o600); err != nil {
			return e.fail(err)
		}
	}
	fmt.Fprintln(e.Stdout, "Done.")
	return ExitOK
}

// searchContext is how many bytes either side of a hit are shown. borg's value.
const searchContext = 32

// cmdDebugSearchRepoObjs finds a byte sequence in the repository's plaintext.
//
// This is how a question like "which chunk holds this filename?" gets answered when the
// archive that referenced it is gone. It reads and decrypts every object, so it costs a
// full pass over the repository.
func cmdDebugSearchRepoObjs(e *Env, args []string) int {
	fs := newFlagSet(e, "debug search-repo-objs")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("debug search-repo-objs needs a search term")
		return ExitError
	}

	var wanted []byte
	switch term := fs.Arg(0); {
	case strings.HasPrefix(term, "hex:"):
		b, err := hex.DecodeString(strings.TrimPrefix(term, "hex:"))
		if err != nil {
			e.errorf("search term needs to be hex:123abc or str:foobar style")
			return ExitError
		}
		wanted = b
	case strings.HasPrefix(term, "str:"):
		wanted = []byte(strings.TrimPrefix(term, "str:"))
	default:
		e.errorf("search term needs to be hex:123abc or str:foobar style")
		return ExitError
	}
	if len(wanted) == 0 {
		e.errorf("search term needs to be hex:123abc or str:foobar style")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	entries, err := o.repo.List(0, nil)
	if err != nil {
		return e.fail(err)
	}

	var lastID []byte
	var lastData []byte
	for i, entry := range entries {
		obj, err := o.repo.Get(entry.ChunkID)
		if err != nil {
			return e.fail(err)
		}
		_, data, err := o.ro.Parse(entry.ChunkID, obj, repoobj.TypeDontCare, repoobj.ParseOptions{})
		if err != nil {
			return e.fail(err)
		}

		// A hit that straddles two objects would be missed by searching each alone. borg
		// stitches the tail of the previous object to the head of this one and searches
		// that too - which is only meaningful because a file's chunks are usually stored
		// in order, so the join is often the real byte sequence.
		//
		// The length guard is a divergence. borg computes the join as
		// last_data[-(len(wanted)-1):], and for a one-byte term that is last_data[-0:],
		// which in Python is the *whole* of last_data rather than nothing - so every hit in
		// the previous object gets reported a second time. borge reports it once.
		if len(wanted) > 1 && lastID != nil {
			joined := append(append([]byte(nil), tail(lastData, len(wanted)-1+searchContext)...),
				head(data, len(wanted)-1+searchContext)...)
			narrow := append(append([]byte(nil), tail(lastData, len(wanted)-1)...),
				head(data, len(wanted)-1)...)
			if bytes.Contains(narrow, wanted) {
				info := fmt.Sprintf("%d %s | %s", i, hex.EncodeToString(lastID), hex.EncodeToString(entry.ChunkID))
				printFinding(e, info, wanted, joined, bytes.Index(joined, wanted))
			}
		}

		if count := bytes.Count(data, wanted); count > 0 {
			info := fmt.Sprintf("%d %s #%d", i, hex.EncodeToString(entry.ChunkID), count)
			printFinding(e, info, wanted, data, bytes.Index(data, wanted))
		}

		lastID, lastData = entry.ChunkID, data
		if (i+1)%10000 == 0 {
			fmt.Fprintf(e.Stdout, "%d objects processed.\n", i+1)
		}
	}
	fmt.Fprintln(e.Stdout, "Done.")
	return ExitOK
}

func printFinding(e *Env, info string, wanted, data []byte, offset int) {
	if offset < 0 {
		return
	}
	// A second divergence from borg, and the same cause. borg writes
	// data[offset - context : offset], and when the hit is closer to the start than the
	// context the left index goes negative, which Python reads as an offset from the *end*:
	// for an object bigger than the context that makes the start index larger than the stop
	// index and the context before the hit prints as empty. borge clamps, and shows the
	// bytes that are actually there.
	before := data[max(0, offset-searchContext):offset]
	end := offset + len(wanted)
	after := data[min(end, len(data)):min(end+searchContext, len(data))]
	fmt.Fprintf(e.Stdout, "%s: %s %s %s == %s %s %s\n",
		info, hex.EncodeToString(before), hex.EncodeToString(wanted), hex.EncodeToString(after),
		pyBytesRepr(before), pyBytesRepr(wanted), pyBytesRepr(after))
}

func head(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func tail(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[len(b)-n:]
}

// ---------------------------------------------------------------- objects

// cmdDebugGetObj writes one repository object to a file, exactly as stored: still
// encrypted, still compressed, header and all.
func cmdDebugGetObj(e *Env, args []string) int {
	fs := newFlagSet(e, "debug get-obj")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 2 {
		e.errorf("debug get-obj needs a hex object id and an output file")
		return ExitError
	}
	id, err := parseChunkID(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	data, err := o.repo.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrObjectNotFound) {
			e.errorf("object %s not found.", fs.Arg(0))
			return ExitError
		}
		return e.fail(err)
	}
	if err := os.WriteFile(fs.Arg(1), data, 0o600); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stdout, "object %s fetched.\n", fs.Arg(0))
	return ExitOK
}

// cmdDebugPutObj stores a file's contents as a repository object under a given id.
//
// Nothing is checked: not that the contents are a valid object, not that the id matches
// them. That is the point - it is how a deliberately corrupt object gets into a repository
// so that check and check --repair can be tested against it.
//
// # A divergence
//
// borge takes the exclusive lock; borg takes a shared one. Writing an object rewrites the
// chunk index at close, and two concurrent writers would lose one of the two objects. The
// visible difference is that this cannot run alongside another borge, which for a command
// that exists to corrupt a repository on purpose is the safer default.
func cmdDebugPutObj(e *Env, args []string) int {
	fs := newFlagSet(e, "debug put-obj")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 2 {
		e.errorf("debug put-obj needs a hex object id and a file to read")
		return ExitError
	}
	id, err := parseChunkID(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	data, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return e.fail(err)
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	if _, err := o.repo.Put(id, data); err != nil {
		return e.fail(err)
	}
	// No cache wraps this command, so the buffered pack has to be flushed here rather
	// than left for whatever would normally do it.
	if err := o.repo.Flush(); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.Stdout, "object %s put.\n", fs.Arg(0))
	return ExitOK
}

// cmdDebugDeleteObj removes objects from the repository.
//
// An unreadable id, or one the repository does not have, is reported and the rest are
// still deleted - borg's behaviour, and the right one for a command usually given a list
// produced by something else.
func cmdDebugDeleteObj(e *Env, args []string) int {
	fs := newFlagSet(e, "debug delete-obj")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() < 1 {
		e.errorf("debug delete-obj needs at least one hex object id")
		return ExitError
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, true)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	status := ExitOK
	for _, hexID := range fs.Args() {
		id, err := parseChunkID(hexID)
		if err != nil {
			fmt.Fprintf(e.Stdout, "object id %s is invalid.\n", hexID)
			status = ExitWarning
			continue
		}
		switch err := o.repo.DeleteObject(id); {
		case err == nil:
			fmt.Fprintf(e.Stdout, "object %s deleted.\n", hexID)
		case errors.Is(err, repository.ErrObjectNotFound):
			fmt.Fprintf(e.Stdout, "object %s not found.\n", hexID)
			status = ExitWarning
		default:
			return e.fail(err)
		}
	}
	fmt.Fprintln(e.Stdout, "Done.")
	return status
}

// cmdDebugIDHash computes the chunk id a file's contents would get in this repository.
//
// It needs the repository because the id hash is keyed: the same bytes have different ids
// in two repositories, which is what stops one repository's chunk ids from telling anyone
// anything about another's.
func cmdDebugIDHash(e *Env, args []string) int {
	fs := newFlagSet(e, "debug id-hash")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		e.errorf("debug id-hash needs a file to hash")
		return ExitError
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	fmt.Fprintf(e.Stdout, "%s\n", hex.EncodeToString(o.key.IDHash(data)))
	return ExitOK
}

// cmdDebugParseObj splits an object file into its metadata and its plaintext.
//
// The object comes from a file rather than the repository, so an object recovered from a
// damaged pack by hand can be examined without putting it back first.
func cmdDebugParseObj(e *Env, args []string) int {
	fs := newFlagSet(e, "debug parse-obj")
	var common commonFlags
	common.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 4 {
		e.errorf("debug parse-obj needs a hex object id, the object file, " +
			"the file to write the plaintext to, and the file to write the metadata to")
		return ExitError
	}
	id, err := parseChunkID(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	obj, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return e.fail(err)
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	meta, data, err := o.ro.Parse(id, obj, repoobj.TypeDontCare, repoobj.ParseOptions{})
	if err != nil {
		return e.fail(err)
	}
	if err := os.WriteFile(fs.Arg(2), data, 0o600); err != nil {
		return e.fail(err)
	}

	var b strings.Builder
	if err := writeDumpJSON(&b, objMetaDump(meta), 0); err != nil {
		return e.fail(err)
	}
	if err := os.WriteFile(fs.Arg(3), []byte(b.String()), 0o600); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// objMetaDump renders an object's metadata in the order it is stored in, so that a dump
// and the msgpack it came from can be read side by side.
func objMetaDump(meta *repoobj.Meta) *dumpObject {
	o := newDumpObject()
	o.set("type", meta.Type)
	if meta.SizeSet {
		o.set("size", int64(meta.Size))
	}
	o.set("ctype", int64(meta.CType))
	o.set("clevel", int64(meta.CLevel))
	o.set("csize", int64(meta.CSize))
	if meta.PSizeSet {
		o.set("psize", int64(meta.PSize))
	}
	if meta.OLevelSet {
		o.set("olevel", int64(meta.OLevel))
	}
	return o
}

// cmdDebugFormatObj builds an object file from a plaintext and a metadata JSON file.
//
// Only "type" is read from the metadata. The compression fields are recomputed, because
// the object is being compressed here and now: keeping the old ctype and csize while
// writing differently compressed bytes would produce an object that cannot be read back.
func cmdDebugFormatObj(e *Env, args []string) int {
	fs := newFlagSet(e, "debug format-obj")
	var common commonFlags
	common.register(fs)
	compression := fs.String("C", "lz4", "compression spec, e.g. zstd,3")
	fs.StringVar(compression, "compression", "lz4", "compression spec")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 4 {
		e.errorf("debug format-obj needs a hex object id, the plaintext file, " +
			"the metadata JSON file, and the object file to write")
		return ExitError
	}
	id, err := parseChunkID(fs.Arg(0))
	if err != nil {
		return e.fail(err)
	}
	data, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return e.fail(err)
	}
	metaJSON, err := os.ReadFile(fs.Arg(2))
	if err != nil {
		return e.fail(err)
	}
	roType, err := objTypeFromJSON(metaJSON)
	if err != nil {
		return e.fail(err)
	}
	compressor, err := compress.FromSpec(*compression)
	if err != nil {
		return e.fail(err)
	}

	path, err := e.resolveRepo(common.repo)
	if err != nil {
		return e.fail(err)
	}
	o, err := e.openRepoRaw(path, false)
	if err != nil {
		return e.fail(err)
	}
	defer o.Close()

	o.ro.SetCompressor(compressor)
	obj, err := o.ro.Format(id, &repoobj.Meta{Meta: compress.Meta{Type: roType}, Type: roType}, data)
	if err != nil {
		return e.fail(err)
	}
	if err := os.WriteFile(fs.Arg(3), obj, 0o600); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// objTypeFromJSON reads the "type" field, defaulting to a file content chunk the way borg
// does. The rest of the document is ignored; see cmdDebugFormatObj.
func objTypeFromJSON(b []byte) (string, error) {
	var doc struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("the metadata file is not JSON: %w", err)
	}
	if doc.Type == nil {
		return repoobj.TypeFileStream, nil
	}
	if *doc.Type == repoobj.TypeDontCare {
		return "", fmt.Errorf("%q is not a concrete object type", *doc.Type)
	}
	return *doc.Type, nil
}

// ---------------------------------------------------------------- helpers

// parseChunkID accepts a chunk id as hex, insisting on the full 32 bytes.
//
// A short id is rejected rather than treated as a prefix. These commands write to the
// repository, and guessing which object a prefix meant is not a guess worth making there.
func parseChunkID(s string) ([]byte, error) {
	id, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("object id %s is invalid: not hexadecimal", s)
	}
	if len(id) != repoobj.ChunkIDSize {
		return nil, fmt.Errorf("object id %s is invalid: %d bytes, want %d",
			s, len(id), repoobj.ChunkIDSize)
	}
	return id, nil
}

// createOut opens an output file, or returns stdout for "-".
func (e *Env) createOut(target string) (io.Writer, func() error, error) {
	if target == "-" {
		return e.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(target)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// pyBytesRepr renders bytes the way Python's repr does, because that is what borg prints
// in a search finding and these findings are meant to be compared.
func pyBytesRepr(b []byte) string {
	quote := byte('\'')
	if bytes.IndexByte(b, '\'') >= 0 && bytes.IndexByte(b, '"') < 0 {
		quote = '"'
	}
	var out strings.Builder
	out.WriteByte('b')
	out.WriteByte(quote)
	for _, c := range b {
		switch {
		case c == quote || c == '\\':
			out.WriteByte('\\')
			out.WriteByte(c)
		case c == '\t':
			out.WriteString(`\t`)
		case c == '\n':
			out.WriteString(`\n`)
		case c == '\r':
			out.WriteString(`\r`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&out, `\x%02x`, c)
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte(quote)
	return out.String()
}
