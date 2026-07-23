package cache

import (
	"runtime"
	"sync/atomic"
)

// actorQueueDepth is the capacity of each shard's request channel. The
// buffer is what lets writers fire-and-forget: a Set is just a struct copy
// into the ring buffer, with no handoff to the shard goroutine on the
// caller's critical path. A full queue is natural backpressure.
const actorQueueDepth = 64

// actorReplyPool is the capacity of each shard's free list of reply
// channels. It only needs to cover the Gets in flight on one shard at a
// time; when the free list runs dry a Get allocates a fresh channel, and
// when it is full a returned channel is dropped for the GC.
const actorReplyPool = 8

type actorOp uint8

const (
	actorGet actorOp = iota
	actorSet
	actorDelete
	actorLen
	actorLoad
	actorGetBatch
)

// actorRequest is the message sent to a shard goroutine. Strings travel as
// 16-byte headers; the underlying key/value bytes are never copied.
type actorRequest struct {
	op     actorOp
	key    string
	value  string
	items  map[string]string  // actorLoad only
	batch  *actorBatch        // actorGetBatch only; see batch.go
	lo, hi int                // actorGetBatch only: batch.idxs[lo:hi] is this shard's work
	resp   chan actorResponse // nil for fire-and-forget writes
}

type actorResponse struct {
	value string
	ok    bool
	n     int
}

// Actor is the share-memory-by-communicating take on sharding: the same
// 256-way key partitioning as Sharded, but instead of a mutex per shard,
// each shard's map is owned by a dedicated goroutine and every operation is
// a message to it. The map is created inside the shard goroutine and never
// escapes, so mutual exclusion is structural — there is not a single lock
// in this file. (The synchronization has not vanished: it lives in the
// channel runtime, which the benchmarks price honestly.)
//
// Writes are asynchronous: Set and Delete return once the request is
// enqueued. Because a key always maps to one shard and a shard's channel is
// FIFO, any Get issued after a Set returns is queued behind it and observes
// it — the linearization point is the enqueue, so callers cannot tell the
// apply is deferred.
type Actor struct {
	reqs []chan actorRequest
	// replies[i] is a free list of reply channels for shard i, itself a
	// channel so the pool needs no lock either.
	replies []chan chan actorResponse
	stopped *atomic.Bool
}

func NewActor() *Actor {
	c := &Actor{
		reqs:    make([]chan actorRequest, shardCount),
		replies: make([]chan chan actorResponse, shardCount),
		stopped: new(atomic.Bool),
	}
	for i := range c.reqs {
		c.reqs[i] = make(chan actorRequest, actorQueueDepth)
		c.replies[i] = make(chan chan actorResponse, actorReplyPool)
		go actorShard(c.reqs[i])
	}
	// Without this an abandoned Actor would leak shardCount goroutines
	// (and the maps they own) forever: each shard goroutine blocks on a
	// channel it keeps reachable. The cleanup closure must not capture c
	// itself or c could never become unreachable.
	reqs, stopped := c.reqs, c.stopped
	runtime.AddCleanup(c, func(struct{}) { actorStop(stopped, reqs) }, struct{}{})
	return c
}

// actorShard owns one shard's map. It is the only goroutine that ever
// touches m. Reply sends never block: reply channels have capacity 1 and
// arrive empty (Get drains exactly one response before pooling; Len and
// Load size their channel to the fan-out).
func actorShard(reqs <-chan actorRequest) {
	m := make(map[string]string)
	for r := range reqs {
		switch r.op {
		case actorGet:
			v, ok := m[r.key]
			r.resp <- actorResponse{value: v, ok: ok}
		case actorSet:
			m[r.key] = r.value
		case actorDelete:
			delete(m, r.key)
		case actorLen:
			r.resp <- actorResponse{n: len(m)}
		case actorLoad:
			for k, v := range r.items {
				m[k] = v
			}
			r.resp <- actorResponse{}
		case actorGetBatch:
			b := r.batch
			for _, i := range b.idxs[r.lo:r.hi] {
				v, ok := m[b.keys[i]]
				b.out[i] = BatchResult{Value: v, OK: ok}
			}
			b.resp <- actorResponse{}
		}
	}
}

func (c *Actor) Get(key string) (string, bool) {
	i := fnv1a(key) & (shardCount - 1)
	var resp chan actorResponse
	select {
	case resp = <-c.replies[i]:
	default:
		resp = make(chan actorResponse, 1)
	}
	c.reqs[i] <- actorRequest{op: actorGet, key: key, resp: resp}
	r := <-resp
	select {
	case c.replies[i] <- resp:
	default:
	}
	return r.value, r.ok
}

func (c *Actor) Set(key, value string) {
	c.reqs[fnv1a(key)&(shardCount-1)] <- actorRequest{op: actorSet, key: key, value: value}
}

func (c *Actor) Delete(key string) {
	c.reqs[fnv1a(key)&(shardCount-1)] <- actorRequest{op: actorDelete, key: key}
}

func (c *Actor) Len() int {
	resp := make(chan actorResponse, shardCount)
	for _, ch := range c.reqs {
		ch <- actorRequest{op: actorLen, resp: resp}
	}
	n := 0
	for range c.reqs {
		n += (<-resp).n
	}
	return n
}

// Load implements BulkLoader. Items are pre-split into per-shard maps here
// (string headers only) so each shard goroutine ingests its partition in
// one message.
func (c *Actor) Load(items map[string]string) {
	parts := make([]map[string]string, shardCount)
	for i := range parts {
		parts[i] = make(map[string]string, len(items)/shardCount+1)
	}
	for k, v := range items {
		parts[fnv1a(k)&(shardCount-1)][k] = v
	}
	resp := make(chan actorResponse, shardCount)
	for i, p := range parts {
		c.reqs[i] <- actorRequest{op: actorLoad, items: p, resp: resp}
	}
	for range parts {
		<-resp
	}
}

// Close stops the shard goroutines. Calling it is optional — an abandoned
// Actor cleans up after itself when collected — but it must not race with
// other method calls: any operation after Close panics on a closed channel.
func (c *Actor) Close() { actorStop(c.stopped, c.reqs) }

func actorStop(stopped *atomic.Bool, reqs []chan actorRequest) {
	if !stopped.CompareAndSwap(false, true) {
		return
	}
	for _, ch := range reqs {
		close(ch)
	}
}
