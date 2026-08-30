// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"os"
	"runtime"
	"strconv"
	"sync"

	"github.com/renesugar/borge/internal/item"
)

// maxExtractWorkers caps the writer pool. Three, and the number moved once during T2.
//
// The first probe replayed extract's own syscall sequence - create, write, fchown, fchmod,
// futimens, close - over 118,866 files, in two arms, because a number without a control
// does not say what limits it:
//
//	workers   one shared directory   one directory each
//	1         22.33s                 22.50s
//	2         12.80s  (1.74x)        11.69s  (1.92x)
//	4         13.09s  (1.71x)         6.56s  (3.43x)
//	8         14.78s  (worse)         5.27s  (4.27x)
//
// The device will go 4.3x faster given separate directories; one directory stops near 1.75x
// and then gets worse, because ext4 serialises creates on the parent's inode lock.
//
// Then the real thing, on two corpora of the same 118,866 files - the flat pathological
// directory of §12.1b, and a tree of 4,953 directories, which is the shape that drains the
// pool at every boundary:
//
//	workers   flat, 1 dir        deep, 4,953 dirs
//	1         39.21s  1.00x      34.81s  1.00x
//	2         18.91s  2.07x      19.33s  1.80x
//	3         18.23s  2.15x      16.60s  2.10x   <- the default
//	4         18.23s  2.15x      17.23s  2.02x
//
// Two things in that table are worth keeping. **The deep tree does not regress**, which was
// the risk: 4,953 barriers cost real time at two workers - 1.80x against the flat corpus's
// 2.07x - but a third worker more than recovers it. And **four is measurably worse than
// three on the deep tree**, 17.23s against 16.60 with sds of 0.06 and 0.07, which is not
// noise; past three the workers contend for the same directory lock the first probe found.
//
// The default was 2 until the deep tree was measured, chosen from the flat corpus alone
// where 2 -> 3 buys 3.6% of wall for 15% more CPU. On the deep tree the same step buys 14%
// for 5%, and real trees have directories. That is the §12.1h lesson arriving a second
// time: sampling one corpus and reading a default off it is not measurement, and the
// corpus that would have contradicted it was the ordinary-looking one.
//
// It is deliberately *not* tied to create's two (§12.1h). Different mechanism, different
// bottleneck, and unifying the constants would couple two numbers that only ever agreed by
// accident - which they no longer do.
const maxExtractWorkers = 3

// extractWorkers is how many goroutines write files. Zero or one means the serial path,
// and the serial path is the one that existed before this file - not a pool of one.
func extractWorkers() int {
	if v, ok := os.LookupEnv("BORGE_EXTRACT_WORKERS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	if n := runtime.NumCPU(); n < maxExtractWorkers {
		return n
	}
	return maxExtractWorkers
}

// writeJob is one regular file handed to the pool.
type writeJob struct {
	it   *item.Item
	path string
}

// writePool spreads writeFile over a few goroutines.
//
// Only the regular-file content path runs here. Everything that touches state shared
// across items - the pending-directory stack, the hardlink map, the safe-parent cache -
// stays on the calling goroutine, and the pool is drained before any of it is read or
// written. That is why those maps can stay unguarded, as their comment in extract.go
// asks: the answer to "parallelising extract has to deal with all three together" is that
// it does not touch them at all.
type writePool struct {
	jobs chan writeJob
	// workers tracks the goroutines themselves; inflight tracks submitted jobs. They are
	// separate because a drain is a barrier, not a shutdown: a deep tree drains at every
	// directory boundary and the pool has to survive it.
	workers  sync.WaitGroup
	inflight sync.WaitGroup

	// mu guards err and serialises OnError, which is caller code and must not be entered
	// from two goroutines at once.
	mu  sync.Mutex
	err error
}

func newWritePool(x *extractor, workers int) *writePool {
	p := &writePool{jobs: make(chan writeJob, workers*2)}
	p.workers.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer p.workers.Done()
			for j := range p.jobs {
				if err := x.writeFile(j.it, j.path); err != nil {
					p.fail(x, j.it.Path, err)
				}
				p.inflight.Done()
			}
		}()
	}
	return p
}

// fail records a per-item failure. A pool worker cannot abandon the extraction on its own -
// the decision belongs to opts.OnError, exactly as on the serial path - so the first error
// that OnError declines to swallow is kept and returned by the next drain.
func (p *writePool) fail(x *extractor, path string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := x.fail(path, err); e != nil && p.err == nil {
		p.err = e
	}
}

func (p *writePool) submit(j writeJob) {
	p.inflight.Add(1)
	p.jobs <- j
}

// drain waits for every job in flight and reports the first error kept.
//
// It is called before anything that depends on files already being on disk: applying a
// directory's attributes, creating a hard link to an earlier file, and the end of the
// extraction. On a deep tree that makes each directory boundary a barrier, which is the
// cost of keeping the ordering rules in one place; on the flat corpus this exists for,
// there is one barrier at the end.
func (p *writePool) drain() error {
	p.inflight.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// close drains and then stops the workers. After it the pool must not be submitted to.
func (p *writePool) close() error {
	err := p.drain()
	close(p.jobs)
	p.workers.Wait()
	return err
}
