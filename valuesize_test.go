package cache

import (
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
)

// BenchmarkValueSize adds the axis the core grid deliberately excludes,
// in the only regime where it is honest to measure it.
//
// The core benchmarks share ONE value string across every Set and never
// look at value bytes, so Set stores a 16-byte header and value size
// provably cannot matter (see the README's measurement note). Real
// workloads are not like that, and the difference is not just "GC
// pauses" — value size sets the heap's whole geometry, and each part of
// that has its own performance mechanism:
//
//   - Residency and dilution: with distinct values the heap carries
//     keys × size bytes, and structure nodes are allocated interleaved
//     with values, so the structure's effective density in LLC and in
//     dTLB reach drops as values grow. A pointer walk misses more, in
//     cache and in the TLB, even though it never reads a value byte.
//   - Displacement: a server that returns a value serializes it —
//     touching size bytes per read, which both costs memory bandwidth
//     and evicts structure from cache. Pointer-dense designs have the
//     most to lose.
//   - Allocator behavior: Go's size-class allocator is still an
//     allocator. 16 B and 4 KB live in different size classes with
//     different span dynamics and fragmentation; every write allocates
//     (and zeroes) a fresh value, as a server storing a just-deserialized
//     body would, plus whatever nodes the design itself churns.
//   - GC: allocation-heavy writers pay mark assists in-line, and mark
//     cycles scan each structure's pointers — a bill that differs by
//     design and compounds with the persistent structures' node garbage.
//
// So this benchmark models the full regime: distinct per-key values at
// prefill, every Set allocating a fresh value of the configured size, and
// every hit Get walking the value at cache-line stride (into a sink) the
// way a serializer would. Size is then a dial that moves the whole system
// from synchronization-bound (16 B — expect the core grid's rankings) to
// memory-bound (4 KB — expect convergence, and watch WHICH designs decay
// fastest on the way).
//
// The key count is capped so the live-value footprint stays bounded
// (~400 MB at the largest configuration). Uniform access, all four mixes.
//
//	go test -bench=BenchmarkValueSize -cpu=8 -count=3
func BenchmarkValueSize(b *testing.B) {
	n := *numKeysFlag
	if n > 100_000 {
		n = 100_000
	}
	keys := makeKeys(n, *keyLenFlag)

	for _, impl := range Concurrent {
		for _, size := range []int{16, 256, 4096} {
			// Prefill with DISTINCT value allocations (same contents,
			// separate buffers — heap geometry is about object count and
			// bytes, not content).
			c := impl.New()
			if bl, ok := c.(BulkLoader); ok {
				items := make(map[string]string, n)
				for _, k := range keys {
					items[k] = makeValue(size)
				}
				bl.Load(items)
			} else {
				for _, k := range keys {
					c.Set(k, makeValue(size))
				}
			}
			src := make([]byte, size)
			for _, mx := range mixes {
				name := fmt.Sprintf("impl=%s/vsize=%d/mix=%s", impl.Name, size, mx.name)
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						r := rand.New(rand.NewSource(nextSeed()))
						local := 0
						for pb.Next() {
							k := keys[r.Intn(n)]
							if r.Float64() < mx.readFrac {
								if v, ok := c.Get(k); ok {
									// Serialize-style touch: one read per
									// cache line of the value.
									for i := 0; i < len(v); i += 64 {
										local += int(v[i])
									}
								}
							} else {
								// A fresh buffer per write, like a server
								// storing a just-deserialized body.
								c.Set(k, string(src))
							}
						}
						valueSink.Add(int64(local))
					})
				})
			}
		}
	}
}

// valueSink keeps the compiler from eliminating the value-touch loop.
var valueSink atomic.Int64
