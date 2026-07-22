package services

import (
	"sync/atomic"
	"testing"
)

// fakeSleepProc is a stand-in for the caffeinate child process; it records kills.
type fakeSleepProc struct{ kills *int32 }

func (p *fakeSleepProc) Kill() error { atomic.AddInt32(p.kills, 1); return nil }
func (p *fakeSleepProc) Wait() error { return nil }

// TestSleepGuardRefcount verifies the assertion process starts on the first
// Acquire, survives while any holder remains, stops on the last Release, and can
// be restarted — the exec is mocked so no real caffeinate runs.
func TestSleepGuardRefcount(t *testing.T) {
	var starts, kills int32
	g := NewSleepGuard(nil)
	g.start = func() (sleepProc, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeSleepProc{kills: &kills}, nil
	}

	g.Acquire()
	g.Acquire()
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("after two Acquire, starts = %d, want 1 (one shared process)", got)
	}
	if got := atomic.LoadInt32(&kills); got != 0 {
		t.Fatalf("process killed too early: kills = %d, want 0", got)
	}

	g.Release()
	if got := atomic.LoadInt32(&kills); got != 0 {
		t.Fatalf("process killed while a holder remained: kills = %d, want 0", got)
	}

	g.Release()
	if got := atomic.LoadInt32(&kills); got != 1 {
		t.Fatalf("process not stopped on last Release: kills = %d, want 1", got)
	}

	// Extra Release is a no-op.
	g.Release()
	if got := atomic.LoadInt32(&kills); got != 1 {
		t.Fatalf("extra Release changed kills: %d, want 1", got)
	}

	// A fresh Acquire starts a new process.
	g.Acquire()
	if got := atomic.LoadInt32(&starts); got != 2 {
		t.Fatalf("re-acquire did not start a new process: starts = %d, want 2", got)
	}

	// Shutdown force-stops regardless of the count.
	g.Shutdown()
	if got := atomic.LoadInt32(&kills); got != 2 {
		t.Fatalf("Shutdown did not stop the process: kills = %d, want 2", got)
	}
}

// TestSleepGuardNil confirms a nil *SleepGuard is a safe no-op (unit-test
// services constructed without a guard never spawn a process).
func TestSleepGuardNil(t *testing.T) {
	var g *SleepGuard
	g.Acquire()
	g.Release()
	g.Shutdown()
}
