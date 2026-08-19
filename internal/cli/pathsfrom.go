// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the --paths-from-* handling in borg's
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
	"strings"
)

// pathsFromFlags are the three ways "create" can be handed its list of paths instead of
// being given them as arguments.
//
// # What they change beyond where the list comes from
//
// A path from one of these lists is archived *and not descended into*. borg's own words:
// "all control is external: it will back up all files given - no more, no less". So a
// directory in the list contributes its own entry and nothing else, and the include and
// exclude patterns are not applied - measured, not inferred: borg archives a path from
// stdin that an --exclude matches.
//
// That makes them a different tool from "borge create DIR": the caller is expected to be
// something like find(1), which has already decided.
type pathsFromFlags struct {
	stdin        bool
	command      bool
	shellCommand bool
	delimiter    string
	delimiterSet bool
}

func (p *pathsFromFlags) register(fs *flagSet) {
	fs.BoolVar(&p.stdin, "paths-from-stdin", false,
		"read the paths to archive from stdin, one per line, and descend into none of them")
	fs.BoolVar(&p.command, "paths-from-command", false,
		"run the given command and read the paths to archive from its output")
	fs.BoolVar(&p.shellCommand, "paths-from-shell-command", false,
		"run the given shell command and read the paths to archive from its output")
	fs.Var(delimiterValue{p}, "paths-delimiter",
		`what separates the paths, with escapes: "\n" (default) or "\0" for find -print0`)
}

// delimiterValue records that the option was given, so that giving it without a source can
// be reported rather than silently ignored.
type delimiterValue struct{ p *pathsFromFlags }

func (d delimiterValue) String() string { return d.p.delimiter }

func (d delimiterValue) Set(v string) error {
	d.p.delimiter, d.p.delimiterSet = v, true
	return nil
}

// any reports whether the paths come from a list rather than from the command line.
func (p *pathsFromFlags) any() bool { return p.stdin || p.command || p.shellCommand }

// check validates the combination, in borg's terms.
//
// positional is what is left on the command line after the archive name: nothing for
// --paths-from-stdin, and the command to run for the other two.
func (p *pathsFromFlags) check(positional []string) error {
	n := 0
	for _, on := range []bool{p.stdin, p.command, p.shellCommand} {
		if on {
			n++
		}
	}
	if n > 1 {
		return errors.New("give one of --paths-from-stdin, --paths-from-command and " +
			"--paths-from-shell-command, not several")
	}
	if p.stdin && len(positional) > 0 {
		return errors.New("--paths-from-stdin reads the paths from stdin, so there is " +
			"nothing to pass as a PATH")
	}
	if (p.command || p.shellCommand) && len(positional) == 0 {
		return errors.New("no command given; the arguments after the archive name are " +
			"the command whose output lists the paths")
	}
	return nil
}

// separator is the delimiter with its escapes resolved.
func (p *pathsFromFlags) separator() (byte, error) {
	if !p.delimiterSet || p.delimiter == "" {
		return '\n', nil
	}
	unescaped, err := unescape(p.delimiter)
	if err != nil {
		return 0, err
	}
	if len(unescaped) != 1 {
		return 0, fmt.Errorf("--paths-delimiter takes one character, not %q", p.delimiter)
	}
	return unescaped[0], nil
}

// unescape resolves the backslash escapes borg's eval_escapes accepts, which is how "\0"
// on a command line becomes a NUL byte rather than a backslash and a zero.
func unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("--paths-delimiter %q ends in a backslash", s)
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		default:
			return "", fmt.Errorf(`--paths-delimiter %q: \%c is not an escape borge knows `+
				`(it knows \n, \t, \r, \0 and \\)`, s, s[i])
		}
	}
	return b.String(), nil
}

// read produces the list of paths.
func (p *pathsFromFlags) read(e *Env, positional []string) ([]string, error) {
	sep, err := p.separator()
	if err != nil {
		return nil, err
	}

	var source io.Reader
	switch {
	case p.stdin:
		source = e.Stdin
		if source == nil {
			return nil, errors.New("--paths-from-stdin: this command has no stdin to read")
		}
	default:
		var cmd *exec.Cmd
		if p.shellCommand {
			cmd = exec.Command("sh", "-c", strings.Join(positional, " "))
		} else {
			cmd = exec.Command(positional[0], positional[1:]...)
		}
		cmd.Stderr = e.Stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("the command listing the paths failed: %w", err)
		}
		source = strings.NewReader(string(out))
	}

	data, err := io.ReadAll(source)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, field := range strings.Split(string(data), string(sep)) {
		// A trailing separator is normal - find(1) writes one after the last path - so an
		// empty entry is skipped rather than treated as the empty path, which create
		// refuses.
		if field != "" {
			paths = append(paths, field)
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("the list of paths is empty; nothing would be archived")
	}
	return paths, nil
}
