package cache

import "sync"

// Mutex guards a single map with one sync.Mutex. Every operation, read or
// write, takes the exclusive lock. Simple and always correct, but reads
// cannot proceed in parallel with each other.
type Mutex struct {
	mu sync.Mutex
	m  map[string]string
}

func NewMutex() *Mutex {
	return &Mutex{m: make(map[string]string)}
}

func (c *Mutex) Get(key string) (string, bool) {
	c.mu.Lock()
	v, ok := c.m[key]
	c.mu.Unlock()
	return v, ok
}

func (c *Mutex) Set(key, value string) {
	c.mu.Lock()
	c.m[key] = value
	c.mu.Unlock()
}

func (c *Mutex) Delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

func (c *Mutex) Len() int {
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	return n
}

// Load implements BulkLoader.
func (c *Mutex) Load(items map[string]string) {
	c.mu.Lock()
	c.m = items
	c.mu.Unlock()
}
