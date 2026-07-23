package thumbs

import (
	"container/heap"
	"context"
	"sync"
)

// Priority tiers for generation slots. A LOWER tier value is HIGHER priority: an
// interactive request (a tile in or near the viewport the browser is asking for
// right now) always preempts warmer work (bulk pre-generation) for the next free
// slot. Within a tier, ordering differs by design (see reqHeap.Less):
//   - interactive: newest-first (LIFO) — when the user navigates to a fresh view,
//     its tiles must jump ahead of the previous view's now-irrelevant backlog.
//   - warmer: oldest-first (FIFO) — the warm-up marches deterministically through
//     its ID list, so it must preserve submission (iteration) order.
const (
	tierInteractive = 0
	tierWarm        = 1
)

// schedReq is one goroutine's pending request for a generation slot. Exactly one
// goroutine owns a schedReq; the scheduler mutates tier/seq/index/granted only
// under its mutex, and the owner only reads ctx (immutable) and blocks on ready,
// so the struct is race-free without its own lock.
type schedReq struct {
	tier    int             // tierInteractive | tierWarm; may be promoted by bump
	seq     int64           // recency stamp from the scheduler's monotonic counter
	ctx     context.Context // request ctx; governs the QUEUE-WAIT only (see Cache.ensure)
	ready   chan struct{}   // closed by the scheduler when this req is granted a slot
	granted bool            // set (under mu) when a slot has been handed to this req
	index   int             // heap index while queued, or -1 when not enqueued
}

// reqHeap is a priority queue of pending slot requests. The top (index 0) is the
// request the next free slot should go to.
type reqHeap []*schedReq

func (h reqHeap) Len() int { return len(h) }

func (h reqHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.tier != b.tier {
		return a.tier < b.tier // interactive (0) outranks warmer (1)
	}
	if a.tier == tierInteractive {
		return a.seq > b.seq // LIFO: most recently requested first
	}
	return a.seq < b.seq // warmer FIFO: oldest submission first
}

func (h reqHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *reqHeap) Push(x any) {
	r := x.(*schedReq)
	r.index = len(*h)
	*h = append(*h, r)
}

func (h *reqHeap) Pop() any {
	old := *h
	n := len(old)
	r := old[n-1]
	old[n-1] = nil
	r.index = -1
	*h = old[:n-1]
	return r
}

// scheduler is a two-tier, priority "weighted semaphore" bounding how many
// thumbnail generations run at once. It does NOT own worker goroutines: the
// caller that acquires a slot runs the generation itself and then releases,
// which keeps the concurrency budget honest while adding no background
// goroutines to leak across library switches. Priority ordering lives entirely
// in reqHeap; the scheduler only grants free slots to the current heap top.
type scheduler struct {
	mu       sync.Mutex
	capacity int
	inUse    int
	seq      int64
	pending  reqHeap
}

// newScheduler returns a scheduler bounding concurrency to capacity (min 1).
func newScheduler(capacity int) *scheduler {
	if capacity < 1 {
		capacity = 1
	}
	return &scheduler{capacity: capacity}
}

// nextSeq returns a fresh monotonic recency stamp (caller holds mu).
func (s *scheduler) nextSeq() int64 {
	s.seq++
	return s.seq
}

// acquire parks r until it is granted a generation slot, returning nil once a
// slot is held (the caller MUST pair a granted acquire with release). If the
// request's context is cancelled while r is still WAITING IN THE QUEUE, acquire
// discards it and returns r.ctx.Err() without ever consuming a slot — this is
// the abandonment path for a tile that scrolled off before generation began. A
// nil ctx never abandons.
func (s *scheduler) acquire(r *schedReq) error {
	s.mu.Lock()
	if r.ctx != nil && r.ctx.Err() != nil {
		s.mu.Unlock()
		return r.ctx.Err()
	}
	r.seq = s.nextSeq()
	r.index = -1
	// Fast path: a slot is free (and by the scheduler's invariant nothing is
	// queued when that is true) — take it without touching the heap.
	if s.inUse < s.capacity && s.pending.Len() == 0 {
		s.inUse++
		r.granted = true
		s.mu.Unlock()
		return nil
	}
	r.ready = make(chan struct{})
	heap.Push(&s.pending, r)
	s.mu.Unlock()

	if r.ctx == nil {
		<-r.ready
		return nil
	}
	select {
	case <-r.ready:
		return nil
	case <-r.ctx.Done():
		s.mu.Lock()
		if r.granted {
			// Raced with a grant that fired just before the ctx did: honor the
			// grant (the slot is ours) so the caller still releases it.
			s.mu.Unlock()
			return nil
		}
		if r.index >= 0 {
			heap.Remove(&s.pending, r.index)
		}
		s.mu.Unlock()
		return r.ctx.Err()
	}
}

// release returns a slot to the pool and hands it to the highest-priority
// waiter, if any.
func (s *scheduler) release() {
	s.mu.Lock()
	s.inUse--
	s.pump()
	s.mu.Unlock()
}

// pump grants free slots to waiters in priority order (caller holds mu). A queued
// request whose context was cancelled before it reached the front is dropped
// here without consuming a slot or being granted, so its generator never runs;
// its acquire() goroutine observes ctx.Done and returns the cancellation error.
func (s *scheduler) pump() {
	for s.inUse < s.capacity && s.pending.Len() > 0 {
		r := s.pending[0]
		if r.ctx != nil && r.ctx.Err() != nil {
			heap.Pop(&s.pending) // abandoned while queued: discard, do not grant
			continue
		}
		heap.Pop(&s.pending)
		s.inUse++
		r.granted = true
		close(r.ready)
	}
}

// bump promotes r to the interactive tier and refreshes its recency so it jumps
// to the front of the queue. It backs two behaviors that share one mechanism:
//   - a singleflight re-request for an already-queued generation (a tile
//     re-entered the viewport) bumps that generation to the top; and
//   - an interactive request coalescing onto a queued WARMER generation promotes
//     it into the interactive tier so it preempts the rest of the warm-up.
//
// It is a no-op once r has been granted a slot or is no longer queued (its work
// is already underway and cannot be reordered).
func (s *scheduler) bump(r *schedReq) {
	if r == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.granted || r.index < 0 {
		return
	}
	r.tier = tierInteractive
	r.seq = s.nextSeq()
	heap.Fix(&s.pending, r.index)
}

// setCapacity changes the concurrency bound at runtime (min 1). Increasing it
// immediately grants slots to any waiters; decreasing it takes effect as
// in-flight generations release (they are never interrupted).
func (s *scheduler) setCapacity(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.capacity = n
	s.pump()
	s.mu.Unlock()
}

// capacityValue returns the current concurrency bound.
func (s *scheduler) capacityValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capacity
}

// pendingLen reports how many requests are currently queued (test introspection).
func (s *scheduler) pendingLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending.Len()
}

// seqCounter returns the current value of the monotonic recency counter (test
// introspection: a bump advances it, so a test can wait for a re-request's bump
// to land without a fixed sleep).
func (s *scheduler) seqCounter() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}
