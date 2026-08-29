// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"bytes"
	"math/rand"
	"sync"
	"testing"

	lz4 "github.com/pierrec/lz4/v4"
)

// Pooling the lz4 compressor is a performance fix with a correctness obligation, and the
// obligation is the same one the chunker had: the bytes must not change.
//
// A compressor carrying state from a previous chunk would produce output that decompresses
// correctly but differs from what a fresh one produces - and a repository's chunks are
// addressed by the hash of their *plaintext*, so nothing downstream would notice. borg
// would still read it. The archives would simply stop being byte-reproducible, quietly,
// which is the kind of drift this port exists to avoid.

// lz4Corpus is inputs chosen for where a hash-table compressor goes wrong: empty, tiny,
// highly repetitive (long matches, table heavily used), incompressible (no matches at
// all), and a size that crosses the 64 KiB window the table's offsets are relative to.
func lz4Corpus() [][]byte {
	rng := rand.New(rand.NewSource(11))
	random := make([]byte, 300<<10)
	rng.Read(random)

	repetitive := bytes.Repeat([]byte("the same sixteen"), 20<<10)

	mixed := make([]byte, 128<<10)
	copy(mixed, repetitive)
	rng.Read(mixed[64<<10:])

	small := make([]byte, 61)
	rng.Read(small)

	return [][]byte{{}, small, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), repetitive, random, mixed}
}

// freshAttempt is what attempt did before pooling: a compressor per call.
func freshAttempt(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	buf := make([]byte, lz4.CompressBlockBound(len(data)))
	var c lz4.Compressor
	n, err := c.CompressBlock(data, buf)
	if err != nil || n == 0 || n >= len(data) {
		return nil
	}
	return buf[:n]
}

// TestLZ4PooledOutputMatchesFresh is the test the change rests on.
//
// One pooled compressor is driven across the whole corpus in sequence, so any state that
// survived a call would show up in the next one's output.
func TestLZ4PooledOutputMatchesFresh(t *testing.T) {
	for round := range 3 { // several passes, so a compressor is reused many times over
		for i, data := range lz4Corpus() {
			want := freshAttempt(data)
			got, err := LZ4{}.attempt(data)
			if err != nil {
				t.Fatalf("round %d input %d: %v", round, i, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("round %d input %d (%d bytes): pooled output differs from fresh "+
					"(%d vs %d bytes)", round, i, len(data), len(got), len(want))
			}
		}
	}
}

// TestLZ4RoundTripsAfterReuse. Identical bytes is the stronger claim, but a compressor
// that produced identical *wrong* bytes would satisfy it, so the data is decompressed too.
func TestLZ4RoundTripsAfterReuse(t *testing.T) {
	for _, data := range lz4Corpus() {
		if len(data) == 0 {
			continue
		}
		var meta Meta
		out, err := (LZ4{}).Compress(&meta, data)
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

// TestLZ4PoolIsSafeUnderConcurrency. A single shared compressor would be a data race here,
// which is why this is a pool: step 2 parallelises create, and this must not have to be
// revisited then. Run with -race for it to mean anything.
func TestLZ4PoolIsSafeUnderConcurrency(t *testing.T) {
	corpus := lz4Corpus()
	want := make([][]byte, len(corpus))
	for i, data := range corpus {
		want[i] = freshAttempt(data)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 20 {
				i := round % len(corpus)
				got, err := LZ4{}.attempt(corpus[i])
				if err != nil {
					t.Errorf("attempt: %v", err)
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
