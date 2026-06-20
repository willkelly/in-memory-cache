package cache

// Naive is a plain map with no synchronization whatsoever.
//
// It is correct only when accessed from a single goroutine. Used
// concurrently it exhibits data races and, for concurrent writes, Go's
// runtime will deliberately crash the process with a fatal
// "concurrent map writes" throw (which is NOT recoverable with recover()).
//
// It serves two purposes in this project:
//   - a single-threaded performance baseline, and
//   - a demonstration (see TestNaiveRace) that the built-in map is not
//     thread-safe.
type Naive struct {
	m map[string]string
}

func NewNaive() *Naive {
	return &Naive{m: make(map[string]string)}
}

func (c *Naive) Get(key string) (string, bool) {
	v, ok := c.m[key]
	return v, ok
}

func (c *Naive) Set(key, value string) {
	c.m[key] = value
}

func (c *Naive) Delete(key string) {
	delete(c.m, key)
}

func (c *Naive) Len() int {
	return len(c.m)
}

// Load implements BulkLoader.
func (c *Naive) Load(items map[string]string) {
	c.m = items
}
