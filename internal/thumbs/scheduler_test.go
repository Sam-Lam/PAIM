package thumbs

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// waitPending blocks until the scheduler has exactly n queued requests, so a
// test can enforce a deterministic enqueue order before releasing a slot. It
// fails the test rather than hanging if that count is never reached.
func waitPending(t *testing.T, s *scheduler, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.pendingLen() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending never reached %d (is %d)", n, s.pendingLen())
}

// occupy takes the scheduler's single slot with a request that never releases
// until the returned func is called, so subsequent acquires park in the queue.
func occupy(t *testing.T, s *scheduler) {
	t.Helper()
	blk := &schedReq{tier: tierInteractive, ctx: context.Background()}
	if err := s.acquire(blk); err != nil {
		t.Fatalf("occupy: %v", err)
	}
}

// TestSchedulerLIFOInteractive verifies interactive requests are served
// newest-first: queue A, B, C behind a stalled worker and they complete C, B, A.
func TestSchedulerLIFOInteractive(t *testing.T) {
	s := newScheduler(1)
	occupy(t, s) // hold the only slot

	order := make(chan string, 3)
	launch := func(name string) {
		go func() {
			r := &schedReq{tier: tierInteractive, ctx: context.Background()}
			_ = s.acquire(r)
			order <- name
			s.release()
		}()
	}
	launch("A")
	waitPending(t, s, 1)
	launch("B")
	waitPending(t, s, 2)
	launch("C")
	waitPending(t, s, 3)

	s.release() // free the held slot; the queued reqs drain newest-first
	got := []string{<-order, <-order, <-order}
	want := []string{"C", "B", "A"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drain order = %v, want %v (LIFO)", got, want)
	}
}

// TestSchedulerInteractivePreemptsWarm verifies an interactive request jumps
// ahead of already-queued warmer work, and warmer work keeps FIFO order.
func TestSchedulerInteractivePreemptsWarm(t *testing.T) {
	s := newScheduler(1)
	occupy(t, s)

	order := make(chan string, 3)
	launch := func(name string, tier int) {
		go func() {
			r := &schedReq{tier: tier, ctx: context.Background()}
			_ = s.acquire(r)
			order <- name
			s.release()
		}()
	}
	launch("W1", tierWarm)
	waitPending(t, s, 1)
	launch("W2", tierWarm)
	waitPending(t, s, 2)
	launch("I", tierInteractive)
	waitPending(t, s, 3)

	s.release()
	got := []string{<-order, <-order, <-order}
	want := []string{"I", "W1", "W2"} // interactive first, then warmer FIFO
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drain order = %v, want %v", got, want)
	}
}

// TestSchedulerBumpPromotes verifies bump() moves an already-queued request to
// the front (a re-request refreshing recency).
func TestSchedulerBumpPromotes(t *testing.T) {
	s := newScheduler(1)
	occupy(t, s)

	order := make(chan string, 2)
	aReq := &schedReq{tier: tierInteractive, ctx: context.Background()}
	go func() {
		_ = s.acquire(aReq)
		order <- "A"
		s.release()
	}()
	waitPending(t, s, 1)
	go func() {
		r := &schedReq{tier: tierInteractive, ctx: context.Background()}
		_ = s.acquire(r)
		order <- "B"
		s.release()
	}()
	waitPending(t, s, 2)

	// Without a bump, B (newer) would drain first. Bump A to the top.
	s.bump(aReq)

	s.release()
	got := []string{<-order, <-order}
	want := []string{"A", "B"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drain order = %v, want %v (bump should promote A)", got, want)
	}
}

// TestSchedulerBumpPromotesWarmToInteractive verifies an interactive re-request
// coalescing onto a queued warmer generation promotes it past other warm work.
func TestSchedulerBumpPromotesWarmToInteractive(t *testing.T) {
	s := newScheduler(1)
	occupy(t, s)

	order := make(chan string, 3)
	w1 := &schedReq{tier: tierWarm, ctx: context.Background()}
	go func() { _ = s.acquire(w1); order <- "W1"; s.release() }()
	waitPending(t, s, 1)
	go func() {
		r := &schedReq{tier: tierWarm, ctx: context.Background()}
		_ = s.acquire(r)
		order <- "W2"
		s.release()
	}()
	waitPending(t, s, 2)

	// W1 was queued first (FIFO it would go first). Promote it to interactive.
	s.bump(w1)

	s.release()
	got := []string{<-order, <-order}
	// W1 is now interactive so it beats the remaining warmer W2 regardless of age.
	want := []string{"W1", "W2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drain order = %v, want %v", got, want)
	}
}

// TestSchedulerAbandonWhileQueued verifies a request cancelled while waiting is
// removed from the queue and never granted a slot.
func TestSchedulerAbandonWhileQueued(t *testing.T) {
	s := newScheduler(1)
	occupy(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		r := &schedReq{tier: tierInteractive, ctx: ctx}
		errCh <- s.acquire(r)
	}()
	waitPending(t, s, 1)

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not return after cancel")
	}
	if s.pendingLen() != 0 {
		t.Errorf("pending = %d after abandon, want 0", s.pendingLen())
	}
}

// TestSchedulerSetCapacityGrants verifies raising capacity immediately grants a
// slot to a waiter.
func TestSchedulerSetCapacityGrants(t *testing.T) {
	s := newScheduler(1)
	occupy(t, s)

	granted := make(chan struct{})
	go func() {
		r := &schedReq{tier: tierInteractive, ctx: context.Background()}
		_ = s.acquire(r)
		close(granted)
		s.release()
	}()
	waitPending(t, s, 1)

	s.setCapacity(2) // now two slots — the waiter should proceed without a release
	select {
	case <-granted:
	case <-time.After(2 * time.Second):
		t.Fatal("raising capacity did not grant the queued request")
	}
	if got := s.capacityValue(); got != 2 {
		t.Errorf("capacity = %d, want 2", got)
	}
}
