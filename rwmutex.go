package cache

import "sync"

// RWMutex guards a single map with a sync.RWMutex. Multiple readers can hold
// the read lock simultaneously, so concurrent Gets proceed in parallel;
// writers still need exclusive access. Expected to beat Mutex on read-heavy
// workloads and to roughly match (or trail, due to bookkeeping overhead) it
// on write-heavy ones.
type RWMutex struct {
	mu sync.RWMutex
	m  map[string]string
}

func NewRWMutex() *RWMutex {
	return &RWMutex{m: make(map[string]string)}
}

func (c *RWMutex) Get(key string) (string, bool) {
	c.mu.RLock()
	v, ok := c.m[key]
	c.mu.RUnlock()
	return v, ok
}

func (c *RWMutex) Set(key, value string) {
	c.mu.Lock()
	c.m[key] = value
	c.mu.Unlock()
}

func (c *RWMutex) Delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

func (c *RWMutex) Len() int {
	c.mu.RLock()
	n := len(c.m)
	c.mu.RUnlock()
	return n
}

// Load implements BulkLoader.
func (c *RWMutex) Load(items map[string]string) {
	c.mu.Lock()
	c.m = items
	c.mu.Unlock()
}
