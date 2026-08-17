// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the paper key export and import in borg's
// src/borg/crypto/keymanager.py.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package key

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// A paper key is a key blob printed as hexadecimal, for storage on paper in a safe.
//
// # Why it is not just a hex dump
//
// It is meant to be typed back in by a human, possibly years later, possibly from a
// photocopy. So every line carries a two-character checksum over its own contents, the
// header carries a checksum over the whole key, and the digits are grouped in sixes.
// That turns "the backup would not restore" into "line 7 has a typo", which is the
// difference between a usable last resort and a decorative one.
//
// The format is borg's, unchanged, so a key printed by either tool can be typed into the
// other.

// paperKeyMagic is the line that identifies the format. It says BORG, not BORGE, because
// the format is borg's and a paper key must remain interchangeable between the two.
const paperKeyMagic = "BORG PAPER KEY v1"

// paperKeyBytesPerLine is how many bytes of key material go on one printed line.
const paperKeyBytesPerLine = 18

// paperKeyRepoIDChars is how much of the repository id the header records. It is an
// identifier, not a secret, and 18 hex characters is enough to tell two repositories
// apart while still fitting on a line.
const paperKeyRepoIDChars = 18

//go:embed paperkey.html
var paperKeyHTML []byte

// paperKeyHTMLAnchor is where the key text is spliced into the template.
var paperKeyHTMLAnchor = []byte("</textarea>")

// sha256Truncated is borg's checksum helper: the hex digest, cut to n characters.
func sha256Truncated(data []byte, n int) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:n]
}

// grouped inserts a space every six characters, which is what makes a long hex run
// readable and typeable.
func grouped(s string) string {
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && i%6 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// ExportPaperKey renders a key blob as a printable paper key.
func ExportPaperKey(blob Blob, repoIDHex string) (string, error) {
	_, b64, err := KeyfileParse(blob.Text, repoIDHex)
	if err != nil {
		return "", err
	}
	binary, err := unwrapBase64(b64)
	if err != nil {
		return "", err
	}

	lines := (len(binary) + paperKeyBytesPerLine - 1) / paperKeyBytesPerLine
	shortID := repoIDHex
	if len(shortID) > paperKeyRepoIDChars {
		shortID = shortID[:paperKeyRepoIDChars]
	}
	wholeChecksum := sha256Truncated(binary, 12)

	var out strings.Builder
	out.WriteString("To restore this key use: borge key import --paper /path/to/repo\n")
	out.WriteString("(borg key import --paper also reads it; the format is the same.)\n\n")
	out.WriteString(paperKeyMagic + "\n")

	idPayload := strconv.Itoa(lines) + "/" + shortID + "/" + wholeChecksum
	fmt.Fprintf(&out, "id: %d / %s / %s - %s\n",
		lines, grouped(shortID), grouped(wholeChecksum), sha256Truncated([]byte(idPayload), 2))

	for i := 0; i < lines; i++ {
		start := i * paperKeyBytesPerLine
		end := min(start+paperKeyBytesPerLine, len(binary))
		chunk := binary[start:end]
		idx := i + 1
		fmt.Fprintf(&out, "%2d: %s - %s\n",
			idx, grouped(hex.EncodeToString(chunk)), lineChecksum(idx, chunk))
	}
	return out.String(), nil
}

// lineChecksum is the two-character check over a numbered line. The index is part of it,
// so two lines cannot be swapped without being noticed.
func lineChecksum(idx int, data []byte) string {
	buf := make([]byte, 0, 2+len(data))
	buf = append(buf, byte(idx>>8), byte(idx))
	buf = append(buf, data...)
	return sha256Truncated(buf, 2)
}

// ImportPaperKey parses a typed-in paper key back into a key blob.
//
// borg asks for the lines one at a time at a prompt. borge parses a whole document
// instead, so the same code serves both a file and an interactive session, and so that
// every checksum failure can be reported with the line it belongs to rather than only
// the first one encountered.
func ImportPaperKey(text, repoIDHex string) (blobText []byte, err error) {
	shortID := repoIDHex
	if len(shortID) > paperKeyRepoIDChars {
		shortID = shortID[:paperKeyRepoIDChars]
	}

	var (
		wantLines    int
		wantChecksum string
		haveID       bool
		parts        [][]byte
		problems     []string
	)

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line == paperKeyMagic {
			continue
		}
		label, rest, found := strings.Cut(line, ":")
		if !found {
			continue // prose, e.g. the instruction line
		}
		label = strings.TrimSpace(label)
		body := strings.ReplaceAll(rest, " ", "")
		payload, checksum, found := strings.Cut(body, "-")
		if !found {
			continue
		}

		if label == "id" {
			if got := sha256Truncated([]byte(strings.ToLower(payload)), 2); got != checksum {
				problems = append(problems, "the id line's checksum does not match; check it for a typo")
				continue
			}
			fields := strings.Split(payload, "/")
			if len(fields) != 3 {
				problems = append(problems, "the id line should have exactly two '/' separators")
				continue
			}
			n, convErr := strconv.Atoi(fields[0])
			if convErr != nil {
				problems = append(problems, "the id line's line count is not a number")
				continue
			}
			if repoIDHex != "" && fields[1] != shortID {
				return nil, fmt.Errorf("%w: this paper key is for repository %s...", ErrRepositoryMismatch, fields[1])
			}
			wantLines, wantChecksum, haveID = n, fields[2], true
			continue
		}

		idx, convErr := strconv.Atoi(label)
		if convErr != nil {
			continue // not a numbered line
		}
		data, decErr := hex.DecodeString(payload)
		if decErr != nil {
			problems = append(problems, fmt.Sprintf("line %d contains something that is not a hex digit", idx))
			continue
		}
		if got := lineChecksum(idx, data); got != checksum {
			problems = append(problems, fmt.Sprintf("line %d's checksum does not match; check it for a typo", idx))
			continue
		}
		for len(parts) < idx {
			parts = append(parts, nil)
		}
		parts[idx-1] = data
	}

	if !haveID {
		problems = append(problems, "no usable id line was found")
	}
	for i, p := range parts {
		if p == nil {
			problems = append(problems, fmt.Sprintf("line %d is missing", i+1))
		}
	}
	if haveID && len(parts) != wantLines {
		problems = append(problems, fmt.Sprintf("the id line announces %d lines, but %d were given",
			wantLines, len(parts)))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("key: this paper key could not be read:\n  - %s", strings.Join(problems, "\n  - "))
	}

	binary := bytes.Join(parts, nil)
	if got := sha256Truncated(binary, 12); got != wantChecksum {
		// Every line checked out individually, so this means the transcription is
		// self-consistent but not the key that was printed - a whole line duplicated, say.
		return nil, fmt.Errorf("key: every line checks out, but the key as a whole does not " +
			"match the checksum on the id line")
	}
	if _, err := checkPaperKeyEnvelope(binary); err != nil {
		return nil, err
	}
	return []byte(KeyfileFormat(repoIDHex, wrapBase64(binary))), nil
}

// checkPaperKeyEnvelope confirms the reconstructed bytes really are a key envelope, so a
// paper key that transcribed cleanly but is not a key is caught here rather than at the
// next unlock.
func checkPaperKeyEnvelope(raw []byte) (*Envelope, error) {
	return ParseEnvelope([]byte(KeyfileFormat("", wrapBase64(raw))), "")
}

// ExportPaperKeyHTML renders the printable QR template with the key spliced in.
//
// The template is borg's paperkey.html, copied verbatim rather than reimplemented: it
// carries a self-contained QR generator and sha256 implementation, and a printed key is
// only useful if the tool that reads it back agrees byte for byte on the layout.
func ExportPaperKeyHTML(blob Blob) ([]byte, error) {
	if !bytes.Contains(paperKeyHTML, paperKeyHTMLAnchor) {
		return nil, fmt.Errorf("key: the paper key template is missing its %s anchor", paperKeyHTMLAnchor)
	}
	replacement := append(append([]byte(nil), blob.Text...), paperKeyHTMLAnchor...)
	return bytes.Replace(paperKeyHTML, paperKeyHTMLAnchor, replacement, 1), nil
}
