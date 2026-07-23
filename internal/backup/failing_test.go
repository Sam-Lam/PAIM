package backup

import (
	"testing"
	"time"
)

// newFailingTestManager builds a Manager wired only for the provider-failing edge
// logic: real option maps are initialized by NewManager, the clock is controllable
// via *now, and OnProviderFailing pushes the provider ID onto a buffered channel.
func newFailingTestManager(t *testing.T, now *time.Time) (*Manager, chan string) {
	t.Helper()
	fired := make(chan string, 16)
	m := NewManager(nil, nil, nil, NewRegistry(), nil, Options{
		Now:               func() time.Time { return *now },
		OnProviderFailing: func(id string) { fired <- id },
	})
	return m, fired
}

// expectFire asserts a single OnProviderFailing for wantID within a short window.
func expectFire(t *testing.T, fired chan string, wantID string) {
	t.Helper()
	select {
	case got := <-fired:
		if got != wantID {
			t.Fatalf("provider-failing fired for %q, want %q", got, wantID)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected provider-failing to fire for %q, but it did not", wantID)
	}
}

// expectNoFire asserts OnProviderFailing does NOT fire within a brief window.
func expectNoFire(t *testing.T, fired chan string) {
	t.Helper()
	select {
	case got := <-fired:
		t.Fatalf("provider-failing fired unexpectedly for %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestNotePermanentFailure_EdgeAndThrottle covers the failing-edge notification:
// it fires on the first permanent failure, stays quiet on repeated failures with
// no intervening completion, and — after a completion resets the state — re-fires
// only once the per-provider hourly throttle has elapsed. It also confirms
// providers are independent and a nil callback is a no-op.
func TestNotePermanentFailure_EdgeAndThrottle(t *testing.T) {
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	now := base
	m, fired := newFailingTestManager(t, &now)

	// 1. First permanent failure for A: edge -> fires.
	m.notePermanentFailure("A")
	expectFire(t, fired, "A")

	// 2. Another permanent failure for A with no completion: NOT an edge -> quiet.
	m.notePermanentFailure("A")
	expectNoFire(t, fired)

	// 3. Provider B is independent: its first failure fires.
	m.notePermanentFailure("B")
	expectFire(t, fired, "B")

	// 4. A completes (clears failing), then fails again 30m later: edge again, but
	//    inside the 1h throttle window -> still quiet.
	m.clearProviderFailing("A")
	now = base.Add(30 * time.Minute)
	m.notePermanentFailure("A")
	expectNoFire(t, fired)

	// 5. A completes again, then fails 61m after the last notify: edge + throttle
	//    elapsed -> fires.
	m.clearProviderFailing("A")
	now = base.Add(61 * time.Minute)
	m.notePermanentFailure("A")
	expectFire(t, fired, "A")
}

// TestNotePermanentFailure_NilCallback confirms a Manager with no OnProviderFailing
// callback never panics and simply does nothing.
func TestNotePermanentFailure_NilCallback(t *testing.T) {
	m := NewManager(nil, nil, nil, NewRegistry(), nil, Options{})
	m.notePermanentFailure("A") // must not panic
	m.clearProviderFailing("A")
}
