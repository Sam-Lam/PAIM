package volumes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatcher_MountUnmount uses a temp dir standing in for /Volumes and asserts
// that creating and removing a subdirectory produces Mounted then Unmounted
// events. It exercises the fsnotify path with a short debounce.
func TestWatcher_MountUnmount(t *testing.T) {
	dir := t.TempDir()

	// A pre-existing entry is part of the baseline and must NOT emit an event.
	if err := os.Mkdir(filepath.Join(dir, "Existing"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWatcher(nil,
		WithWatchDir(dir),
		WithDebounce(50*time.Millisecond),
		WithPollInterval(200*time.Millisecond),
	)
	events, err := w.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	newMount := filepath.Join(dir, "SDCARD")
	if err := os.Mkdir(newMount, 0o755); err != nil {
		t.Fatal(err)
	}

	ev := waitEvent(t, events, 3*time.Second)
	if ev.Type != EventMounted || ev.MountPoint != newMount {
		t.Fatalf("got %+v, want Mounted %s", ev, newMount)
	}

	if err := os.Remove(newMount); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, events, 3*time.Second)
	if ev.Type != EventUnmounted || ev.MountPoint != newMount {
		t.Fatalf("got %+v, want Unmounted %s", ev, newMount)
	}
}

// TestWatcher_PollFallback removes fsnotify's usefulness by relying on the poll
// tick: it creates a dir and expects the periodic reconcile to still catch it.
// (The debounce is set long so the fsnotify-driven reconcile does not fire
// first; the poll interval is short.)
func TestWatcher_PollFallback(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWatcher(nil,
		WithWatchDir(dir),
		WithDebounce(10*time.Second),  // effectively disables fsnotify-driven reconcile
		WithPollInterval(100*time.Millisecond),
	)
	events, err := w.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	newMount := filepath.Join(dir, "NAS")
	if err := os.Mkdir(newMount, 0o755); err != nil {
		t.Fatal(err)
	}

	ev := waitEvent(t, events, 3*time.Second)
	if ev.Type != EventMounted || ev.MountPoint != newMount {
		t.Fatalf("got %+v, want Mounted %s via poll", ev, newMount)
	}
}

// TestWatcher_HiddenIgnored ensures hidden entries never produce events.
func TestWatcher_HiddenIgnored(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWatcher(nil,
		WithWatchDir(dir),
		WithDebounce(50*time.Millisecond),
		WithPollInterval(150*time.Millisecond),
	)
	events, err := w.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := os.Mkdir(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".timemachine"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real entry after the hidden ones proves the watcher is alive and only
	// the real mount is reported.
	real := filepath.Join(dir, "REAL")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}

	ev := waitEvent(t, events, 3*time.Second)
	if ev.MountPoint != real {
		t.Fatalf("got %+v, want only REAL", ev)
	}
}

func waitEvent(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}
