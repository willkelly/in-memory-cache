package cache

import (
	"flag"
	"fmt"
	"math/rand"
	"sync"
	"testing"
)

// Tunable benchmark parameters, settable on the `go test` command line, e.g.
//
//	go test -bench=. -keys=1000000 -keylen=16
//
// Keep -keys modest (the default) for quick local runs; use 1,000,000 for the
// numbers reported in the blog post. Beware memory: keys * valueSize bytes
// must fit in RAM, and the large-value regime multiplies that substantially.
var (
	numKeysFlag = flag.Int("keys", 100_000, "number of distinct keys")
	keyLenFlag  = flag.Int("keylen", 16, "minimum key length in bytes")
)

// Benchmark axes. The full cross product is large; in practice run -bench
// with a regexp to isolate the axis you care about, e.g.
//
//	go test -bench='BenchmarkCache/sharded/.*/zipf/' -cpu=1,2,4,8

type mix struct {
	name     string
	readFrac float64 // fraction of operations that are reads
}

var mixes = []mix{
	{"r100", 1.00}, // read-only
	{"r90", 0.90},  // read-heavy
	{"r50", 0.50},  // balanced
	{"r10", 0.10},  // write-heavy
}

// benchValueBytes is the (fixed) size of the value used in every benchmark.
//
// Value size is deliberately NOT a benchmark axis: values are shared,
// immutable Go strings, so Set stores a 16-byte header and never touches the
// value bytes. Varying the size changes neither op throughput nor allocations
// (verified: 64 B and 16 KB are identical within noise, 0 B/op). Value size
// only affects total memory footprint and, via that, GC pause behavior --
// which is a separate experiment from the synchronization cost measured here.
const benchValueBytes = 64

type distribution struct {
	name string
	zipf bool
}

var distributions = []distribution{
	{"uniform", false},
	{"zipf", true},
}

// Zipf parameters. s>1 controls skew; ~1.1 gives a realistically hot head
// without being degenerate. v=1 anchors the distribution at rank 0.
const (
	zipfS = 1.1
	zipfV = 1.0
)

// BenchmarkCache drives every concurrent implementation across the read/write
// mix, value size, and access distribution axes. Vary GOMAXPROCS with the
// -cpu flag to expose contention scaling.
//
// naive is excluded here: it is not safe under RunParallel and would crash
// the process. See BenchmarkSequential for its single-threaded baseline.
func BenchmarkCache(b *testing.B) {
	n := *numKeysFlag
	keys := makeKeys(n, *keyLenFlag)

	val := makeValue(benchValueBytes)
	for _, impl := range Concurrent {
		for _, d := range distributions {
			for _, mx := range mixes {
				// key=value sub-benchmark names let benchstat pivot
				// these axes into comparison tables, e.g.
				//   benchstat -col /impl results.txt
				name := fmt.Sprintf("impl=%s/dist=%s/mix=%s", impl.Name, d.name, mx.name)
				// Build and prefill the cache OUTSIDE b.Run. The framework
				// re-invokes the b.Run closure many times while ramping b.N;
				// prefilling 1M keys inside it would rebuild the whole map on
				// every ramp step and dominate wall-clock time.
				c := impl.New()
				prefill(c, keys, val)
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						r := rand.New(rand.NewSource(nextSeed()))
						var z *rand.Zipf
						if d.zipf {
							z = rand.NewZipf(r, zipfS, zipfV, uint64(n-1))
						}
						for pb.Next() {
							var idx int
							if z != nil {
								idx = int(z.Uint64())
							} else {
								idx = r.Intn(n)
							}
							k := keys[idx]
							if r.Float64() < mx.readFrac {
								c.Get(k)
							} else {
								c.Set(k, val)
							}
						}
					})
				})
			}
		}
	}
}

// BenchmarkSequential measures each implementation (including naive) from a
// single goroutine. This is the uncontended baseline: it shows the raw
// per-operation overhead of each synchronization strategy before any
// contention effects enter. naive should be the floor.
func BenchmarkSequential(b *testing.B) {
	n := *numKeysFlag
	keys := makeKeys(n, *keyLenFlag)
	val := makeValue(64)

	for _, impl := range All {
		for _, mx := range mixes {
			name := fmt.Sprintf("impl=%s/mix=%s", impl.Name, mx.name)
			// Prefill outside b.Run (see note in BenchmarkCache).
			c := impl.New()
			prefill(c, keys, val)
			b.Run(name, func(b *testing.B) {
				r := rand.New(rand.NewSource(nextSeed()))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					k := keys[r.Intn(n)]
					if r.Float64() < mx.readFrac {
						c.Get(k)
					} else {
						c.Set(k, val)
					}
				}
			})
		}
	}
}

// --- Correctness tests -----------------------------------------------------

// TestBasic exercises the single-threaded contract of every implementation.
func TestBasic(t *testing.T) {
	for _, impl := range All {
		t.Run(impl.Name, func(t *testing.T) {
			c := impl.New()

			if _, ok := c.Get("missing"); ok {
				t.Fatal("Get on empty cache reported ok")
			}
			c.Set("a", "1")
			c.Set("b", "2")
			if v, ok := c.Get("a"); !ok || v != "1" {
				t.Fatalf("Get(a) = %q,%v; want 1,true", v, ok)
			}
			c.Set("a", "3") // overwrite
			if v, _ := c.Get("a"); v != "3" {
				t.Fatalf("Get(a) after overwrite = %q; want 3", v)
			}
			if n := c.Len(); n != 2 {
				t.Fatalf("Len = %d; want 2", n)
			}
			c.Delete("a")
			if _, ok := c.Get("a"); ok {
				t.Fatal("Get(a) ok after Delete")
			}
			if n := c.Len(); n != 1 {
				t.Fatalf("Len = %d after Delete; want 1", n)
			}
		})
	}
}

// TestConcurrentSmoke hammers each concurrent implementation from many
// goroutines. Run with -race to confirm the synchronization is sound:
//
//	go test -race -run TestConcurrentSmoke
func TestConcurrentSmoke(t *testing.T) {
	const goroutines = 8
	const opsPer = 2_000
	keys := makeKeys(256, 8)

	for _, impl := range Concurrent {
		t.Run(impl.Name, func(t *testing.T) {
			c := impl.New()
			var wg sync.WaitGroup
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func(seed int64) {
					defer wg.Done()
					r := rand.New(rand.NewSource(seed))
					for i := 0; i < opsPer; i++ {
						k := keys[r.Intn(len(keys))]
						switch r.Intn(3) {
						case 0:
							c.Set(k, "v")
						case 1:
							c.Get(k)
						default:
							c.Delete(k)
						}
					}
				}(nextSeed())
			}
			wg.Wait()
		})
	}
}
