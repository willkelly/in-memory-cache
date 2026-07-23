package cache

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// The mutex design's batched get, in two versions. The serial one is
// trivial and worth having (it amortizes locking). The parallel one is the
// point of comparison with batch_actor.go: to use more than one core, this
// design has to conjure workers, partition the work, publish it race-free,
// and join — per call, and again for every future operation that wants
// parallelism. The imports above are the tell: runtime, sync, and
// sync/atomic appear in this file only for the machinery, none of which
// exists in the actor's version.

// GetBatch amortizes locking: one Lock/Unlock per shard touched instead of
// one per key. The lookups themselves remain serial on the calling
// goroutine.
func (c *Sharded) GetBatch(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	if len(keys) == 0 {
		return out
	}
	idxs, starts := groupByShard(keys, lowBitsShard)
	for s := 0; s < shardCount; s++ {
		part := idxs[starts[s]:starts[s+1]]
		if len(part) == 0 {
			continue
		}
		sh := c.shards[s]
		sh.mu.Lock()
		for _, i := range part {
			v, ok := sh.m[keys[i]]
			out[i] = BatchResult{Value: v, OK: ok}
		}
		sh.mu.Unlock()
	}
	return out
}

// GetBatchParallel fans the per-shard work out across transient worker
// goroutines that claim shards off an atomic cursor. Correctness rests on
// three separate mechanisms, each of which had to be chosen and must be
// re-verified whenever this code changes: the cursor hands each shard to
// exactly one worker (no double-visit, no skip); each worker takes the
// shard's mutex against concurrent writers; and the WaitGroup join
// publishes the workers' writes to the caller. A production version would
// also want a size heuristic — below a few thousand keys the ~10µs of
// spawns costs more than it buys (see BenchmarkGetBatch).
func (c *Sharded) GetBatchParallel(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	if len(keys) == 0 {
		return out
	}
	idxs, starts := groupByShard(keys, lowBitsShard)
	workers := min(runtime.GOMAXPROCS(0), shardCount)
	var cursor atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				s := int(cursor.Add(1)) - 1
				if s >= shardCount {
					return
				}
				part := idxs[starts[s]:starts[s+1]]
				if len(part) == 0 {
					continue
				}
				sh := c.shards[s]
				sh.mu.Lock()
				for _, i := range part {
					v, ok := sh.m[keys[i]]
					out[i] = BatchResult{Value: v, OK: ok}
				}
				sh.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}
