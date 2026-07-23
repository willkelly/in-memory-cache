package cache

import (
	"math/bits"
	"sync/atomic"
)

// HAMT answers cow's obvious follow-up question: if copy-on-write dies
// because every Set copies the whole map, what if a write only copied the
// part of the structure it actually touches?
//
// The read path is identical to cow's — one atomic load of an immutable
// root, then plain reads, no lock anywhere. The difference is the shape of
// what the pointer points at: instead of a flat Go map, a *persistent* hash
// array mapped trie (HAMT). Nodes are immutable after publication, so a
// writer can build a new version of the trie that shares almost everything
// with the old one, copying only the O(log n) nodes on the path from the
// root to the changed slot. cow's O(n) write becomes an O(log n) write —
// at ~1M keys, a handful of small node copies instead of a million-entry
// map copy.
//
// That change also removes the writer mutex. cow serialized writers with a
// lock because two concurrent full-map copies would lose one of the writes.
// Here a write is: load the root, build the new path off it, publish with a
// single CompareAndSwap. If the CAS fails another writer got there first;
// throw the path away and retry against the new root. This is optimistic
// concurrency, and it makes HAMT the repo's only design that is lock-free
// for both reads and writes — no mutex, no channel, nothing that can block
// a thread that loses a race.
//
// Two classic lock-free hazards are handled structurally:
//
//   - ABA: every successful write installs a freshly allocated *hamtRoot,
//     and the GC cannot recycle the old allocation while any competing
//     writer still holds it as its expected value. A CAS therefore succeeds
//     only against the exact snapshot the writer read.
//   - Torn reads: nodes are fully built before the CAS publishes them, and
//     the atomic load/CAS pair gives acquire/release ordering, so a reader
//     that sees the new root sees every node behind it.
//
// The root pointer also carries the entry count, so Len is an O(1) read of
// a consistent snapshot — the CAS that publishes a change publishes its
// count in the same swap.
//
// What this design does NOT fix is write *serialization*: all writers still
// contend on one pointer, so writes cannot scale across cores the way
// sharded's do — a failed CAS wastes an entire path copy. Lock-free is not
// contention-free; the benchmarks quantify the difference.
//
// Trie layout (the CHAMP refinement of Bagwell's HAMT): each node consumes
// hamtBits bits of the key's 64-bit hash to pick one of 64 slots. Two
// bitmaps say what each occupied slot holds — an inline key/value entry
// (dataMap) or a child node (nodeMap) — and popcount over the bitmap turns
// a slot's bit into an index into the packed entries/nodes slice, so a node
// only allocates space for the slots actually in use. Keys whose full
// 64-bit hashes collide (possible, though astronomically rare with distinct
// keys under FNV-1a) fall through all 11 levels into a linear-scan
// collision node.
// The chunk width is a genuine design parameter: Clojure's persistent maps
// use 5 bits (32-way), this implementation defaults to 6 (64-way). Wider
// nodes mean a shallower trie — fewer pointer hops per read and fewer
// nodes copied per write — but every copied node is bigger and the CAS
// window longer. To make the trade-off measurable, the write-side
// internals take the width as a parameter (they already thread `shift`, so
// the width rides along in a register for free) and BenchmarkHAMTWidth
// sweeps it. Valid widths are 1..6 — the bitmaps are uint64, so a chunk
// must index at most 64 slots. The production cache pins hamtBits at
// compile time, so Get's hot loop still const-folds its masks.
const (
	hamtBits  = 6
	hamtWidth = 1 << hamtBits // 64-way branching
	hamtMask  = hamtWidth - 1

	// hashBits is the point at which the hash is exhausted: shifts advance
	// 0, 6, ..., 60 (the last level uses the top 4 bits), and a merge asked
	// to split at shift >= 64 has no bits left to split on, so it makes a
	// collision node instead.
	hashBits = 64
)

// hamtEntry is one key/value pair stored inline in a node. The key's hash
// is stored alongside it so that pushing an entry down a level (when a new
// key lands in its slot) never needs to rehash the key.
type hamtEntry struct {
	hash       uint64
	key, value string
}

// hamtNode is an immutable trie node. Invariants (checked by the test
// suite's invariant walker):
//
//   - dataMap and nodeMap are disjoint; their popcounts equal
//     len(entries) and len(nodes); slices are packed in ascending bit order.
//   - A collision node (collision=true) uses only entries, holds >= 2 of
//     them, and appears only below the last bitmap level.
//   - Canonical form: no node other than the root holds just a single
//     inline entry and nothing else — deletes collapse such nodes into
//     their parent, so equal contents always mean equal structure.
//
// Nodes reachable from a published root are never mutated; all updates copy.
type hamtNode struct {
	dataMap   uint64
	nodeMap   uint64
	collision bool
	entries   []hamtEntry
	nodes     []*hamtNode
}

// hamtEmptyNode is the shared root of every empty HAMT.
var hamtEmptyNode = &hamtNode{}

// hamtRoot is what the cache's atomic pointer points at: one immutable
// snapshot of the trie plus its size. Both change together in one CAS.
type hamtRoot struct {
	node  *hamtNode
	count int
}

// HAMT is the lock-free cache: an atomic pointer to an immutable snapshot.
type HAMT struct {
	root atomic.Pointer[hamtRoot]
}

func NewHAMT() *HAMT {
	c := &HAMT{}
	c.root.Store(&hamtRoot{node: hamtEmptyNode})
	return c
}

func (c *HAMT) Get(key string) (string, bool) {
	return hamtGet(c.root.Load().node, fnv1a(key), key)
}

// hamtGet is the production-width descent. hamtBits is a constant here,
// so the masks fold; hamtLookup below is the same loop for any width.
func hamtGet(n *hamtNode, hash uint64, key string) (string, bool) {
	for shift := uint(0); ; shift += hamtBits {
		if n.collision {
			for i := range n.entries {
				if n.entries[i].key == key {
					return n.entries[i].value, true
				}
			}
			return "", false
		}
		bit := uint64(1) << ((hash >> shift) & hamtMask)
		switch {
		case n.dataMap&bit != 0:
			e := &n.entries[bits.OnesCount64(n.dataMap&(bit-1))]
			if e.key == key {
				return e.value, true
			}
			return "", false
		case n.nodeMap&bit != 0:
			n = n.nodes[bits.OnesCount64(n.nodeMap&(bit-1))]
		default:
			return "", false
		}
	}
}

// hamtLookup is hamtGet for a caller-chosen width: ShardedHAMT reads its
// 32-way tries through it, and the tests use it to walk synthetic-hash and
// swept-width tries. Width arrives in a register, so the cost over the
// const-folded hamtGet is small — but it is measurable, which is why HAMT
// keeps its own const copy of this loop.
func hamtLookup(n *hamtNode, hash uint64, width uint, key string) (string, bool) {
	for shift := uint(0); ; shift += width {
		if n.collision {
			for i := range n.entries {
				if n.entries[i].key == key {
					return n.entries[i].value, true
				}
			}
			return "", false
		}
		bit := uint64(1) << ((hash >> shift) & (1<<width - 1))
		switch {
		case n.dataMap&bit != 0:
			e := &n.entries[bits.OnesCount64(n.dataMap&(bit-1))]
			if e.key == key {
				return e.value, true
			}
			return "", false
		case n.nodeMap&bit != 0:
			n = n.nodes[bits.OnesCount64(n.nodeMap&(bit-1))]
		default:
			return "", false
		}
	}
}

func (c *HAMT) Set(key, value string) {
	hash := fnv1a(key)
	for {
		old := c.root.Load()
		node, delta := hamtInsert(old.node, hash, hamtBits, 0, key, value)
		if c.root.CompareAndSwap(old, &hamtRoot{node: node, count: old.count + delta}) {
			return
		}
	}
}

func (c *HAMT) Delete(key string) {
	hash := fnv1a(key)
	for {
		old := c.root.Load()
		node, found := hamtDelete(old.node, hash, hamtBits, 0, key)
		if !found {
			// Nothing to remove in this snapshot; the no-op linearizes at
			// the Load, no CAS needed.
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

func (c *HAMT) Len() int {
	return c.root.Load().count
}

// hamtInsert returns a new trie with key set to value, sharing all
// untouched nodes with n. delta is 1 if the key is new, 0 if it replaced
// an existing entry. width is the chunk width in bits (hamtBits in
// production; a sweep variable in BenchmarkHAMTWidth).
func hamtInsert(n *hamtNode, hash uint64, width, shift uint, key, value string) (node *hamtNode, delta int) {
	if n.collision {
		for i := range n.entries {
			if n.entries[i].key == key {
				return &hamtNode{collision: true, entries: setAt(n.entries, i, hamtEntry{hash, key, value})}, 0
			}
		}
		return &hamtNode{collision: true, entries: insertAt(n.entries, len(n.entries), hamtEntry{hash, key, value})}, 1
	}
	bit := uint64(1) << ((hash >> shift) & (1<<width - 1))
	switch {
	case n.dataMap&bit != 0:
		di := bits.OnesCount64(n.dataMap & (bit - 1))
		e := n.entries[di]
		if e.key == key {
			return &hamtNode{dataMap: n.dataMap, nodeMap: n.nodeMap,
				entries: setAt(n.entries, di, hamtEntry{hash, key, value}), nodes: n.nodes}, 0
		}
		// Slot taken by a different key: push both entries one level down
		// into a new child, where more hash bits will separate them.
		child := hamtMerge(e, hamtEntry{hash, key, value}, width, shift+width)
		return &hamtNode{
			dataMap: n.dataMap &^ bit,
			nodeMap: n.nodeMap | bit,
			entries: removeAt(n.entries, di),
			nodes:   insertAt(n.nodes, bits.OnesCount64(n.nodeMap&(bit-1)), child),
		}, 1
	case n.nodeMap&bit != 0:
		ni := bits.OnesCount64(n.nodeMap & (bit - 1))
		child, delta := hamtInsert(n.nodes[ni], hash, width, shift+width, key, value)
		return &hamtNode{dataMap: n.dataMap, nodeMap: n.nodeMap,
			entries: n.entries, nodes: setAt(n.nodes, ni, child)}, delta
	default:
		return &hamtNode{dataMap: n.dataMap | bit, nodeMap: n.nodeMap,
			entries: insertAt(n.entries, bits.OnesCount64(n.dataMap&(bit-1)), hamtEntry{hash, key, value}),
			nodes:   n.nodes}, 1
	}
}

// hamtMerge builds the minimal subtree holding two entries whose hashes
// agreed on every chunk above shift. While their chunks keep colliding it
// descends another level; when the hash runs out entirely it gives up and
// stores both in a collision node.
func hamtMerge(e1, e2 hamtEntry, width, shift uint) *hamtNode {
	if shift >= hashBits {
		return &hamtNode{collision: true, entries: []hamtEntry{e1, e2}}
	}
	c1 := (e1.hash >> shift) & (1<<width - 1)
	c2 := (e2.hash >> shift) & (1<<width - 1)
	if c1 == c2 {
		return &hamtNode{
			nodeMap: 1 << c1,
			nodes:   []*hamtNode{hamtMerge(e1, e2, width, shift+width)},
		}
	}
	if c1 < c2 {
		return &hamtNode{dataMap: 1<<c1 | 1<<c2, entries: []hamtEntry{e1, e2}}
	}
	return &hamtNode{dataMap: 1<<c1 | 1<<c2, entries: []hamtEntry{e2, e1}}
}

// hamtDelete returns a new trie without key, or found=false (and n itself)
// if the key is absent. A nil node means the subtree became empty. To keep
// the trie canonical it collapses on the way back up: a child left holding
// a single inline entry is dissolved into its parent, which can cascade
// further up the path.
func hamtDelete(n *hamtNode, hash uint64, width, shift uint, key string) (node *hamtNode, found bool) {
	if n.collision {
		for i := range n.entries {
			if n.entries[i].key == key {
				if len(n.entries) == 1 {
					return nil, true
				}
				return &hamtNode{collision: true, entries: removeAt(n.entries, i)}, true
			}
		}
		return n, false
	}
	bit := uint64(1) << ((hash >> shift) & (1<<width - 1))
	switch {
	case n.dataMap&bit != 0:
		di := bits.OnesCount64(n.dataMap & (bit - 1))
		if n.entries[di].key != key {
			return n, false
		}
		if n.dataMap == bit && n.nodeMap == 0 {
			return nil, true
		}
		return &hamtNode{dataMap: n.dataMap &^ bit, nodeMap: n.nodeMap,
			entries: removeAt(n.entries, di), nodes: n.nodes}, true
	case n.nodeMap&bit != 0:
		ni := bits.OnesCount64(n.nodeMap & (bit - 1))
		child, found := hamtDelete(n.nodes[ni], hash, width, shift+width, key)
		if !found {
			return n, false
		}
		switch {
		case child == nil:
			if n.nodeMap == bit && n.dataMap == 0 {
				return nil, true
			}
			return &hamtNode{dataMap: n.dataMap, nodeMap: n.nodeMap &^ bit,
				entries: n.entries, nodes: removeAt(n.nodes, ni)}, true
		case child.nodeMap == 0 && len(child.entries) == 1:
			// The child is down to one inline entry (a shrunken collision
			// node included): inline it here. The caller applies the same
			// check to what we return, so collapse cascades toward the root.
			return &hamtNode{
				dataMap: n.dataMap | bit,
				nodeMap: n.nodeMap &^ bit,
				entries: insertAt(n.entries, bits.OnesCount64(n.dataMap&(bit-1)), child.entries[0]),
				nodes:   removeAt(n.nodes, ni),
			}, true
		default:
			return &hamtNode{dataMap: n.dataMap, nodeMap: n.nodeMap,
				entries: n.entries, nodes: setAt(n.nodes, ni, child)}, true
		}
	default:
		return n, false
	}
}

// Load implements BulkLoader. Building by repeated persistent inserts would
// copy the wide top-level nodes n times over; instead the trie is built
// with in-place mutation, which is safe because nothing else can see the
// nodes until the final Store publishes them.
func (c *HAMT) Load(items map[string]string) {
	root := &hamtNode{}
	for k, v := range items {
		hamtInsertMut(root, fnv1a(k), hamtBits, 0, k, v)
	}
	if len(items) == 0 {
		root = hamtEmptyNode
	}
	c.root.Store(&hamtRoot{node: root, count: len(items)})
}

// hamtInsertMut is hamtInsert without the copying, for nodes that are still
// private to their builder. Never call it on a published node.
func hamtInsertMut(n *hamtNode, hash uint64, width, shift uint, key, value string) {
	if n.collision {
		for i := range n.entries {
			if n.entries[i].key == key {
				n.entries[i].value = value
				return
			}
		}
		n.entries = append(n.entries, hamtEntry{hash, key, value})
		return
	}
	bit := uint64(1) << ((hash >> shift) & (1<<width - 1))
	switch {
	case n.dataMap&bit != 0:
		di := bits.OnesCount64(n.dataMap & (bit - 1))
		if n.entries[di].key == key {
			n.entries[di].value = value
			return
		}
		child := hamtMerge(n.entries[di], hamtEntry{hash, key, value}, width, shift+width)
		n.dataMap &^= bit
		n.nodeMap |= bit
		n.entries = removeAtInPlace(n.entries, di)
		n.nodes = insertAtInPlace(n.nodes, bits.OnesCount64(n.nodeMap&(bit-1)), child)
	case n.nodeMap&bit != 0:
		hamtInsertMut(n.nodes[bits.OnesCount64(n.nodeMap&(bit-1))], hash, width, shift+width, key, value)
	default:
		n.dataMap |= bit
		n.entries = insertAtInPlace(n.entries, bits.OnesCount64(n.dataMap&(bit-1)), hamtEntry{hash, key, value})
	}
}

// --- small slice helpers ---------------------------------------------------
//
// The persistent variants always return a freshly allocated slice: a node's
// backing array may be shared with older snapshots that concurrent readers
// are still traversing, so it must never be written through. The InPlace
// variants are for Load's private build only.

func setAt[T any](s []T, i int, v T) []T {
	out := make([]T, len(s))
	copy(out, s)
	out[i] = v
	return out
}

func insertAt[T any](s []T, i int, v T) []T {
	out := make([]T, len(s)+1)
	copy(out, s[:i])
	out[i] = v
	copy(out[i+1:], s[i:])
	return out
}

func removeAt[T any](s []T, i int) []T {
	out := make([]T, len(s)-1)
	copy(out, s[:i])
	copy(out[i:], s[i+1:])
	return out
}

func insertAtInPlace[T any](s []T, i int, v T) []T {
	var zero T
	s = append(s, zero)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func removeAtInPlace[T any](s []T, i int) []T {
	copy(s[i:], s[i+1:])
	return s[:len(s)-1]
}
