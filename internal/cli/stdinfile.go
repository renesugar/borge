// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's stdin/--content-from-command handling in
// src/borg/archiver/create_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"strconv"

	"github.com/renesugar/borge/internal/archive"
)

// stdinFlags describe the single file a stream is stored as.
//
// Two things produce such a stream: a "-" among the paths, which reads standard input, and
// --content-from-command, which runs a command and reads its output. Both exist for the
// same job - archiving something that was never a file, a database dump being the usual
// case - so the naming and ownership options are shared.
type stdinFlags struct {
	name    string
	mode    string
	user    string
	group   string
	fromCmd bool
}

func (s *stdinFlags) register(fs *flagSet) {
	fs.StringVar(&s.name, "stdin-name", "stdin", "the path to store streamed content under")
	fs.StringVar(&s.mode, "stdin-mode", "0660", "the permission bits to store for it")
	fs.StringVar(&s.user, "stdin-user", "", "the owner to store for it (default: store none)")
	fs.StringVar(&s.group, "stdin-group", "", "the group to store for it (default: store none)")
	fs.BoolVar(&s.fromCmd, "content-from-command", false,
		"run the given command and archive its output as one file")
}

// options resolve the names to ids and the mode to bits.
//
// borg refuses an unknown user or group - "no such user: alice" - rather than storing the
// name with no id behind it, and so does this: an archive whose ownership cannot be
// restored is worse than one that was never made.
func (s *stdinFlags) options() (archive.StreamOptions, error) {
	opts := archive.StreamOptions{Name: s.name}
	if s.name == "" {
		return opts, errors.New("--stdin-name must not be empty; it is the path the " +
			"content is stored under")
	}

	mode, err := strconv.ParseInt(s.mode, 8, 64)
	if err != nil || mode < 0 || mode > 0o7777 {
		return opts, fmt.Errorf("--stdin-mode %q is not an octal permission, e.g. 0644", s.mode)
	}
	opts.Mode = mode

	if s.user != "" {
		u, err := user.Lookup(s.user)
		if err != nil {
			return opts, fmt.Errorf("no such user: %s", s.user)
		}
		id, err := strconv.ParseInt(u.Uid, 10, 64)
		if err != nil {
			return opts, fmt.Errorf("user %s has no numeric id", s.user)
		}
		opts.User, opts.UID = s.user, &id
	}
	if s.group != "" {
		g, err := user.LookupGroup(s.group)
		if err != nil {
			return opts, fmt.Errorf("no such group: %s", s.group)
		}
		id, err := strconv.ParseInt(g.Gid, 10, 64)
		if err != nil {
			return opts, fmt.Errorf("group %s has no numeric id", s.group)
		}
		opts.Group, opts.GID = s.group, &id
	}
	return opts, nil
}

// streamSource opens one stream and returns a cleanup to run when it has been read.
type streamSource func(*Env) (io.Reader, func() error, error)

// splitStreams separates the streams from the paths.
//
// borg treats a "-" path as standard input's content and --content-from-command as a
// command producing it; everything else is walked. The paths are returned with the "-"
// entries removed, so the walk never sees one.
func splitStreams(e *Env, paths []string, fromCommand bool, positional []string) ([]streamSource, []string, error) {
	if fromCommand {
		cmdline := append([]string{}, positional...)
		return []streamSource{func(e *Env) (io.Reader, func() error, error) {
			cmd := exec.Command(cmdline[0], cmdline[1:]...)
			cmd.Stderr = e.Stderr
			out, err := cmd.StdoutPipe()
			if err != nil {
				return nil, nil, err
			}
			if err := cmd.Start(); err != nil {
				return nil, nil, fmt.Errorf("could not run %s: %w", cmdline[0], err)
			}
			// Waited for after the content has been read, and a non-zero exit fails the
			// backup: a truncated dump stored as a complete one is the worst outcome here.
			return out, func() error {
				if err := cmd.Wait(); err != nil {
					return fmt.Errorf("the command producing the content failed: %w", err)
				}
				return nil
			}, nil
		}}, nil, nil
	}

	var streams []streamSource
	var kept []string
	for _, p := range paths {
		if p != "-" {
			kept = append(kept, p)
			continue
		}
		streams = append(streams, func(e *Env) (io.Reader, func() error, error) {
			if e.Stdin == nil {
				return nil, nil, errors.New(`"-" reads the content from standard input, ` +
					"and this command has none")
			}
			return e.Stdin, func() error { return nil }, nil
		})
	}
	if len(streams) > 1 {
		return nil, nil, errors.New(`"-" may be given once: standard input is one stream, ` +
			"so a second would store an empty file")
	}
	return streams, kept, nil
}
