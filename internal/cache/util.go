// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// writeFileAtomic writes through a temporary file and a rename, so an interrupted save
// leaves the previous cache rather than a truncated one.
//
// A truncated cache that still parses is the dangerous case: the missing entries would
// only cost time, but a partially written entry that decodes could hand out a wrong chunk
// list, which costs data. A rename is atomic, so neither can happen.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	f, err := os.CreateTemp(dir, ".borge-cache-*")
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("cache: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("cache: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return os.Rename(tmp, path)
}
