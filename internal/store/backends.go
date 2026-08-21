// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/store.py's get_backend (borgstore 0.6.1,
// BSD 3-Clause, Copyright (C) 2026 Thomas Waldmann), together with the backend selection
// borg does around it in src/borg/repository.py.
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"fmt"
	"strings"

	"github.com/renesugar/borge/internal/location"
)

// NewBackend returns the backend a location names.
//
// This is the one place that turns "where the repository is" into "how its bytes are
// reached", so it is also the one place that has to say no. Everything above it - the
// repository, the CLI, every command - holds a location and never asks what kind it is.
//
// borgstore's get_backend does the same job from a URL string; borg calls it for every
// proto but "rest", which it builds itself because the server is borg (repository.py's
// build_rest_backend). borge will need the same exception when that backend lands.
func NewBackend(loc *location.Location) (Backend, error) {
	if loc == nil {
		return nil, fmt.Errorf("store: a repository location is required")
	}
	switch loc.Proto {
	case "file":
		return NewPosixFS(loc.Path, nil)
	case "ssh":
		// Not "not implemented": borg 2 uses ssh:// for borg 1.x repositories only, and
		// reading those is a §0.6 non-goal. Saying so by name beats a message that
		// suggests waiting for a later release.
		return nil, fmt.Errorf("store: %s names a borg 1.x repository, and borge does not read "+
			"borg 1.x repositories (docs/PORTING_PLAN.md §0.6)", loc.Canonical())
	case "rclone":
		// The scheme is stripped and the rest handed over exactly as written: rclone's
		// remote syntax is rclone's business (see NewRclone).
		return NewRclone(strings.TrimPrefix(loc.Processed, "rclone:"))
	case "rest", "sftp", "s3", "b2", "http", "https":
		return nil, fmt.Errorf("store: the %s backend is not implemented yet "+
			"(docs/PORTING_PLAN.md §11.5); this borge reads local repositories only", loc.Proto)
	default:
		return nil, fmt.Errorf("store: no backend for %q", loc.Proto)
	}
}
