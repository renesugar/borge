// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// The path cache: which paths an archive contains, so `find` can decide an archive holds
// no match without decoding its item stream.
//
// # Why this is safe in a way the files cache is not
//
// The files cache next door is dangerous because it answers a question about a file that
// may since have changed, and a wrong answer silently stores stale contents. This cache
// answers a question about an *archive*, and an archive is immutable once written. It is
// keyed by archive id - the hash of its content, not its name - so an entry either belongs
// to the archive being read or does not exist. There is no stale state to reason about;
// the only failure modes are absence and corruption, and both fall back to reading the
// item stream.
//
// # What it is allowed to decide
//
// Only that an archive can be skipped. `find` emits whole items - --json-lines prints every
// field, and --format can name any of them - so the cache cannot serve output. What it can
// do is prove a *negative*: if no path in the archive matches, the archive contributes
// nothing whatever the output format, so not reading it cannot change what `find` prints.
// That is the whole contract, and it is why a corrupt or missing cache costs time and
// nothing else.

// pathsMagic identifies the file and its layout. A file that does not start with it is
// treated as corrupt, which is also what an older or newer layout is.
var pathsMagic = [8]byte{'b', 'o', 'r', 'g', 'e', 'p', 'c', '1'}

// PathsFile is where one archive's paths are cached.
func PathsFile(repoID, archiveID []byte) (string, error) {
	dir, err := Dir(repoID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "paths", hex.EncodeToString(archiveID)), nil
}

// WritePaths stores an archive's paths, replacing any existing entry.
//
// The write goes to a temporary file and is renamed, so a crash leaves either the old entry
// or none - never half of one. A reader that finds a truncated file rejects it on the
// checksum anyway; the rename is belt and braces because this runs alongside backups.
func WritePaths(repoID, archiveID []byte, paths []string) error {
	name, err := PathsFile(repoID, archiveID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}

	var plain bytes.Buffer
	for _, p := range paths {
		plain.WriteString(p)
		plain.WriteByte('\n')
	}
	sum := sha256.Sum256(plain.Bytes())

	var body bytes.Buffer
	body.Write(pathsMagic[:])
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(paths)))
	body.Write(n[:])
	body.Write(sum[:])

	// Paths compress about elevenfold on a real corpus - 117 bytes each becomes about 11 -
	// because they share directories and extensions. Level 1: this is a cache, and the time
	// spent compressing it comes out of the backup that populates it.
	enc, err := zstd.NewWriter(&body, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return err
	}
	if _, err := enc.Write(plain.Bytes()); err != nil {
		enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(name), ".paths-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// ErrNoPaths means the cache has nothing usable for this archive: no entry, or one that
// failed its checks. Callers read the item stream instead.
var ErrNoPaths = errors.New("cache: no path entry")

// ReadPaths returns an archive's cached paths.
//
// Every way of being wrong returns ErrNoPaths rather than an error the caller might report:
// a missing file, a truncated one, a foreign one, a corrupted one, and one whose contents
// do not match the checksum written beside them. The caller's fallback is correct in all of
// those cases, so none of them is worth a message.
func ReadPaths(repoID, archiveID []byte) ([]string, error) {
	name, err := PathsFile(repoID, archiveID)
	if err != nil {
		return nil, ErrNoPaths
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, ErrNoPaths
	}
	defer f.Close()

	var head [8 + 8 + 32]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return nil, ErrNoPaths
	}
	if !bytes.Equal(head[:8], pathsMagic[:]) {
		return nil, ErrNoPaths
	}
	count := binary.LittleEndian.Uint64(head[8:16])
	var want [32]byte
	copy(want[:], head[16:])

	dec, err := zstd.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, ErrNoPaths
	}
	defer dec.Close()
	plain, err := io.ReadAll(dec)
	if err != nil {
		return nil, ErrNoPaths
	}
	if sha256.Sum256(plain) != want {
		return nil, ErrNoPaths
	}

	paths := make([]string, 0, count)
	for len(plain) > 0 {
		i := bytes.IndexByte(plain, '\n')
		if i < 0 {
			return nil, ErrNoPaths // no trailing newline: not a file this wrote
		}
		paths = append(paths, string(plain[:i]))
		plain = plain[i+1:]
	}
	if uint64(len(paths)) != count {
		return nil, ErrNoPaths
	}
	return paths, nil
}

// ForgetPaths removes an archive's entry. Deleting an archive should not leave its paths
// behind: the id will never be asked for again, so the entry is pure waste.
func ForgetPaths(repoID, archiveID []byte) error {
	name, err := PathsFile(repoID, archiveID)
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}
