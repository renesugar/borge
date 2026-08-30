// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/repoobj"
)

// Chunking a file across a worker pool (plans/PORTING_PLAN.md §12.5 step 2).
//
// # What is parallel and what is not
//
// Reading and cutting stay serial: the chunker is stateful, and one is reused per Builder,
// which §12.1a documented as correct only while the write path is serial. What goes to the
// pool is the per-chunk CPU - the id hash and repoobj.Format, which is compression and
// encryption. The profile in §12.1b put those at roughly 13 s of a 30 s create.
//
// The tail is serial again and in chunk order: the deduplication check, the pack append and
// the index updates all touch state shared across the whole archive, and a file's chunk list
// has to come back in the order the file was cut.
//
// Order matters because a file's chunk list is the file: extraction concatenates the
// chunks in the order the list gives them, so a list out of order restores a corrupted
// file with a perfectly valid set of chunk ids.
//
// It is *not* kept for byte-reproducibility of the repository, which was checked and does
// not exist: two serial creates of the same tree already produce different pack names,
// because the archive's own metadata carries timestamps and its chunks share packs with
// the file chunks. An earlier draft of this comment claimed otherwise.
//
// # Why the pieces are safe to call from several goroutines
//
// Each was made so deliberately:
//
//   - Key.IDHash is a pure function of its arguments.
//   - The AEAD key's nonce counter is mutex-guarded with the cipher working outside the
//     lock, and its own comment says a worker pool is the reason: "a repeated (session key,
//     nonce) pair breaks both ciphers completely".
//   - The lz4 and zstd compressors are pooled, each with a race test, because a compressor
//     built per call was the bug that made them pools.
//
// # The copy this costs
//
// Chunker.Next hands back a slice that aliases its own buffer and is valid only until the
// next call - the serial path uses it immediately, a worker cannot. So every chunk is
// copied before it is handed over. At memcpy speed that is noise in time; in memory it is
// workers x pipelineQueueDepth chunks in flight, which is why the depth is small.
//
// # Why there is a switch
//
// BORGE_CREATE_WORKERS=1 puts it back on one goroutine, as BORGE_PACK_ASYNC does for pack
// writing. It is what lets one binary be measured both ways on a machine noisy enough to
// have produced two false readings already this stage, and it is the way out if some
// workload is worse in the pool than out of it.

// pipelineQueueDepth is how many chunks may be queued per worker.
//
// Deliberately small: a chunk can be the chunker's maximum size, 8 MiB at borg's defaults,
// so a generous queue would undo the peak-RSS work of §12.1g.
const pipelineQueueDepth = 2

// maxCreateWorkers caps the default at two, which is what reproduces rather than what the
// machine advertises.
//
// On the i5-9300H this was measured on - four physical cores, eight threads - a 1.2 GB
// create over two sweeps of 1, 2, 3, 4 and 6 workers:
//
//	workers   run 1    run 2    peak RSS (run 2)
//	1         1.00x    1.00x    243 MB
//	2         1.57x    1.64x    356 MB
//	3         1.59x    1.51x    386 MB
//	4         1.35x    1.60x    429 MB
//	6         1.17x    1.59x    491 MB
//
// The step from one worker to two is large and repeatable. Nothing beyond two reproduces:
// the first sweep looked like a clean peak at two or three with a fall-off after, and the
// second put four and six back at 1.6x, so the differences among 2, 3, 4 and 6 are inside
// this machine's noise and no contention story is supported by them.
//
// Memory is the signal that does reproduce, because it barely moves with machine load: it
// climbs monotonically with every worker, since each holds chunks in flight. So the default
// is two on the grounds that more buys no measurable time and costs measurable memory -
// which is the trade §12.3 cares about, where CPU is battery and memory is scarce.
//
// This is one machine. BORGE_CREATE_WORKERS is the knob because nobody has measured a
// sixteen-core desktop or a phone, and a scaling rule extrapolated from a single data point
// is how a plausible constant becomes a permanent wrong default.
const maxCreateWorkers = 2

// pipelineMinFileSize is the size below which a file is chunked on one goroutine.
//
// The pipeline pays when a chunk is big enough that hashing and compressing it dwarfs the
// cost of handing it to another goroutine, and does not when it is not. Measured at both
// extremes on the same binary:
//
//	118,866 files averaging 1.6 kB   1.07x faster, 1.52x the CPU   a bad trade
//	one file of 1.2 GB, 2 MB chunks  1.6x faster, 1.2x the CPU     a good one
//
// A file smaller than a chunk produces one chunk, and pipelining one chunk is pure
// overhead. Eight MiB is four default-sized chunks - enough to fill the workers once - and
// is chosen conservatively: the crossover between those two extremes has not been measured,
// so this errs towards the serial path, which is the one that costs nothing when it is
// wrong.
const pipelineMinFileSize = 8 << 20

// createWorkers is how many goroutines format chunks. Zero or one means the serial path.
func createWorkers() int {
	if v, ok := os.LookupEnv("BORGE_CREATE_WORKERS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	if n := runtime.NumCPU(); n < maxCreateWorkers {
		return n
	}
	return maxCreateWorkers
}

// formatted is one chunk after the parallel stage.
type formatted struct {
	data []byte
	id   []byte
	obj  []byte
	// hashing is this chunk's share of the id hash, added to the stats by the collector
	// so that one goroutine owns the counters.
	hashing time.Duration
	err     error
}

// formatChunk is the parallel stage: hash, then compress and encrypt.
func (b *Builder) formatChunk(data []byte) formatted {
	started := time.Now()
	id := b.manifest.Key().IDHash(data)
	hashing := time.Since(started)

	obj, err := b.ro.Format(id, &repoobj.Meta{Type: repoobj.TypeFileStream}, data)
	return formatted{data: data, id: id, obj: obj, hashing: hashing, err: err}
}

// chunkFilePipelined is ChunkFile with hashing and formatting spread over a pool.
//
// Chunk i is dispatched to worker i%workers and read back from that same worker, which is
// how the order is kept without a reordering buffer. A worker that draws a slow chunk holds
// up its own slot rather than the queue, which is the price of not buffering.
func (b *Builder) chunkFilePipelined(r io.Reader, workers int) ([]item.ChunkListEntry, error) {
	ch, err := b.contentChunker(b.chunkerParams, b.chunkSeed, r)
	if err != nil {
		return nil, err
	}

	jobs := make([]chan []byte, workers)
	results := make([]chan formatted, workers)
	for i := range jobs {
		jobs[i] = make(chan []byte, pipelineQueueDepth)
		results[i] = make(chan formatted, pipelineQueueDepth)
		go func(in <-chan []byte, out chan<- formatted) {
			defer close(out)
			for data := range in {
				out <- b.formatChunk(data)
			}
		}(jobs[i], results[i])
	}

	// The cutting goroutine. It owns ChunkingTime; the collector owns every other counter,
	// so no counter has two writers.
	var cutErr error
	var chunkingTime time.Duration
	go func() {
		defer func() {
			for _, c := range jobs {
				close(c)
			}
		}()
		for i := 0; ; i++ {
			started := time.Now()
			c, err := ch.Next()
			chunkingTime += time.Since(started)
			if err == io.EOF {
				return
			}
			if err != nil {
				cutErr = err
				return
			}
			// Copied because the slice aliases the chunker's buffer, which the next
			// call overwrites. See the note above.
			data := append([]byte(nil), chunkData(c)...)
			jobs[i%workers] <- data
		}
	}()

	var out []item.ChunkListEntry
	var collectErr error
	for i := 0; ; i++ {
		res, ok := <-results[i%workers]
		if !ok {
			break
		}
		if collectErr != nil {
			// Drain rather than return, so the cutting goroutine cannot block writing
			// into a channel nobody is reading and leak itself and its workers.
			continue
		}
		if res.err != nil {
			collectErr = res.err
			continue
		}
		entry, err := b.commitChunk(res)
		if err != nil {
			collectErr = err
			continue
		}
		out = append(out, entry)
	}

	// Every result channel is closed, which happens after the cutting goroutine's deferred
	// close of the job channels, which happens after cutErr and chunkingTime were written.
	// That chain is what makes reading them here safe without a lock.
	b.stats.ChunkingTime += chunkingTime
	if collectErr != nil {
		return nil, collectErr
	}
	if cutErr != nil {
		return nil, cutErr
	}
	return out, nil
}

// commitChunk is the serial tail: deduplicate, store, and record.
//
// It is AddChunk with the hashing and formatting already done, and it must stay the only
// writer of the index and the pack.
func (b *Builder) commitChunk(res formatted) (item.ChunkListEntry, error) {
	size := int64(len(res.data))
	b.stats.HashingTime += res.hashing
	b.stats.Chunks++
	b.stats.OriginalSize += size

	if _, seen := b.chunks.Get(res.id); seen {
		return item.ChunkListEntry{ID: res.id, Size: size}, nil
	}
	if size > int64(^uint32(0)) {
		return item.ChunkListEntry{}, fmt.Errorf("archive: chunk of %d bytes is too large", size)
	}

	results, err := b.repo.Put(res.id, res.obj)
	if err != nil {
		return item.ChunkListEntry{}, err
	}
	if err := b.chunks.Add(res.id, uint32(size)); err != nil {
		return item.ChunkListEntry{}, err
	}
	if err := b.chunks.UpdatePackInfo(results); err != nil {
		return item.ChunkListEntry{}, err
	}

	b.stats.NewChunks++
	b.stats.DedupedSize += size
	return item.ChunkListEntry{ID: res.id, Size: size}, nil
}
