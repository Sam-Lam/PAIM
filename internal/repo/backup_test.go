package repo

import (
	"context"
	"sync"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

func TestEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	j1, created1, err := r.Enqueue(ctx, "asset-1", "localfs", "/backup/a", 0)
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if !created1 {
		t.Error("first enqueue should create a job")
	}

	j2, created2, err := r.Enqueue(ctx, "asset-1", "localfs", "/backup/a", 0)
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if created2 {
		t.Error("second enqueue for same triple should NOT create a job")
	}
	if j1.ID != j2.ID {
		t.Errorf("idempotent enqueue returned different jobs: %q vs %q", j1.ID, j2.ID)
	}

	// A different destination is a distinct job.
	_, created3, err := r.Enqueue(ctx, "asset-1", "localfs", "/backup/b", 0)
	if err != nil {
		t.Fatalf("enqueue 3: %v", err)
	}
	if !created3 {
		t.Error("enqueue for a different destination should create a job")
	}

	summary, err := r.QueueSummary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	var pending int64
	for _, s := range summary {
		if s.Status == domain.JobStatusPending {
			pending = s.Count
		}
	}
	if pending != 2 {
		t.Errorf("pending jobs = %d, want 2", pending)
	}
}

func TestNextPendingClaimsOldestAndTransitions(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	first, _, _ := r.Enqueue(ctx, "a1", "localfs", "/d", 0)
	second, _, _ := r.Enqueue(ctx, "a2", "localfs", "/d", 0)

	claimed, err := r.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending 1: %v", err)
	}
	if claimed == nil || claimed.ID != first.ID {
		t.Fatalf("claimed %v, want oldest %q", claimed, first.ID)
	}
	if claimed.Status != domain.JobStatusRunning {
		t.Errorf("claimed status = %q, want running", claimed.Status)
	}
	if claimed.StartedAt == nil {
		t.Error("claimed job StartedAt not set")
	}

	claimed2, err := r.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending 2: %v", err)
	}
	if claimed2 == nil || claimed2.ID != second.ID {
		t.Fatalf("second claim = %v, want %q", claimed2, second.ID)
	}

	// Nothing left to claim.
	none, err := r.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending 3: %v", err)
	}
	if none != nil {
		t.Errorf("expected nil when queue drained, got %v", none)
	}
}

func TestNextPendingAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	const n = 40
	for i := 0; i < n; i++ {
		if _, _, err := r.Enqueue(ctx, "asset", "localfs", string(rune('a'+i%26))+string(rune('0'+i/26)), 0); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	var claimErr error

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := r.NextPending(ctx)
				if err != nil {
					mu.Lock()
					claimErr = err
					mu.Unlock()
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if claimErr != nil {
		t.Fatalf("claim error: %v", claimErr)
	}
	if len(seen) != n {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %q claimed %d times, want exactly 1 (not atomic)", id, c)
		}
	}
}

func TestMarkFailedIncrementsRetriesAndRequeue(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	job, _, _ := r.Enqueue(ctx, "a", "localfs", "/d", 0)
	if _, err := r.NextPending(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := r.MarkFailed(ctx, job.ID, "disk full"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := r.GetByID(ctx, job.ID)
	if got.Status != domain.JobStatusFailed || got.Retries != 1 || got.ErrorMessage != "disk full" {
		t.Errorf("after fail: status %q retries %d msg %q", got.Status, got.Retries, got.ErrorMessage)
	}

	if err := r.Requeue(ctx, job.ID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	got, _ = r.GetByID(ctx, job.ID)
	if got.Status != domain.JobStatusPending {
		t.Errorf("after requeue status = %q, want pending", got.Status)
	}

	// Fail again -> retries must accumulate.
	if _, err := r.NextPending(ctx); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if err := r.MarkFailed(ctx, job.ID, "again"); err != nil {
		t.Fatalf("mark failed 2: %v", err)
	}
	got, _ = r.GetByID(ctx, job.ID)
	if got.Retries != 2 {
		t.Errorf("retries = %d, want 2", got.Retries)
	}
}

func TestPauseResumeCancelAndCompleted(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	job, _, _ := r.Enqueue(ctx, "a", "localfs", "/d", 0)

	if err := r.Pause(ctx, job.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := r.GetByID(ctx, job.ID); got.Status != domain.JobStatusPaused {
		t.Errorf("paused status = %q", got.Status)
	}
	// Pause must not claim a paused job.
	if claimed, _ := r.NextPending(ctx); claimed != nil {
		t.Errorf("NextPending claimed a paused job: %v", claimed)
	}

	if err := r.Resume(ctx, job.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := r.GetByID(ctx, job.ID); got.Status != domain.JobStatusPending {
		t.Errorf("resumed status = %q", got.Status)
	}

	if err := r.Cancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got, _ := r.GetByID(ctx, job.ID); got.Status != domain.JobStatusCancelled {
		t.Errorf("cancelled status = %q", got.Status)
	}

	other, _, _ := r.Enqueue(ctx, "b", "localfs", "/d", 0)
	if _, err := r.NextPending(ctx); err != nil {
		t.Fatalf("claim other: %v", err)
	}
	if err := r.MarkCompleted(ctx, other.ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if got, _ := r.GetByID(ctx, other.ID); got.Status != domain.JobStatusCompleted || got.CompletedAt == nil {
		t.Errorf("completed job wrong: %+v", got)
	}
}

func TestResetRunningOnStartup(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	a, _, _ := r.Enqueue(ctx, "a", "localfs", "/d", 0)
	r.Enqueue(ctx, "b", "localfs", "/d", 0)
	if _, err := r.NextPending(ctx); err != nil { // claims a -> running
		t.Fatalf("claim: %v", err)
	}

	n, err := r.ResetRunningOnStartup(ctx)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n != 1 {
		t.Errorf("reset %d jobs, want 1", n)
	}
	got, _ := r.GetByID(ctx, a.ID)
	if got.Status != domain.JobStatusPending || got.StartedAt != nil {
		t.Errorf("reset job wrong: status %q startedAt %v", got.Status, got.StartedAt)
	}
}

func TestListJobsFilterAndPaginate(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	for i := 0; i < 3; i++ {
		r.Enqueue(ctx, "asset", "localfs", string(rune('a'+i)), 0)
	}
	claimed, _ := r.NextPending(ctx)
	_ = claimed

	pending := domain.JobStatusPending
	jobs, total, err := r.ListJobs(ctx, &pending, Page{Limit: 1})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if total != 2 {
		t.Errorf("pending total = %d, want 2", total)
	}
	if len(jobs) != 1 {
		t.Errorf("returned %d jobs, want 1 (limit)", len(jobs))
	}
}

// TestClaimNextForProviderOldestFirst confirms that oldest-first claiming (the
// default) returns a provider's jobs in FIFO (creation) order regardless of
// SortKey.
func TestClaimNextForProviderOldestFirst(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	// Enqueue three jobs for one provider with out-of-order sort keys.
	a, _, _ := r.Enqueue(ctx, "a1", "localfs", "prov", 300)
	b, _, _ := r.Enqueue(ctx, "a2", "localfs", "prov", 100)
	c, _, _ := r.Enqueue(ctx, "a3", "localfs", "prov", 200)

	want := []string{a.ID, b.ID, c.ID} // creation order
	for i, id := range want {
		claimed, err := r.ClaimNextForProvider(ctx, "prov", false)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if claimed == nil || claimed.ID != id {
			t.Fatalf("claim %d = %v, want %s (FIFO)", i, claimed, id)
		}
	}
	if extra, _ := r.ClaimNextForProvider(ctx, "prov", false); extra != nil {
		t.Fatalf("expected no more jobs, got %v", extra)
	}
}

// TestClaimNextForProviderNewestFirst confirms that newest-first claiming returns
// a provider's jobs in descending SortKey order (newest media first).
func TestClaimNextForProviderNewestFirst(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	a, _, _ := r.Enqueue(ctx, "a1", "localfs", "prov", 300)
	_, _, _ = r.Enqueue(ctx, "a2", "localfs", "prov", 100)
	c, _, _ := r.Enqueue(ctx, "a3", "localfs", "prov", 200)

	// Expect 300 (a) -> 200 (c) -> 100 (b).
	first, _ := r.ClaimNextForProvider(ctx, "prov", true)
	if first == nil || first.ID != a.ID {
		t.Fatalf("first newest claim = %v, want sortKey 300", first)
	}
	second, _ := r.ClaimNextForProvider(ctx, "prov", true)
	if second == nil || second.ID != c.ID {
		t.Fatalf("second newest claim = %v, want sortKey 200", second)
	}
}

// TestClaimNextForProviderScopesToProvider confirms a claim never crosses into a
// different provider's jobs.
func TestClaimNextForProviderScopesToProvider(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	_, _, _ = r.Enqueue(ctx, "a1", "localfs", "provA", 1)
	b, _, _ := r.Enqueue(ctx, "a2", "localfs", "provB", 1)

	claimed, err := r.ClaimNextForProvider(ctx, "provB", false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != b.ID {
		t.Fatalf("claim = %v, want provB job %s", claimed, b.ID)
	}
	// provA still has its pending job untouched.
	if again, _ := r.ClaimNextForProvider(ctx, "provB", false); again != nil {
		t.Fatalf("provB should be drained, got %v", again)
	}
}

// TestRevertToPendingNoRetryIncrement confirms a running job returns to pending
// without touching its retry counter (the cooldown path).
func TestRevertToPendingNoRetryIncrement(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	job, _, _ := r.Enqueue(ctx, "a1", "localfs", "prov", 0)
	claimed, _ := r.ClaimNextForProvider(ctx, "prov", false)
	if claimed == nil || claimed.Status != domain.JobStatusRunning {
		t.Fatalf("claim to running failed: %v", claimed)
	}
	if err := r.RevertToPending(ctx, job.ID); err != nil {
		t.Fatalf("revert: %v", err)
	}
	got, _ := r.GetByID(ctx, job.ID)
	if got.Status != domain.JobStatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if got.Retries != 0 {
		t.Fatalf("retries = %d, want 0 (cooldown must not burn a retry)", got.Retries)
	}
}

// TestMarkCompletedWithNote confirms a completed job can carry a non-fatal note.
func TestMarkCompletedWithNote(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	job, _, _ := r.Enqueue(ctx, "a1", "localfs", "prov", 0)
	if err := r.MarkCompletedWithNote(ctx, job.ID, "verify unavailable (mirror)"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, _ := r.GetByID(ctx, job.ID)
	if got.Status != domain.JobStatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.ErrorMessage != "verify unavailable (mirror)" {
		t.Fatalf("note = %q, want the mirror note", got.ErrorMessage)
	}
}
