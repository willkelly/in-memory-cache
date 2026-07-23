package cache

import (
	"math/bits"
	"sync/atomic"
)

// Ctrie is the last rung of the persistent-trie ladder, and it comes from
// one observation about hamt256: a shard table is just a trie node whose
// slots can be swapped atomically in place. If one level of CAS-able slots
// fixes write contention, put one at *every* level.
//
// That is the I-node (indirection node) idea from Prokopec's concurrent
// hash trie (the Ctrie, the structure behind Scala's TrieMap): above every
// branch node sits a permanent one-pointer cell holding the current
// immutable version of the branch. A write walks down to the deepest
// I-node its key touches, builds a new version of THAT ONE branch node,
// and CASes the I-node. Two things fall out:
//
//   - Contention granularity becomes per-branch at every depth — finer
//     than any fixed shard count, and it adapts to the tree: two writers
//     conflict only if their keys share a branch node, and the deeper the
//     tree, the fewer keys do.
//   - The path copy disappears. hamt copies every node from root to leaf
//     because publishing at the root is the only mutation point; here the
//     mutation point sits directly above the change, so the ancestors are
//     untouched. A write allocates O(1) nodes — one small C-node — no
//     matter how deep the trie. (This also makes a bulk-load fast path
//     unnecessary: n Sets do O(n) total work, so there is no BulkLoader
//     here, unlike cow and hamt.)
//
// The price is on the read path: every level is now two pointer loads
// (I-node, then its C-node) instead of one, and the loads are dependent.
// The benchmarks price that honestly.
//
// Two deliberate simplifications versus the published Ctrie:
//
//   - No generation stamps, so no O(1) global snapshot. The full Ctrie's
//     crown jewel is that a GCAS protocol over generation-stamped I-nodes
//     restores exactly what sharding destroyed — a lock-free O(1)
//     point-in-time view of the whole map. That protocol (GCAS, RDCSS,
//     failed-node recovery) is a paper's worth of machinery; this
//     implementation trades it away, which also demotes Len to an O(n)
//     walk whose result is a moving count, like sharded's.
//   - No contraction. The paper entombs and collapses single-entry
//     branches after deletes; here deleted structure is left in place
//     (an emptied C-node just stays an empty C-node). This is what makes
//     the retry loops safe to run LOCALLY: an I-node, once linked into a
//     C-node, is never unlinked or replaced, so a writer that loses a CAS
//     can simply reload that same I-node and try again — no restart from
//     the root, and no way for a competitor to detach the subtree under
//     it (the hazard that forces the full Ctrie's tombstone dance).
//     The cost is that memory tracks the high-water key-path set rather
//     than the live key set under delete-heavy churn over ever-new keys.
//
// Layout is CHAMP like hamt's, at 32-way per the width sweep (the
// write-optimized choice, and C-node copies are the whole write cost
// here). Entries reuse hamtEntry, so full-hash collisions land in a
// collision C-node past the last chunk, exactly as in hamt.
const (
	ctrieBits = 5 // 32-way
	ctrieMask = 1<<ctrieBits - 1
)

// ctrieINode is the mutable cell: the only thing in this structure that
// ever changes, and only by CAS. Once a C-node links an I-node into a
// slot, that I-node owns the slot forever.
type ctrieINode struct {
	main atomic.Pointer[ctrieCNode]
}

// ctrieCNode is an immutable branch node. Same invariants as hamtNode
// (disjoint bitmaps, popcount-packed slices, collision nodes only below
// the last bitmap level) except canonical form: with no contraction,
// empty and single-entry C-nodes may persist after deletes.
type ctrieCNode struct {
	dataMap   uint64
	nodeMap   uint64
	collision bool
	entries   []hamtEntry
	inodes    []*ctrieINode
}

type Ctrie struct {
	root ctrieINode
}

func NewCtrie() *Ctrie {
	c := &Ctrie{}
	c.root.main.Store(&ctrieCNode{})
	return c
}

func (c *Ctrie) Get(key string) (string, bool) {
	return ctrieGet(&c.root, fnv1a(key), key)
}

func (c *Ctrie) Set(key, value string) {
	ctrieSet(&c.root, fnv1a(key), key, value)
}

func (c *Ctrie) Delete(key string) {
	ctrieDelete(&c.root, fnv1a(key), key)
}

// Len walks the trie. O(n), and the count is not a point-in-time snapshot
// (subtrees are visited at slightly different moments) — the same
// semantics as sharded's lock-by-lock Len, for the same reason.
func (c *Ctrie) Len() int {
	return ctrieCount(c.root.main.Load())
}

func ctrieCount(cn *ctrieCNode) int {
	n := len(cn.entries)
	for _, in := range cn.inodes {
		n += ctrieCount(in.main.Load())
	}
	return n
}

func ctrieGet(in *ctrieINode, hash uint64, key string) (string, bool) {
	for shift := uint(0); ; shift += ctrieBits {
		cn := in.main.Load()
		if cn.collision {
			for i := range cn.entries {
				if cn.entries[i].key == key {
					return cn.entries[i].value, true
				}
			}
			return "", false
		}
		bit := uint64(1) << ((hash >> shift) & ctrieMask)
		switch {
		case cn.dataMap&bit != 0:
			e := &cn.entries[bits.OnesCount64(cn.dataMap&(bit-1))]
			if e.key == key {
				return e.value, true
			}
			return "", false
		case cn.nodeMap&bit != 0:
			in = cn.inodes[bits.OnesCount64(cn.nodeMap&(bit-1))]
		default:
			return "", false
		}
	}
}

// ctrieSet is one CAS on the deepest I-node the key touches. A failed CAS
// reloads the SAME I-node and retries there — sound only because I-nodes
// are permanent (see the type comment).
func ctrieSet(in *ctrieINode, hash uint64, key, value string) {
	shift := uint(0)
	for {
		cn := in.main.Load()
		if cn.collision {
			var newCn *ctrieCNode
			for i := range cn.entries {
				if cn.entries[i].key == key {
					newCn = &ctrieCNode{collision: true, entries: setAt(cn.entries, i, hamtEntry{hash, key, value})}
					break
				}
			}
			if newCn == nil {
				newCn = &ctrieCNode{collision: true, entries: insertAt(cn.entries, len(cn.entries), hamtEntry{hash, key, value})}
			}
			if in.main.CompareAndSwap(cn, newCn) {
				return
			}
			continue
		}
		bit := uint64(1) << ((hash >> shift) & ctrieMask)
		switch {
		case cn.dataMap&bit != 0:
			di := bits.OnesCount64(cn.dataMap & (bit - 1))
			e := cn.entries[di]
			if e.key == key {
				newCn := &ctrieCNode{dataMap: cn.dataMap, nodeMap: cn.nodeMap,
					entries: setAt(cn.entries, di, hamtEntry{hash, key, value}), inodes: cn.inodes}
				if in.main.CompareAndSwap(cn, newCn) {
					return
				}
				continue
			}
			// Push both entries down behind a fresh I-node. The chain is
			// private until the CAS publishes it.
			child := ctrieMergeINode(e, hamtEntry{hash, key, value}, shift+ctrieBits)
			newCn := &ctrieCNode{
				dataMap: cn.dataMap &^ bit,
				nodeMap: cn.nodeMap | bit,
				entries: removeAt(cn.entries, di),
				inodes:  insertAt(cn.inodes, bits.OnesCount64(cn.nodeMap&(bit-1)), child),
			}
			if in.main.CompareAndSwap(cn, newCn) {
				return
			}
			continue
		case cn.nodeMap&bit != 0:
			in = cn.inodes[bits.OnesCount64(cn.nodeMap&(bit-1))]
			shift += ctrieBits
		default:
			newCn := &ctrieCNode{dataMap: cn.dataMap | bit, nodeMap: cn.nodeMap,
				entries: insertAt(cn.entries, bits.OnesCount64(cn.dataMap&(bit-1)), hamtEntry{hash, key, value}),
				inodes:  cn.inodes}
			if in.main.CompareAndSwap(cn, newCn) {
				return
			}
			continue
		}
	}
}

// ctrieMergeINode builds the private subtree holding two entries whose
// hashes agreed on every chunk above shift, each level behind its own
// I-node so later writers can CAS at any depth.
func ctrieMergeINode(e1, e2 hamtEntry, shift uint) *ctrieINode {
	in := &ctrieINode{}
	in.main.Store(ctrieMergeCNode(e1, e2, shift))
	return in
}

func ctrieMergeCNode(e1, e2 hamtEntry, shift uint) *ctrieCNode {
	if shift >= hashBits {
		return &ctrieCNode{collision: true, entries: []hamtEntry{e1, e2}}
	}
	c1 := (e1.hash >> shift) & ctrieMask
	c2 := (e2.hash >> shift) & ctrieMask
	if c1 == c2 {
		return &ctrieCNode{
			nodeMap: 1 << c1,
			inodes:  []*ctrieINode{ctrieMergeINode(e1, e2, shift+ctrieBits)},
		}
	}
	if c1 < c2 {
		return &ctrieCNode{dataMap: 1<<c1 | 1<<c2, entries: []hamtEntry{e1, e2}}
	}
	return &ctrieCNode{dataMap: 1<<c1 | 1<<c2, entries: []hamtEntry{e2, e1}}
}

// ctrieDelete removes the entry from its C-node and leaves the structure
// otherwise as-is: an emptied C-node stays, a lone survivor is not
// inlined upward (no contraction; see the type comment for why that is
// the simplification that keeps local retries safe).
func ctrieDelete(in *ctrieINode, hash uint64, key string) {
	shift := uint(0)
	for {
		cn := in.main.Load()
		if cn.collision {
			di := -1
			for i := range cn.entries {
				if cn.entries[i].key == key {
					di = i
					break
				}
			}
			if di < 0 {
				return // no-op delete; linearizes at the Load above
			}
			newCn := &ctrieCNode{collision: true, entries: removeAt(cn.entries, di)}
			if in.main.CompareAndSwap(cn, newCn) {
				return
			}
			continue
		}
		bit := uint64(1) << ((hash >> shift) & ctrieMask)
		switch {
		case cn.dataMap&bit != 0:
			di := bits.OnesCount64(cn.dataMap & (bit - 1))
			if cn.entries[di].key != key {
				return
			}
			newCn := &ctrieCNode{dataMap: cn.dataMap &^ bit, nodeMap: cn.nodeMap,
				entries: removeAt(cn.entries, di), inodes: cn.inodes}
			if in.main.CompareAndSwap(cn, newCn) {
				return
			}
			continue
		case cn.nodeMap&bit != 0:
			in = cn.inodes[bits.OnesCount64(cn.nodeMap&(bit-1))]
			shift += ctrieBits
		default:
			return
		}
	}
}
