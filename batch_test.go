package cache

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"testing"
)

// batchImpls returns the implementations that support GetBatch, discovered
// through the optional interface so new implementations join automatically.
func batchImpls() []Impl {
	var out []Impl
	for _, impl := range Concurrent {
		if _, ok := impl.New().(BatchGetter); ok {
			out = append(out, impl)
		}
	}
	return out
}

// TestGetBatch checks the positional contract: res[i] answers keys[i],
// including misses, duplicates, and the empty batch.
func TestGetBatch(t *testing.T) {
	for _, impl := range batchImpls() {
		t.Run(impl.Name, func(t *testing.T) {
			c := impl.New()
			bg := c.(BatchGetter)

			want := map[string]string{}
			for i := 0; i < 500; i++ {
				k := "k" + strconv.Itoa(i)
				v := "v" + strconv.Itoa(i)
				c.Set(k, v)
				want[k] = v
			}

			if res := bg.GetBatch(nil); len(res) != 0 {
				t.Fatalf("GetBatch(nil) returned %d results", len(res))
			}

			// Query mixing hits, misses, and a hot duplicated key.
			var query []string
			for i := 0; i < 500; i += 2 {
				query = append(query, "k"+strconv.Itoa(i))
			}
			for i := 0; i < 50; i++ {
				query = append(query, "missing"+strconv.Itoa(i))
			}
			for i := 0; i < 100; i++ {
				query = append(query, "k7") // duplicates coalesce into one shard message
			}

			checkBatch := func(mode string, res []BatchResult) {
				t.Helper()
				if len(res) != len(query) {
					t.Fatalf("%s: got %d results for %d keys", mode, len(res), len(query))
				}
				for i, k := range query {
					v, ok := want[k]
					if res[i].OK != ok || res[i].Value != v {
						t.Fatalf("%s: res[%d] (key %q) = %q,%v; want %q,%v",
							mode, i, k, res[i].Value, res[i].OK, v, ok)
					}
				}
			}
			checkBatch("batch", bg.GetBatch(query))
			if pbg, ok := c.(ParallelBatchGetter); ok {
				if res := pbg.GetBatchParallel(nil); len(res) != 0 {
					t.Fatalf("GetBatchParallel(nil) returned %d results", len(res))
				}
				checkBatch("parbatch", pbg.GetBatchParallel(query))
			}
		})
	}
}

// TestGetBatchConcurrent hammers GetBatch against concurrent writers. Run
// with -race: it verifies the scatter-gather writes into the shared result
// slice are properly ordered by the ack channel. Values are constrained so
// every observed result must be a value some writer actually stored.
func TestGetBatchConcurrent(t *testing.T) {
	keys := makeKeys(256, 8)
	valid := map[string]bool{"": true, "v1": true, "v2": true}

	for _, impl := range batchImpls() {
		t.Run(impl.Name, func(t *testing.T) {
			c := impl.New()
			bg := c.(BatchGetter)
			var wg sync.WaitGroup
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func(seed int64) {
					defer wg.Done()
					r := rand.New(rand.NewSource(seed))
					for i := 0; i < 500; i++ {
						k := keys[r.Intn(len(keys))]
						switch r.Intn(3) {
						case 0:
							c.Set(k, "v1")
						case 1:
							c.Set(k, "v2")
						default:
							c.Delete(k)
						}
					}
				}(nextSeed())
			}
			pbg, hasPar := c.(ParallelBatchGetter)
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 100; i++ {
						res := bg.GetBatch(keys)
						if hasPar && i%2 == 1 {
							res = pbg.GetBatchParallel(keys)
						}
						for _, r := range res {
							if !valid[r.Value] {
								t.Errorf("batch returned impossible value %q", r.Value)
								return
							}
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}

// makeBatches prebuilds query batches so RNG cost stays outside the timed
// loop. Zipf batches contain many duplicate hot keys — the coalescing case.
func makeBatches(keys []string, zipf bool, size, count int) [][]string {
	r := rand.New(rand.NewSource(nextSeed()))
	var z *rand.Zipf
	if zipf {
		z = rand.NewZipf(r, zipfS, zipfV, uint64(len(keys)-1))
	}
	batches := make([][]string, count)
	for i := range batches {
		bk := make([]string, size)
		for j := range bk {
			if z != nil {
				bk[j] = keys[z.Uint64()]
			} else {
				bk[j] = keys[r.Intn(len(keys))]
			}
		}
		batches[i] = bk
	}
	return batches
}

// BenchmarkGetBatch measures multi-key reads from a single caller, the
// latency-oriented complement to BenchmarkCache's saturated throughput.
// mode=loop issues one Get per key (the only option without the batch
// protocol); mode=batch uses GetBatch. Compare the ns/key metric: ns/op is
// per *batch* and scales with size. The shard goroutines are the only
// concurrency in the actor's batch mode — fan-out parallelism comes from
// the topology, not from the benchmark.
func BenchmarkGetBatch(b *testing.B) {
	n := *numKeysFlag
	keys := makeKeys(n, *keyLenFlag)
	val := makeValue(benchValueBytes)
	sizes := []int{16, 256, 4096, 16384}
	// The prebuilt batches are cycled, so the benchmark's memory working
	// set is (number of batches) x (batch size) distinct key positions —
	// hold that product constant or the axis lies. With a fixed batch
	// *count* instead, small sizes would recycle a few thousand keys whose
	// buckets stay cached and accidentally measure warm-cache lookups;
	// per-key cost would appear to grow with batch size when it is really
	// growing with the sampled working set.
	const batchWorkingSet = 1 << 19

	for _, impl := range batchImpls() {
		c := impl.New()
		prefill(c, keys, val)
		bg := c.(BatchGetter)
		for _, d := range distributions {
			for _, size := range sizes {
				batches := makeBatches(keys, d.zipf, size, batchWorkingSet/size)
				perKey := func(b *testing.B) {
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(size), "ns/key")
				}
				name := fmt.Sprintf("impl=%s/dist=%s/size=%d", impl.Name, d.name, size)
				b.Run(name+"/mode=loop", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						for _, k := range batches[i%len(batches)] {
							c.Get(k)
						}
					}
					perKey(b)
				})
				b.Run(name+"/mode=batch", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						bg.GetBatch(batches[i%len(batches)])
					}
					perKey(b)
				})
				if pbg, ok := c.(ParallelBatchGetter); ok {
					b.Run(name+"/mode=parbatch", func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
							pbg.GetBatchParallel(batches[i%len(batches)])
						}
						perKey(b)
					})
				}
			}
		}
	}
}
