// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the version command in borg's
// src/borg/archiver/version_cmd.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/renesugar/borge/internal/version"
)

// cmdVersion prints the client and server versions.
//
// # Why there is a "server" at all
//
// borg prints "<client> / <server>" because a repository may be reached over ssh, where
// the other end runs its own borg and the two versions can differ - which is the usual
// cause of an unexplained failure. borge has no remote backends yet, so the two are always
// the same value.
//
// Printing both anyway is deliberate: the shape is what scripts parse, and a borge that
// printed one field today and two after the remote backends land would break them then.
func cmdVersion(e *Env, args []string) int {
	fs := newFlagSet(e, "version")
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	long := fs.Bool("long", false, "also print the build and interoperability details (borge only)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	client := version.Version
	// No remote backends yet, so the far end is this same process.
	server := client

	switch {
	case *asJSON:
		// Two keys, because that is what borg sends. borge used to add revision,
		// borg_series, borg_commit and repository_version here - useful facts, and not
		// borg's document: --json is an API, and a frontend iterating the object would
		// see four fields borg never produces. They are all in "version --long", which is
		// borge's own output and the right home for them. See docs/DIVERGENCES.md #42.
		out := versionJSON{Client: client, Server: server}
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return e.fail(err)
		}
	case *long:
		fmt.Fprint(e.Stdout, version.Long())
	default:
		fmt.Fprintf(e.Stdout, "%s / %s\n", client, server)
	}
	return ExitOK
}

// versionJSON is the machine-readable form. The borg fields are what make an answer to
// "can this build read that repository?" possible without running it.
type versionJSON struct {
	Client string `json:"client"`
	Server string `json:"server"`
}
