package cache

import "sync/atomic"

// ShardedHAMT answers hamt's write-serialization problem the same way
// sharded answers mutex's: partition the key space into 256 independent
// persistent tries, each with its own CAS-published root. Every operation
// is exactly hamt's — load a root, build a path, publish with one CAS —
// so the design stays fully lock-free; there is simply no longer a single
// pointer every writer must fight over. Two effects compound:
//
//   - Write contention divides by the shard count, and with it the retry
//     waste: a conflict now requires two writers to collide on the same
//     1/256th of the key space inside one CAS window.
//   - The tries get shallower. n/256 keys per trie is ~1.3 levels less
//     depth, so reads chase fewer pointers and writes copy fewer, smaller
//     nodes — sharding accidentally buys back most of the trie's read
//     deficit against cow.
//
// What dies is the global snapshot, the one thing hamt could do that no
// other design here can: Len (and point-in-time iteration in general) now
// sums 256 per-shard snapshots taken at slightly different moments. Each
// shard is still a perfect snapshot of itself — transactions just shrink
// from "the whole map" to "keys that share a shard".
//
// Picking the shard index is unexpectedly delicate, because the trie
// consumes hash chunks from the BOTTOM up:
//
//   - sharded's own recipe — the low bits — is out: all keys in a shard
//     would share their first trie chunk(s), degenerating the top of
//     every trie toward a single-child chain.
//   - The raw top byte is also out, for a subtler reason: FNV-1a XORs
//     each input byte into the LOW bits and only the multiplies carry
//     entropy upward, so for short keys the top byte is barely mixed (the
//     final byte gets exactly one multiply). Measured: 2,000 sequential
//     keys landed in just 21 of 256 shards.
//
// So the shard index is the top byte of hash * 2^64/φ (Fibonacci
// hashing): one multiply spreads the well-mixed low bits into the high
// bits. Fixing the shard then constrains keys only in that mixed
// product, which is uncorrelated with any particular low chunk — the
// trie's branching is untouched.
const (
	hamtShardBits = 8                  // 256 shards, mirroring sharded's shardCount
	fibMix        = 0x9E3779B97F4A7C15 // 2^64 / golden ratio, odd

	// hamt256 runs its tries at 32-way, not hamt's 64. The width sweep
	// (BenchmarkHAMTWidth) shows 32-way trades ~4% on reads for 16-24%
	// off writes and 27% off write garbage — the right side of the trade
	// for the write-scaling variant, and the same call Clojure made.
	// hamt keeps 64 because its niche is read-mostly snapshots.
	hamt256Bits = 5
)

type hamtShard struct {
	root atomic.Pointer[hamtRoot]
	// pad to a cache line so one shard's CAS traffic does not false-share
	// with its neighbors (same trick as sharded's shard struct).
	_ [56]byte
}

type ShardedHAMT struct {
	shards [1 << hamtShardBits]hamtShard
}

func NewShardedHAMT() *ShardedHAMT {
	c := &ShardedHAMT{}
	empty := &hamtRoot{node: hamtEmptyNode} // immutable, safe to share
	for i := range c.shards {
		c.shards[i].root.Store(empty)
	}
	return c
}

// hamtShardIndex is the single source of truth for routing: shardFor and
// Load must agree, or bulk-loaded keys become unreachable through Get.
func hamtShardIndex(hash uint64) uint64 {
	return (hash * fibMix) >> (64 - hamtShardBits)
}

func (c *ShardedHAMT) shardFor(hash uint64) *hamtShard {
	return &c.shards[hamtShardIndex(hash)]
}

func (c *ShardedHAMT) Get(key string) (string, bool) {
	hash := fnv1a(key)
	return hamtLookup(c.shardFor(hash).root.Load().node, hash, hamt256Bits, key)
}

func (c *ShardedHAMT) Set(key, value string) {
	hash := fnv1a(key)
	sh := c.shardFor(hash)
	for {
		old := sh.root.Load()
		node, delta := hamtInsert(old.node, hash, hamt256Bits, 0, key, value)
		if sh.root.CompareAndSwap(old, &hamtRoot{node: node, count: old.count + delta}) {
			return
		}
	}
}

func (c *ShardedHAMT) Delete(key string) {
	hash := fnv1a(key)
	sh := c.shardFor(hash)
	for {
		old := sh.root.Load()
		node, found := hamtDelete(old.node, hash, hamt256Bits, 0, key)
		if !found {
			return
		}
		if node == nil {
			node = hamtEmptyNode
		}
		if sh.root.CompareAndSwap(old, &hamtRoot{node: node, count: old.count - 1}) {
			return
		}
	}
}

// Len sums the per-shard counts. Each count is a consistent snapshot of
// its shard, but the sum is not a snapshot of the whole cache — the same
// semantics (and the same reason) as sharded's lock-by-lock Len.
func (c *ShardedHAMT) Len() int {
	n := 0
	for i := range c.shards {
		n += c.shards[i].root.Load().count
	}
	return n
}

// Load implements BulkLoader: one private transient build per shard, then
// 256 publishes.
func (c *ShardedHAMT) Load(items map[string]string) {
	var roots [1 << hamtShardBits]*hamtNode
	var counts [1 << hamtShardBits]int
	for i := range roots {
		roots[i] = &hamtNode{}
	}
	for k, v := range items {
		hash := fnv1a(k)
		s := hamtShardIndex(hash)
		hamtInsertMut(roots[s], hash, hamt256Bits, 0, k, v)
		counts[s]++
	}
	for i := range c.shards {
		node := roots[i]
		if counts[i] == 0 {
			node = hamtEmptyNode
		}
		c.shards[i].root.Store(&hamtRoot{node: node, count: counts[i]})
	}
}
