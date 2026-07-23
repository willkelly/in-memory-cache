package cache

import (
	"fmt"
	"math/bits"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The generic suite (TestBasic, TestConcurrentSmoke, the benchmarks) already
// drives HAMT through the Cache interface. The tests here reach the parts a
// black-box workload almost never touches — full 64-bit hash collisions, the
// delete-collapse cascade — and check the structural invariants that make
// the persistent trie correct.

// checkHAMTInvariants walks the trie and verifies every structural invariant
// documented on hamtNode. prefix/depth track the hash bits consumed on the
// way down so entry placement can be re-derived and checked; width is the
// chunk width the trie was built with.
func checkHAMTInvariants(t *testing.T, n *hamtNode, width uint, prefix uint64, depth uint, isRoot bool) int {
	t.Helper()
	shift := depth * width

	if n.collision {
		if shift < hashBits {
			t.Fatalf("collision node above the last bitmap level (depth %d)", depth)
		}
		if len(n.entries) < 2 && !isRoot {
			t.Fatalf("collision node with %d entries (want >= 2)", len(n.entries))
		}
		if n.dataMap != 0 || n.nodeMap != 0 || len(n.nodes) != 0 {
			t.Fatal("collision node with bitmap state")
		}
		for i := range n.entries {
			if h := n.entries[i].hash; h != prefix {
				t.Fatalf("collision entry %q has hash %#x, node prefix %#x", n.entries[i].key, h, prefix)
			}
		}
		return len(n.entries)
	}

	if n.dataMap&n.nodeMap != 0 {
		t.Fatalf("dataMap and nodeMap overlap: %#x & %#x", n.dataMap, n.nodeMap)
	}
	if got, want := len(n.entries), bits.OnesCount64(n.dataMap); got != want {
		t.Fatalf("len(entries)=%d, popcount(dataMap)=%d", got, want)
	}
	if got, want := len(n.nodes), bits.OnesCount64(n.nodeMap); got != want {
		t.Fatalf("len(nodes)=%d, popcount(nodeMap)=%d", got, want)
	}
	if !isRoot {
		if len(n.entries)+len(n.nodes) == 0 {
			t.Fatal("empty non-root node")
		}
		// Canonical form: a lone inline entry must have been collapsed into
		// the parent.
		if n.nodeMap == 0 && len(n.entries) == 1 {
			t.Fatal("non-root node holding a single inline entry")
		}
	}

	total := 0
	// Every entry's hash must route to exactly the slot it occupies, on
	// every level down to here.
	di, ni := 0, 0
	for b := uint(0); b < 1<<width; b++ {
		bit := uint64(1) << b
		switch {
		case n.dataMap&bit != 0:
			e := n.entries[di]
			di++
			h := e.hash
			want := prefix | uint64(b)<<shift
			mask := uint64(1)<<(shift+width) - 1
			if shift+width >= 64 {
				mask = ^uint64(0)
			}
			if h&mask != want {
				t.Fatalf("entry %q misplaced: hash %#x, slot prefix %#x (depth %d)", e.key, h, want, depth)
			}
			total++
		case n.nodeMap&bit != 0:
			child := n.nodes[ni]
			ni++
			total += checkHAMTInvariants(t, child, width, prefix|uint64(b)<<shift, depth+1, false)
		}
	}
	return total
}

// checkHAMT verifies the invariants and that the trie size matches Len.
// It also verifies every stored hash is really the key's fnv1a hash (the
// walker itself can't, because collision tests feed it synthetic hashes).
func checkHAMT(t *testing.T, c *HAMT) {
	t.Helper()
	root := c.root.Load()
	if n := checkHAMTInvariants(t, root.node, hamtBits, 0, 0, true); n != root.count {
		t.Fatalf("trie holds %d entries, count says %d", n, root.count)
	}
	var walk func(n *hamtNode)
	walk = func(n *hamtNode) {
		for i := range n.entries {
			if e := &n.entries[i]; e.hash != fnv1a(e.key) {
				t.Fatalf("entry %q stores hash %#x, fnv1a says %#x", e.key, e.hash, fnv1a(e.key))
			}
		}
		for _, child := range n.nodes {
			walk(child)
		}
	}
	walk(root.node)
}

// TestHAMTCollision forces full 64-bit hash collisions (impossible to reach
// through Get/Set with realistic keys) by calling the internal insert/delete
// with identical synthetic hashes, and checks the whole lifecycle: the
// collision node forms at the bottom of the trie, lookups scan it, deleting
// down to one entry collapses the entire single-child chain back to an
// inline entry at the root.
func TestHAMTCollision(t *testing.T) {
	const h = 0xdeadbeefcafef00d

	n := hamtEmptyNode
	for _, k := range []string{"a", "b", "c"} {
		var delta int
		n, delta = hamtInsert(n, h, hamtBits, 0, k, k+"-val")
		if delta != 1 {
			t.Fatalf("insert %q: delta = %d, want 1", k, delta)
		}
	}

	// The three entries must share one collision node at the bottom of an
	// 11-level single-child chain.
	depth := 0
	for cur := n; ; depth++ {
		if cur.collision {
			if len(cur.entries) != 3 {
				t.Fatalf("collision node has %d entries, want 3", len(cur.entries))
			}
			break
		}
		if len(cur.nodes) != 1 || len(cur.entries) != 0 {
			t.Fatalf("depth %d: chain node has %d nodes, %d entries", depth, len(cur.nodes), len(cur.entries))
		}
		cur = cur.nodes[0]
	}
	if want := int(hashBits+hamtBits-1) / hamtBits; depth != want {
		t.Fatalf("collision node at depth %d, want %d", depth, want)
	}
	for _, k := range []string{"a", "b", "c"} {
		if v, ok := hamtLookup(n, h, hamtBits, k); !ok || v != k+"-val" {
			t.Fatalf("lookup %q = %q,%v", k, v, ok)
		}
	}

	// Replacing inside the collision node must not grow it.
	n2, delta := hamtInsert(n, h, hamtBits, 0, "b", "b-new")
	if delta != 0 {
		t.Fatalf("replace in collision node: delta = %d, want 0", delta)
	}
	if v, _ := hamtLookup(n2, h, hamtBits, "b"); v != "b-new" {
		t.Fatalf("after replace: b = %q", v)
	}
	if v, _ := hamtLookup(n, h, hamtBits, "b"); v != "b-val" {
		t.Fatalf("persistence violated: old snapshot's b = %q", v)
	}

	// A miss inside the collision node.
	if _, ok := hamtLookup(n, h, hamtBits, "zzz"); ok {
		t.Fatal("lookup of absent key in collision node succeeded")
	}
	if _, found := hamtDelete(n, h, hamtBits, 0, "zzz"); found {
		t.Fatal("delete of absent key in collision node reported found")
	}

	// Delete down to one entry: the chain must collapse to a root holding a
	// single inline entry.
	n3, found := hamtDelete(n, h, hamtBits, 0, "a")
	if !found {
		t.Fatal("delete a: not found")
	}
	n3, found = hamtDelete(n3, h, hamtBits, 0, "c")
	if !found {
		t.Fatal("delete c: not found")
	}
	if n3.collision || n3.nodeMap != 0 || len(n3.entries) != 1 {
		t.Fatalf("chain did not collapse: %+v", n3)
	}
	if v, ok := hamtLookup(n3, h, hamtBits, "b"); !ok || v != "b-val" {
		t.Fatalf("after collapse: b = %q,%v", v, ok)
	}

	// Deleting the last entry empties the trie.
	n4, found := hamtDelete(n3, h, hamtBits, 0, "b")
	if !found || n4 != nil {
		t.Fatalf("deleting last entry: node=%v found=%v, want nil,true", n4, found)
	}
}

// TestHAMTCollisionPublicAPI drives collision-node paths through the
// PRODUCTION Get/Set/Delete, not the internal helpers. The trick: seed the
// trie (via internal inserts) with decoy entries whose synthetic hash is
// fnv1a("a") — the real hash of a real key. Production operations on "a"
// then descend the exact path to the collision node. Without this test a
// bug confined to Get's own collision scan is invisible: no realistic key
// set produces full 64-bit fnv1a collisions, so the branch is otherwise
// unreachable (mutation-verified — a planted wrong-value bug there passed
// the entire suite).
func TestHAMTCollisionPublicAPI(t *testing.T) {
	h := fnv1a("a")
	n, _ := hamtInsert(hamtEmptyNode, h, hamtBits, 0, "decoy1", "d1")
	n, _ = hamtInsert(n, h, hamtBits, 0, "decoy2", "d2")
	n, _ = hamtInsert(n, h, hamtBits, 0, "a", "a-val")
	c := NewHAMT()
	c.root.Store(&hamtRoot{node: n, count: 3})

	// Production Get: hit inside the collision node's scan.
	if v, ok := c.Get("a"); !ok || v != "a-val" {
		t.Fatalf("Get(a) = %q,%v; want a-val,true", v, ok)
	}
	// Production Set: replace inside the collision node.
	c.Set("a", "a-new")
	if v, ok := c.Get("a"); !ok || v != "a-new" {
		t.Fatalf("Get(a) after Set = %q,%v; want a-new,true", v, ok)
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	// Production Delete: remove from the collision node. Two decoys stay
	// behind, so "a"'s path still ends at the collision node...
	c.Delete("a")
	if c.Len() != 2 {
		t.Fatalf("Len after Delete = %d, want 2", c.Len())
	}
	// ...which makes this Get a MISS decided inside the collision scan.
	if v, ok := c.Get("a"); ok {
		t.Fatalf("Get(a) after Delete = %q,%v; want miss", v, ok)
	}
	// And a no-op production Delete that walks the collision node.
	c.Delete("a")
	if c.Len() != 2 {
		t.Fatalf("Len after no-op Delete = %d, want 2", c.Len())
	}
}

// TestHAMTPartialCollision exercises hashes that agree on some leading
// chunks and then diverge: the merge must descend exactly to the first
// differing chunk and split there. The hashes are built from hamtBits so
// the test holds at any production width.
func TestHAMTPartialCollision(t *testing.T) {
	// Agree on chunks 0..4 (both zero), differ at chunk 5.
	const h1 = uint64(0)
	const h2 = uint64(1) << (5 * hamtBits)

	n, _ := hamtInsert(hamtEmptyNode, h1, hamtBits, 0, "one", "1")
	n, _ = hamtInsert(n, h2, hamtBits, 0, "two", "2")

	cur := n
	for depth := 0; depth < 5; depth++ {
		if len(cur.nodes) != 1 || len(cur.entries) != 0 {
			t.Fatalf("depth %d: want pure chain node, got %d nodes / %d entries", depth, len(cur.nodes), len(cur.entries))
		}
		cur = cur.nodes[0]
	}
	if len(cur.entries) != 2 || cur.nodeMap != 0 {
		t.Fatalf("split node: want 2 inline entries, got %+v", cur)
	}
	if v, ok := hamtLookup(n, h1, hamtBits, "one"); !ok || v != "1" {
		t.Fatalf("one = %q,%v", v, ok)
	}
	if v, ok := hamtLookup(n, h2, hamtBits, "two"); !ok || v != "2" {
		t.Fatalf("two = %q,%v", v, ok)
	}

	// Deleting one side must collapse the chain: "two" ends up inline at
	// the root.
	n2, found := hamtDelete(n, h1, hamtBits, 0, "one")
	if !found {
		t.Fatal("delete one: not found")
	}
	if n2.nodeMap != 0 || len(n2.entries) != 1 || n2.entries[0].key != "two" {
		t.Fatalf("chain did not collapse after delete: %+v", n2)
	}
}

// TestHAMTDifferential runs a long random op sequence against a plain map
// and checks full agreement (including Len) throughout, plus the structural
// invariants at every checkpoint. This is the broad-coverage test for the
// path-copy logic: with 512 keys and 20k ops it exercises inserts, replaces,
// deletes, node splits, and collapse cascades in every trie region.
func TestHAMTDifferential(t *testing.T) {
	c := NewHAMT()
	ref := make(map[string]string)
	keys := makeKeys(512, 4)
	r := rand.New(rand.NewSource(42))

	for op := 0; op < 20_000; op++ {
		k := keys[r.Intn(len(keys))]
		switch r.Intn(4) {
		case 0, 1: // Set twice as often as Delete so the trie grows
			v := fmt.Sprintf("v%d", op)
			c.Set(k, v)
			ref[k] = v
		case 2:
			c.Delete(k)
			delete(ref, k)
		case 3:
			want, wantOK := ref[k]
			if got, ok := c.Get(k); ok != wantOK || got != want {
				t.Fatalf("op %d: Get(%q) = %q,%v; want %q,%v", op, k, got, ok, want, wantOK)
			}
		}
		if c.Len() != len(ref) {
			t.Fatalf("op %d: Len = %d, want %d", op, c.Len(), len(ref))
		}
		if op%1000 == 0 {
			checkHAMT(t, c)
		}
	}
	checkHAMT(t, c)
	for k, want := range ref {
		if got, ok := c.Get(k); !ok || got != want {
			t.Fatalf("final: Get(%q) = %q,%v; want %q,true", k, got, ok, want)
		}
	}
}

// TestHAMTLoad checks the BulkLoader fast path builds the same structure the
// persistent inserts would (same invariants, same contents).
func TestHAMTLoad(t *testing.T) {
	keys := makeKeys(10_000, 8)
	items := make(map[string]string, len(keys))
	for i, k := range keys {
		items[k] = fmt.Sprintf("v%d", i)
	}
	c := NewHAMT()
	c.Load(items)

	if c.Len() != len(items) {
		t.Fatalf("Len = %d, want %d", c.Len(), len(items))
	}
	checkHAMT(t, c)
	for i, k := range keys {
		if v, ok := c.Get(k); !ok || v != fmt.Sprintf("v%d", i) {
			t.Fatalf("Get(%q) = %q,%v", k, v, ok)
		}
	}

	// Loading an empty map must yield a working empty cache.
	c2 := NewHAMT()
	c2.Load(map[string]string{})
	if c2.Len() != 0 {
		t.Fatalf("empty Load: Len = %d", c2.Len())
	}
	c2.Set("x", "1")
	if v, ok := c2.Get("x"); !ok || v != "1" {
		t.Fatalf("Set after empty Load: %q,%v", v, ok)
	}
}

// TestHAMTConcurrentCount is the linearizability check for the count-in-root
// design: the delta computed against a snapshot is only published if the CAS
// against that same snapshot wins, so the count can never drift — not under
// racing distinct keys, and not under racing Set/Delete of the same key.
func TestHAMTConcurrentCount(t *testing.T) {
	const goroutines = 8
	const perG = 2_000

	// Disjoint key ranges: after all sets, exactly goroutines*perG entries;
	// after all deletes, exactly zero.
	c := NewHAMT()
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
	checkHAMT(t, c)

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

	// Everyone hammers the SAME small key set with mixed Set/Delete. The
	// invariant: count always equals the number of keys present, so at the
	// end Len must match a sequential recount via Get.
	keys := makeKeys(16, 4)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < perG; i++ {
				k := keys[r.Intn(len(keys))]
				if r.Intn(2) == 0 {
					c.Set(k, "v")
				} else {
					c.Delete(k)
				}
			}
		}(nextSeed())
	}
	wg.Wait()
	live := 0
	for _, k := range keys {
		if _, ok := c.Get(k); ok {
			live++
		}
	}
	if n := c.Len(); n != live {
		t.Fatalf("count drifted: Len = %d, but %d keys are present", n, live)
	}
	checkHAMT(t, c)
}

// TestHAMTConcurrentValues is the value-level linearizability test the
// count/structure checks cannot substitute for (mutation-verified: a
// lost-update bug that drops a value replacement on CAS failure keeps
// every published root self-consistent and passes every other test).
//
// Each writer owns one key and publishes strictly increasing versions, so
// the per-key value history is fully ordered. Concurrent readers then
// assert the two properties that order implies: every observed value is
// well-formed for the key it was read from (no cross-key leakage), and a
// reader never sees a version go backwards (no stale snapshot revival).
// A churn goroutine inserts and deletes filler keys so writers lose CAS
// races against structure-changing (delta != 0) conflicts too, not just
// against each other's replacements.
func TestHAMTConcurrentValues(t *testing.T) {
	const writers = 4
	const readers = 4
	const versions = 5_000

	c := NewHAMT()
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
				// Read-your-own-write: single writer per key, so the
				// value must be exactly what was just written — catches
				// a dropped CAS retry at its first occurrence rather
				// than only when the LAST write is the dropped one.
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

	// Last write per key must have won.
	for w := 0; w < writers; w++ {
		want := fmt.Sprintf("%d:%d", w, versions)
		if v, ok := c.Get(keys[w]); !ok || v != want {
			t.Errorf("final Get(%s) = %q,%v; want %q", keys[w], v, ok, want)
		}
	}
	checkHAMT(t, c)
}

func parseVersioned(v string) (owner, version int, err error) {
	i := strings.IndexByte(v, ':')
	if i < 0 {
		return 0, 0, fmt.Errorf("no separator in %q", v)
	}
	if owner, err = strconv.Atoi(v[:i]); err != nil {
		return 0, 0, err
	}
	version, err = strconv.Atoi(v[i+1:])
	return owner, version, err
}

// TestHAMTMutCollision drives the transient builder (hamtInsertMut) into
// the branches a real Load can never reach with realistic keys: the
// collision-node append (needs a third key on one full 64-bit hash) and
// both same-key replace paths (unreachable from Load at all, since map
// keys are unique). Mirrors TestHAMTCollision, which covers the same
// branches of the persistent hamtInsert.
func TestHAMTMutCollision(t *testing.T) {
	const h = 0x0123456789abcdef
	root := &hamtNode{}
	hamtInsertMut(root, h, hamtBits, 0, "a", "1")
	hamtInsertMut(root, h, hamtBits, 0, "b", "2")  // push-down: merge builds the collision node
	hamtInsertMut(root, h, hamtBits, 0, "c", "3")  // collision append path
	hamtInsertMut(root, h, hamtBits, 0, "b", "2x") // replace inside the collision node

	// A second slot at the root, then replace it: the bitmap-level
	// same-key branch. h2 differs from h in the first chunk only.
	const h2 = h ^ 1
	hamtInsertMut(root, h2, hamtBits, 0, "d", "4")
	hamtInsertMut(root, h2, hamtBits, 0, "d", "4x")

	if n := checkHAMTInvariants(t, root, hamtBits, 0, 0, true); n != 4 {
		t.Fatalf("trie holds %d entries, want 4", n)
	}
	for k, want := range map[string]string{"a": "1", "b": "2x", "c": "3"} {
		if v, ok := hamtLookup(root, h, hamtBits, k); !ok || v != want {
			t.Fatalf("lookup %q = %q,%v; want %q,true", k, v, ok, want)
		}
	}
	if v, ok := hamtLookup(root, h2, hamtBits, "d"); !ok || v != "4x" {
		t.Fatalf("lookup d = %q,%v; want 4x,true", v, ok)
	}
	if _, ok := hamtLookup(root, h, hamtBits, "zzz"); ok {
		t.Fatal("lookup of absent key in collision node succeeded")
	}
}

// hamtW is the HAMT with a runtime-set chunk width, mirroring shardedN:
// the production HAMT pins the width at compile time (so Get's masks
// const-fold), while a width-sensitivity sweep needs it as data. It reuses
// the production write-side internals verbatim — they already take the
// width as a parameter — and hamtLookup for reads.
type hamtW struct {
	bits uint
	root atomic.Pointer[hamtRoot]
}

func newHAMTW(bits uint) *hamtW {
	c := &hamtW{bits: bits}
	c.root.Store(&hamtRoot{node: hamtEmptyNode})
	return c
}

func (c *hamtW) get(key string) (string, bool) {
	return hamtLookup(c.root.Load().node, fnv1a(key), c.bits, key)
}

func (c *hamtW) set(key, value string) {
	hash := fnv1a(key)
	for {
		old := c.root.Load()
		node, delta := hamtInsert(old.node, hash, c.bits, 0, key, value)
		if c.root.CompareAndSwap(old, &hamtRoot{node: node, count: old.count + delta}) {
			return
		}
	}
}

func (c *hamtW) delete(key string) {
	hash := fnv1a(key)
	for {
		old := c.root.Load()
		node, found := hamtDelete(old.node, hash, c.bits, 0, key)
		if !found {
			return
		}
		if node == nil {
			node = hamtEmptyNode
		}
		if c.root.CompareAndSwap(old, &hamtRoot{node: node, count: old.count - 1}) {
			return
		}
	}
}

func (c *hamtW) load(items map[string]string) {
	root := &hamtNode{}
	for k, v := range items {
		hamtInsertMut(root, fnv1a(k), c.bits, 0, k, v)
	}
	if len(items) == 0 {
		root = hamtEmptyNode
	}
	c.root.Store(&hamtRoot{node: root, count: len(items)})
}

// TestHAMTWidths runs the differential test and a synthetic-collision
// lifecycle at several chunk widths, so the width parameter threaded
// through the internals is exercised at its boundary geometries (partial
// last-level chunks, different collision depths) and not just at the
// production width.
func TestHAMTWidths(t *testing.T) {
	for _, w := range []uint{3, 4, 5, 6} {
		t.Run(fmt.Sprintf("bits=%d", w), func(t *testing.T) {
			c := newHAMTW(w)
			ref := make(map[string]string)
			keys := makeKeys(256, 4)
			r := rand.New(rand.NewSource(int64(w)))
			for op := 0; op < 6_000; op++ {
				k := keys[r.Intn(len(keys))]
				switch r.Intn(4) {
				case 0, 1:
					v := fmt.Sprintf("v%d", op)
					c.set(k, v)
					ref[k] = v
				case 2:
					c.delete(k)
					delete(ref, k)
				default:
					want, wantOK := ref[k]
					if got, ok := c.get(k); ok != wantOK || got != want {
						t.Fatalf("op %d: get(%q) = %q,%v; want %q,%v", op, k, got, ok, want, wantOK)
					}
				}
				if got := c.root.Load().count; got != len(ref) {
					t.Fatalf("op %d: count = %d, want %d", op, got, len(ref))
				}
				if op%500 == 0 {
					root := c.root.Load()
					if n := checkHAMTInvariants(t, root.node, w, 0, 0, true); n != len(ref) {
						t.Fatalf("op %d: trie holds %d entries, count says %d", op, n, len(ref))
					}
				}
			}

			// The transient builder at this width: BenchmarkHAMTWidth
			// prefills through load -> hamtInsertMut, so its width
			// threading needs the same verification as the persistent
			// path (a hamtBits leftover in any of its three width sites
			// would corrupt exactly the swept widths and nothing a
			// width-6 test can see).
			loaded := newHAMTW(w)
			items := make(map[string]string, len(keys))
			for i, k := range keys {
				items[k] = fmt.Sprintf("l%d", i)
			}
			loaded.load(items)
			root := loaded.root.Load()
			if got := checkHAMTInvariants(t, root.node, w, 0, 0, true); got != len(items) {
				t.Fatalf("load built %d entries, want %d", got, len(items))
			}
			if root.count != len(items) {
				t.Fatalf("load count = %d, want %d", root.count, len(items))
			}
			for i, k := range keys {
				if v, ok := loaded.get(k); !ok || v != fmt.Sprintf("l%d", i) {
					t.Fatalf("width %d: get(%q) = %q,%v after load", w, k, v, ok)
				}
			}

			// Full-hash collision lifecycle at this width: build, replace,
			// miss, then delete down to the collapse cascade.
			const ch = 0x5a5a5a5a5a5a5a5a
			n, _ := hamtInsert(hamtEmptyNode, ch, w, 0, "x", "1")
			n, _ = hamtInsert(n, ch, w, 0, "y", "2")
			n, _ = hamtInsert(n, ch, w, 0, "z", "3")
			checkHAMTInvariants(t, n, w, 0, 0, true)
			for k, want := range map[string]string{"x": "1", "y": "2", "z": "3"} {
				if v, ok := hamtLookup(n, ch, w, k); !ok || v != want {
					t.Fatalf("collision lookup %q = %q,%v; want %q,true", k, v, ok, want)
				}
			}
			n, found := hamtDelete(n, ch, w, 0, "y")
			if !found {
				t.Fatal("delete y: not found")
			}
			n, found = hamtDelete(n, ch, w, 0, "z")
			if !found {
				t.Fatal("delete z: not found")
			}
			if n.collision || n.nodeMap != 0 || len(n.entries) != 1 {
				t.Fatalf("chain did not collapse at width %d: %+v", w, n)
			}
			if v, ok := hamtLookup(n, ch, w, "x"); !ok || v != "1" {
				t.Fatalf("after collapse: x = %q,%v", v, ok)
			}
		})
	}
}

// BenchmarkHAMTWidth sweeps the trie's branching factor to answer "would
// Clojure's 32-way (or narrower) be faster?" — wider nodes mean a
// shallower trie (fewer hops per read, fewer nodes copied per write) but
// each copied node is bigger and the CAS window longer. Uniform keys;
// run at a high core count to include the retry effects:
//
//	go test -bench=BenchmarkHAMTWidth -cpu=8 -count=3 -keys=1000000
//
// Reads here go through the width-parameterized lookup rather than the
// const-folded production Get, which costs a nanosecond or two uniformly
// across widths — fine for comparing widths against each other, not for
// comparing against BenchmarkCache's hamt numbers.
func BenchmarkHAMTWidth(b *testing.B) {
	n := *numKeysFlag
	keys := makeKeys(n, *keyLenFlag)
	val := makeValue(benchValueBytes)
	items := make(map[string]string, n)
	for _, k := range keys {
		items[k] = val
	}

	for _, w := range []uint{3, 4, 5, 6} {
		c := newHAMTW(w)
		c.load(items)
		for _, mx := range mixes {
			b.Run(fmt.Sprintf("width=%d/mix=%s", 1<<w, mx.name), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					r := rand.New(rand.NewSource(nextSeed()))
					for pb.Next() {
						k := keys[r.Intn(n)]
						if r.Float64() < mx.readFrac {
							c.get(k)
						} else {
							c.set(k, val)
						}
					}
				})
			})
		}
	}
}

// TestShardedHAMT: the generic suite already drives hamt256 through the
// Cache interface; this adds the sharded-specific checks — per-shard
// structural invariants, count consistency under concurrency, and that
// shard routing actually distributes (top-byte routing over fnv1a).
func TestShardedHAMT(t *testing.T) {
	c := NewShardedHAMT()
	ref := make(map[string]string)
	keys := makeKeys(2_000, 6)
	r := rand.New(rand.NewSource(7))
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
	}

	// Per-shard invariants + occupancy: with 2000 keys over 256 shards the
	// routing must touch a healthy majority of shards (a top-bits routing
	// bug — e.g. shifting by the wrong amount — would pile keys into few).
	total, occupied := 0, 0
	for i := range c.shards {
		root := c.shards[i].root.Load()
		got := checkHAMTInvariants(t, root.node, hamt256Bits, 0, 0, true)
		if got != root.count {
			t.Fatalf("shard %d: trie holds %d entries, count says %d", i, got, root.count)
		}
		total += got
		if got > 0 {
			occupied++
		}
	}
	if total != len(ref) {
		t.Fatalf("shards hold %d entries, want %d", total, len(ref))
	}
	if occupied < 200 {
		t.Fatalf("only %d/256 shards occupied with %d keys: routing is skewed", occupied, len(ref))
	}
}

// TestShardedHAMTLoad covers the bulk-load path end to end: every loaded
// key must be reachable through Get, which pins Load's routing to
// shardFor's (mutation-verified: a routing divergence between the two
// passes every other test — Len still adds up because each side is
// internally consistent — while ~255/256 of loaded keys silently vanish
// from Get's view).
func TestShardedHAMTLoad(t *testing.T) {
	keys := makeKeys(10_000, 8)
	items := make(map[string]string, len(keys))
	for i, k := range keys {
		items[k] = fmt.Sprintf("v%d", i)
	}
	c := NewShardedHAMT()
	c.Load(items)

	if c.Len() != len(items) {
		t.Fatalf("Len = %d, want %d", c.Len(), len(items))
	}
	for i, k := range keys {
		if v, ok := c.Get(k); !ok || v != fmt.Sprintf("v%d", i) {
			t.Fatalf("Get(%q) = %q,%v after Load", k, v, ok)
		}
	}
	if _, ok := c.Get("not-loaded"); ok {
		t.Fatal("Get of absent key hit after Load")
	}

	// Trie-shape health, the check the occupancy count can't do: routing
	// by the hash's LOW bits also spreads keys evenly across shards, but
	// it fixes every key's first trie chunk within a shard, degenerating
	// each root into a single-child chain. With sound routing, a root
	// holding >= 8 keys picks >= 2 distinct first chunks (the chance of 8
	// uniform keys sharing one 5-bit chunk is 32^-7).
	for i := range c.shards {
		root := c.shards[i].root.Load()
		if got := checkHAMTInvariants(t, root.node, hamt256Bits, 0, 0, true); got != root.count {
			t.Fatalf("shard %d: trie holds %d entries, count says %d", i, got, root.count)
		}
		if root.count >= 8 && len(root.node.entries)+len(root.node.nodes) < 2 {
			t.Fatalf("shard %d: %d keys but a single-slot root — routing correlates with trie chunks", i, root.count)
		}
	}

	// The all-shards-empty branch: loading an empty map must still leave
	// every shard usable.
	c2 := NewShardedHAMT()
	c2.Load(map[string]string{})
	if c2.Len() != 0 {
		t.Fatalf("empty Load: Len = %d", c2.Len())
	}
	c2.Set("x", "1")
	if v, ok := c2.Get("x"); !ok || v != "1" {
		t.Fatalf("Set after empty Load: %q,%v", v, ok)
	}
}

// TestShardedHAMTConcurrentCount mirrors TestHAMTConcurrentCount for the
// sharded variant: per-shard counts published by CAS must never drift.
func TestShardedHAMTConcurrentCount(t *testing.T) {
	const goroutines = 8
	const perG = 2_000
	c := NewShardedHAMT()
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

// TestHAMTDeleteDefensive covers hamtDelete's defensive paths — a child
// collapsing to nil mid-recursion. Canonical published tries never contain
// the inputs that trigger them (1-entry collision nodes and 1-entry bitmap
// children are inlined into their parents on the way up), but hamtDelete
// handles them anyway so a non-canonical input degrades gracefully instead
// of corrupting the trie.
func TestHAMTDeleteDefensive(t *testing.T) {
	const h = 0xabc

	// Deleting the only entry of a collision node empties it.
	leaf := func() *hamtNode {
		return &hamtNode{collision: true, entries: []hamtEntry{{h, "a", "1"}}}
	}
	if n, found := hamtDelete(leaf(), h, hamtBits, 0, "a"); !found || n != nil {
		t.Fatalf("delete from 1-entry collision node = %+v,%v; want nil,true", n, found)
	}

	// A parent whose only child empties becomes empty itself...
	bit := uint64(1) << (h & hamtMask)
	parent := &hamtNode{nodeMap: bit, nodes: []*hamtNode{leaf()}}
	if n, found := hamtDelete(parent, h, hamtBits, 0, "a"); !found || n != nil {
		t.Fatalf("parent of emptied only child = %+v,%v; want nil,true", n, found)
	}

	// ...unless it holds something else, in which case only the child goes.
	sibling := hamtEntry{hash: (h &^ uint64(hamtMask)) | ((h + 1) & hamtMask), key: "z", value: "9"}
	parent2 := &hamtNode{
		dataMap: bit << 1, nodeMap: bit,
		entries: []hamtEntry{sibling},
		nodes:   []*hamtNode{leaf()},
	}
	n, found := hamtDelete(parent2, h, hamtBits, 0, "a")
	if !found || n == nil || n.nodeMap != 0 || len(n.entries) != 1 || n.entries[0].key != "z" {
		t.Fatalf("parent keeping sibling = %+v,%v; want just z", n, found)
	}
}

// TestHAMTSnapshotIsolation pins down the persistence property the whole
// design rests on: a reader holding an old root sees that snapshot forever,
// unaffected by later writes.
func TestHAMTSnapshotIsolation(t *testing.T) {
	c := NewHAMT()
	c.Set("a", "1")
	c.Set("b", "2")

	snap := c.root.Load()

	c.Set("a", "changed")
	c.Delete("b")
	for i := 0; i < 1000; i++ {
		c.Set(fmt.Sprintf("filler%d", i), "x")
	}

	if v, ok := hamtLookup(snap.node, fnv1a("a"), hamtBits, "a"); !ok || v != "1" {
		t.Fatalf("snapshot a = %q,%v; want 1,true", v, ok)
	}
	if v, ok := hamtLookup(snap.node, fnv1a("b"), hamtBits, "b"); !ok || v != "2" {
		t.Fatalf("snapshot b = %q,%v; want 2,true", v, ok)
	}
	if snap.count != 2 {
		t.Fatalf("snapshot count = %d, want 2", snap.count)
	}
}
