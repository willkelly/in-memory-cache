package cache

import (
	"sync"
	"sync/atomic"
)

// COW is a copy-on-write cache: the read path is fully lock-free, at the cost
// of an expensive write path.
//
// Readers do a single atomic load of an immutable map pointer and read from
// it with no lock at all. Writers serialize on a mutex, copy the entire map,
// apply their change to the copy, and atomically publish the new map. Because
// a published map is never mutated again, readers holding an older pointer
// remain correct — they simply observe a slightly stale snapshot.
//
// This is the *race-free* realization of the "two maps" idea. The naive
// version (mutate the readers' map in place after an atomic pointer swap) has
// a data race: a reader that loaded the pointer just before the swap is still
// reading the old map while the writer mutates it. Atomically swapping the
// pointer does not help, because there is no way in the Go standard library
// to know when all readers have moved off the old map (the RCU grace-period
// problem). Copy-on-write sidesteps that entirely by never mutating a map
// that has been published.
//
// Trade-off: every write is O(n) in the number of keys. COW only makes sense
// for read-mostly workloads; under write-heavy load it is expected to be the
// slowest implementation by a wide margin, which is itself an instructive
// result. A production variant would batch many writes between publishes.
type COW struct {
	mu  sync.Mutex // serializes writers only; readers never take it
	ptr atomic.Pointer[map[string]string]
}

func NewCOW() *COW {
	c := &COW{}
	m := make(map[string]string)
	c.ptr.Store(&m)
	return c
}

func (c *COW) Get(key string) (string, bool) {
	m := *c.ptr.Load()
	v, ok := m[key]
	return v, ok
}

func (c *COW) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *c.ptr.Load()
	next := make(map[string]string, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[key] = value
	c.ptr.Store(&next)
}

func (c *COW) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *c.ptr.Load()
	if _, ok := old[key]; !ok {
		return
	}
	next := make(map[string]string, len(old))
	for k, v := range old {
		if k == key {
			continue
		}
		next[k] = v
	}
	c.ptr.Store(&next)
}

func (c *COW) Len() int {
	return len(*c.ptr.Load())
}

// Load implements BulkLoader. Without it, prefilling n keys via Set would be
// O(n^2) because each Set copies the whole map.
func (c *COW) Load(items map[string]string) {
	c.mu.Lock()
	c.ptr.Store(&items)
	c.mu.Unlock()
}
