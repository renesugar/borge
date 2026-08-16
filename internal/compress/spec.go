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
