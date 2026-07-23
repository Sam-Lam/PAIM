package thumbs

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeTinyJPEG writes a minimal valid JPEG at dst (so Cache size/stat checks pass).
func writeTinyJPEG(dst string) error {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

// blockGen is a generator for scheduling tests: it records the order generations
// begin (i.e. the order the scheduler grants slots), tracks peak concurrency, and
// can block selected sources on a per-source gate. Note the render context passed
// in by Cache.ensure is DETACHED from the request ctx, so gating deliberately does
// not observe request cancellation — that is what proves an in-flight render
// completes despite abandonment.
type blockGen struct {
	mu        sync.Mutex
	started   []string                 // srcs in slot-grant order
	completed []string                 // srcs that finished
	gates     map[string]chan struct{} // src -> release chan (absent/nil = no block)

	calls     atomic.Int32
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func newBlockGen() *blockGen {
	return &blockGen{gates: map[string]chan struct{}{}}
}

func (g *blockGen) generate(ctx context.Context, src, dst string, sizePx, quality int) error {
	g.calls.Add(1)
	n := g.inFlight.Add(1)
	for {
		m := g.maxFlight.Load()
		if n <= m || g.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	g.mu.Lock()
	g.started = append(g.started, src)
	gate := g.gates[src]
	g.mu.Unlock()
	if gate != nil {
		<-gate // block until released (request-ctx cancellation is intentionally ignored)
	}
	g.inFlight.Add(-1)
	g.mu.Lock()
	g.completed = append(g.completed, src)
	g.mu.Unlock()
	return writeTinyJPEG(dst)
}

func (g *blockGen) startedList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.started...)
}

// waitCond polls pred until true or fails the test.
func waitCond(t *testing.T, msg string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition never met: %s", msg)
}

// TestEnsureAbandonedInFlightCompletesAndCaches verifies the queue-wait vs. exec
// context split: once generation starts, cancelling the request ctx does NOT
// abort it — the render finishes and lands in the cache (pure win).
func TestEnsureAbandonedInFlightCompletesAndCaches(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "img.jpg")
	writeJPEG(t, src)

	gen := newBlockGen()
	gate := make(chan struct{})
	gen.gates[src] = gate
	c := newCache(dir, gen, 2, nil)

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		path string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		p, err := c.Ensure(ctx, src, "abcd1234", SizeGrid)
		done <- res{p, err}
	}()

	waitCond(t, "generation in flight", func() bool { return gen.inFlight.Load() == 1 })
	cancel()    // abandon the request while the render runs
	close(gate) // let the (detached) render finish

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ensure returned err = %v, want nil (in-flight render should complete)", r.err)
		}
		if !fileExists(r.path) {
			t.Fatalf("thumbnail not cached at %s after abandonment", r.path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ensure did not return after gate release")
	}
	if got := gen.calls.Load(); got != 1 {
		t.Errorf("generator ran %d times, want 1", got)
	}
}

// TestEnsureCancelledBeforeStartDiscarded verifies a request cancelled while
// still queued is dropped without ever invoking the generator.
func TestEnsureCancelledBeforeStartDiscarded(t *testing.T) {
	dir := t.TempDir()
	blkSrc := filepath.Join(dir, "blocker.jpg")
	aSrc := filepath.Join(dir, "a.jpg")
	writeJPEG(t, blkSrc)
	writeJPEG(t, aSrc)

	gen := newBlockGen()
	blkGate := make(chan struct{})
	gen.gates[blkSrc] = blkGate
	c := newCache(dir, gen, 1, nil) // single slot

	// Occupy the only slot with a blocking generation.
	go func() { _, _ = c.Ensure(context.Background(), blkSrc, "blocker0", SizeGrid) }()
	waitCond(t, "blocker holding slot", func() bool { return gen.inFlight.Load() == 1 })

	// A parks behind the blocker; cancel it before the slot ever frees.
	ctx, cancel := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	go func() {
		_, err := c.Ensure(ctx, aSrc, "aaaa1111", SizeGrid)
		aErr <- err
	}()
	waitCond(t, "A queued", func() bool { return c.sched.pendingLen() == 1 })

	cancel()
	select {
	case err := <-aErr:
		if err != context.Canceled {
			t.Fatalf("A err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A did not return after cancel")
	}

	close(blkGate) // let the blocker finish
	// The generator must have run for the blocker only — never for the cancelled A.
	waitCond(t, "blocker completed", func() bool { return gen.calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond) // give any erroneous A generation a chance to appear
	if got := gen.calls.Load(); got != 1 {
		t.Errorf("generator ran %d times, want 1 (A must not generate)", got)
	}
	for _, s := range gen.startedList() {
		if s == aSrc {
			t.Errorf("generator ran for cancelled-while-queued source %s", aSrc)
		}
	}
}

// TestEnsureReRequestBumpsRecency verifies a singleflight re-request bumps the
// in-flight generation to the front: queue A then B behind a stalled worker, then
// re-request A — A now completes before B.
func TestEnsureReRequestBumpsRecency(t *testing.T) {
	dir := t.TempDir()
	blkSrc := filepath.Join(dir, "blocker.jpg")
	aSrc := filepath.Join(dir, "a.jpg")
	bSrc := filepath.Join(dir, "b.jpg")
	for _, s := range []string{blkSrc, aSrc, bSrc} {
		writeJPEG(t, s)
	}

	gen := newBlockGen()
	blkGate := make(chan struct{})
	gen.gates[blkSrc] = blkGate
	c := newCache(dir, gen, 1, nil)

	// Occupy the slot.
	go func() { _, _ = c.Ensure(context.Background(), blkSrc, "blocker0", SizeGrid) }()
	waitCond(t, "blocker holding slot", func() bool { return gen.inFlight.Load() == 1 })

	// Queue A, then B. B is newer, so without a bump B would drain first.
	go func() { _, _ = c.Ensure(context.Background(), aSrc, "aaaa1111", SizeGrid) }()
	waitCond(t, "A queued", func() bool { return c.sched.pendingLen() == 1 })
	go func() { _, _ = c.Ensure(context.Background(), bSrc, "bbbb2222", SizeGrid) }()
	waitCond(t, "B queued", func() bool { return c.sched.pendingLen() == 2 })

	// Re-request A (same key): joins the singleflight and bumps A to the top.
	base := c.sched.seqCounter()
	go func() { _, _ = c.Ensure(context.Background(), aSrc, "aaaa1111", SizeGrid) }()
	waitCond(t, "re-request bumped A", func() bool { return c.sched.seqCounter() > base })

	close(blkGate) // drain
	waitCond(t, "A and B generated", func() bool { return gen.calls.Load() == 3 })

	// Exclude the blocker; A must have started before B.
	var order []string
	for _, s := range gen.startedList() {
		if s == aSrc {
			order = append(order, "A")
		} else if s == bSrc {
			order = append(order, "B")
		}
	}
	if !reflect.DeepEqual(order, []string{"A", "B"}) {
		t.Errorf("generation order = %v, want [A B] (re-request should bump A ahead of B)", order)
	}
}

// TestEnsureRespectsConcurrency verifies the parallelism bound caps how many
// generations run at once (interactive and warm share the pool).
func TestEnsureRespectsConcurrency(t *testing.T) {
	const capacity = 2
	const total = 6
	dir := t.TempDir()

	gen := newBlockGen()
	gate := make(chan struct{})
	srcs := make([]string, total)
	for i := 0; i < total; i++ {
		srcs[i] = filepath.Join(dir, string(rune('a'+i))+".jpg")
		writeJPEG(t, srcs[i])
		gen.gates[srcs[i]] = gate // all block on one shared gate
	}
	c := newCache(dir, gen, capacity, nil)

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.Ensure(context.Background(), srcs[i], "hash"+string(rune('a'+i)), SizeGrid)
		}(i)
	}

	// Exactly `capacity` should be in flight; the rest wait in the queue.
	waitCond(t, "pool saturated", func() bool {
		return gen.inFlight.Load() == capacity && c.sched.pendingLen() == total-capacity
	})
	time.Sleep(20 * time.Millisecond) // let any over-admission surface
	if got := gen.inFlight.Load(); got > capacity {
		t.Fatalf("in-flight = %d, exceeds capacity %d", got, capacity)
	}

	close(gate)
	wg.Wait()
	if got := gen.maxFlight.Load(); got != capacity {
		t.Errorf("peak concurrency = %d, want %d", got, capacity)
	}
	if got := gen.calls.Load(); got != total {
		t.Errorf("generator ran %d times, want %d", got, total)
	}
}

// TestFlightJoinFiresBump verifies the singleflight fires the winner's bump hook
// for every follower (the mechanism behind re-request recency refresh).
func TestFlightJoinFiresBump(t *testing.T) {
	var f flight
	started := make(chan struct{})
	release := make(chan struct{})
	var bumps atomic.Int32

	go func() {
		_, _ = f.Do("k", func(call *flightCall) (string, error) {
			call.setBump(func() { bumps.Add(1) })
			close(started)
			<-release
			return "v", nil
		})
	}()
	<-started

	const followers = 3
	var wg sync.WaitGroup
	for i := 0; i < followers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Do("k", func(*flightCall) (string, error) { return "", nil })
		}()
	}
	waitCond(t, "all followers bumped", func() bool { return bumps.Load() == followers })
	close(release)
	wg.Wait()
	if got := bumps.Load(); got != followers {
		t.Errorf("bump fired %d times, want %d", got, followers)
	}
}
