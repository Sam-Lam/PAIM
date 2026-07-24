package backup

import (
	"math"
	"testing"
	"time"
)

func TestCompletionTrackerZeroRateBelowMinSamples(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	c := newCompletionTracker()

	if rate, last := c.stats(base); rate != 0 || !last.IsZero() {
		t.Fatalf("empty tracker: rate=%v last=%v, want 0 / zero", rate, last)
	}

	// Two completions is still below minRateSamples (3) -> unknown (0) rate, but
	// last-completed is tracked.
	c.record(base)
	c.record(base.Add(1 * time.Minute))
	rate, last := c.stats(base.Add(2 * time.Minute))
	if rate != 0 {
		t.Errorf("rate with 2 samples = %v, want 0 (below min)", rate)
	}
	if !last.Equal(base.Add(1 * time.Minute)) {
		t.Errorf("last-completed = %v, want %v", last, base.Add(1*time.Minute))
	}
}

func TestCompletionTrackerRateOverWindow(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	c := newCompletionTracker()

	// 6 completions, one per minute at t=0..5.
	for i := 0; i < 6; i++ {
		c.record(base.Add(time.Duration(i) * time.Minute))
	}
	// now = t=5min, oldest = t=0 -> elapsed 5 min, count 6 -> 1.2/min.
	now := base.Add(5 * time.Minute)
	rate, last := c.stats(now)
	if math.Abs(rate-1.2) > 1e-9 {
		t.Errorf("rate = %v, want 1.2/min", rate)
	}
	if !last.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("last = %v, want %v", last, base.Add(5*time.Minute))
	}
}

func TestCompletionTrackerDecaysWhenIdle(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	c := newCompletionTracker()
	for i := 0; i < 5; i++ {
		c.record(base.Add(time.Duration(i) * time.Minute))
	}
	// At the moment of the last completion the rate is high; as the queue idles the
	// same samples divide by a larger now-oldest span, so the rate decays.
	rateFresh, _ := c.stats(base.Add(4 * time.Minute))
	rateLater, _ := c.stats(base.Add(9 * time.Minute))
	if rateLater >= rateFresh {
		t.Errorf("idle rate did not decay: fresh=%v later=%v", rateFresh, rateLater)
	}
}

func TestCompletionTrackerPrunesOutsideWindow(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	c := newCompletionTracker()

	// Old burst that will fall outside the 15-min window by the time we read.
	for i := 0; i < 4; i++ {
		c.record(base.Add(time.Duration(i) * time.Minute))
	}
	// Read far in the future: all samples are older than completionWindow -> pruned
	// -> below min samples -> zero rate. last-completed still remembered.
	now := base.Add(completionWindow + 10*time.Minute)
	rate, last := c.stats(now)
	if rate != 0 {
		t.Errorf("rate after all samples aged out = %v, want 0", rate)
	}
	if !last.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("last = %v, want %v", last, base.Add(3*time.Minute))
	}
	if n := len(c.times); n != 0 {
		t.Errorf("retained samples after prune = %d, want 0", n)
	}
}
