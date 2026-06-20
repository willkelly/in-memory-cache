package cache

import "sync"

// SyncMap wraps the standard library's sync.Map.
//
// sync.Map is optimized for two specific patterns: (1) keys that are written
// once and read many times, and (2) goroutines operating on disjoint key
// sets. Under those patterns its read path is nearly lock-free. For general
// read/write churn over a shared key set it often loses to a sharded mutex
// map because of interface boxing and internal bookkeeping. Including it
// shows readers when the stdlib's own answer is and isn't the right tool.
type SyncMap struct {
	m sync.Map
}

func NewSyncMap() *SyncMap {
	return &SyncMap{}
}

func (c *SyncMap) Get(key string) (string, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return "", false
	}
	return v.(string), true
}

func (c *SyncMap) Set(key, value string) {
	c.m.Store(key, value)
}

func (c *SyncMap) Delete(key string) {
	c.m.Delete(key)
}

func (c *SyncMap) Len() int {
	n := 0
	c.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// Note: SyncMap intentionally does NOT implement BulkLoader. There is no way
// to bulk-assign a sync.Map, so the harness prefills it with a Store loop,
// which is O(n) and fine.
