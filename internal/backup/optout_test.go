package backup_test

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// jobsByDestination indexes an asset's jobs by their destination (provider ID).
func jobsByDestination(t *testing.T, h *testHarness, assetID string) map[string]domain.BackupJob {
	t.Helper()
	jobs, err := h.jobs.JobsForAsset(context.Background(), assetID)
	if err != nil {
		t.Fatalf("jobs for asset: %v", err)
	}
	out := make(map[string]domain.BackupJob, len(jobs))
	for _, j := range jobs {
		out[j.Destination] = j
	}
	return out
}

// TestEnqueueForAsset_SkipCreatesOptedOut verifies the per-import provider
// opt-out: a skipped enabled provider gets a durable opted_out job (SortKey
// stamped identically to a real job) instead of a pending one, the non-skipped
// provider gets a pending job, and the returned "created" count reflects only
// the real pending work (so the importer stamps BackupStatus=pending correctly).
func TestEnqueueForAsset_SkipCreatesOptedOut(t *testing.T) {
	h := newHarness(t)
	keep := h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)
	skip := h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)
	capture := time.Date(2019, 6, 12, 10, 0, 0, 0, time.UTC)
	asset := h.addAssetWith(t, "/library/2019/2019-06-12/IMG.JPG", &capture)

	ctx := context.Background()
	var created int
	err := h.db.Transaction(func(tx *gorm.DB) error {
		m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
		var e error
		created, e = m.EnqueueForAsset(ctx, tx, asset.ID, []string{skip.ID})
		return e
	})
	if err != nil {
		t.Fatalf("enqueue for asset: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1 (only the non-skipped provider is real pending work)", created)
	}

	byDest := jobsByDestination(t, h, asset.ID)
	if len(byDest) != 2 {
		t.Fatalf("want 2 jobs (one per provider), got %d", len(byDest))
	}
	if got := byDest[keep.ID].Status; got != domain.JobStatusPending {
		t.Fatalf("kept provider job status = %q, want pending", got)
	}
	if got := byDest[skip.ID].Status; got != domain.JobStatusOptedOut {
		t.Fatalf("skipped provider job status = %q, want opted_out", got)
	}
	wantKey := backup.SortKeyForAsset(*asset)
	if byDest[skip.ID].SortKey != wantKey {
		t.Fatalf("opted_out SortKey = %d, want %d (must be stamped like a real job)", byDest[skip.ID].SortKey, wantKey)
	}
	if byDest[keep.ID].SortKey != wantKey {
		t.Fatalf("pending SortKey = %d, want %d", byDest[keep.ID].SortKey, wantKey)
	}
}

// TestClaimNeverTouchesOptedOut verifies the worker pool never claims an
// opted_out job: with only an opted_out job present, no upload happens and the
// job stays opted_out.
func TestClaimNeverTouchesOptedOut(t *testing.T) {
	h := newHarness(t)
	fake := okPlugin("localfs")
	h.registry.Register("localfs", func() backup.Plugin { return fake })
	skip := h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)
	asset := h.addAsset(t)

	ctx := context.Background()
	job, created, err := h.jobs.EnqueueOptedOut(ctx, asset.ID, skip.PluginName, skip.ID, 0)
	if err != nil {
		t.Fatalf("enqueue opted-out: %v", err)
	}
	if !created {
		t.Fatalf("expected opted-out job to be created")
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	// Give workers ample time to (not) claim the opted_out job.
	time.Sleep(80 * time.Millisecond)
	if got := h.jobByID(t, job.ID).Status; got != domain.JobStatusOptedOut {
		t.Fatalf("opted_out job status = %q, want opted_out (must never be claimed)", got)
	}
	if fake.uploadCount() != 0 {
		t.Fatalf("uploads = %d, want 0 (opted_out must not upload)", fake.uploadCount())
	}
}

// TestOptedOut_SafeToEraseChain verifies the aggregate/safe-to-erase chain:
// opting the ONLY required provider out leaves the required set empty, so the
// asset aggregate is "none" (not complete) and thus not safe-to-erase; a mirror
// opt-out never affects the verdict (a completed required backup still reads
// complete).
func TestOptedOut_SafeToEraseChain(t *testing.T) {
	h := newHarness(t)
	required := h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)
	mirror := h.addProviderFull(t, "mirror", true, domain.UploadOrderOldestFirst)

	mirrorIDs, err := h.providers.MirrorIDs(context.Background())
	if err != nil {
		t.Fatalf("mirror ids: %v", err)
	}
	isMirror := func(id string) bool { return mirrorIDs[id] }

	// Only required provider, opted out -> required set empty -> none (not complete).
	onlyRequiredSkipped := []domain.BackupJob{
		{Destination: required.ID, Status: domain.JobStatusOptedOut},
	}
	if got := backup.AggregateBackupStatus(onlyRequiredSkipped, isMirror); got != domain.BackupStatusNone {
		t.Fatalf("required-only opt-out aggregate = %q, want none (not complete -> not safe to erase)", got)
	}

	// Required completed + mirror opted out -> complete (mirror opt-out unaffected).
	mirrorSkipped := []domain.BackupJob{
		{Destination: required.ID, Status: domain.JobStatusCompleted},
		{Destination: mirror.ID, Status: domain.JobStatusOptedOut},
	}
	if got := backup.AggregateBackupStatus(mirrorSkipped, isMirror); got != domain.BackupStatusComplete {
		t.Fatalf("mirror opt-out aggregate = %q, want complete (mirror never blocks the verdict)", got)
	}

	// Two required providers, one pending one opted out -> pending (still work to do).
	mixed := []domain.BackupJob{
		{Destination: required.ID, Status: domain.JobStatusPending},
		{Destination: "other-required", Status: domain.JobStatusOptedOut},
	}
	if got := backup.AggregateBackupStatus(mixed, isMirror); got != domain.BackupStatusPending {
		t.Fatalf("mixed aggregate = %q, want pending", got)
	}
}
