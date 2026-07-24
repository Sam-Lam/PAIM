package backup

import (
	"sync"
	"time"
)

const (
	// completionWindow is the trailing window over which the completion rate is
	// measured. 15 minutes gives a stable completed-jobs-per-minute number on a slow
	// HDD-backed queue without lagging behind a genuine slowdown for too long.
	completionWindow = 15 * time.Minute
	// completionRingCap bounds the retained completion timestamps so a fast burst of
	// small files cannot grow memory without limit; the oldest entries are dropped
	// first (they would age out of the window anyway).
	completionRingCap = 2048
	// minRateSamples is the fewest retained completions required before a rate is
	// reported. Below it the rate is 0 (unknown) so the UI renders "—" rather than a
	// wild estimate extrapolated from one or two data points.
	minRateSamples = 3
)

// completionTracker records recent backup-job completion timestamps and derives a
// rolling completed-jobs-per-minute rate over completionWindow, plus the most
// recent completion time. It is safe for concurrent use: upload workers call
// record on completion while the services layer reads stats for the queue summary.
//
// The rate is the count of completions retained in the window divided by the
// elapsed minutes from the oldest retained completion to now. Anchoring the
// denominator to now (rather than the window length) means the number is accurate
// during warm-up (a minute of history divides by ~a minute) and decays honestly
// toward zero when the queue goes idle, so a derived ETA lengthens rather than
// freezing at a stale value.
type completionTracker struct {
	mu    sync.Mutex
	times []time.Time // completion timestamps within the window, oldest first
	last  time.Time   // most recent completion (zero when none)
}

// newCompletionTracker returns an empty tracker.
func newCompletionTracker() *completionTracker { return &completionTracker{} }

// record notes a completion at time t, updating the last-completed time and
// pruning entries older than the window.
func (c *completionTracker) record(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.last) {
		c.last = t
	}
	c.times = append(c.times, t)
	c.prune(t)
	if len(c.times) > completionRingCap {
		c.times = c.times[len(c.times)-completionRingCap:]
	}
}

// prune drops timestamps older than the window relative to now. The caller holds
// the lock.
func (c *completionTracker) prune(now time.Time) {
	cutoff := now.Add(-completionWindow)
	i := 0
	for i < len(c.times) && c.times[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		c.times = c.times[i:]
	}
}

// stats returns the completed-jobs-per-minute rate over the trailing window and
// the most recent completion time (zero when none). It returns a zero rate when
// fewer than minRateSamples completions are retained or the elapsed span is
// non-positive.
func (c *completionTracker) stats(now time.Time) (jobsPerMinute float64, last time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	last = c.last
	n := len(c.times)
	if n < minRateSamples {
		return 0, last
	}
	elapsedMin := now.Sub(c.times[0]).Minutes()
	if elapsedMin <= 0 {
		return 0, last
	}
	return float64(n) / elapsedMin, last
}
