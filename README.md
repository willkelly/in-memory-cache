# in-memory-cache

Companion code for a blog post comparing in-memory cache implementations in
Go under different concurrent access patterns. The core implementations use
only the standard library; two third-party maps (`xsync`, `otter`) are
included for reference.

Every implementation is a `string -> string` map satisfying the same
[`Cache`](cache.go) interface, so a single benchmark harness can drive them
all under identical workloads. No eviction, no bounds — the focus is purely
the cost of *synchronization*.

## Implementations

| Name      | File                       | Idea | Notes |
|-----------|----------------------------|------|-------|
| `naive`   | [naive.go](naive.go)       | Plain map, no locking | Not thread-safe. Single-threaded baseline; concurrent writes crash the process. |
| `mutex`   | [mutex.go](mutex.go)       | One `sync.Mutex` for all ops | Reads cannot run in parallel. |
| `rwmutex` | [rwmutex.go](rwmutex.go)   | `sync.RWMutex` | Parallel reads; exclusive writes. |
| `syncmap` | [syncmap.go](syncmap.go)   | `sync.Map` | The stdlib's own answer; wins only for read-mostly / disjoint-key patterns. |
| `sharded` | [sharded.go](sharded.go)   | Lock striping (256 shards) | Canonical high-throughput design. Weak under skew. |
| `actor`   | [actor.go](actor.go)       | Goroutine-per-shard, channels only | No mutexes anywhere; writes are fire-and-forget. See below. |
| `cow`     | [cow.go](cow.go)           | Copy-on-write via `atomic.Pointer` | Lock-free reads; O(n) writes. |
| `hamt`    | [hamt.go](hamt.go)         | Persistent HAMT, CAS-published root | Lock-free reads *and* writes; O(log n) path-copy per write. See below. |
| `hamt256` | [hamtsharded.go](hamtsharded.go) | 256 independent persistent tries, 32-way | `hamt`'s design × `sharded`'s partitioning: still lock-free, writes scale. Gives up the global snapshot. |
| `ctrie`   | [ctrie.go](ctrie.go)       | Ctrie: CAS cell above *every* node | Sharding folded into the structure: per-branch contention, O(1)-node writes. No-snapshot variant of Prokopec's Ctrie. See below. |
| `syncXmap`| [syncxmap.go](syncxmap.go) | `xsync.Map` (third-party) | CLHT-based; obstruction-free reads. |
| `otter`   | [otter.go](otter.go)       | `otter` cache (third-party) | Full caching library (eviction, etc.), included for scale. |

### `hamt`: fixing `cow`'s writes with CAS + a persistent trie

`cow` poses an obvious follow-up: its read path is unbeatable (one atomic
load, then a plain map read), but every write copies the entire map. What if
a write only copied the part of the structure it touches?

`hamt` keeps `cow`'s exact read-side contract — an `atomic.Pointer` to an
immutable snapshot, readers never synchronize — but the snapshot is a
*persistent* hash array mapped trie ([CHAMP](https://michael.steindorfer.name/publications/oopsla15.pdf)
layout: two bitmaps + popcount-packed arrays, 64-way branching on 6-bit
chunks of the key's FNV-1a hash). Because nodes are immutable, a writer
builds a new version of the trie that *shares* everything except the
O(log n) path from root to the changed slot — a handful of small node
copies instead of a million-entry map copy.

That structural change also removes the writer mutex, making `hamt` the
repo's only fully lock-free design. A write is: load the root, build the
new path off it, publish with one `CompareAndSwap` of a `{root, count}`
pair. Lose the race and you throw the path away and retry against the
winner's root — optimistic concurrency. Two classic lock-free hazards are
handled structurally rather than cleverly: ABA cannot happen because every
publish installs a freshly allocated root the GC cannot recycle while a
competitor still holds the old one, and torn reads cannot happen because
nodes are fully built before the CAS's release/acquire edge publishes them.

What CAS does **not** fix is write *serialization*. Every writer still
targets one pointer (one contended cache line), so writes cannot scale
across cores the way `sharded`'s do — and unlike a mutex, a lost race here
wastes an entire path copy rather than just parking a thread. Lock-free
relocates the cost of contention; it does not remove it. The numbers make
both halves of the story concrete (same 32-core machine as the actor
tables, `-keys=1000000`, 8 cores, mean of 3 passes — a quick pass, not
comparable to the headline tables):

| mix, uniform (8 cores) | sharded | cow | hamt | hamt B/op |
|---|--:|--:|--:|--:|
| read-only (r100) | 26 | **16** | 30 | 0 |
| read-heavy (r90) | **27** | 14,200,000 | 172 | 613 |
| balanced (r50) | **32** | 72,900,000 | 863 | 3,700 |
| write-heavy (r10) | **36** | 125,100,000 | 1,472 | 6,880 |

Reading the columns: against `cow`, the persistent trie delivers exactly
what it promised — writes collapse from 14 *milliseconds* to 172 *nano*seconds
at r90 (five orders of magnitude), while pure reads concede a bit
under 2× (the trie walks ~4 pointer hops where `cow` does one map lookup)
and land at parity with `sharded`. Under zipf skew the read story improves
further: hot paths stay resident in cache, and with no lock to bounce,
`hamt`'s read-only mix (15 ns) *beats* `sharded` (22 ns).

Against `sharded` under write pressure, the single CAS target loses by
6–40×, and the allocation column says why more precisely than the latency
column does. Sequentially, a `hamt` write costs ~2.0 KB of path copy; under
8-core contention the same write burns ~7 KB — the difference is path
copies built against a root that a competing writer replaced first, thrown
away, and rebuilt (plus the GC churn of gigabytes per second of dead
copies). The retries also erase the parallelism itself: write-heavy
throughput at 8 cores (≈0.7 M ops/s) is *below* the single-core rate
(≈0.8 M ops/s). Negative scaling with zero locks — the cleanest
demonstration in this repo that lock-free is a *progress guarantee*, not a
performance one.

There is no free lunch — but for read-mostly workloads that want O(1)
consistent snapshots (`Len` here; in general iteration, range queries, or
point-in-time reads, which no other design in this repo can offer without
stopping the world), the persistent trie occupies a spot none of the
lock-based designs can reach.

#### Does the branching factor matter? (Clojure picks 32)

Clojure's persistent maps famously use 32-way branching; `hamt` defaults
to 64. Wider nodes mean a shallower trie — fewer pointer hops per read,
fewer nodes copied per write — but every copied node is bigger and the
CAS window longer. The width is threaded through the trie internals as a
parameter precisely so this is measurable: `BenchmarkHAMTWidth` sweeps it
(same machine, `-keys=1000000`, 8 cores, uniform, mean of 3; all widths go
through the same parameterized lookup, worth ~2 ns over production `Get`,
so compare the columns to each other, not to the tables above):

| mix, uniform (8 cores) | 8-way | 16-way | 32-way | 64-way |
|---|--:|--:|--:|--:|
| read-only (r100) | 43 | 40 | 34 | **32** |
| read-heavy (r90) | 140 | **121** | 133 | 159 |
| balanced (r50) | 596 | **560** | 598 | 790 |
| write-heavy (r10) | 1,036 | **923** | 1,089 | 1,347 |
| …its garbage (B/op) | 3,931 | 4,111 | 5,110 | 7,040 |

The read row rewards width monotonically — but weakly, because at a
million keys the walk is DRAM-bound and one extra level is one more
overlapped miss (64→32 costs 4%, 64→8 costs 33%). The write rows reward
*narrowness*: halving the node width halves the bytes each copied level
drags along (see the garbage row), which shortens the CAS window and cuts
retries too. The curve bottoms out at 16–32 — narrower still (8-way) and
the added depth starts costing more copies than the thinner nodes save.
Clojure's 32 lands almost exactly at the crossover: versus 64-way it
gives up 4% on pure reads and takes 16–24% off every write mix (and 27%
off the garbage). `hamt` keeps 64 because its niche in this repo is the
read-mostly snapshot case (and the sweep is one `hamtBits` edit to
disagree with); `hamt256`, whose reason to exist is write scaling, commits
to 32 — see below.

#### `hamt256`: sharding the tries

The write table above says contention, not copying, is `hamt`'s real
write problem — so apply the oldest trick in the repo: partition.
[`hamt256`](hamtsharded.go) is 256 independent persistent tries, each
with its own cache-line-padded, CAS-published root. Still not a lock in
sight; there is simply no longer a single pointer every writer fights
over. And sharding pays a second, quieter dividend: each trie holds
1/256th of the keys, so it is ~1.3 levels shallower — writes copy fewer,
smaller nodes even before contention enters.

One subtlety earned its own test: the shard index cannot be the hash's
low bits (`sharded`'s recipe — the trie consumes those, and every trie
root would degenerate into a single-child chain) and cannot be the raw top
byte either (FNV-1a folds entropy into the *low* bits; the top byte is so
poorly mixed that 2,000 sequential keys landed in 21 of 256 shards).
`hamt256` routes on the top byte of `hash × 2⁶⁴/φ` — Fibonacci hashing,
one multiply that spreads the well-mixed low bits upward without
correlating with any chunk the trie uses.

`hamt256` also commits to 32-way tries (its own `hamt256Bits`, decoupled
from `hamt`'s 64): the width sweep's crossover logic applies per shard,
and measured at 64-way first, write-heavy was 312 ns / 1,180 B — the
switch to 32 took another 21% off latency and 29% off garbage for ~4 ns
on pure reads. The shipped configuration:

| mix, uniform (8 cores) | sharded | hamt | hamt256 | hamt256 B/op |
|---|--:|--:|--:|--:|
| read-only (r100) | **26** | 30 | 34 | 0 |
| read-heavy (r90) | **27** | 172 | 65 | 92 |
| balanced (r50) | **32** | 863 | 155 | 465 |
| write-heavy (r10) | **36** | 1,472 | 247 | 841 |

Writes improve 2.6–6× over `hamt`, and the garbage column tells you why:
under a kilobyte per write instead of ~7.6 KB — thinner nodes and
shallower tries account for some, but most of it is the failed-CAS retry
waste simply vanishing (two writers must now collide on the same 1/256th
of the key space inside one CAS window). Under zipf the read side is the
star: 14 ns read-only, faster than every lock-based design in the repo.

What the remaining 2–7× gap to `sharded` buys is worth naming precisely.
It is no longer contention — it is the price of *persistence itself*: a
mutex write mutates a map bucket in place (0 B/op), while a persistent
write must allocate its path and feed the old one to the GC, ~900 B and
6 allocations every time, forever. In exchange, `hamt256` keeps
lock-freedom and per-shard O(1) snapshots — but note what sharding took
away: `hamt`'s single global snapshot (`Len`, point-in-time iteration)
shrank to 256 per-shard snapshots summed at slightly different moments,
the same semantics `sharded` has. The ladder is: `cow` = perfect
snapshots, unusable writes; `hamt` = perfect snapshots, serialized
writes; `hamt256` = shard-local snapshots, scaling writes; `sharded` =
no snapshots, fastest writes. Pick your rung.

### `ctrie`: folding the sharding into the structure

`hamt256` invites one more question: a shard table is just a fixed
256-wide node whose slots can be swapped atomically in place — so why
bolt it on *in front of* the trie instead of building the trie out of
such nodes? Putting a CAS-able cell above **every** branch node is the
I-node (indirection node) idea from Prokopec's concurrent hash trie — the
Ctrie, the structure behind Scala's `TrieMap` — and
[ctrie.go](ctrie.go) implements its core.

A write walks to the deepest I-node its key touches, builds a new version
of that ONE branch node, and CASes the cell. Two things fall out. First,
contention granularity becomes per-branch at every depth — finer than any
fixed shard count, and adaptive. Second, and less obviously, the *path
copy disappears*: `hamt` copies root-to-leaf because the root pointer is
its only mutation point, but here the mutation point sits directly above
the change, so ancestors are untouched and a write allocates O(1) nodes.
(That also kills the need for a bulk-load fast path: n Sets are O(n)
total work.) Two simplifications versus the published Ctrie are
documented in the file: no generation stamps (so no O(1) global snapshot,
and `Len` is an O(n) walk with `sharded`-style moving-count semantics)
and no contraction — deleted structure lingers as husks, which is exactly
what makes a failed CAS safe to retry *locally*: an I-node, once linked,
is never unlinked, so nothing can detach the subtree under a competitor.

The full-sweep numbers ([results/linux/](results/linux/), same machine,
`-keys=1000000`, 8 cores, 5 repetitions — this table is the canonical
recent dataset; earlier per-section tables were quick passes):

| mix, uniform (8 cores) | sharded | hamt | hamt256 | ctrie | ctrie B/op |
|---|--:|--:|--:|--:|--:|
| read-only (r100) | **26** | 31 | 33 | 36 | 0 |
| read-heavy (r90) | **27** | 172 | 63 | 49 | 34 |
| balanced (r50) | **32** | 850 | 170 | 93 | 172 |
| write-heavy (r10) | **35** | 1,524 | 268 | **132** | 310 |

Read the garbage ladder across the three lock-free rungs at write-heavy:
6,900 B (`hamt`: path copy × retry waste) → 841 B (`hamt256`: contention
gone, tries thinner) → **310 B** (`ctrie`: one small C-node per write, no
path). Each rung removed the term the previous experiment isolated. The
read column shows what each I-node costs: every level is two dependent
pointer loads instead of one, worth ~5 ns over `hamt` at this depth.
And the scaling column is the payoff (write-heavy ns/op by cores):

| cores | 2 | 4 | 8 | 16 |
|---|--:|--:|--:|--:|
| hamt | 2,132 | 1,698 | 1,524 | 1,653 |
| hamt256 | 864 | 456 | 268 | 186 |
| ctrie | 522 | 269 | **132** | **80** |
| sharded | 118 | 64 | 35 | 23 |

`hamt` is flat (serialized); `hamt256` and `ctrie` genuinely scale, and
at 16 cores `ctrie` is within 3.6× of `sharded` — the residue being the
price of persistence (310 B of immutable-node garbage per write) plus the
I-node read tax, not contention. The full Ctrie's generation machinery
would buy back the global O(1) snapshot on top of this; that protocol
(GCAS/RDCSS) is a paper's worth of subtlety and is left as the noted
next rung.

### How much does each design rely on friendly key placement?

`dist=zipf` skews *popularity* but leaves *placement* accidental — the
hot keys still hash all over each structure. `BenchmarkAdversarial`
(sweep phase D) attacks placement directly: constant-size 2,048-key
working sets drawn so every key lands in one shard (or an 8-shard
cluster) of a specific design's routing — `lowshard*` targets `sharded`'s
low-bits router (and pins the tries' first chunk), `mixshard*` targets
`hamt256`'s multiplicative router. Write-heavy, 8 cores, ns/op:

| impl | uniform | lowshard1 | lowshard8 | mixshard1 | mixshard8 | worst/uniform |
|---|--:|--:|--:|--:|--:|--:|
| mutex | 62 | 59 | 63 | 62 | 63 | 1.03× |
| sharded | **11** | 77 | 39 | 13 | 12 | **7.1×** |
| hamt | 1,314 | 1,280 | 1,385 | 1,375 | 1,390 | 1.06× |
| hamt256 | 229 | 198 | 189 | 634 | 301 | **2.8×** |
| ctrie | 72 | 78 | 82 | 76 | 74 | **1.13×** |

(Read-only tells the same story where it matters: `sharded` degrades
7.0× under `lowshard1` — a lock convoy on *reads* — while every other
design is flat.)

Three lessons. Each fixed router is blind to the other's attack —
`sharded` shrugs at `mixshard*`, `hamt256` shrugs at `lowshard*` —
because placement sensitivity is a property of the routing function, and
any fixed router has exactly one worst case. Second, `mutex` and `hamt`
are perfectly placement-general the degenerate way: already fully
serialized, nothing to concentrate. Third, `ctrie` is the only design
that is both fast and flat (worst case 1.13×): it has no routing layer to
attack — clustered keys just concentrate traffic deeper in the tree,
where the per-branch CAS granularity absorbs it. Its worst attacked cell
(82 ns) roughly matches `sharded`'s (77 ns), while unattacked `sharded`
is 7× faster: that is the peak-performance-versus-generality trade in one
row pair.

The trade sharpens with concurrency. Rerunning the attacked column at
higher thread counts (write-heavy, ns/op):

| threads | sharded attacked | ctrie attacked | sharded uniform |
|---|--:|--:|--:|
| 8 | 77 | 80 | 15 |
| 16 | 99 | **63** | 15 |
| 32 | **226** | **58** | 11 |

The convoy on the hot shard's mutex worsens *superlinearly* as waiters
stack up (5× over its own baseline at 8 threads, 20× at 32), while
attacked `ctrie` keeps improving — clustered keys simply engage more
I-nodes deeper down. The crossover is at ~16 threads; at 32, `ctrie` is
4× ahead under attack while still ~5× behind on friendly uniform keys.
This is the small-scale shadow of a familiar distributed-systems fact:
consistent hashing across machines is `sharded` writ large, hot-shard
pathology included, and the fix there — adaptive partitioning, splitting
ranges where load concentrates — is ctrie's granularity idea at fleet
scale. The bigger the machine (or fleet), the more the worst case, not
the average, is what you feel.

### The value-size dial: from sync-bound to memory-bound

`BenchmarkValueSize` (sweep phase E) runs the realistic value regime —
distinct per-key values, a fresh allocation per write, hit reads touching
the value at cache-line stride like a serializer (see the measurement
note above for the mechanisms: LLC/TLB dilution, displacement, allocator
size classes, GC assists). Value size then acts as a dial that fades
synchronization out of the picture (8 cores, 100k keys, ns/op):

| read-only | 16 B | 256 B | 4 KB |   | write-heavy | 16 B | 256 B | 4 KB |
|---|--:|--:|--:|---|---|--:|--:|--:|
| syncXmap | 4.2 | 7.5 | 99 |   | syncXmap | 19 | 40 | 357 |
| cow | 3.8 | 7.4 | 98 |   | sharded | 19 | 39 | 368 |
| sharded | 8.8 | 13 | 100 |   | syncmap | 43 | 70 | 387 |
| hamt | 6.5 | 12 | 102 |   | ctrie | 73 | 88 | 440 |
| ctrie | 8.6 | 15 | 109 |   | hamt256 | 173 | 172 | 501 |
| syncmap | 7.8 | 16 | 116 |   | mutex | 96 | 134 | 576 |
| mutex | 68 | 122 | 460 |   | hamt | 1,027 | 1,010 | 1,077 |

At 16 B the algorithms separate by an order of magnitude, exactly as in
the core grid. At 4 KB every competent design converges onto the memory
wall: read-only lands at 98–116 ns for *everything* but `mutex` (a 26×
spread compressed to 18%), and write-heavy compresses a 19× spread to
~1.6× as the ~3.7 KB per-write allocate/zero/GC bill becomes the common
term (`hamt` stays out at ~1 µs — its node churn is independent of value
size — and `cow` stays at milliseconds). This is the in-process version
of the Track B lesson: past a certain payload size the synchronization
strategy stops being the bottleneck, and the interesting engineering
moves to memory — though the *garbage* columns still differ by design,
which is what a longer-running service will feel as GC pressure.

### `actor`: sharding without mutexes

`actor` answers the question "sharding already removes contention — do we
need locks at all?" It keeps `sharded`'s 256-way key partitioning but
replaces each shard's mutex with a dedicated goroutine that *owns* the
shard's map. Every operation is a message on that shard's buffered channel;
the map is created inside the goroutine and never escapes, so mutual
exclusion is structural — there is not a single lock in [actor.go](actor.go),
only channels ("share memory by communicating").

Design points:

- **Writes are fire-and-forget.** `Set`/`Delete` return once the request is
  enqueued. A key always maps to one shard and a shard's channel is FIFO, so
  any `Get` issued after a `Set` returns queues behind it and observes it —
  the linearization point is the enqueue.
- **Nothing is copied.** Requests carry string *headers* (16 B) through the
  channel; key/value bytes never move. Steady state is 0 allocs/op: `Get`
  reply channels come from a per-shard free list that is itself a channel,
  so even the pool is lock-free.
- **Reads pay a round trip.** A `Get` is two channel handoffs and usually a
  goroutine wakeup; that latency is irreducible in this design.

Measured (32-core machine, `-keys=100000`, single quick pass — not
comparable to the headline tables):

| mix, dist (8 cores) | sharded | actor | ratio |
|---|--:|--:|--:|
| read-only, uniform | 8.3 ns | 94 ns | 11× slower |
| write-heavy (r10), uniform | 11.7 ns | 49 ns | 4× slower |
| write-heavy (r10), zipf | 40.5 ns | 47 ns | **≈parity** |

The moral: channels are built on the same runtime primitives as mutexes, so
"no locks" relocates synchronization rather than removing it, and reads
additionally pay scheduler latency. The actor design closes the gap only
where its asynchrony helps — hot-key *write* skew, where fire-and-forget
enqueues absorb bursts that would serialize on a hot shard's mutex. Its real
selling points are elsewhere: no lock-ordering discipline, natural
backpressure, and trivial extension to operations that would be awkward
under a lock (atomic read-modify-write, per-shard TTL sweeps) — the shard
goroutine can do anything to its map with no further synchronization.

### Extending the protocol: batched, coalescing gets

The batched multi-get — `GetBatch(keys) []BatchResult`, positional, with
duplicate hot keys coalescing into one shard message — makes the
extensibility claim concrete. It is deliberately split across three files
so you can diff what the feature costs each design:

- [batch.go](batch.go) — the shared contract and the key grouping: a
  two-pass counting sort that permutes key *positions* by shard. Keys are
  never moved or copied; consumers index back into the caller's slice. The
  grouping is also the coalescing step — every occurrence of a hot key
  lands in one shard's contiguous range.
- [batch_actor.go](batch_actor.go) — the actor's version: send one message
  per shard touched, wait for the acks. **Every line is sequential code.**
  The shard goroutines run their partitions concurrently because they
  already exist — the topology is a standing worker pool. The change to the
  actor core ([actor.go](actor.go)) is three request fields and a six-line
  `case`.
- [batch_sharded.go](batch_sharded.go) — the mutex design's version, twice.
  The serial one (lock each shard once) is trivial. The parallel one has to
  build, per call, what the actor was born with: spawn workers, hand out
  shards via an atomic cursor, lock each shard against writers, join on a
  WaitGroup. The imports are the tell — `runtime`, `sync`, and
  `sync/atomic` appear only in this file's machinery. It is real concurrent
  code with real ways to be wrong, and it must be rebuilt (and re-reviewed)
  for every future operation that wants parallelism; the actor gets each
  new parallel operation as plain sequential code on the same protocol.

Measured (same 32-core machine, `-keys=1000000` so lookups actually miss
cache, uniform, single caller, `ns/key`; loop = one `Get` per key):

| batch size | sharded loop | sharded batch | sharded parbatch | actor loop | actor batch |
|---|--:|--:|--:|--:|--:|
| 16 | 124 | 176 | 544 | 451 | 551 |
| 256 | 118 | **98** | 109 | 453 | 442 |
| 4,096 | 114 | 92 | **40** | 466 | 66 |
| 16,384 | 121 | 98 | **34** | 440 | **34** |

Read it column-wise. The serial columns are *flat* — per-key cost does not
depend on batch size, only on how cold the touched memory is (see the
measurement note below). Serial batching buys a steady ~20% over the loop
from lock amortization and visiting each shard's keys together. The two
parallel columns are pure fixed-cost-amortization curves: a batch pays a
fan-out toll up front (~32 goroutine spawns for `parbatch`, ~250 shard
wakeups for `actor`), which crushes tiny batches, breaks even in the
mid-hundreds, and vanishes at scale — where both parallel designs converge
to an identical **34 ns/key, ~3× the serial batch**, because both are then
bound by the same DRAM misses overlapped across the same cores. The actor
lags `parbatch` at mid sizes (250 wakeups cost more than 32 spawns) and
matches it exactly at the top.

So the mutex design *can* match the actor's batch speed — the point is what
it took: forty lines of bespoke fork/join concurrency versus a sequential
method on an existing protocol.

The persistent designs answer the same question a third way
([batch_persistent.go](batch_persistent.go)), and the numbers say
something none of the columns above could: batching only pays where the
design has a per-call cost to amortize — or a snapshot to share. Measured
(32-core machine, `-keys=1000000`, uniform, 8 cores, ns/key at size
4,096):

| | loop | batch | what batch amortizes |
|---|--:|--:|---|
| sharded | 142 | 118 | 256 lock acquisitions — wins |
| cow | 92 | 88 | one map-pointer load — nothing to win |
| hamt | 232 | 180 | one root load + per-Get overhead — modest win |
| hamt256 | 210 | **313** | grouping costs MORE than 4,096 root loads — loses |
| ctrie | 256 | 263 | nothing — parity, as predicted |

`hamt256`'s batch is deliberately kept as the honest negative result: it
runs the same counting-sort grouping as `sharded`'s (with its own router
— using `sharded`'s low-bits routing here would silently consult the
wrong shards), but where `sharded` recoups the grouping by taking 256
locks instead of 4,096, `hamt256` recoups only one 1-ns atomic load per
key. The grouping is pure overhead unless you want what it actually
buys: per-shard consistency.

Which is the real story: for `cow` and `hamt`, `GetBatch` is not a
performance feature at all but a *semantic* one — every answer comes from
ONE atomic snapshot, a multi-key consistent read that no lock-striped
design can offer at any price (`sharded`'s batch reads shard 3's keys,
then, while writers keep writing, shard 7's). TestGetBatchSnapshot makes
it concrete: a writer bumps k1 then k2 forever, so any true snapshot must
see version(k1) ≥ version(k2); the snapshot batches never violate it,
while a per-shard batch (or a plain Get loop) can. That is `hamt`'s niche
stated one more way: the only fast structure here whose multi-key reads
are transactions.

> **Measurement note: hold the working set constant.** The benchmark cycles
> prebuilt batches, so its memory working set is (batch count) × (batch
> size) distinct keys. An earlier version fixed the *count* at 32, and
> small-batch numbers looked 3× better than large ones — 8K recycled keys
> whose map buckets sat warm in L2/L3, versus 500K keys thrashing DRAM. The
> per-key cost wasn't growing with batch size; it was growing with the
> sampled working set. The giveaway was `mode=loop` — no batch machinery at
> all — "growing" identically. BenchmarkGetBatch now holds the total draw
> count at 2¹⁹ across sizes so the axis measures batching, not cache
> residency.

Reproduce with:

```sh
go test -bench=BenchmarkGetBatch -benchmem -keys=1000000
```

## Measurement design

Two tracks, deliberately separated:

- **Track A — in-process micro-benchmarks (the real measurement).**
  `testing.B` + `b.RunParallel`, calling `Get`/`Set` directly. This is where
  the synchronization strategies actually separate (nanosecond scale).
- **Track B — end-to-end HTTP (reality check).** [cmd/server](cmd/server)
  exposes a cache over JSON. Drive it with a load generator; expect the
  implementations to converge, because HTTP + JSON cost dwarfs the lock
  differences. Use a coordinated-omission-aware tool (`fortio`, `wrk2`) for
  honest tail latencies.

### Benchmark axes

- **Implementation** — the table above (`naive` only in the sequential bench).
- **Read/write mix** — `r100`, `r90`, `r50`, `r10` (read fraction).
- **Access distribution** — `uniform` and `zipf` (s=1.1 hot-key skew). This is
  *popularity* skew; where the hot keys land in each structure stays accidental.
- **Key placement** — `BenchmarkAdversarial` (sweep phase D) attacks placement
  directly: constant-size working sets chosen so every key lands in one (or a
  cluster of 8) of a specific design's shards/subtrees. This measures how much
  each design depends on keys arriving in friendly patterns — its *data
  generality* — separately for the low-bits router (`sharded`, and the tries'
  first chunk) and the multiplicative router (`hamt256`).
- **Value size** — `BenchmarkValueSize` (sweep phase E); see the note below for
  why it is a separate regime rather than an axis of the core grid.
- **GOMAXPROCS** — via the `-cpu` flag; this is where contention scaling shows.
- **Key cardinality / length** — `-keys` and `-keylen` flags.

> **Value size is a regime, not a knob.** In the core grid all Sets share ONE
> immutable value string and nobody reads value bytes, so `Set` stores a
> 16-byte header and varying the size changes nothing (measured: 64 B and
> 16 KB identical within noise, 0 B/op) — which is why the grid fixes it at
> 64 B. Real workloads are different, and not just for the GC: with distinct
> per-key values the heap carries keys × size bytes, and value size sets the
> whole *heap geometry*. Structure nodes interleave with values, so pointer
> walks lose LLC density and dTLB reach as values grow; serving a read means
> serializing the value, which costs bandwidth and evicts structure from
> cache (pointer-dense designs have the most to lose); Go's size-class
> allocator has real span/fragmentation dynamics that differ between 16 B
> and 4 KB objects; and allocation-heavy writers pay GC mark assists in-line
> while mark cycles scan each design's pointers. `BenchmarkValueSize` models
> the full regime — distinct values at prefill, a fresh allocation per Set,
> and hit Gets touching the value at cache-line stride like a serializer —
> so the size axis acts as a dial from synchronization-bound (16 B) to
> memory-bound (4 KB), and the interesting data is which designs decay
> fastest along the way.

## Results

Headline run: 1,000,000 keys, `-count=10`, GOMAXPROCS swept 1→8, on a 20-core
i7-14700K, **pinned to one thread per physical P-core** (the chip is hybrid; see
[Pinning](#pinning-to-p-cores)). Full data in [results/](results/)
(`summary.txt`, `by-impl.txt`); regenerate the figures with `go run ./cmd/charts`.

A second complete dataset lives in [results/linux/](results/linux/): the full
five-phase sweep (core grid incl. `hamt`/`hamt256`/`ctrie`, adversarial
placement, value-size regime) on the 32-core Linux machine, 1M keys,
`-count=5`, cores 1→16. The trie-family and generality tables above the actor
section come from it. The two datasets are different machines — compare within
a dataset, not across.

### Throughput vs cores, by read/write mix (uniform)

![Throughput vs cores, uniform distribution](charts/throughput_by_mix_uniform.png)

`sharded` and `cow` scale up with cores; `mutex` is flat-to-declining (no read
parallelism, plus lock cache-line contention). `cow` owns read-only and
collapses to ≈0 throughput once writes appear (off-scale — see the table).

### Scaling efficiency (read-only, uniform)

![Scaling efficiency, read-only](charts/scaling_efficiency_r100_uniform.png)

Speedup vs each design's own 1-core baseline. `mutex` is *below* 1× (negative
scaling); `rwmutex` plateaus ~2× (the reader-counter wall); `sharded` hits 6.9×;
`cow`/`syncmap` track or slightly exceed the ideal 8× line — though `syncmap`'s
near-linear slope flatters a poor 1-core baseline (great scaling, still mediocre
absolute).

### Effect of skew (Zipfian, s=1.1)

![Throughput vs cores, zipf distribution](charts/throughput_by_mix_zipf.png)

![Skew speedup at 8 cores](charts/skew_speedup_8cores.png)

Skew is not uniformly "worse": reads get *faster* almost everywhere (hot keys
stay in CPU cache), but `sharded`'s balanced mix gets *slower* (0.82×) — hot keys
collide on a few shards while the rest sit idle. `cow` is the control: its
balanced mix is flat (1.03×), because it copies the whole map on every write
regardless of key, so the distribution can't change its write cost.

### Shard count: why 256?

![Shard count vs throughput](charts/shard_count_8cores.png)

Sweeping the shard count (balanced mix, uniform, 8 cores): one lock → 256 is a
9× throughput jump (4.6 → 43 Mops/s), then it flattens — 1024 buys +13%, 4096
only +18%, for 4×/16× the maps + mutexes. 256 sits in the knee. Reproduce with
`go test -bench=BenchmarkShardCount -cpu=8`.

### Latency at 8 cores (ns/op, lower is better)

Uniform distribution:

| mix | mutex | rwmutex | syncmap | sharded | cow |
|---|--:|--:|--:|--:|--:|
| read-only (r100) | 168 | 53 | 30 | 21 | **11.5** |
| read-heavy (r90) | 168 | 259 | 37 | **22** | 12,000,000 |
| balanced (r50) | 190 | 282 | 57 | **24** | 46,500,000 |
| write-heavy (r10) | 208 | 222 | 73 | **25** | 82,500,000 |

Zipfian distribution (s=1.1):

| mix | mutex | rwmutex | syncmap | sharded | cow |
|---|--:|--:|--:|--:|--:|
| read-only (r100) | 106 | 49 | 16 | 17 | **7** |
| read-heavy (r90) | 112 | 225 | 24 | **24** | 9,040,000 |
| balanced (r50) | 126 | 183 | 46 | **29** | 45,100,000 |
| write-heavy (r10) | 131 | 142 | 68 | **32** | 84,000,000 |

The whole column is ns: `cow`'s eight-figure write cells (≈82 ms per `Set`) are
real — it copies the entire million-entry map on every write. Overall geomean vs
the `mutex` baseline: `sharded` −58 %, `syncmap` −15 %, `rwmutex` +6 %, `cow` off
the chart (writes dominate).

## Running

```sh
# Correctness (fast):
go test -run Test ./...

# Confirm the concurrent implementations are race-free:
go test -race -run TestConcurrentSmoke

# See that the naive map is NOT thread-safe (expected to crash / fail):
INMEMCACHE_RACE_DEMO=1 go test -race -run TestNaiveRace

# Full Track A sweep across core counts (the headline numbers):
go test -bench=BenchmarkCache -benchmem -cpu=1,2,4,8 -keys=1000000

# Isolate one axis, e.g. zipf access across cores:
go test -bench='BenchmarkCache/impl=sharded/dist=zipf/' -cpu=1,2,4,8

# Uncontended per-op baseline (includes naive):
go test -bench=BenchmarkSequential -benchmem

# Placement generality (needs -keys=1000000 to fill the adversarial sets):
go test -bench=BenchmarkAdversarial -cpu=8 -count=3 -keys=1000000

# Value-size regime (distinct values, per-write allocation, serializer reads):
go test -bench=BenchmarkValueSize -cpu=8 -count=3

# Trie branching-factor sweep:
go test -bench=BenchmarkHAMTWidth -cpu=8 -count=3 -keys=1000000

# Track B HTTP server:
go run ./cmd/server -impl=sharded -addr=:8080
```

### Pinning to P-cores

The published numbers were measured on a hybrid CPU (8 performance + 12
efficiency cores). Unpinned, the OS scheduler can place benchmark goroutines on
E-cores or hyperthread siblings as GOMAXPROCS rises and migrate them mid-run,
which confounds the scaling curves. To avoid that, the benchmark pins itself to
a processor-affinity mask given by `INMEMCACHE_AFFINITY` (a `TestMain` in
[affinity_windows_test.go](affinity_windows_test.go) calls
`SetProcessAffinityMask` before any benchmark runs).

Find the right mask for your machine with [cmd/cpuinfo](cmd/cpuinfo), which reads
the kernel's per-logical-processor `EfficiencyClass`:

```sh
go run ./cmd/cpuinfo
# -> e.g. "AFFINITY mask (1/P-core): 0x5555" on an i7-14700K
```

Then pass it to any run (it propagates to each `go test` child):

```sh
INMEMCACHE_AFFINITY=0x5555 KEYS=1000000 COUNT=10 CPU=1,2,4,8 bash sweep.sh
```

The process logs `[affinity] requested=0x5555 set_ok=true effective=0x5555` to
stderr so you can confirm the pin took. Pinning is Windows-only and a no-op when
the env var is unset (so the benchmark runs unmodified on any platform).

### Statistical summary with benchstat

The sweep is run with repetition (`-count`) and summarized with
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat), which reports
means with variation and significance tests — the rigor a single `-bench` run
lacks. Two runners:

- [sweep.sh](sweep.sh) — **the publication runner** (used for the numbers above).
  Runs in three phases and merges them, measuring `cow`'s O(keys) write cells
  with a small fixed iteration count (they are ~10⁶× slower, so a few samples
  suffice) while everything else gets precise time-based measurement. Streams
  results live to `results/bench.txt`.
- [bench.ps1](bench.ps1) — a simpler single-pass PowerShell alternative for
  quick local runs.

```sh
# Publication sweep (bash):
KEYS=1000000 COUNT=10 CPU=1,2,4,8 bash sweep.sh
```
```powershell
# Quick single-pass check (PowerShell):
.\bench.ps1 -Keys 5000 -Count 6 -Cpu 4 -Benchtime 100ms
```

Both write three files to `results/`:

- `bench.txt`   — raw `go test -bench` output (UTF-8, re-readable by benchstat).
- `summary.txt` — per-benchmark mean ± coefficient of variation.
- `by-impl.txt` — implementations pivoted into columns (`benchstat -col /impl`),
  with % delta and p-values vs. the baseline implementation.

The `impl=…/dist=…/mix=…` sub-benchmark naming is what lets benchstat pivot any
axis; e.g. `benchstat -col /mix results/bench.txt` compares mixes instead. Aim
for variation under ~5%; if it's higher, raise `-benchtime` and `-Count`, and
close background apps.

> One-off install of benchstat:
> `go install golang.org/x/perf/cmd/benchstat@latest`

> Memory note: because all keys share one value buffer, memory is modest —
> roughly `keys * (key length + ~48 B map overhead)`, e.g. a few hundred MB at
> `-keys=1000000`. (The `cow` write path transiently allocates a second copy
> of the map's headers.)

### Reproducibility / methodology notes

- Each benchmark goroutine uses its own `*rand.Rand` (seeded from a counter),
  so there is no shared-RNG lock contention polluting the numbers, and runs
  are deterministic.
- Per-op RNG cost is constant across implementations, so it does not affect
  their *relative* ranking.
- Never benchmark with `-race` on; it changes timings by 5–20×. Use it only
  for the correctness passes above.
- `cow` writes are O(n); write-heavy `cow` benchmarks are intentionally slow
  and will report few iterations. That is the honest result, not a bug.
