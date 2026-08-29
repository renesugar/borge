// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"bytes"
	"math/rand"
	"sync"
	"testing"
)

// Pooling zstd encoders carries the same obligation as pooling the lz4 compressor: the
// bytes must not change. §12.2 says as much - "the output is unchanged" - but said it of a
// change that was never made, so it is checked here rather than inherited.
//
// A reused encoder that carried state would still decompress, and borg would still read
// it, because chunks are addressed by the hash of their plaintext. Archives would just
// quietly stop being byte-reproducible.

func zstdCorpus() [][]byte {
	rng := rand.New(rand.NewSource(23))
	random := make([]byte, 300<<10)
	rng.Read(random)
	repetitive := bytes.Repeat([]byte("compressible sixteen"), 30<<10)
	mixed := make([]byte, 200<<10)
	copy(mixed, repetitive)
	rng.Read(mixed[100<<10:])
	small := []byte("short")
	return [][]byte{{}, small, repetitive, random, mixed}
}

// TestZstdPooledOutputMatchesFresh across several levels and repeated use.
func TestZstdPooledOutputMatchesFresh(t *testing.T) {
	for _, level := range []int{1, 3, 10} {
		c := Zstd{level: level}
		// Two passes: the second necessarily comes from the pool.
		var first [][]byte
		for pass := range 2 {
			for i, data := range zstdCorpus() {
				var meta Meta
				got, err := c.Compress(&meta, data)
				if err != nil {
					t.Fatalf("level %d pass %d input %d: %v", level, pass, i, err)
				}
				if pass == 0 {
					first = append(first, got)
					continue
				}
				if !bytes.Equal(got, first[i]) {
					t.Errorf("level %d input %d: a pooled encoder produced different bytes "+
						"(%d vs %d)", level, i, len(got), len(first[i]))
				}
			}
		}
	}
}

// TestZstdRoundTripsFromThePool, because identical wrong bytes would satisfy the test
// above on its own.
func TestZstdRoundTripsFromThePool(t *testing.T) {
	c := Zstd{level: 3}
	for _, data := range zstdCorpus() {
		if len(data) == 0 {
			continue
		}
		var meta Meta
		out, err := c.Compress(&meta, data)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Decompress(&meta, out)
		if err != nil {
			t.Fatalf("decompressing %d bytes: %v", len(data), err)
		}
		if !bytes.Equal(back, data) {
			t.Errorf("a %d-byte input did not survive the round trip", len(data))
		}
	}
}

// TestZstdPoolIsSafeUnderConcurrency. The create pipeline is the reason this is a pool at
// all; run with -race for it to mean anything.
func TestZstdPoolIsSafeUnderConcurrency(t *testing.T) {
	c := Zstd{level: 3}
	corpus := zstdCorpus()
	want := make([][]byte, len(corpus))
	for i, data := range corpus {
		var meta Meta
		out, err := c.Compress(&meta, data)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = out
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 15 {
				i := round % len(corpus)
				var meta Meta
				got, err := c.Compress(&meta, corpus[i])
				if err != nil {
					t.Errorf("Compress: %v", err)
					return
				}
				if !bytes.Equal(got, want[i]) {
					t.Errorf("input %d compressed differently under concurrency", i)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestZstdLevelsDoNotShareAnEncoder. One pool per level, or a level-3 request could be
// served by a level-10 encoder and silently produce the wrong compression.
func TestZstdLevelsDoNotShareAnEncoder(t *testing.T) {
	data := bytes.Repeat([]byte("some fairly compressible text here"), 4096)
	var lo, hi Meta
	low, err := (Zstd{level: 1}).Compress(&lo, data)
	if err != nil {
		t.Fatal(err)
	}
	high, err := (Zstd{level: 10}).Compress(&hi, data)
	if err != nil {
		t.Fatal(err)
	}
	// Run them again in the other order; each must reproduce its own output.
	var lo2, hi2 Meta
	high2, err := (Zstd{level: 10}).Compress(&hi2, data)
	if err != nil {
		t.Fatal(err)
	}
	low2, err := (Zstd{level: 1}).Compress(&lo2, data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(low, low2) {
		t.Error("level 1 produced different bytes after level 10 had run")
	}
	if !bytes.Equal(high, high2) {
		t.Error("level 10 produced different bytes after level 1 had run")
	}
}
