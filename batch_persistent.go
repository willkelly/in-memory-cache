package cache

// The persistent designs' batched gets. Where the mutex design amortizes
// LOCKS (batch_sharded.go) and the actor amortizes MESSAGES
// (batch_actor.go), a persistent snapshot has nothing to amortize — the
// entire feature is one atomic root load. What that buys instead is a
// semantic upgrade no lock-striped design can offer at any price: every
// answer in the batch comes from ONE point-in-time snapshot. A sharded
// batch reads shard 3's keys, then — while writers keep writing — shard
// 7's; the results can be mutually inconsistent (TestGetBatchSnapshot
// makes this concrete). cow and hamt answer the whole batch from one
// frozen world.
//
// The ladder repeats in miniature:
//
//   - cow, hamt: one root load, then a read-only loop. Globally
//     consistent batch.
//   - hamt256: group positions by shard (its OWN router — Fibonacci
//     mixing, not sharded's low bits), one root load per shard touched.
//     Consistent per shard, like its Len.
//   - ctrie: one O(1) Snapshot, then the loop against the frozen world.
//     With the full generation protocol the ctrie joins the consistent-
//     batch club — and pays on the write side instead: each snapshot
//     obliges subsequent writes to renew the paths they touch.

// GetBatch answers every key from a single frozen map snapshot.
func (c *COW) GetBatch(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	m := *c.ptr.Load()
	for i, k := range keys {
		v, ok := m[k]
		out[i] = BatchResult{Value: v, OK: ok}
	}
	return out
}

// GetBatch answers every key from a single frozen trie snapshot.
func (c *HAMT) GetBatch(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	root := c.root.Load().node
	for i, k := range keys {
		v, ok := hamtGet(root, fnv1a(k), k)
		out[i] = BatchResult{Value: v, OK: ok}
	}
	return out
}

// GetBatch answers each shard's keys from that shard's frozen snapshot:
// one root load per shard touched, consistency per shard. Note the router
// passed to the grouping — hamt256's shard function is NOT sharded's
// low-bits one (see hamtsharded.go), and using the wrong router here
// would silently answer every key from the wrong shard's snapshot.
// (1 << hamtShardBits == shardCount, so the grouping's arrays fit.)
func (c *ShardedHAMT) GetBatch(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	if len(keys) == 0 {
		return out
	}
	idxs, starts := groupByShard(keys, func(k string) uint64 { return hamtShardIndex(fnv1a(k)) })
	for s := 0; s < 1<<hamtShardBits; s++ {
		part := idxs[starts[s]:starts[s+1]]
		if len(part) == 0 {
			continue
		}
		node := c.shards[s].root.Load().node
		for _, i := range part {
			k := keys[i]
			v, ok := hamtLookup(node, fnv1a(k), hamt256Bits, k)
			out[i] = BatchResult{Value: v, OK: ok}
		}
	}
	return out
}

// GetBatch answers every key from one O(1) snapshot — a globally
// consistent multi-key read, like cow's and hamt's.
func (c *Ctrie) GetBatch(keys []string) []BatchResult {
	return c.Snapshot().GetBatch(keys)
}
