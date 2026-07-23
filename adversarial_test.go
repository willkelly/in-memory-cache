package cache

import (
	"fmt"
	"math/rand"
	"testing"
)

// BenchmarkAdversarial measures how much each design depends on keys
// LANDING in friendly places, as opposed to being uniformly popular.
// dist=zipf skews popularity but leaves placement accidental (the hot head
// still hashes all over the structure); here popularity is uniform and
// PLACEMENT is the attack. Each pattern selects a fixed-size working set
// from the prefilled keys by a predicate on the key's hash:
//
//   - uniform:    no constraint — the baseline every design likes.
//   - lowshard1:  all keys share their low 8 hash bits. That is one
//     `sharded` shard (lock convoy), and it also pins the tries' first
//     chunk, funneling hamt/ctrie traffic into a single level-0 subtree.
//     hamt256 shrugs: its Fibonacci-mixed routing still spreads these.
//   - lowshard8:  the clustered version — traffic confined to 8 of 256
//     low-bit shards, modeling tenant/prefix clustering.
//   - mixshard1:  all keys land in one hamt256 shard (equal top byte of
//     hash*fibMix). sharded shrugs: their low bits are unconstrained.
//   - mixshard8:  the clustered version of that.
//
// The working set is the SAME SIZE for every pattern (see the README's
// measurement note on holding the working set constant), so columns
// differ only in placement. Requires -keys large enough that each
// predicate can fill the set (1,000,000 comfortably suffices).
//
//	go test -bench=BenchmarkAdversarial -cpu=8 -count=3 -keys=1000000
func BenchmarkAdversarial(b *testing.B) {
	n := *numKeysFlag
	keys := makeKeys(n, *keyLenFlag)
	val := makeValue(benchValueBytes)
	const setSize = 2048

	patterns := []struct {
		name string
		pick func(h uint64) bool
	}{
		{"uniform", func(h uint64) bool { return true }},
		{"lowshard1", func(h uint64) bool { return h&(shardCount-1) == 0 }},
		{"lowshard8", func(h uint64) bool { return h&(shardCount-1) < 8 }},
		{"mixshard1", func(h uint64) bool { return hamtShardIndex(h) == 0 }},
		{"mixshard8", func(h uint64) bool { return hamtShardIndex(h) < 8 }},
	}

	// Select every pattern's set in the SAME fixed shuffled order. Taking
	// the first matches in key order would give the uniform baseline a
	// heap-contiguous working set (its predicate accepts everything) while
	// the 1-in-256 patterns stride the whole key array — a key-byte
	// locality difference that has nothing to do with placement. Shuffling
	// first makes all five columns sample the heap at equal dispersion, so
	// they really do differ only in where the keys LAND.
	order := rand.New(rand.NewSource(1)).Perm(n)
	sets := make([][]string, len(patterns))
	for i, p := range patterns {
		set := make([]string, 0, setSize)
		for _, idx := range order {
			if k := keys[idx]; p.pick(fnv1a(k)) {
				set = append(set, k)
				if len(set) == setSize {
					break
				}
			}
		}
		if len(set) < setSize {
			b.Skipf("pattern %s: only %d/%d keys available; run with -keys=1000000", p.name, len(set), setSize)
		}
		sets[i] = set
	}

	// Two mixes bracket the story: pure reads expose convoying on locks,
	// write-heavy exposes contention on whatever the writes serialize on.
	adversarialMixes := []mix{{"r100", 1.00}, {"r10", 0.10}}

	for _, impl := range []string{"mutex", "sharded", "hamt", "hamt256", "ctrie"} {
		impl := impl
		c, err := New(impl)
		if err != nil {
			b.Fatal(err)
		}
		prefill(c, keys, val) // full n keys; patterns vary only what is TOUCHED
		for pi, p := range patterns {
			set := sets[pi]
			for _, mx := range adversarialMixes {
				name := fmt.Sprintf("impl=%s/pattern=%s/mix=%s", impl, p.name, mx.name)
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						r := rand.New(rand.NewSource(nextSeed()))
						for pb.Next() {
							k := set[r.Intn(setSize)]
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
