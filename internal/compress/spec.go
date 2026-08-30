// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of CompressionSpec from borg's src/borg/helpers/parseformat.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package compress

import (
	"fmt"
	"strconv"
	"strings"
)

// helpText marks a declaration that exists only to carry user-facing documentation.
//
// The doc comment above such a declaration is help text and nothing else: docgen renders
// it into "borge help", so a maintainer's note in it would be printed at a user. Notes
// belong in the code below it.
const helpText = "user-facing help text"

// Compression applies to each chunk as it is stored, and is chosen with -C or
// --compression. A chunk's id is the hash of its *plaintext*, so compression sits below
// deduplication: changing it does not change which chunks exist, only how large they are.
// That is why "borge recreate --compression" cannot work and "borge repo-compress" exists.
//
//borge:doc user
//borge:help compression/intro
var _ = helpText

// A chunk that does not get smaller is stored uncompressed whatever the setting, so a
// high level costs time and never costs space.
//
//borge:doc user
//borge:help compression/incompressible
//borge:claim compression/incompressible-stored-plain
//borge:about decideCompress
var _ = helpText

// borge compresses with a pure-Go zstd implementation whose encoder has four levels where
// libzstd has twenty-two. Levels are mapped onto those four, so "zstd,16" and "zstd,22"
// produce identical output, as do "lzma,0", "lzma,6" and "lzma,9".
//
// This costs no interoperability - the level is metadata, borg reads borge's chunks and
// the stored level records what was asked for - but a high level does not compress as hard
// as borg's would. "borge benchmark cpu --compressing" shows the ratio each level actually
// achieves on this machine. See docs/DIVERGENCES.md #16.
//
//borge:doc user
//borge:help compression/levels
//borge:about parseSpec
var _ = helpText

// Default is what borge compresses with when nothing is asked for.
//
// **This diverges from borg, which defaults to lz4** (DIVERGENCES #46). Measured on 2026-08-30
// by R0 T6, unencrypted so the codec was the only variable, on two corpora chosen as
// opposites - 479 MB of small text files, and 106 MB of JPEGs that no codec can shrink:
//
//	spec          repo      vs lz4    create     extract
//	lz4           188.7 MiB    -       37.1s      20.4s
//	zstd,1        141.4 MiB  -25.1%    47.5s      21.4s
//	zstd,3        138.2 MiB  -26.8%    52.9s      22.0s
//	auto,zstd,3   138.2 MiB  -26.8%    58.8s      22.1s
//
// A quarter of the repository, for 28% more wall time on a backup. Storage is paid for as
// long as the archive is kept and the CPU is paid once, and on a remote backend the same
// 25% is 25% fewer bytes over the wire - which is usually the slower half of a backup.
//
// Level 1 rather than 3 because 3 buys 1.7 more points of ratio for 14 more points of wall
// time. Level 1 is zstd's SpeedFastest; borge's pure-Go encoder has four levels where
// libzstd has 22, so "zstd,1" and "zstd,2" are the same encoder (see the note on levels
// below).
//
// Not auto,zstd,3, which reaches the same ratio as plain zstd,3 for 59% more wall time
// because it compresses everything twice. It is the right choice only for data nothing can
// compress, where it bails out after lz4 - and on the JPEG corpus every spec produced the
// same repository to within 0.02%, so that case is decided by cost alone.
//
// This costs no interoperability: the codec is recorded per chunk and borg reads zstd.
const Default = "zstd,1"

// Spec is a parsed --compression argument.
//
// The accepted grammar, from borg:
//
//	none
//	lz4
//	zlib[,LEVEL]        LEVEL 0..9,      default 6
//	lzma[,LEVEL]        LEVEL 0..9,      default 6
//	zstd[,LEVEL]        LEVEL -128..22,  default 3
//	auto,SPEC           probe with lz4, then SPEC if it looks worthwhile
//	obfuscate,LEVEL,SPEC   LEVEL 1..6, 110..123 or 250
//
// auto and obfuscate nest, so "obfuscate,110,auto,zstd,3" is valid.
type Spec struct {
	Name  string
	Level int
	// Inner is set for the meta specs (auto, obfuscate).
	Inner *Spec
	// hasLevel distinguishes an explicit level from the default.
	hasLevel bool
}

// ParseSpec parses a --compression argument.
func ParseSpec(s string) (*Spec, error) {
	values := strings.Split(s, ",")
	if len(values) == 0 || values[0] == "" {
		return nil, fmt.Errorf("compress: empty compression specification")
	}
	return parseSpec(values)
}

func parseSpec(values []string) (*Spec, error) {
	name := values[0]
	count := len(values)
	spec := &Spec{Name: name}

	switch name {
	case "none", "lz4":
		if count != 1 {
			return nil, fmt.Errorf("compress: %s takes no level", name)
		}
		spec.Level = UnknownLevel

	case "zlib", "lzma":
		level := DefaultZlibLevel // same default as lzma
		switch count {
		case 1:
		case 2:
			n, err := strconv.Atoi(values[1])
			if err != nil {
				return nil, fmt.Errorf("compress: %s level %q is not a number", name, values[1])
			}
			if n < 0 || n > 9 {
				return nil, fmt.Errorf("compress: %s level must be 0..9, got %d", name, n)
			}
			level, spec.hasLevel = n, true
		default:
			return nil, fmt.Errorf("compress: too many arguments for %s", name)
		}
		spec.Level = level

	case "zstd":
		level := DefaultZstdLevel
		switch count {
		case 1:
		case 2:
			n, err := strconv.Atoi(values[1])
			if err != nil {
				return nil, fmt.Errorf("compress: zstd level %q is not a number", values[1])
			}
			// Negative levels are zstd's "fast" levels; -128 is the smallest the
			// clevel byte can hold, since it is read back as an int8.
			if n < -128 || n > 22 {
				return nil, fmt.Errorf("compress: zstd level must be -128..22, got %d", n)
			}
			level, spec.hasLevel = n, true
		default:
			return nil, fmt.Errorf("compress: too many arguments for zstd")
		}
		spec.Level = level

	case "auto":
		if count < 2 || count > 3 {
			return nil, fmt.Errorf("compress: auto takes a compression specification, e.g. auto,zstd,3")
		}
		inner, err := parseSpec(values[1:])
		if err != nil {
			return nil, err
		}
		spec.Inner = inner
		spec.Level = UnknownLevel

	case "obfuscate":
		if count < 3 || count > 5 {
			return nil, fmt.Errorf("compress: obfuscate takes a level and a compression specification, " +
				"e.g. obfuscate,110,zstd,3")
		}
		n, err := strconv.Atoi(values[1])
		if err != nil {
			return nil, fmt.Errorf("compress: obfuscate level %q is not a number", values[1])
		}
		ok := (n >= ObfuscateRelativeMin && n <= ObfuscateRelativeMax) ||
			(n >= ObfuscateAbsoluteMin && n <= ObfuscateAbsoluteMax) ||
			n == ObfuscatePadme
		if !ok {
			return nil, fmt.Errorf("compress: obfuscate level must be 1..6, 110..123 or 250, got %d", n)
		}
		spec.Level = n
		inner, err := parseSpec(values[2:])
		if err != nil {
			return nil, err
		}
		spec.Inner = inner

	case "zlib_legacy":
		return nil, fmt.Errorf("compress: zlib_legacy is a borg 1.x format; borge reads borg 2 repositories only")

	default:
		return nil, fmt.Errorf("compress: unsupported compression type %q, expected one of: %s",
			name, strings.Join(Names(), ", "))
	}
	return spec, nil
}

// String renders the spec back to its --compression form. Round-tripping matters
// because borg stores the spec string in archive metadata.
func (s *Spec) String() string {
	switch s.Name {
	case "none", "lz4":
		return s.Name
	case "auto":
		return "auto," + s.Inner.String()
	case "obfuscate":
		return fmt.Sprintf("obfuscate,%d,%s", s.Level, s.Inner)
	default:
		return fmt.Sprintf("%s,%d", s.Name, s.Level)
	}
}

// Compressor builds the compressor this spec describes.
func (s *Spec) Compressor() (Compressor, error) {
	switch s.Name {
	case "none":
		return None{}, nil
	case "lz4":
		return LZ4{}, nil
	case "zlib":
		return NewZlib(s.Level)
	case "lzma":
		return NewLZMA(s.Level)
	case "zstd":
		return NewZstd(s.Level)
	case "auto":
		inner, err := s.Inner.Compressor()
		if err != nil {
			return nil, err
		}
		return NewAuto(inner), nil
	case "obfuscate":
		inner, err := s.Inner.Compressor()
		if err != nil {
			return nil, err
		}
		return NewObfuscateSize(s.Level, inner)
	default:
		return nil, fmt.Errorf("compress: unsupported compression type %q", s.Name)
	}
}

// FromSpec is the one-call path from a --compression string to a compressor.
func FromSpec(s string) (Compressor, error) {
	spec, err := ParseSpec(s)
	if err != nil {
		return nil, err
	}
	return spec.Compressor()
}

// SpecDoc describes one accepted --compression specification for the documentation.
type SpecDoc struct {
	// Syntax is what a user writes, with the optional parts shown: "zstd[,LEVEL]".
	Syntax string
	// Name is the first comma-separated field, which is what parseSpec switches on.
	Name string
	// Description is the user-facing explanation, as "borge help compression" prints it.
	Description string
}

// SpecDocs lists the compression specifications borge accepts, in the order the
// documentation presents them: cheapest first, then the two that nest.
//
// This is the source the help topic renders. TestSpecDocsCoverTheParser checks it against
// parseSpec in both directions, so a codec cannot be added without appearing here and a
// line here cannot describe something the parser rejects.
//
//borge:enumerates compression-specs
func SpecDocs() []SpecDoc {
	return []SpecDoc{
		{"none", "none", "store the chunk as it is"},
		{"lz4", "lz4", "very fast, modest ratio. borg's default, and borge's until 2026-08-30."},
		{"zstd[,LEVEL]", "zstd", "level -128 to 22, default 3. borge defaults to zstd,1, which is a quarter smaller than lz4 on text for about 28% more time."},
		{"zlib[,LEVEL]", "zlib", "level 0 to 9, default 6"},
		{"lzma[,LEVEL]", "lzma", "level 0 to 9, default 6"},
		{"auto,SPEC", "auto", "try lz4 first, and use SPEC only if it compresses meaningfully better"},
		{"obfuscate,N,SPEC", "obfuscate", "compress with SPEC, then pad the result to hide its true size"},
	}
}
