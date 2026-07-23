package cache

import (
	"fmt"
	"math/bits"
	"math/rand"
	"sync"
	"testing"
)

// checkCtrieInvariants walks the trie and verifies structure: bitmap and
// slice agreement, entry placement by stored hash, collision nodes only
// past the last bitmap level. Unlike the hamt walker it does NOT demand
// canonical form — with no contraction, empty and single-entry C-nodes
// are legal residue of deletes. Returns the entry count.
func checkCtrieInvariants(t *testing.T, cn *ctrieCNode, prefix uint64, shift uint) int {
	t.Helper()
	if cn.collision {
		if shift < hashBits {
			t.Fatalf("collision node above the last bitmap level (shift %d)", shift)
		}
		if cn.dataMap != 0 || cn.nodeMap != 0 || len(cn.inodes) != 0 {
			t.Fatal("collision node with bitmap state")
		}
		for i := range cn.entries {
			if h := cn.entries[i].hash; h != prefix {
				t.Fatalf("collision entry %q has hash %#x, node prefix %#x", cn.entries[i].key, h, prefix)
			}
		}
		return len(cn.entries)
	}
	if cn.dataMap&cn.nodeMap != 0 {
		t.Fatalf("dataMap and nodeMap overlap: %#x & %#x", cn.dataMap, cn.nodeMap)
	}
	if got, want := len(cn.entries), bits.OnesCount64(cn.dataMap); got != want {
		t.Fatalf("len(entries)=%d, popcount(dataMap)=%d", got, want)
	}
	if got, want := len(cn.inodes), bits.OnesCount64(cn.nodeMap); got != want {
		t.Fatalf("len(inodes)=%d, popcount(nodeMap)=%d", got, want)
	}
	total := 0
	di, ni := 0, 0
	for b := uint(0); b < 1<<ctrieBits; b++ {
		bit := uint64(1) << b
		switch {
		case cn.dataMap&bit != 0:
			e := cn.entries[di]
			di++
			want := prefix | uint64(b)<<shift
			mask := uint64(1)<<(shift+ctrieBits) - 1
			if shift+ctrieBits >= 64 {
				mask = ^uint64(0)
			}
			if e.hash&mask != want {
				t.Fatalf("entry %q misplaced: hash %#x, slot prefix %#x (shift %d)", e.key, e.hash, want, shift)
			}
			total++
		case cn.nodeMap&bit != 0:
			child := cn.inodes[ni].main.Load()
			ni++
			total += checkCtrieInvariants(t, child, prefix|uint64(b)<<shift, shift+ctrieBits)
		}
	}
	return total
}

// checkCtrie verifies invariants, that the walker's count matches Len,
// and that every stored hash is really fnv1a of its key.
func checkCtrie(t *testing.T, c *Ctrie) {
	t.Helper()
	root := c.root.main.Load()
	if n, l := checkCtrieInvariants(t, root, 0, 0), c.Len(); n != l {
		t.Fatalf("walker counts %d entries, Len says %d", n, l)
	}
	var walk func(cn *ctrieCNode)
	walk = func(cn *ctrieCNode) {
		for i := range cn.entries {
			if e := &cn.entries[i]; e.hash != fnv1a(e.key) {
				t.Fatalf("entry %q stores hash %#x, fnv1a says %#x", e.key, e.hash, fnv1a(e.key))
			}
		}
		for _, in := range cn.inodes {
			walk(in.main.Load())
		}
	}
	walk(root)
}

// TestCtrieDifferential is the broad-coverage random-ops test against a
// reference map, exercising inserts, replaces, deletes, push-downs, and
// the no-contraction residue (emptied C-nodes must stay correct for later
// lookups and re-inserts).
func TestCtrieDifferential(t *testing.T) {
	c := NewCtrie()
	ref := make(map[string]string)
	keys := makeKeys(512, 4)
	r := rand.New(rand.NewSource(99))
	for op := 0; op < 20_000; op++ {
		k := keys[r.Intn(len(keys))]
		switch r.Intn(4) {
		case 0, 1:
			v := fmt.Sprintf("v%d", op)
			c.Set(k, v)
			ref[k] = v
		case 2:
			c.Delete(k)
			delete(ref, k)
		default:
			want, wantOK := ref[k]
			if got, ok := c.Get(k); ok != wantOK || got != want {
				t.Fatalf("op %d: Get(%q) = %q,%v; want %q,%v", op, k, got, ok, want, wantOK)
			}
		}
		if c.Len() != len(ref) {
			t.Fatalf("op %d: Len = %d, want %d", op, c.Len(), len(ref))
		}
		if op%1000 == 0 {
			checkCtrie(t, c)
		}
	}
	checkCtrie(t, c)
	for k, want := range ref {
		if got, ok := c.Get(k); !ok || got != want {
			t.Fatalf("final: Get(%q) = %q,%v; want %q,true", k, got, ok, want)
		}
	}
}

// TestCtrieCollisionPublicAPI drives full-hash-collision paths through the
// production methods with the decoy trick: internal inserts seed entries
// under the synthetic hash fnv1a("a"), so Get/Set/Delete on "a" descend
// the real path into the collision node.
func TestCtrieCollisionPublicAPI(t *testing.T) {
	h := fnv1a("a")
	c := NewCtrie()
	ctrieSet(&c.root, h, "decoy1", "d1")
	ctrieSet(&c.root, h, "decoy2", "d2")
	ctrieSet(&c.root, h, "a", "a-val")

	if v, ok := c.Get("a"); !ok || v != "a-val" {
		t.Fatalf("Get(a) = %q,%v; want a-val,true", v, ok)
	}
	c.Set("a", "a-new") // replace inside the collision node
	if v, ok := c.Get("a"); !ok || v != "a-new" {
		t.Fatalf("Get(a) after Set = %q,%v", v, ok)
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	c.Delete("a")
	if c.Len() != 2 {
		t.Fatalf("Len after Delete = %d, want 2", c.Len())
	}
	if v, ok := c.Get("a"); ok {
		t.Fatalf("Get(a) after Delete = %q,%v; want miss", v, ok)
	}
	c.Delete("a") // no-op through the collision scan
	if c.Len() != 2 {
		t.Fatalf("Len after no-op Delete = %d, want 2", c.Len())
	}
}

// TestCtrieCollisionLifecycle exercises the collision node through the
// internal hash-injecting API, including deleting it down to EMPTY (the
// no-contraction husk) and inserting into the husk again.
func TestCtrieCollisionLifecycle(t *testing.T) {
	const h = 0x7777777777777777
	c := NewCtrie()
	for _, k := range []string{"x", "y", "z"} {
		ctrieSet(&c.root, h, k, k+"-val")
	}
	checkCtrieInvariants(t, c.root.main.Load(), 0, 0)
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	for _, k := range []string{"x", "y", "z"} {
		if v, ok := ctrieGet(&c.root, h, k); !ok || v != k+"-val" {
			t.Fatalf("get %q = %q,%v", k, v, ok)
		}
	}
	// Delete every entry: the collision node empties but stays linked.
	for _, k := range []string{"x", "y", "z"} {
		ctrieDelete(&c.root, h, k)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after emptying = %d, want 0", c.Len())
	}
	if _, ok := ctrieGet(&c.root, h, "x"); ok {
		t.Fatal("get from emptied collision node hit")
	}
	// The husk must accept new entries.
	ctrieSet(&c.root, h, "x", "again")
	if v, ok := ctrieGet(&c.root, h, "x"); !ok || v != "again" {
		t.Fatalf("reinsert into husk: %q,%v", v, ok)
	}
	checkCtrieInvariants(t, c.root.main.Load(), 0, 0)
}

// TestCtrieCollisionConcurrent focuses all contention on a single
// collision node: every goroutine writes distinct keys under ONE shared
// synthetic hash, so every CAS targets the same I-node and the collision
// branches' retry paths — which no realistic key set can ever contend
// (mutation-verified: a drop-on-retry bug there survives the rest of the
// suite) — execute thousands of times per run.
func TestCtrieCollisionConcurrent(t *testing.T) {
	const h = 0x1234123412341234
	const goroutines = 8
	const perG = 500
	c := NewCtrie()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ctrieSet(&c.root, h, fmt.Sprintf("g%d-%d", g, i), "v")
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n != goroutines*perG {
		t.Fatalf("after concurrent colliding sets: Len = %d, want %d", n, goroutines*perG)
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perG; i++ {
			k := fmt.Sprintf("g%d-%d", g, i)
			if _, ok := ctrieGet(&c.root, h, k); !ok {
				t.Fatalf("key %q lost", k)
			}
		}
	}

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ctrieDelete(&c.root, h, fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n != 0 {
		t.Fatalf("after concurrent colliding deletes: Len = %d, want 0", n)
	}
}

// TestCtrieConcurrentValues is the value-level linearizability test (same
// design as TestHAMTConcurrentValues): per-key single writers publishing
// increasing versions, concurrent readers asserting legality and
// monotonicity, churn on other keys forcing structural CAS conflicts —
// here spread across many I-nodes rather than one root.
func TestCtrieConcurrentValues(t *testing.T) {
	const writers = 4
	const readers = 4
	const versions = 5_000

	c := NewCtrie()
	keys := make([]string, writers)
	for w := range keys {
		keys[w] = fmt.Sprintf("key-%d", w)
		c.Set(keys[w], fmt.Sprintf("%d:0", w))
	}

	var wg sync.WaitGroup
	churnDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-churnDone:
				return
			default:
			}
			k := fmt.Sprintf("churn-%d", i%64)
			c.Set(k, "x")
			c.Delete(k)
		}
	}()

	var writerWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func(w int) {
			defer writerWG.Done()
			for i := 1; i <= versions; i++ {
				c.Set(keys[w], fmt.Sprintf("%d:%d", w, i))
				// Read-your-own-write: each key has a single writer, so
				// the value must be EXACTLY what was just written. This
				// catches a dropped CAS retry at its first occurrence;
				// the final-value check alone would miss all but the
				// last (mutation-measured: ~5 of 6 runs escape it here,
				// where per-I-node conflicts are rare).
				if v, ok := c.Get(keys[w]); !ok || v != fmt.Sprintf("%d:%d", w, i) {
					t.Errorf("writer %d: wrote version %d, read back %q,%v", w, i, v, ok)
					return
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			last := make([]int, writers)
			for i := 0; i < versions; i++ {
				w := rnd.Intn(writers)
				v, ok := c.Get(keys[w])
				if !ok {
					t.Errorf("Get(%s) missed; owned keys are never deleted", keys[w])
					return
				}
				owner, ver, err := parseVersioned(v)
				if err != nil || owner != w {
					t.Errorf("Get(%s) = %q: not a value written for that key", keys[w], v)
					return
				}
				if ver < last[w] {
					t.Errorf("Get(%s) went backwards: version %d after %d", keys[w], ver, last[w])
					return
				}
				last[w] = ver
			}
		}(nextSeed())
	}

	writerWG.Wait()
	close(churnDone)
	wg.Wait()

	for w := 0; w < writers; w++ {
		want := fmt.Sprintf("%d:%d", w, versions)
		if v, ok := c.Get(keys[w]); !ok || v != want {
			t.Errorf("final Get(%s) = %q,%v; want %q", keys[w], v, ok, want)
		}
	}
	checkCtrie(t, c)
}

// TestCtrieConcurrentCount: quiescent Len must be exact after concurrent
// disjoint-key sets and deletes (Len is a traversal here, so this checks
// the structure rather than a counter).
func TestCtrieConcurrentCount(t *testing.T) {
	const goroutines = 8
	const perG = 2_000
	c := NewCtrie()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				c.Set(fmt.Sprintf("g%d-%d", g, i), "v")
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n != goroutines*perG {
		t.Fatalf("after concurrent sets: Len = %d, want %d", n, goroutines*perG)
	}
	checkCtrie(t, c)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				c.Delete(fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n != 0 {
		t.Fatalf("after concurrent deletes: Len = %d, want 0", n)
	}
}
