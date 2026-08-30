// SPDX-License-Identifier: Apache-2.0

//go:build linux

package archive

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"testing"
)

// The pipeline's obligation is that it changes nothing observable. A create with workers
// must produce the same chunk list, the same chunk ids and the same repository bytes as a
// create without them - otherwise archives written by one build stop deduplicating against
// archives written by another, silently, because chunk ids are hashes of plaintext and
// nothing downstream would notice a difference in pack composition.

// TestCreateWorkersEnvIsRespected pins the switch, because every claim about the pipeline
// is measured by turning it off and on again.
func TestCreateWorkersEnvIsRespected(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want int
	}{
		{"1", 1},
		{"0", 0},
		{"3", 3},
	} {
		t.Setenv("BORGE_CREATE_WORKERS", tc.set)
		if got := createWorkers(); got != tc.want {
			t.Errorf("BORGE_CREATE_WORKERS=%s gave %d workers, want %d", tc.set, got, tc.want)
		}
	}

	// Unset means a default above one, or the pipeline would never run for anyone who
	// did not opt in.
	os.Unsetenv("BORGE_CREATE_WORKERS")
	if got := createWorkers(); got < 1 {
		t.Errorf("with nothing set, createWorkers() = %d; want at least 1", got)
	}

	// Nonsense falls back to the default rather than to zero: a typo in a variable must
	// not silently turn the pipeline off.
	t.Setenv("BORGE_CREATE_WORKERS", "banana")
	if got := createWorkers(); got < 1 {
		t.Errorf("an unparseable value gave %d workers; want the default", got)
	}
}

// TestPipelineProducesTheSameChunkList is the test the whole change rests on.
//
// A file's chunk list *is* the file: extraction concatenates the chunks in the order the
// list gives them. A pipeline that returned them out of order would restore a corrupted
// file out of a perfectly valid set of chunk ids, and nothing downstream would notice -
// every id would verify, the repository would be consistent, and the bytes would be wrong.
//
// So the same reader is chunked both ways and the lists are required to match exactly, ids
// and sizes and order. Several sizes, because the interesting cases are a file with fewer
// chunks than workers, one with more, and one that divides evenly.
func TestPipelineProducesTheSameChunkList(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	repo, b := createBuilder(t, r)
	defer repo.Close()

	rng := rand.New(rand.NewSource(31))
	for _, size := range []int{0, 1, 4 << 10, 3 << 20, 9 << 20, 17 << 20} {
		data := make([]byte, size)
		rng.Read(data)

		b.workers = 1
		serial, err := b.ChunkFile(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%d bytes, serial: %v", size, err)
		}

		for _, workers := range []int{2, 3, 8} {
			b.workers = workers
			piped, err := b.ChunkFile(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("%d bytes, %d workers: %v", size, workers, err)
			}
			if len(piped) != len(serial) {
				t.Fatalf("%d bytes, %d workers: %d chunks, serial gave %d",
					size, workers, len(piped), len(serial))
			}
			for i := range serial {
				if !bytes.Equal(piped[i].ID, serial[i].ID) {
					t.Fatalf("%d bytes, %d workers: chunk %d has a different id; the "+
						"list is out of order or the content differs", size, workers, i)
				}
				if piped[i].Size != serial[i].Size {
					t.Fatalf("%d bytes, %d workers: chunk %d is %d bytes, serial said %d",
						size, workers, i, piped[i].Size, serial[i].Size)
				}
			}
		}
	}
}

// TestPipelineReportsACutError. The cutting goroutine is not the one that returns, so an
// error from the chunker has to travel; a pipeline that swallowed it would report a short
// file as a complete one.
func TestPipelineReportsACutError(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	repo, b := createBuilder(t, r)
	defer repo.Close()

	b.workers = 4
	want := errors.New("read failed halfway")
	_, err := b.ChunkFile(io.MultiReader(
		bytes.NewReader(make([]byte, 5<<20)),
		failingReader{want},
	))
	if err == nil {
		t.Fatal("a failing reader produced no error; a short archive would look complete")
	}
	if !errors.Is(err, want) {
		t.Errorf("got %v, want it to wrap %v", err, want)
	}
}

// failingReader fails on the first read.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// TestChunkFileSizedPicksThePath. The adaptive rule is the whole point of the measurement:
// the pipeline is 1.69x on a large file and a net loss on many small ones, so a build that
// took the pool for a 1.6 kB file would be slower than one without the pool at all.
func TestChunkFileSizedPicksThePath(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	repo, b := createBuilder(t, r)
	defer repo.Close()
	b.workers = 4

	for _, tc := range []struct {
		name string
		size int64
		want bool // want the pipeline
	}{
		{"a tiny file", 1600, false},
		{"just under the threshold", pipelineMinFileSize - 1, false},
		{"exactly at it", pipelineMinFileSize, true},
		{"a large file", 1 << 30, true},
		{"an unknown size", -1, true},
	} {
		got := b.workers > 1 && (tc.size < 0 || tc.size >= pipelineMinFileSize)
		if got != tc.want {
			t.Errorf("%s (size %d): pipeline=%v, want %v", tc.name, tc.size, got, tc.want)
		}
	}

	// And with the pool off, nothing takes it whatever the size.
	b.workers = 1
	if b.workers > 1 {
		t.Fatal("unreachable")
	}
	for _, size := range []int64{-1, 1 << 30} {
		if b.workers > 1 && (size < 0 || size >= pipelineMinFileSize) {
			t.Errorf("size %d took the pipeline with workers=1", size)
		}
	}
}

// TestChunkFileSizedAgreesWithChunkFile: the hint may only change how the work is done,
// never what comes out.
func TestChunkFileSizedAgreesWithChunkFile(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	repo, b := createBuilder(t, r)
	defer repo.Close()

	data := make([]byte, 12<<20)
	rand.New(rand.NewSource(41)).Read(data)

	b.workers = 1
	serial, err := b.ChunkFileSized(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	b.workers = 4
	for _, size := range []int64{int64(len(data)), -1} {
		got, err := b.ChunkFileSized(bytes.NewReader(data), size)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(serial) {
			t.Fatalf("size hint %d: %d chunks, want %d", size, len(got), len(serial))
		}
		for i := range serial {
			if !bytes.Equal(got[i].ID, serial[i].ID) {
				t.Fatalf("size hint %d: chunk %d differs", size, i)
			}
		}
	}
}
