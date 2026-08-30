// SPDX-License-Identifier: Apache-2.0

package chunker

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// Reusing a chunker is a performance fix with a correctness obligation.
//
// borge built a chunker per file where borg builds one per archive, and the keyed tables
// are derived from a CSPRNG: 1.75 ms for fastcdc, 4.35 ms for buzhash64, which over the
// 118,866-file corpus the brief names is about 3.5 minutes of table construction before a
// byte is chunked (plans/PORTING_PLAN.md §12.1).
//
// The obligation is that a reset chunker chunks exactly as a fresh one does. If it did
// not, every archive written after this change would have different chunk boundaries from
// every archive written before it - deduplication against existing archives would quietly
// stop working, and the interop gate would be comparing borge against a borge that no
// longer chunks like borg.

// resetParams is one configuration per algorithm, using borg's own defaults where they
// exist so the test exercises what users actually run.
func resetParams() map[string]Params {
	return map[string]Params{
		AlgoFastCDC:   DefaultParams(),
		AlgoBuzhash64: {Algorithm: AlgoBuzhash64, ChunkMinExp: 19, ChunkMaxExp: 23, HashMaskBits: 21, WindowSize: 4095, NCLevel: 2},
		AlgoBuzhash:   {Algorithm: AlgoBuzhash, ChunkMinExp: 19, ChunkMaxExp: 23, HashMaskBits: 21, WindowSize: 4095},
		AlgoFixed:     {Algorithm: AlgoFixed, BlockSize: 1 << 20},
	}
}

// chunkAll drains a chunker into a copied list, because Next's slice is only valid until
// the call after it.
func chunkAll(t *testing.T, c Chunker) [][]byte {
	t.Helper()
	var out [][]byte
	for {
		ch, err := c.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		// Sparse chunks carry no Data; their identity is their size.
		if ch.Allocation != AllocData {
			out = append(out, make([]byte, ch.Size))
			continue
		}
		out = append(out, append([]byte(nil), ch.Data...))
	}
}

// streams returns inputs chosen for where chunking goes wrong: empty, shorter than the
// minimum chunk, longer than the maximum, and a compressible run that produces long
// stretches with no cut point.
func streams() [][]byte {
	rng := rand.New(rand.NewSource(1))
	big := make([]byte, 20<<20)
	rng.Read(big)
	runs := make([]byte, 12<<20)
	for i := range runs {
		runs[i] = byte(i / 4096)
	}
	small := make([]byte, 100)
	rng.Read(small)
	return [][]byte{{}, small, runs, big}
}

// TestResetChunksIdentically is the test the whole change rests on.
//
// Checked by mutation, because a test of a reset that cannot fail is worse than none:
// omitting any of driver.reset's n, pending, eof, done or bytesRead assignments makes it
// fail. pos is the one field it does not pin, and driver.reset says why.
func TestResetChunksIdentically(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	for algo, params := range resetParams() {
		t.Run(algo, func(t *testing.T) {
			// One chunker, reused across every stream in turn.
			reused, err := New(params, key, 4242, bytes.NewReader(nil))
			if err != nil {
				t.Fatal(err)
			}
			for i, data := range streams() {
				fresh, err := New(params, key, 4242, bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				want := chunkAll(t, fresh)

				reused.Reset(bytes.NewReader(data))
				got := chunkAll(t, reused)

				if len(got) != len(want) {
					t.Fatalf("stream %d: reused produced %d chunk(s), fresh produced %d",
						i, len(got), len(want))
				}
				for j := range want {
					if !bytes.Equal(got[j], want[j]) {
						t.Fatalf("stream %d chunk %d: reused and fresh differ (%d vs %d bytes)",
							i, j, len(got[j]), len(want[j]))
					}
				}
			}
		})
	}
}

// TestResetAfterAPartialReadStartsClean. A file whose chunks the caller stopped consuming
// - an error mid-archive, a cancelled create - leaves buffered bytes and a scan position
// behind. The next Reset must not inherit them.
func TestResetAfterAPartialReadStartsClean(t *testing.T) {
	key := make([]byte, 32)
	data := make([]byte, 8<<20)
	rand.New(rand.NewSource(2)).Read(data)

	for algo, params := range resetParams() {
		t.Run(algo, func(t *testing.T) {
			c, err := New(params, key, 1, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			// Take one chunk and abandon the rest.
			if _, err := c.Next(); err != nil && err != io.EOF {
				t.Fatal(err)
			}

			c.Reset(bytes.NewReader(data))
			got := chunkAll(t, c)

			fresh, err := New(params, key, 1, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			want := chunkAll(t, fresh)

			if len(got) != len(want) {
				t.Fatalf("after an abandoned read, reset produced %d chunk(s), want %d",
					len(got), len(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("chunk %d differs after an abandoned read", i)
				}
			}
		})
	}
}

// TestResetReportsTheSameTotals, because the driver's counters feed the archive's
// statistics and a counter that carried over would inflate the next file's numbers.
func TestResetReportsTheSameTotals(t *testing.T) {
	key := make([]byte, 32)
	data := make([]byte, 4<<20)
	rand.New(rand.NewSource(3)).Read(data)

	c, err := New(DefaultParams(), key, 0, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	total := func() int {
		n := 0
		for _, ch := range chunkAll(t, c) {
			n += len(ch)
		}
		return n
	}
	first := total()
	c.Reset(bytes.NewReader(data))
	if second := total(); second != first {
		t.Errorf("the same stream chunked to %d bytes then %d after Reset", first, second)
	}
	if first != len(data) {
		t.Errorf("chunked %d bytes of a %d-byte stream", first, len(data))
	}
}
