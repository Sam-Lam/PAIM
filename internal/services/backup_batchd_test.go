package services

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// TestBackupServiceCancelAllPending covers count + transition + emission: pending
// AND paused jobs move to cancelled, a running job is untouched, and a
// backup:queue-changed event is emitted (mirroring RetryAllFailed's shape).
func TestBackupServiceCancelAllPending(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "cancelall.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	jobs := repo.NewBackupRepo(gdb)
	assets := repo.NewAssetRepo(gdb)
	manager := backup.NewManager(
		backup.NewRepoJobQueue(gdb), assets, backup.NewRepoProviderStore(gdb),
		backup.NewRegistry(), nil, backup.Options{},
	)
	emitter := &captureEmitter{}
	svc := NewBackupService(manager, jobs, assets, nil, nil, emitter, nil)

	mk := func(st domain.JobStatus) {
		j := &domain.BackupJob{AssetID: "a", Plugin: "localfs", Destination: "dest", Status: st}
		if err := gdb.Create(j).Error; err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	mk(domain.JobStatusPending)
	mk(domain.JobStatusPaused)
	mk(domain.JobStatusRunning)
	mk(domain.JobStatusCompleted)

	n, err := svc.CancelAllPending(ctx)
	if err != nil {
		t.Fatalf("CancelAllPending: %v", err)
	}
	if n != 2 {
		t.Fatalf("CancelAllPending count = %d, want 2 (pending + paused)", n)
	}

	count := func(st domain.JobStatus) int64 {
		var c int64
		if err := gdb.Model(&domain.BackupJob{}).Where("status = ?", st).Count(&c).Error; err != nil {
			t.Fatalf("count %s: %v", st, err)
		}
		return c
	}
	if got := count(domain.JobStatusCancelled); got != 2 {
		t.Errorf("cancelled = %d, want 2", got)
	}
	if got := count(domain.JobStatusRunning); got != 1 {
		t.Errorf("running = %d, want 1 (untouched)", got)
	}
	if got := count(domain.JobStatusCompleted); got != 1 {
		t.Errorf("completed = %d, want 1 (untouched)", got)
	}
	if got := emitter.byName(EventBackupQueueChanged); len(got) == 0 {
		t.Fatal("CancelAllPending did not emit backup:queue-changed")
	}
}

func TestBackupETA(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// 120 jobs at 2/min => 60 minutes => 3600s.
	secs, at, ok := backupETA(now, 120, 2.0, false)
	if !ok {
		t.Fatal("expected an ETA for a positive rate + remaining work")
	}
	if secs != 3600 {
		t.Errorf("etaSeconds = %d, want 3600", secs)
	}
	if !at.Equal(now.Add(3600 * time.Second)) {
		t.Errorf("etaAt = %v, want %v", at, now.Add(3600*time.Second))
	}

	// Zero rate -> unknown.
	if _, _, ok := backupETA(now, 120, 0, false); ok {
		t.Error("zero rate should yield no ETA")
	}
	// No remaining work -> unknown.
	if _, _, ok := backupETA(now, 0, 5, false); ok {
		t.Error("empty queue should yield no ETA")
	}
	// Paused -> unknown even with a good rate.
	if _, _, ok := backupETA(now, 120, 5, true); ok {
		t.Error("paused queue should yield no ETA")
	}
	// Rounds up to whole seconds (never fractional / infinite).
	secs2, _, ok := backupETA(now, 1, 3.0, false) // 1 job / (3/min) = 20s
	if !ok || secs2 != 20 {
		t.Errorf("etaSeconds = %d ok=%v, want 20 true", secs2, ok)
	}
}

func TestBadgeLabelPolicy(t *testing.T) {
	// No ops -> clear.
	if got := BadgeLabel(nil); got != "" {
		t.Errorf("empty ops label = %q, want \"\"", got)
	}
	// A non-badgeable byte-based op (backup upload) is ignored.
	if got := BadgeLabel([]OperationInfo{
		{Kind: OpKindBackupUpload, BytesDone: 5, BytesTotal: 10},
	}); got != "" {
		t.Errorf("backup-upload-only label = %q, want \"\" (not badgeable)", got)
	}
	// A single determinate import -> its percentage.
	if got := BadgeLabel([]OperationInfo{
		{Kind: OpKindImport, FilesDone: 42, FilesTotal: 100},
	}); got != "42%" {
		t.Errorf("import 42/100 label = %q, want 42%%", got)
	}
	// Indeterminate op (no total) -> clear.
	if got := BadgeLabel([]OperationInfo{
		{Kind: OpKindAnalyze, FilesDone: 5, FilesTotal: 0},
	}); got != "" {
		t.Errorf("indeterminate label = %q, want \"\"", got)
	}
	// The PRIMARY (largest total) op wins: warm-up 900/1000 vs reorganize 1/10.
	if got := BadgeLabel([]OperationInfo{
		{Kind: OpKindReorganize, FilesDone: 1, FilesTotal: 10},
		{Kind: OpKindThumbnailWarmup, FilesDone: 900, FilesTotal: 1000},
	}); got != "90%" {
		t.Errorf("primary-op label = %q, want 90%%", got)
	}
	// Clamp: done >= total -> 100%.
	if got := BadgeLabel([]OperationInfo{
		{Kind: OpKindImport, FilesDone: 150, FilesTotal: 100},
	}); got != "100%" {
		t.Errorf("over-100 label = %q, want 100%%", got)
	}
}

// fakeBadgeSetter records the sequence of badge operations for throttle/dedup
// assertions.
type fakeBadgeSetter struct {
	mu   sync.Mutex
	sets []string
	rms  int
	fail bool
}

func (f *fakeBadgeSetter) SetBadge(label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, label)
	if f.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeBadgeSetter) RemoveBadge() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rms++
	if f.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func TestBadgeControllerDedupAndClear(t *testing.T) {
	tracker := NewActivityTracker()
	setter := &fakeBadgeSetter{}
	c := NewBadgeController(tracker, setter, nil)

	// A live import at 42%. Two ticks with the same label -> one SetBadge (dedup).
	src := &fakeActivitySource{ops: []OperationInfo{{Kind: OpKindImport, FilesDone: 42, FilesTotal: 100}}}
	tracker.Register(src)
	c.tick()
	c.tick()
	setter.mu.Lock()
	if len(setter.sets) != 1 || setter.sets[0] != "42%" {
		setter.mu.Unlock()
		t.Fatalf("sets = %v, want exactly one \"42%%\"", setter.sets)
	}
	setter.mu.Unlock()

	// Progress advances -> a new label is pushed.
	src.set([]OperationInfo{{Kind: OpKindImport, FilesDone: 43, FilesTotal: 100}})
	c.tick()
	setter.mu.Lock()
	if len(setter.sets) != 2 || setter.sets[1] != "43%" {
		setter.mu.Unlock()
		t.Fatalf("sets = %v, want a second \"43%%\"", setter.sets)
	}
	setter.mu.Unlock()

	// Op ends -> badge cleared once, and a redundant tick does not re-clear.
	src.set(nil)
	c.tick()
	c.tick()
	setter.mu.Lock()
	if setter.rms != 1 {
		setter.mu.Unlock()
		t.Fatalf("RemoveBadge calls = %d, want 1", setter.rms)
	}
	setter.mu.Unlock()
}

func TestBadgeControllerNilSetterIsNoop(t *testing.T) {
	tracker := NewActivityTracker()
	tracker.Register(&fakeActivitySource{ops: []OperationInfo{{Kind: OpKindImport, FilesDone: 1, FilesTotal: 2}}})
	c := NewBadgeController(tracker, nil, nil)
	c.tick() // must not panic
}

// fakeActivitySource is a mutable activitySource for badge tests.
type fakeActivitySource struct {
	mu  sync.Mutex
	ops []OperationInfo
}

func (f *fakeActivitySource) set(ops []OperationInfo) {
	f.mu.Lock()
	f.ops = ops
	f.mu.Unlock()
}

func (f *fakeActivitySource) activeOps() []OperationInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]OperationInfo(nil), f.ops...)
}

func (f *fakeActivitySource) cancelActive() {}
