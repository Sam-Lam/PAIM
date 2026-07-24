package repo

import (
	"context"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// countByStatus returns a status->count map from a QueueSummary.
func countByStatus(t *testing.T, r *BackupRepo) map[domain.JobStatus]int64 {
	t.Helper()
	rows, err := r.QueueSummary(context.Background())
	if err != nil {
		t.Fatalf("queue summary: %v", err)
	}
	out := make(map[domain.JobStatus]int64)
	for _, row := range rows {
		out[row.Status] = row.Count
	}
	return out
}

func TestCancelAllPendingPausedFlipsOnlyPendingAndPaused(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	// pending
	pj, _, err := r.Enqueue(ctx, "asset-p", "localfs", "/dst", 0)
	if err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	// paused
	sj, _, err := r.Enqueue(ctx, "asset-s", "localfs", "/dst", 0)
	if err != nil {
		t.Fatalf("enqueue paused: %v", err)
	}
	if err := r.Pause(ctx, sj.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// running (claimed)
	rj, _, err := r.Enqueue(ctx, "asset-r", "localfs", "/dst", 0)
	if err != nil {
		t.Fatalf("enqueue running: %v", err)
	}
	// Move rj to running by direct update (transition guard: pending->running is a
	// claim, done here without a provider round-trip).
	if err := r.db.WithContext(ctx).Model(&domain.BackupJob{}).
		Where("id = ?", rj.ID).Update("status", domain.JobStatusRunning).Error; err != nil {
		t.Fatalf("set running: %v", err)
	}
	// completed
	cj, _, err := r.Enqueue(ctx, "asset-c", "localfs", "/dst", 0)
	if err != nil {
		t.Fatalf("enqueue completed: %v", err)
	}
	if err := r.MarkCompleted(ctx, cj.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	n, err := r.CancelAllPendingPaused(ctx)
	if err != nil {
		t.Fatalf("cancel all: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled = %d, want 2 (pending + paused only)", n)
	}

	counts := countByStatus(t, r)
	if counts[domain.JobStatusCancelled] != 2 {
		t.Errorf("cancelled count = %d, want 2", counts[domain.JobStatusCancelled])
	}
	if counts[domain.JobStatusRunning] != 1 {
		t.Errorf("running count = %d, want 1 (running untouched)", counts[domain.JobStatusRunning])
	}
	if counts[domain.JobStatusCompleted] != 1 {
		t.Errorf("completed count = %d, want 1 (completed untouched)", counts[domain.JobStatusCompleted])
	}
	if counts[domain.JobStatusPending] != 0 || counts[domain.JobStatusPaused] != 0 {
		t.Errorf("pending/paused should be zero after cancel-all, got p=%d s=%d",
			counts[domain.JobStatusPending], counts[domain.JobStatusPaused])
	}
	_ = pj
	_ = sj

	// A second call flips nothing.
	n2, err := r.CancelAllPendingPaused(ctx)
	if err != nil {
		t.Fatalf("cancel all (2): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second cancel-all = %d, want 0", n2)
	}
}

func TestBytesRemainingSumsActiveJobAssets(t *testing.T) {
	ctx := context.Background()
	gdb := newTestDB(t)
	assets := NewAssetRepo(gdb)
	r := NewBackupRepo(gdb)

	// Assets behind various jobs.
	aPending := mustCreateAsset(t, assets, "hash-pending", 100, domain.MediaTypePhoto)
	aPaused := mustCreateAsset(t, assets, "hash-paused", 200, domain.MediaTypePhoto)
	aRunning := mustCreateAsset(t, assets, "hash-running", 400, domain.MediaTypeVideo)
	aCompleted := mustCreateAsset(t, assets, "hash-completed", 800, domain.MediaTypePhoto)

	// Empty queue: zero.
	if got, err := r.BytesRemaining(ctx); err != nil || got != 0 {
		t.Fatalf("bytes remaining (empty) = %d, err %v; want 0", got, err)
	}

	mkJob := func(assetID string, status domain.JobStatus) {
		j, _, err := r.Enqueue(ctx, assetID, "localfs", "/dst", 0)
		if err != nil {
			t.Fatalf("enqueue %s: %v", assetID, err)
		}
		if status != domain.JobStatusPending {
			if err := gdb.WithContext(ctx).Model(&domain.BackupJob{}).
				Where("id = ?", j.ID).Update("status", status).Error; err != nil {
				t.Fatalf("set status %s: %v", status, err)
			}
		}
	}
	mkJob(aPending.ID, domain.JobStatusPending)
	mkJob(aPaused.ID, domain.JobStatusPaused)
	mkJob(aRunning.ID, domain.JobStatusRunning)
	mkJob(aCompleted.ID, domain.JobStatusCompleted) // excluded from remaining

	// Pending(100) + Paused(200) + Running(400) = 700; completed excluded.
	got, err := r.BytesRemaining(ctx)
	if err != nil {
		t.Fatalf("bytes remaining: %v", err)
	}
	if got != 700 {
		t.Errorf("bytes remaining = %d, want 700", got)
	}

	// A second provider job for the same pending asset counts again (per-upload).
	if _, _, err := r.Enqueue(ctx, aPending.ID, "rclone", "/dst2", 0); err != nil {
		t.Fatalf("enqueue second provider: %v", err)
	}
	got2, err := r.BytesRemaining(ctx)
	if err != nil {
		t.Fatalf("bytes remaining (2): %v", err)
	}
	if got2 != 800 { // 700 + another 100 for the pending asset's second job
		t.Errorf("bytes remaining with second provider = %d, want 800", got2)
	}
}
