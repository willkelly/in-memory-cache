package cache

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// makeKeys returns n distinct keys, each padded with leading zeros to be at
// least length bytes long. length must be large enough to hold the decimal
// digits of n-1 (e.g. >= 7 for n = 1,000,000) or keys will not be unique.
//
// Real cache keys are short; keep length small (16-64) for the realistic
// case and only crank it up to study the cost of hashing long keys.
func makeKeys(n, length int) []string {
	keys := make([]string, n)
	for i := range keys {
		s := strconv.Itoa(i)
		if len(s) < length {
			s = strings.Repeat("0", length-len(s)) + s
		}
		keys[i] = s
	}
	return keys
}

// makeValue returns a string of exactly size bytes. Value contents are
// irrelevant to the benchmark; only the length matters (it drives memory
// bandwidth and GC pressure).
func makeValue(size int) string {
	return strings.Repeat("x", size)
}

// prefill loads a cache with the given keys, all mapped to val. It uses the
// BulkLoader fast path when available (mandatory for COW, which would
// otherwise be O(n^2)) and falls back to a Set loop otherwise.
func prefill(c Cache, keys []string, val string) {
	if bl, ok := c.(BulkLoader); ok {
		m := make(map[string]string, len(keys))
		for _, k := range keys {
			m[k] = val
		}
		bl.Load(m)
		return
	}
	for _, k := range keys {
		c.Set(k, val)
	}
}

// seedCounter hands out distinct, deterministic RNG seeds to each benchmark
// goroutine. We avoid time-based seeds so runs are reproducible; uniqueness
// (not unpredictability) is what matters here. Each goroutine gets its own
// *rand.Rand built from this seed so there is no shared-RNG lock contention
// polluting the measurement.
var seedCounter atomic.Int64

func nextSeed() int64 {
	return seedCounter.Add(1)
}
