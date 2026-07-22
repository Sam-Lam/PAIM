package services

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeSource is a controllable activitySource: it reports a fixed set of ops and
// records cancelActive calls.
type fakeSource struct {
	mu        sync.Mutex
	ops       []OperationInfo
	cancelled int
}

func (f *fakeSource) activeOps() []OperationInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops
}

func (f *fakeSource) cancelActive() {
	f.mu.Lock()
	f.cancelled++
	f.mu.Unlock()
}

func (f *fakeSource) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// drainingSource reports one op until cancelActive is called, then reports none —
// modeling an operation that stops promptly once cancelled.
type drainingSource struct {
	mu        sync.Mutex
	active    bool
	cancelled int
}

func (d *drainingSource) activeOps() []OperationInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return nil
	}
	return []OperationInfo{{Kind: "import", Label: "Importing files", FilesDone: 5, FilesTotal: 10}}
}

func (d *drainingSource) cancelActive() {
	d.mu.Lock()
	d.cancelled++
	d.active = false
	d.mu.Unlock()
}

func TestActivityTracker_SnapshotEmpty(t *testing.T) {
	tr := NewActivityTracker()
	if got := tr.Snapshot(); len(got) != 0 {
		t.Fatalf("empty tracker Snapshot = %d ops, want 0", len(got))
	}
	// A registered source that is idle contributes nothing.
	tr.Register(&fakeSource{ops: nil})
	if got := tr.Snapshot(); len(got) != 0 {
		t.Fatalf("idle source Snapshot = %d ops, want 0", len(got))
	}
	// A nil source is ignored.
	tr.Register(nil)
	if got := tr.Snapshot(); len(got) != 0 {
		t.Fatalf("after nil Register Snapshot = %d ops, want 0", len(got))
	}
}

func TestActivityTracker_SnapshotAggregates(t *testing.T) {
	tr := NewActivityTracker()
	a := &fakeSource{ops: []OperationInfo{{Kind: "import", Label: "Importing files"}}}
	b := &fakeSource{ops: []OperationInfo{
		{Kind: "backup_upload", Label: "Uploading a backup", BytesDone: 1, BytesTotal: 2},
		{Kind: "backup_upload", Label: "Uploading a backup"},
	}}
	tr.Register(a)
	tr.Register(&fakeSource{ops: nil}) // idle, contributes nothing
	tr.Register(b)

	ops := tr.Snapshot()
	if len(ops) != 3 {
		t.Fatalf("Snapshot = %d ops, want 3 (1 import + 2 backup)", len(ops))
	}
	kinds := map[string]int{}
	for _, o := range ops {
		kinds[o.Kind]++
	}
	if kinds["import"] != 1 || kinds["backup_upload"] != 2 {
		t.Fatalf("kinds = %v, want 1 import + 2 backup_upload", kinds)
	}
}

func TestActivityTracker_CancelAll(t *testing.T) {
	tr := NewActivityTracker()
	a := &fakeSource{ops: []OperationInfo{{Kind: "import"}}}
	b := &fakeSource{ops: []OperationInfo{{Kind: "cleanup"}}}
	tr.Register(a)
	tr.Register(b)

	tr.CancelAll()
	if a.cancelCount() != 1 || b.cancelCount() != 1 {
		t.Fatalf("CancelAll: a=%d b=%d, want 1 each", a.cancelCount(), b.cancelCount())
	}
}

func TestConfirmQuit_CancelsAndQuitsWhenDrained(t *testing.T) {
	src := &drainingSource{active: true}
	tr := NewActivityTracker()
	tr.Register(src)

	var quits int
	app := NewAppService(tr)
	app.Quit = func() { quits++ }
	// Guard against the wait loop ever sleeping: the source drains on cancel, so
	// the first Snapshot after CancelAll is empty and no poll should occur.
	slept := 0
	app.sleep = func(time.Duration) { slept++ }

	if err := app.ConfirmQuit(context.Background()); err != nil {
		t.Fatalf("ConfirmQuit: %v", err)
	}
	if src.cancelled != 1 {
		t.Fatalf("cancelActive calls = %d, want 1", src.cancelled)
	}
	if slept != 0 {
		t.Fatalf("slept %d times, want 0 (ops drained immediately on cancel)", slept)
	}
	if quits != 1 {
		t.Fatalf("quit calls = %d, want 1", quits)
	}
}

func TestConfirmQuit_QuitsAfterGraceWhenStuck(t *testing.T) {
	// A source whose op never drains, even after cancel (e.g. a backup upload that
	// is not aborted mid-flight): ConfirmQuit must still quit once the bounded
	// grace period elapses. A fake clock advances only via the injected sleep, so
	// the loop is guaranteed to terminate.
	src := &fakeSource{ops: []OperationInfo{{Kind: "backup_upload", Label: "Uploading a backup"}}}
	tr := NewActivityTracker()
	tr.Register(src)

	var quits int
	app := NewAppService(tr)
	app.Quit = func() { quits++ }
	app.grace = 500 * time.Millisecond
	app.poll = 100 * time.Millisecond
	now := time.Unix(0, 0)
	app.now = func() time.Time { return now }
	app.sleep = func(d time.Duration) { now = now.Add(d) }

	if err := app.ConfirmQuit(context.Background()); err != nil {
		t.Fatalf("ConfirmQuit: %v", err)
	}
	if src.cancelCount() != 1 {
		t.Fatalf("cancelActive calls = %d, want 1", src.cancelCount())
	}
	if quits != 1 {
		t.Fatalf("quit calls = %d, want 1 (must quit after grace even if stuck)", quits)
	}
}

func TestConfirmQuit_NoTrackerStillQuits(t *testing.T) {
	app := NewAppService(nil)
	var quits int
	app.Quit = func() { quits++ }
	if err := app.ConfirmQuit(context.Background()); err != nil {
		t.Fatalf("ConfirmQuit: %v", err)
	}
	if quits != 1 {
		t.Fatalf("quit calls = %d, want 1", quits)
	}
}

func TestActiveOperations_ReturnsSnapshot(t *testing.T) {
	tr := NewActivityTracker()
	tr.Register(&fakeSource{ops: []OperationInfo{{Kind: "analyze", Label: "Analyzing a source"}}})
	app := NewAppService(tr)

	ops, err := app.ActiveOperations(context.Background())
	if err != nil {
		t.Fatalf("ActiveOperations: %v", err)
	}
	if len(ops) != 1 || ops[0].Kind != "analyze" {
		t.Fatalf("ActiveOperations = %v, want one analyze op", ops)
	}
}
