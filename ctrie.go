package cache

import (
	"math/bits"
	"runtime"
	"sync/atomic"
)

// Ctrie is the last rung of the persistent-trie ladder: Prokopec's
// concurrent hash trie (the structure behind Scala's TrieMap), with the
// full generation/GCAS machinery for O(1) lock-free snapshots.
//
// The base idea carries over from the earlier no-snapshot version: above
// every immutable branch node (C-node) sits a permanent mutable cell (the
// I-node), so a write builds ONE new C-node and CASes the deepest I-node
// it touches — per-branch contention granularity, O(1) allocation per
// write, no path copying. What the full protocol adds is the ability to
// freeze the entire trie in O(1):
//
//   - Every I-node carries a generation token. Snapshot() swaps the root
//     for a copy with a FRESH generation (sharing the entire old tree)
//     and hands back the old root as a frozen view.
//   - A write commits only if its I-node's generation still matches the
//     root's (GCAS): the new C-node is installed carrying a `prev` link
//     to the one it replaces, and is then committed (prev cleared) or
//     aborted (rolled back to prev) according to a re-read of the root
//     generation. A write racing a snapshot therefore lands wholly on one
//     side of it: either it commits with a pre-swap root observation (and
//     every reader, frozen and live alike, orders it BEFORE the snapshot —
//     the deciding reads below are what make that agreement hold), or its
//     commit re-read sees the new generation and aborts, restarting the
//     write after the snapshot.
//   - The root swap itself is an RDCSS: a descriptor is installed and
//     completed (helped by any thread that trips over it) only if the
//     root I-node's main pointer is still exactly what the snapshot read
//     — otherwise a root-level write slipped in and the snapshot retries.
//   - Writers descending into the current trie renew stale-generation
//     I-nodes lazily (copy the cell, GCAS it into the parent), so a
//     snapshot's cost is not paid at snapshot time but amortized over
//     the writes that follow, one path at a time — the same
//     copy-on-write bargain as cow, at path granularity.
//
// What the snapshot buys back is exactly what sharding gave away: Len is
// again an exact point-in-time count, and GetBatch answers every key
// from one frozen world (see TestGetBatchSnapshot). Reads pay one extra
// atomic load per visited node (the prev check); writes pay one small
// GCAS-state allocation; a snapshot makes the next write to each path
// pay one renewal copy.
//
// Simplifications versus the paper, both documented where they bite:
//
//   - No contraction (as before): deleted structure lingers as husks.
//     I-nodes are never unlinked, which is also what keeps the failure
//     paths simple — a failed CAS restarts the operation from the root
//     (the paper's tomb/clean protocol is what a restartless retry would
//     require).
//   - Snapshots are read-only views (Get/GetBatch/Len), not writable
//     forks. Writable forks need a second fresh generation per snapshot
//     and renewal on the snapshot side too; nothing in this repo's
//     harness would exercise it.
//
// Layout is CHAMP at 32-way, entries reuse hamtEntry, and full-hash
// collisions land in a collision C-node past the last chunk, as in hamt.
const (
	ctrieBits = 5 // 32-way
	ctrieMask = 1<<ctrieBits - 1
)

// ctrieGen is a generation token; only its pointer identity matters.
type ctrieGen struct{ _ byte }

// ctrieGCAS is the in-flight state of a GCAS on some I-node's main
// pointer: prev is the committed C-node being replaced. failed marks a
// decided abort (the main pointer must roll back to prev).
type ctrieGCAS struct {
	prev   *ctrieCNode
	failed bool
}

// ctrieINode is the mutable cell above every branch: the only thing in
// the structure that changes, and only by (G)CAS. Once a C-node links an
// I-node into a slot, that I-node owns the slot for that generation;
// renewal replaces the CELL in the parent, never mutates it.
type ctrieINode struct {
	main atomic.Pointer[ctrieCNode]
	gen  *ctrieGen
}

// ctrieCNode is an immutable branch node, plus the GCAS prev link (nil
// once the node is committed; set only between installation and
// commit/abort). Same structural invariants as before; no canonical form
// (husks are legal).
type ctrieCNode struct {
	prev      atomic.Pointer[ctrieGCAS]
	dataMap   uint64
	nodeMap   uint64
	collision bool
	entries   []hamtEntry
	inodes    []*ctrieINode
}

// rootRef is what the cache's root pointer holds. prev==nil marks a
// committed reference; otherwise it is an RDCSS descriptor proposing to
// replace `prev` with `clean`, valid only while prev.in's main is still
// expMain. won records the winning helper's outcome for the initiator.
type rootRef struct {
	in      *ctrieINode
	prev    *rootRef
	expMain *ctrieCNode
	clean   *rootRef
	won     atomic.Int32 // 0 pending, 1 committed, 2 aborted
}

type Ctrie struct {
	root atomic.Pointer[rootRef]
}

func NewCtrie() *Ctrie {
	c := &Ctrie{}
	in := &ctrieINode{gen: &ctrieGen{}}
	in.main.Store(&ctrieCNode{})
	c.root.Store(&rootRef{in: in})
	return c
}

// CtrieSnapshot is a frozen point-in-time view of a Ctrie.
type CtrieSnapshot struct {
	root *ctrieINode
}

// --- root RDCSS ------------------------------------------------------------

// rdcssReadRoot returns the current committed root, helping any in-flight
// snapshot swap to completion first.
func (c *Ctrie) rdcssReadRoot() *rootRef {
	for {
		r := c.root.Load()
		if r.prev == nil {
			return r
		}
		c.rdcssComplete(r)
	}
}

// rdcssComplete drives the descriptor d (currently installed in c.root)
// to its outcome. The winning CAS is the atomic decide-and-apply: commit
// only if the old root's main is still exactly expMain and clean of
// in-flight GCAS state. Helpers may evaluate stale conditions, but only
// the first CAS from d wins, and the generation check inside GCAS
// commits backstops the one racy interleaving (a root-level write
// installed but not yet committed when a stale commit wins: its commit
// re-reads the root, sees the new generation, and aborts).
func (c *Ctrie) rdcssComplete(d *rootRef) {
	for c.root.Load() == d {
		cur := d.prev.in.main.Load()
		if cur == d.expMain && cur.prev.Load() == nil {
			if c.root.CompareAndSwap(d, d.clean) {
				d.won.Store(1)
				return
			}
		} else {
			if c.root.CompareAndSwap(d, d.prev) {
				d.won.Store(2)
				return
			}
		}
	}
}

// Snapshot freezes the trie in O(1): the old root becomes an immutable
// view, the live trie continues under a fresh generation, and the two
// share every node until writes lazily un-share the touched paths.
func (c *Ctrie) Snapshot() *CtrieSnapshot {
	for {
		r := c.rdcssReadRoot()
		m := c.gcasRead(r.in)
		newRoot := &ctrieINode{gen: &ctrieGen{}}
		newRoot.main.Store(m)
		d := &rootRef{in: newRoot, prev: r, expMain: m, clean: &rootRef{in: newRoot}}
		if c.root.CompareAndSwap(r, d) {
			c.rdcssComplete(d)
			for d.won.Load() == 0 {
				// The winning helper stores the outcome right after its
				// CAS, but it can be descheduled between the two — yield
				// rather than burn the core waiting for it.
				runtime.Gosched()
			}
			if d.won.Load() == 1 {
				return &CtrieSnapshot{root: r.in}
			}
		}
	}
}

// --- GCAS ------------------------------------------------------------------

// gcas installs n over old in in's main pointer and commits it iff in's
// generation still matches the root's. Returns false (caller restarts
// from the root) if the installation lost a race or the commit aborted.
func (c *Ctrie) gcas(in *ctrieINode, old, n *ctrieCNode) bool {
	n.prev.Store(&ctrieGCAS{prev: old})
	if !in.main.CompareAndSwap(old, n) {
		return false
	}
	c.gcasComplete(in, n)
	return n.prev.Load() == nil
}

// gcasComplete drives an installed-but-undecided main node to commit
// (prev cleared) or abort (main rolled back), deciding by comparing in's
// generation with the CURRENT root's — which makes commit-after-snapshot
// impossible. Helps any in-flight root swap first, so the decision uses
// a settled root.
func (c *Ctrie) gcasComplete(in *ctrieINode, n *ctrieCNode) {
	for {
		st := n.prev.Load()
		if st == nil {
			return
		}
		if st.failed {
			in.main.CompareAndSwap(n, st.prev)
			return
		}
		r := c.root.Load()
		if r.prev != nil {
			c.rdcssComplete(r)
			continue
		}
		if r.in.gen == in.gen {
			if n.prev.CompareAndSwap(st, nil) {
				return
			}
		} else {
			n.prev.CompareAndSwap(st, &ctrieGCAS{prev: st.prev, failed: true})
		}
	}
}

// gcasRead returns in's committed main, helping any in-flight GCAS to a
// decision first. Writers use this so they always build on settled state.
func (c *Ctrie) gcasRead(in *ctrieINode) *ctrieCNode {
	for {
		n := in.main.Load()
		if n.prev.Load() == nil {
			return n
		}
		c.gcasComplete(in, n)
	}
}

// frozenRead returns the committed main of in within a FROZEN tree, and
// unlike liveRead it ARBITRATES: an undecided GCAS is decided — aborted
// — by CASing its state to failed, so the single CAS on prev is the
// decision point for every observer (the paper's read-only GCAS commit).
//
// This is the linchpin the first review of this file got wrong: a writer
// whose generation check read the root BEFORE the snapshot swap can
// otherwise commit AFTER it (the swap's RDCSS condition only inspects
// the root I-node), and a passive reader would see the same frozen key
// change values between two reads — or, on the renewal path, capture the
// pre-write state into the live generation and strand the committed
// write on an orphaned cell. Forcing the decision closes both: either
// our abort wins (the writer's commit CAS fails, it restarts into the
// current generation) or the writer's commit already won (we return the
// new value consistently — the write was installed before the freeze, so
// linearizing it before the snapshot is legal).
func frozenRead(in *ctrieINode) *ctrieCNode {
	for {
		n := in.main.Load()
		st := n.prev.Load()
		if st == nil {
			return n
		}
		if st.failed {
			in.main.CompareAndSwap(n, st.prev)
			return st.prev
		}
		if n.prev.CompareAndSwap(st, &ctrieGCAS{prev: st.prev, failed: true}) {
			in.main.CompareAndSwap(n, st.prev)
			return st.prev
		}
		// Lost the decision race: prev is now nil (committed) or failed —
		// reload and resolve accordingly.
	}
}

// --- operations ------------------------------------------------------------

func (c *Ctrie) Get(key string) (string, bool) {
	return ctrieGet(c, fnv1a(key), key)
}

func (c *Ctrie) Set(key, value string) {
	ctrieSet(c, fnv1a(key), key, value)
}

func (c *Ctrie) Delete(key string) {
	ctrieDelete(c, fnv1a(key), key)
}

// Len is an exact point-in-time count: it freezes a snapshot in O(1) and
// walks that. (The walk is still O(n); what the snapshot changes is that
// the count is of ONE instant, not a moving sum. The snapshot also costs
// the live trie a renewal wave — see Snapshot.)
func (c *Ctrie) Len() int {
	return c.Snapshot().Len()
}

func (s *CtrieSnapshot) Len() int {
	return ctrieCount(s.root)
}

func ctrieCount(in *ctrieINode) int {
	cn := frozenRead(in)
	n := len(cn.entries)
	for _, child := range cn.inodes {
		n += ctrieCount(child)
	}
	return n
}

// Get on a snapshot reads the frozen world.
func (s *CtrieSnapshot) Get(key string) (string, bool) {
	return ctrieLookup(nil, s.root, fnv1a(key), key, true)
}

// GetBatch on a snapshot answers every key from the same frozen world.
func (s *CtrieSnapshot) GetBatch(keys []string) []BatchResult {
	out := make([]BatchResult, len(keys))
	for i, k := range keys {
		v, ok := ctrieLookup(nil, s.root, fnv1a(k), k, true)
		out[i] = BatchResult{Value: v, OK: ok}
	}
	return out
}

func ctrieGet(c *Ctrie, hash uint64, key string) (string, bool) {
	return ctrieLookup(c, c.rdcssReadRoot().in, hash, key, false)
}

// ctrieLookup descends with the read discipline the caller's world
// demands. Frozen (snapshot) lookups arbitrate via frozenRead. Live
// lookups (c non-nil, frozen=false) use the DECIDING read gcasRead: a
// passive live read was this file's second confirmed bug — after a
// snapshot legalized a late commit by linearizing it before the freeze,
// a passive live Get issued after Snapshot() returned could still serve
// the pre-write value, an inversion (Set < Snapshot < Get < Set) no
// linearization can explain. Deciding reads keep every observer
// agreeing: current-generation in-flight writes get helped to commit,
// stale-generation ones (doomed anyway) get aborted, and the fast path
// (prev == nil) costs the same one branch either way.
func ctrieLookup(c *Ctrie, in *ctrieINode, hash uint64, key string, frozen bool) (string, bool) {
	for shift := uint(0); ; shift += ctrieBits {
		var cn *ctrieCNode
		if frozen {
			cn = frozenRead(in)
		} else {
			cn = c.gcasRead(in)
		}
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

func ctrieSet(c *Ctrie, hash uint64, key, value string) {
	for {
		r := c.rdcssReadRoot()
		if ctrieInsertRec(c, r.in, r.in.gen, hash, 0, key, value) {
			return
		}
	}
}

// ctrieInsertRec descends toward the key's slot, renewing stale-
// generation children on the way, and performs exactly one GCAS. false
// means restart from the root (lost a race, or a snapshot moved the
// generation).
func ctrieInsertRec(c *Ctrie, in *ctrieINode, gen *ctrieGen, hash uint64, shift uint, key, value string) bool {
	cn := c.gcasRead(in)
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
		return c.gcas(in, cn, newCn)
	}
	bit := uint64(1) << ((hash >> shift) & ctrieMask)
	switch {
	case cn.dataMap&bit != 0:
		di := bits.OnesCount64(cn.dataMap & (bit - 1))
		e := cn.entries[di]
		if e.key == key {
			newCn := &ctrieCNode{dataMap: cn.dataMap, nodeMap: cn.nodeMap,
				entries: setAt(cn.entries, di, hamtEntry{hash, key, value}), inodes: cn.inodes}
			return c.gcas(in, cn, newCn)
		}
		// Push both entries down behind a fresh I-node (of the current
		// generation). The chain is private until the GCAS publishes it.
		child := ctrieMergeINode(e, hamtEntry{hash, key, value}, shift+ctrieBits, gen)
		newCn := &ctrieCNode{
			dataMap: cn.dataMap &^ bit,
			nodeMap: cn.nodeMap | bit,
			entries: removeAt(cn.entries, di),
			inodes:  insertAt(cn.inodes, bits.OnesCount64(cn.nodeMap&(bit-1)), child),
		}
		return c.gcas(in, cn, newCn)
	case cn.nodeMap&bit != 0:
		ni := bits.OnesCount64(cn.nodeMap & (bit - 1))
		child := cn.inodes[ni]
		if child.gen != gen {
			// The child cell predates the latest snapshot: renew it (copy
			// the CELL, not the tree) into this generation, then restart
			// at this level so the descent continues into the renewed
			// cell. The frozen side keeps the old cell untouched.
			renewed := &ctrieINode{gen: gen}
			renewed.main.Store(frozenRead(child))
			newCn := &ctrieCNode{dataMap: cn.dataMap, nodeMap: cn.nodeMap,
				entries: cn.entries, inodes: setAt(cn.inodes, ni, renewed)}
			if !c.gcas(in, cn, newCn) {
				return false
			}
			return ctrieInsertRec(c, renewed, gen, hash, shift+ctrieBits, key, value)
		}
		return ctrieInsertRec(c, child, gen, hash, shift+ctrieBits, key, value)
	default:
		newCn := &ctrieCNode{dataMap: cn.dataMap | bit, nodeMap: cn.nodeMap,
			entries: insertAt(cn.entries, bits.OnesCount64(cn.dataMap&(bit-1)), hamtEntry{hash, key, value}),
			inodes:  cn.inodes}
		return c.gcas(in, cn, newCn)
	}
}

// ctrieMergeINode builds the private subtree holding two entries whose
// hashes agreed on every chunk above shift, each level behind its own
// I-node of the current generation.
func ctrieMergeINode(e1, e2 hamtEntry, shift uint, gen *ctrieGen) *ctrieINode {
	in := &ctrieINode{gen: gen}
	in.main.Store(ctrieMergeCNode(e1, e2, shift, gen))
	return in
}

func ctrieMergeCNode(e1, e2 hamtEntry, shift uint, gen *ctrieGen) *ctrieCNode {
	if shift >= hashBits {
		return &ctrieCNode{collision: true, entries: []hamtEntry{e1, e2}}
	}
	c1 := (e1.hash >> shift) & ctrieMask
	c2 := (e2.hash >> shift) & ctrieMask
	if c1 == c2 {
		return &ctrieCNode{
			nodeMap: 1 << c1,
			inodes:  []*ctrieINode{ctrieMergeINode(e1, e2, shift+ctrieBits, gen)},
		}
	}
	if c1 < c2 {
		return &ctrieCNode{dataMap: 1<<c1 | 1<<c2, entries: []hamtEntry{e1, e2}}
	}
	return &ctrieCNode{dataMap: 1<<c1 | 1<<c2, entries: []hamtEntry{e2, e1}}
}

func ctrieDelete(c *Ctrie, hash uint64, key string) {
	for {
		r := c.rdcssReadRoot()
		if ctrieDeleteRec(c, r.in, r.in.gen, hash, 0, key) {
			return
		}
	}
}

// ctrieDeleteRec mirrors the insert descent (including renewal); as
// before there is no contraction — an emptied C-node stays, a lone
// survivor is not inlined upward.
func ctrieDeleteRec(c *Ctrie, in *ctrieINode, gen *ctrieGen, hash uint64, shift uint, key string) bool {
	cn := c.gcasRead(in)
	if cn.collision {
		di := -1
		for i := range cn.entries {
			if cn.entries[i].key == key {
				di = i
				break
			}
		}
		if di < 0 {
			return true // no-op delete; linearizes at the gcasRead
		}
		newCn := &ctrieCNode{collision: true, entries: removeAt(cn.entries, di)}
		return c.gcas(in, cn, newCn)
	}
	bit := uint64(1) << ((hash >> shift) & ctrieMask)
	switch {
	case cn.dataMap&bit != 0:
		di := bits.OnesCount64(cn.dataMap & (bit - 1))
		if cn.entries[di].key != key {
			return true
		}
		newCn := &ctrieCNode{dataMap: cn.dataMap &^ bit, nodeMap: cn.nodeMap,
			entries: removeAt(cn.entries, di), inodes: cn.inodes}
		return c.gcas(in, cn, newCn)
	case cn.nodeMap&bit != 0:
		ni := bits.OnesCount64(cn.nodeMap & (bit - 1))
		child := cn.inodes[ni]
		if child.gen != gen {
			renewed := &ctrieINode{gen: gen}
			renewed.main.Store(frozenRead(child))
			newCn := &ctrieCNode{dataMap: cn.dataMap, nodeMap: cn.nodeMap,
				entries: cn.entries, inodes: setAt(cn.inodes, ni, renewed)}
			if !c.gcas(in, cn, newCn) {
				return false
			}
			return ctrieDeleteRec(c, renewed, gen, hash, shift+ctrieBits, key)
		}
		return ctrieDeleteRec(c, child, gen, hash, shift+ctrieBits, key)
	default:
		return true
	}
}
