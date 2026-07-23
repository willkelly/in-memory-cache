package cache

import (
	"fmt"
	"math/bits"
	"math/rand"
	"strconv"
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
			child := frozenRead(cn.inodes[ni])
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
	root := frozenRead(c.rdcssReadRoot().in)
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
			walk(frozenRead(in))
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
	ctrieSet(c, h, "decoy1", "d1")
	ctrieSet(c, h, "decoy2", "d2")
	ctrieSet(c, h, "a", "a-val")

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
		ctrieSet(c, h, k, k+"-val")
	}
	checkCtrieInvariants(t, frozenRead(c.rdcssReadRoot().in), 0, 0)
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	for _, k := range []string{"x", "y", "z"} {
		if v, ok := ctrieGet(c, h, k); !ok || v != k+"-val" {
			t.Fatalf("get %q = %q,%v", k, v, ok)
		}
	}
	// Delete every entry: the collision node empties but stays linked.
	for _, k := range []string{"x", "y", "z"} {
		ctrieDelete(c, h, k)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after emptying = %d, want 0", c.Len())
	}
	if _, ok := ctrieGet(c, h, "x"); ok {
		t.Fatal("get from emptied collision node hit")
	}
	// The husk must accept new entries.
	ctrieSet(c, h, "x", "again")
	if v, ok := ctrieGet(c, h, "x"); !ok || v != "again" {
		t.Fatalf("reinsert into husk: %q,%v", v, ok)
	}
	checkCtrieInvariants(t, frozenRead(c.rdcssReadRoot().in), 0, 0)
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
				ctrieSet(c, h, fmt.Sprintf("g%d-%d", g, i), "v")
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
			if _, ok := ctrieGet(c, h, k); !ok {
				t.Fatalf("key %q lost", k)
			}
		}
	}

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ctrieDelete(c, h, fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n != 0 {
		t.Fatalf("after concurrent colliding deletes: Len = %d, want 0", n)
	}
}

// TestCtrieSnapshot pins the snapshot contract single-threaded: O(1)
// freeze, complete isolation from later writes (including deletes and
// new keys), exact Len, and the live trie continuing correctly through
// the lazy renewals the snapshot forces.
func TestCtrieSnapshot(t *testing.T) {
	const n = 1000
	c := NewCtrie()
	for i := 0; i < n; i++ {
		c.Set(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	snap := c.Snapshot()
	if got := snap.Len(); got != n {
		t.Fatalf("snapshot Len = %d, want %d", got, n)
	}

	// Hammer the live trie: overwrite half, delete half, add new keys.
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("k%d", i)
		if i%2 == 0 {
			c.Set(k, "changed")
		} else {
			c.Delete(k)
		}
	}
	for i := 0; i < 500; i++ {
		c.Set(fmt.Sprintf("new%d", i), "x")
	}

	// The snapshot is completely unmoved.
	if got := snap.Len(); got != n {
		t.Fatalf("snapshot Len after live churn = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("k%d", i)
		if v, ok := snap.Get(k); !ok || v != fmt.Sprintf("v%d", i) {
			t.Fatalf("snapshot Get(%q) = %q,%v; want v%d,true", k, v, ok, i)
		}
	}
	if _, ok := snap.Get("new0"); ok {
		t.Fatal("snapshot sees a key created after the freeze")
	}

	// The live trie is right, through all the renewals.
	if got, want := c.Len(), n/2+500; got != want {
		t.Fatalf("live Len = %d, want %d", got, want)
	}
	if v, ok := c.Get("k0"); !ok || v != "changed" {
		t.Fatalf("live Get(k0) = %q,%v", v, ok)
	}
	if _, ok := c.Get("k1"); ok {
		t.Fatal("live Get(k1) survived delete")
	}
	checkCtrie(t, c)

	// A second snapshot sees the new world; the first still the old.
	snap2 := c.Snapshot()
	if v, ok := snap2.Get("new0"); !ok || v != "x" {
		t.Fatalf("snap2 Get(new0) = %q,%v", v, ok)
	}
	if v, ok := snap.Get("k0"); !ok || v != "v0" {
		t.Fatalf("snap1 disturbed by snap2: k0 = %q,%v", v, ok)
	}
}

// TestCtrieSnapshotConcurrent takes snapshots in the middle of write
// traffic and checks the two properties that define them: each snapshot
// is internally frozen (re-reads agree), and successive snapshots move
// forward in time (per-key versions never regress across snapshots,
// since each key has a single ordered writer). The snapshot loop runs
// until the writers FINISH, so overlap is guaranteed rather than raced
// for, and the final assertions (the loop's last[] must have reached
// every writer's final version through the monotonic checks) make the
// assertions provably non-vacuous — a review found an earlier version
// often completed its fixed-count loop before any writer published.
func TestCtrieSnapshotConcurrent(t *testing.T) {
	const writers = 4
	const versions = 3_000

	c := NewCtrie()
	keys := make([]string, writers)
	for w := range keys {
		keys[w] = fmt.Sprintf("key-%d", w)
		c.Set(keys[w], "0")
	}

	var writerWG sync.WaitGroup
	var churnWG sync.WaitGroup
	churnDone := make(chan struct{})
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
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
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func(w int) {
			defer writerWG.Done()
			for i := 1; i <= versions; i++ {
				c.Set(keys[w], fmt.Sprintf("%d", i))
			}
		}(w)
	}
	writersDone := make(chan struct{})
	go func() { writerWG.Wait(); close(writersDone) }()

	last := make([]int, writers)
	snapshots := 0
	for done := false; !done; {
		select {
		case <-writersDone:
			done = true // one final snapshot below observes the end state
		default:
		}
		snap := c.Snapshot()
		snapshots++
		for w := range keys {
			v1, ok1 := snap.Get(keys[w])
			v2, ok2 := snap.Get(keys[w])
			if !ok1 || !ok2 || v1 != v2 {
				t.Fatalf("snapshot %d not frozen: %q,%v then %q,%v", snapshots, v1, ok1, v2, ok2)
			}
			ver, err := strconv.Atoi(v1)
			if err != nil {
				t.Fatalf("snapshot %d: bad value %q", snapshots, v1)
			}
			if ver < last[w] {
				t.Fatalf("snapshot %d went backwards on %s: %d after %d", snapshots, keys[w], ver, last[w])
			}
			last[w] = ver
		}
		if l1, l2 := snap.Len(), snap.Len(); l1 != l2 {
			t.Fatalf("snapshot %d Len not frozen: %d then %d", snapshots, l1, l2)
		}
	}

	close(churnDone)
	churnWG.Wait()

	// Non-vacuity: the monotonic chain must have carried every key all the
	// way to its final version.
	for w := range keys {
		if last[w] != versions {
			t.Fatalf("snapshot chain ended at version %d for %s, want %d (%d snapshots)",
				last[w], keys[w], versions, snapshots)
		}
	}
	checkCtrie(t, c)
}

// TestCtrieSnapshotLiveOrder pins the cross-API ordering that the review
// found broken twice: a snapshot taken at time T and a LIVE Get issued
// after T must agree on where every write falls. With one writer bumping
// integer versions, snapshot-version <= subsequent-live-version must
// hold; the passive-live-read bug inverted it (the snapshot contained a
// late-committing write that a later live Get had not yet observed),
// tripping a checker like this one within milliseconds.
func TestCtrieSnapshotLiveOrder(t *testing.T) {
	c := NewCtrie()
	// Prefill so the written keys sit behind level-1 I-nodes (the root
	// I-node is guarded by the RDCSS condition itself; depth is where the
	// bug lived).
	for _, k := range makeKeys(128, 4) {
		c.Set(k, "x")
	}
	const key = "hot"
	c.Set(key, "0")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.Set(key, strconv.Itoa(i))
		}
	}()

	for i := 0; i < 30_000; i++ {
		snap := c.Snapshot()
		sv, ok1 := snap.Get(key)
		lv, ok2 := c.Get(key)
		if !ok1 || !ok2 {
			t.Fatal("hot key missing")
		}
		s, _ := strconv.Atoi(sv)
		l, _ := strconv.Atoi(lv)
		if s > l {
			t.Fatalf("iteration %d: snapshot has version %d, later live read got %d", i, s, l)
		}
	}
	close(stop)
	wg.Wait()
	checkCtrie(t, c)
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
