// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of get_tar_filter and the filter plumbing in borg's
// src/borg/archiver/tar_cmds.py and src/borg/helpers/process.py.
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
	"os/exec"
	"strconv"
	"strings"

	"github.com/renesugar/borge/internal/compress"
)

// --tar-filter: what compresses a tarball on the way out and decompresses it on the way in.
//
// # Why a filter program and not a library
//
// borg's own comment gives three reasons for shelling out: a compressor running in-process
// competes with borg for CPU; a library ties the tool to whatever formats that library
// supports; and a system may ship something better than the built-in, like pigz or pxz,
// which use several threads. borge inherits the decision rather than re-deciding it,
// because the decision is *visible*: "--tar-filter 'pigz -9'" has to mean the same thing in
// both tools, and a borge that compressed in-process would silently ignore what it was told.
//
// zstd is borg's one exception - it is compressed in-process, since libzstd's threading
// runs outside the GIL and there is no better external tool - and borge follows that too.
//
// # What borge did before this
//
// borge had no --tar-filter and compressed a ".gz" name with compress/gzip. Two silent
// differences, in opposite directions:
//
//	.tar.xz .tar.zst .tar.bz2 .tar.lz4    borg compressed, borge wrote a PLAIN TAR
//	                                      under a compressed name
//	foo.gz                                borg wrote a plain tar, borge compressed it
//
// The first is the bad one: "borge export-tar arch backup.tar.xz" produced a file that no
// xz can open, named as though it could, and reported success. See DIVERGENCES.md #49.

// inProcessZstd is borg's IN_PROCESS_ZSTD sentinel: not a command line, a request to use
// the linked-in zstd rather than to run anything.
const inProcessZstd = "zstd (in-process)"

// tarFilterFor is borg's get_tar_filter: the filter a file name implies under the default
// --tar-filter=auto. An empty result means no filtering at all.
//
// The suffixes are borg's exactly, and the exactness matters in both directions: ".tar.gz"
// and ".tgz" are compressed, a bare ".gz" is not. borge used to match any ".gz", so
// "export-tar arch dump.gz" gzipped where borg would not - the sort of difference that only
// shows up when somebody's script feeds the file to something else.
func tarFilterFor(name string, decompress bool) string {
	suffixes := []struct {
		endings  []string
		compress string
	}{
		{[]string{".tar.gz", ".tgz"}, "gzip"},
		{[]string{".tar.bz2", ".tbz"}, "bzip2"},
		{[]string{".tar.xz", ".txz"}, "xz"},
		{[]string{".tar.lz4"}, "lz4"},
		{[]string{".tar.zstd", ".tar.zst", ".tzst"}, inProcessZstd},
	}
	for _, s := range suffixes {
		for _, end := range s.endings {
			if !strings.HasSuffix(name, end) {
				continue
			}
			if s.compress == inProcessZstd {
				return inProcessZstd
			}
			if decompress {
				return s.compress + " -d"
			}
			return s.compress
		}
	}
	// Also the answer for "-", which ends with none of these: a tar written to stdout is
	// not compressed unless --tar-filter says so.
	return ""
}

// zstdWorkers reads BORGE_ZSTD_MT_WORKERS, borg's knob for multithreaded zstd.
//
// Zero means "the library's default", which for borge is one worker per CPU - so borge is
// multithreaded where borg is single-threaded unless told otherwise. Only the speed differs;
// the bytes a decompressor sees are the same either way.
func (e *Env) zstdWorkers() int {
	v, ok := e.lookupBorg("ZSTD_MT_WORKERS")
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		// borg ignores an unparseable value rather than failing the backup, and so does
		// this: a mistyped tuning variable should not stop a tarball being written.
		return 0
	}
	return n
}

// tarWriteFilter wires a filter between borge and the destination:
//
//	borge -> filter -> sink
//
// Write the tar to the returned WriteCloser. Close finishes the filter and waits for it,
// which is when a failing filter is reported - a compressor that dies half way through has
// still produced a truncated file, and saying so is the whole point of waiting.
func (e *Env) tarWriteFilter(cmdline string, sink io.Writer) (io.WriteCloser, error) {
	if cmdline == "" {
		return nopWriteCloser{sink}, nil
	}
	if cmdline == inProcessZstd {
		return compress.NewZstdStreamWriter(sink, e.zstdWorkers())
	}
	cmd, err := e.filterCommand(cmdline)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = sink
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, filterStartError(cmdline, err)
	}
	return &filterWriter{cmdline: cmdline, cmd: cmd, stdin: stdin}, nil
}

// tarReadFilter wires a filter between the source and borge:
//
//	source -> filter -> borge
//
// Close waits for the filter, so a decompressor that reports corruption fails the import
// rather than being taken for a short tarball.
func (e *Env) tarReadFilter(cmdline string, source io.Reader) (io.ReadCloser, error) {
	if cmdline == "" {
		return io.NopCloser(source), nil
	}
	if cmdline == inProcessZstd {
		return compress.NewZstdStreamReader(source)
	}
	cmd, err := e.filterCommand(cmdline)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = source
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, filterStartError(cmdline, err)
	}
	return &filterReader{cmdline: cmdline, cmd: cmd, stdout: stdout}, nil
}

// filterCommand splits a filter command line and prepares it to run.
//
// The environment is passed through unchanged. borg's prepare_subprocess_env has work to do
// here - a PyInstaller bundle rewrites LD_LIBRARY_PATH and a system binary must not pick up
// the bundle's libraries - and a Go binary has no such problem.
//
// One thing borge does not reproduce: borg's filter ignores SIGINT, so a Ctrl-C reaches
// borg first and borg kills the filter. Go cannot run code between fork and exec, so
// borge's filter dies first and borge then fails writing to a closed pipe. Both tools leave
// a truncated output file; only the message differs.
func (e *Env) filterCommand(cmdline string) (*exec.Cmd, error) {
	words, err := splitCommandLine(cmdline)
	if err != nil {
		return nil, fmt.Errorf("filter %s: %w", cmdline, err)
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("filter %s: an empty command line is not permitted", cmdline)
	}
	cmd := exec.Command(words[0], words[1:]...)
	cmd.Env = os.Environ()
	cmd.Stderr = e.Stderr
	return cmd, nil
}

// filterStartError reports a filter that could not be started, in borg's two parts: which
// executable was not found, and that the filter therefore could not be created.
func filterStartError(cmdline string, err error) error {
	return fmt.Errorf("filter %s: process creation failed: %w", cmdline, err)
}

// filterExitError reports a filter that ran and failed, in borg's wording.
func filterExitError(cmdline string, err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("filter %s failed, rc=%d", cmdline, exit.ExitCode())
	}
	return fmt.Errorf("filter %s failed: %w", cmdline, err)
}

type filterWriter struct {
	cmdline string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
}

func (f *filterWriter) Write(p []byte) (int, error) { return f.stdin.Write(p) }

// Close closes the pipe first, which is how the filter learns there is no more input, and
// only then waits. Waiting with the pipe still open would hang forever.
func (f *filterWriter) Close() error {
	closeErr := f.stdin.Close()
	if err := f.cmd.Wait(); err != nil {
		return filterExitError(f.cmdline, err)
	}
	return closeErr
}

type filterReader struct {
	cmdline string
	cmd     *exec.Cmd
	stdout  io.ReadCloser
}

func (f *filterReader) Read(p []byte) (int, error) { return f.stdout.Read(p) }

// Close waits for the filter and reports a non-zero exit.
//
// Nothing drains what is left in the pipe: borge stops reading when the tar stream ends,
// which for a concatenated or padded file can be before the filter has written everything.
// The filter then dies of a broken pipe, and the exit status of a process killed by SIGPIPE
// is not a failure of the import - so it is not reported as one.
func (f *filterReader) Close() error {
	f.stdout.Close()
	if err := f.cmd.Wait(); err != nil {
		if isSignalExit(err) {
			return nil
		}
		return filterExitError(f.cmdline, err)
	}
	return nil
}

// isSignalExit reports whether a process died of a signal rather than exiting.
func isSignalExit(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	// ExitCode is -1 exactly when the process was terminated by a signal.
	return exit.ExitCode() < 0
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// splitCommandLine is Python's shlex.split in POSIX mode, which is how borg turns
// "--tar-filter 'gzip -9'" into an argv.
//
// No shell is involved in either tool: there is no globbing, no variable substitution and
// no pipeline, and borg's own comment on that is "Sorry pal, shell mode is a no-no". A
// filter needing a pipeline can be given as a script.
func splitCommandLine(s string) ([]string, error) {
	var words []string
	var current strings.Builder
	started := false

	flush := func() {
		if started {
			words = append(words, current.String())
			current.Reset()
			started = false
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			// Single quotes are literal: not even a backslash escapes inside them.
			started = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("No closing quotation")
			}
			current.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case '"':
			started = true
			closed := false
			for i++; i < len(s); i++ {
				if s[i] == '"' {
					closed = true
					break
				}
				// Inside double quotes a backslash escapes only these four; before
				// anything else it stays a backslash, which is POSIX shell behaviour and
				// shlex's.
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				current.WriteByte(s[i])
			}
			if !closed {
				return nil, fmt.Errorf("No closing quotation")
			}
		case '\\':
			started = true
			if i+1 >= len(s) {
				return nil, fmt.Errorf("No escaped character")
			}
			i++
			current.WriteByte(s[i])
		default:
			started = true
			current.WriteByte(c)
		}
	}
	flush()
	return words, nil
}
