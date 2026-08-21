// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borg's src/borg/archiver/serve_cmd.py and of
// borg_permissions in src/borg/repository.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/renesugar/borge/internal/store"
)

// serve is the other end of a rest:// repository.
//
// It has two modes in borg and only one of them is borge's:
//
//   - "borg serve" with no option serves a *borg 1.x* repository over the old RPC
//     protocol, which is what "borg transfer --from-borg1" talks to. Reading borg 1
//     repositories is a §0.6 non-goal, so borge refuses this by name rather than by
//     pretending the option is unknown.
//   - "borg serve --rest --backend FILE:<path>" serves a current repository as the server
//     side of a rest:// URL, speaking HTTP over stdin and stdout. That is this.
//
// The client starts it: directly for a local rest:// repository, or through ssh for a
// remote one. It is not a command a person usually runs.
//
// # stdout belongs to the protocol
//
// Every byte written to stdout here is part of an HTTP response. A stray print - a
// warning, a progress line, a debug message - corrupts the stream, and the client's
// failure would be a parse error a long way from the cause. So this command writes
// diagnostics to stderr only, and TestServeKeepsStdoutForTheProtocol holds it.

// stringList collects a repeatable option, as borg's "action=append" does.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ", ") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func cmdServe(e *Env, args []string) int {
	fs := newFlagSet(e, "serve")
	rest := fs.Bool("rest", false,
		"serve a current repository as a rest:// server (HTTP over stdio); requires --backend")
	backend := fs.String("backend", "",
		"(with --rest) the repository to serve, as FILE:<path>")
	permissions := fs.String("permissions", "",
		"all, no-delete, write-only or read-only (default from BORGE_REPO_PERMISSIONS)")
	var restrictToPaths, restrictToRepositories stringList
	fs.Var(&restrictToPaths, "restrict-to-path",
		"only serve repositories under PATH (repeatable)")
	fs.Var(&restrictToRepositories, "restrict-to-repository",
		"only serve the repository at PATH, and no subdirectory of it (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	if !*rest {
		// borg's other mode, and the reason it cannot be borge's: it speaks borg 1.x's
		// RPC protocol to a borg 1.x repository.
		e.errorf("borge serve without --rest serves a borg 1.x repository over the legacy " +
			"protocol, and borge does not read borg 1.x repositories (docs/PORTING_PLAN.md §0.6). " +
			"Use 'borge serve --rest --backend FILE:<path>' to serve a current repository.")
		return ExitError
	}
	if *backend == "" {
		// borg's message, near enough to match a script that greps for it.
		e.errorf("borge serve --rest requires --backend FILE:<path>.")
		return ExitError
	}

	path, err := servePath(*backend)
	if err != nil {
		return e.fail(err)
	}
	if err := checkServeRestrictions(path, restrictToPaths, restrictToRepositories); err != nil {
		return e.fail(err)
	}

	mode := *permissions
	if mode == "" {
		if v, ok := e.lookupBorg("REPO_PERMISSIONS"); ok && v != "" {
			mode = v
		} else {
			mode = "all"
		}
	}
	perms, err := servePermissions(mode)
	if err != nil {
		return e.fail(err)
	}

	backendStore, err := store.NewPosixFS(path, perms)
	if err != nil {
		return e.fail(err)
	}
	if err := store.NewRESTServer(backendStore).ServeStdio(e.Stdin, e.Stdout); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// servePath turns the FILE: backend argument into an absolute path.
//
// borg's own comment calls FILE: a hack, and it is one worth keeping: a file:// URI cannot
// carry a relative path or a "~", and this argument is built from whatever the client
// wrote after "rest://…/". So it is resolved here the way borg resolves it - user
// expansion, then absolute - rather than being required to be absolute already.
func servePath(backend string) (string, error) {
	if !strings.HasPrefix(backend, "FILE:") {
		return "", fmt.Errorf("--backend must be FILE:<path>, not %q", backend)
	}
	path := strings.TrimPrefix(backend, "FILE:")
	if path == "" {
		return "", fmt.Errorf("--backend FILE: needs a path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	return filepath.Abs(path)
}

// checkServeRestrictions enforces --restrict-to-path and --restrict-to-repository.
//
// This is the only thing standing between an ssh key that may run "borge serve" and every
// repository on the machine, so it is checked before the repository is opened and the
// comparison is on resolved paths: without resolving, a symlink or a "../.." inside the
// requested path would step outside the allowed one.
func checkServeRestrictions(path string, paths, repositories stringList) error {
	if len(paths) == 0 && len(repositories) == 0 {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// A repository that does not exist yet is allowed by --restrict-to-repository
		// (borg says so explicitly: the client may initialise one there), so a path that
		// cannot be resolved is compared in its cleaned form instead.
		resolved = filepath.Clean(path)
	}
	withSeparator := resolved + string(filepath.Separator)

	for _, allowed := range paths {
		prefix, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			prefix = filepath.Clean(allowed)
		}
		// Subdirectories are allowed, which is the difference from the other option.
		if strings.HasPrefix(withSeparator, prefix+string(filepath.Separator)) {
			return nil
		}
	}
	for _, allowed := range repositories {
		exact, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			exact = filepath.Clean(allowed)
		}
		if exact == resolved {
			return nil
		}
	}
	// borg's PathNotAllowed, whose message names the path that was refused.
	return fmt.Errorf("path not allowed: %s", resolved)
}

// servePermissions maps borg's four permission names onto the namespace permissions the
// store enforces.
//
// The table is borg's borg_permissions, and every entry in it has a reason that is not
// obvious from its name - which is why it is copied rather than derived:
//
//   - locks/ is writable and deletable in *every* mode, including read-only, because a
//     reader still takes a shared lock and has to be able to remove it again;
//   - config/ allows overwriting in every writing mode, because the manifest lives there;
//   - index/ allows overwriting and deleting even under no-delete, because incremental
//     indexes are merged and the merged one replaces them;
//   - packs/ allows reading under write-only, and that is deliberate: "borg create" has to
//     be able to see which chunks are already there, or it would store everything twice.
func servePermissions(mode string) (store.Permissions, error) {
	switch mode {
	case "all":
		// nil is "no restriction at all": the permission system is not used.
		return nil, nil
	case "no-delete":
		return store.Permissions{
			"":         "lr",
			"archives": "lrw",
			"cache":    "lrwWD",
			"config":   "lrW",
			"index":    "lrwWD",
			"keys":     "lr",
			"locks":    "lrwD",
			"packs":    "lrw",
		}, nil
	case "write-only":
		return store.Permissions{
			"":         "l",
			"archives": "lw",
			"cache":    "lrwWD",
			"config":   "lrW",
			"index":    "lrwWD",
			"keys":     "lr",
			"locks":    "lrwD",
			"packs":    "lw",
		}, nil
	case "read-only":
		return store.Permissions{"": "lr", "locks": "lrwD"}, nil
	default:
		return nil, fmt.Errorf("Invalid BORG_REPO_PERMISSIONS value: %s, should be one of: "+
			"all, no-delete, write-only, read-only.", mode)
	}
}
