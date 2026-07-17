package services

import (
	"testing"
	"time"
)

func TestThrottleAllow(t *testing.T) {
	tr := &throttle{interval: 50 * time.Millisecond}

	if !tr.allow() {
		t.Fatal("first allow() should permit emission")
	}
	if tr.allow() {
		t.Fatal("immediate second allow() should be throttled")
	}
	time.Sleep(60 * time.Millisecond)
	if !tr.allow() {
		t.Fatal("allow() should permit again after the interval elapses")
	}
}

func TestThrottleRateBound(t *testing.T) {
	// Over a 250ms window at a 100ms interval, at most 3 emissions are permitted
	// (t=0, ~100ms, ~200ms), confirming the <=10/sec bound holds with margin.
	tr := newThrottle()
	permitted := 0
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if tr.allow() {
			permitted++
		}
		time.Sleep(5 * time.Millisecond)
	}
	if permitted > 4 {
		t.Fatalf("throttle permitted %d emissions in 250ms; want <=4 (<=10/sec)", permitted)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		done, total int64
		want        float64
	}{
		{0, 0, 0},
		{0, 10, 0},
		{5, 10, 50},
		{10, 10, 100},
		{15, 10, 100}, // clamped
	}
	for _, c := range cases {
		if got := percent(c.done, c.total); got != c.want {
			t.Errorf("percent(%d,%d)=%v want %v", c.done, c.total, got, c.want)
		}
	}
}
