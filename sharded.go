package cache

import "sync"

// shardCount is the number of independent map+lock partitions. It must be a
// power of two so the shard index is a cheap bitmask of the key hash.
const shardCount = 256

type shard struct {
	mu sync.Mutex
	m  map[string]string
	// pad keeps each shard's mutex on its own cache line so that locking
	// one shard does not cause false sharing with a neighbor. 64 bytes is
	// a common cache-line size; the exact value is not load-bearing.
	_ [64]byte
}

// Sharded (a.k.a. lock striping) partitions the key space across many
// independent maps, each with its own mutex. A key is routed to a shard by
// its hash, so operations on different shards never contend. This is the
// canonical high-throughput design: with enough shards relative to the
// number of cores, contention all but disappears under a uniform key
// distribution. Its weakness is a skewed (Zipfian) workload, where hot keys
// concentrate on a few shards.
type Sharded struct {
	shards [shardCount]*shard
}

func NewSharded() *Sharded {
	s := &Sharded{}
	for i := range s.shards {
		s.shards[i] = &shard{m: make(map[string]string)}
	}
	return s
}

// fnv1a is an inlined FNV-1a hash. We compute it ourselves instead of using
// hash/fnv to avoid allocating a hasher and to avoid the io.Writer path.
func fnv1a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func (c *Sharded) shardFor(key string) *shard {
	return c.shards[fnv1a(key)&(shardCount-1)]
}

func (c *Sharded) Get(key string) (string, bool) {
	sh := c.shardFor(key)
	sh.mu.Lock()
	v, ok := sh.m[key]
	sh.mu.Unlock()
	return v, ok
}

func (c *Sharded) Set(key, value string) {
	sh := c.shardFor(key)
	sh.mu.Lock()
	sh.m[key] = value
	sh.mu.Unlock()
}

func (c *Sharded) Delete(key string) {
	sh := c.shardFor(key)
	sh.mu.Lock()
	delete(sh.m, key)
	sh.mu.Unlock()
}

func (c *Sharded) Len() int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// Load implements BulkLoader, distributing items across shards.
func (c *Sharded) Load(items map[string]string) {
	for k, v := range items {
		sh := c.shardFor(k)
		sh.m[k] = v
	}
}
