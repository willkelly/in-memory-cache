package cache

// The actor's batched get. Read this file alongside batch_sharded.go: there
// is no goroutine spawn, no lock, no atomic, and no join logic here,
// because the concurrency already exists — the 256 shard goroutines are a
// standing worker pool and this call is just a scatter-gather conversation
// with them. Every line below is sequential code.

// actorBatch is the shared context of one Actor.GetBatch call. Each shard
// message references it and carries only an index range, so a request stays
// small no matter the batch size. Shards scatter answers into disjoint
// positions of out; the resp channel's capacity is the number of shards
// touched, so their acks never block.
type actorBatch struct {
	keys []string
	idxs []int
	out  []BatchResult
	resp chan actorResponse
}

// GetBatch is a scatter-gather query: one message per shard touched, then
// the shard goroutines perform their lookups concurrently with each other
// while this goroutine waits for the acks. Consistency is unchanged: a
// batch queues behind earlier writes on each shard exactly like a single
// Get would.
func (c *Actor) GetBatch(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	if len(keys) == 0 {
		return out
	}
	idxs, starts := groupByShard(keys, lowBitsShard)
	touched := 0
	for s := 0; s < shardCount; s++ {
		if starts[s+1] > starts[s] {
			touched++
		}
	}
	b := &actorBatch{keys: keys, idxs: idxs, out: out, resp: make(chan actorResponse, touched)}
	for s := 0; s < shardCount; s++ {
		if starts[s+1] == starts[s] {
			continue
		}
		c.reqs[s] <- actorRequest{op: actorGetBatch, batch: b, lo: starts[s], hi: starts[s+1]}
	}
	for i := 0; i < touched; i++ {
		<-b.resp
	}
	return out
}
