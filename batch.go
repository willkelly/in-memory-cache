package cache

// Batched (multi-key) gets. The feature is deliberately split across three
// files so the cost of adding it to each design can be compared directly:
//
//   - batch.go         — the shared contract and key grouping (design-neutral)
//   - batch_actor.go   — the actor's implementation: sends messages, waits
//     for acks. Sequential code; the parallelism arrives via the protocol.
//     (Plus a six-line case in actor.go's shard loop.)
//   - batch_sharded.go — the mutex design's implementation: the serial
//     version is easy, but making it parallel means hand-building a
//     spawn/partition/join harness — concurrent code that must be reviewed
//     for races and rebuilt for every future operation that wants it.
//
// Neither path copies any key or value bytes: grouping permutes positions,
// shards/workers index back into the caller's slice, and results scatter
// into disjoint ranges of one output slice.

// BatchResult is one entry of a GetBatch reply, positionally matching the
// query slice.
type BatchResult struct {
	Value string
	OK    bool
}

// BatchGetter is an optional extension for implementations that can answer
// many Gets in one call. The result slice is positional: res[i] answers
// keys[i], and duplicate keys are answered at every position they occupy.
type BatchGetter interface {
	GetBatch(keys []string) []BatchResult
}

// ParallelBatchGetter marks implementations whose batched get fans the
// per-shard work out across CPUs by hand. (Actor's plain GetBatch is
// already parallel; this interface exists for designs that need a separate
// method because their sequential and parallel versions are different code.)
type ParallelBatchGetter interface {
	GetBatchParallel(keys []string) []BatchResult
}

// groupByShard partitions the *positions* of keys by owning shard with a
// two-pass counting sort: shard s owns idxs[starts[s]:starts[s+1]], where
// idxs is a permutation of [0, len(keys)). The keys themselves are never
// moved or copied — consumers index back into the caller's slice. This is
// the coalescing step: however many times a hot key repeats, all of its
// positions land in one shard's contiguous range and ride in one message.
//
// shardOf is a byte, which requires shardCount <= 256.
func groupByShard(keys []string) (idxs []int, starts *[shardCount + 1]int) {
	shardOf := make([]uint8, len(keys))
	starts = new([shardCount + 1]int)
	for i, k := range keys {
		s := int(fnv1a(k) & (shardCount - 1))
		shardOf[i] = uint8(s)
		starts[s+1]++ // s must be int here: uint8 s+1 wraps 255 -> 0
	}
	for s := 0; s < shardCount; s++ {
		starts[s+1] += starts[s]
	}
	idxs = make([]int, len(keys))
	next := *starts
	for i := range keys {
		s := shardOf[i]
		idxs[next[s]] = i
		next[s]++
	}
	return idxs, starts
}
