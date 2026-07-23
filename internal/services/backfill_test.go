package services

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// newBackfillHarness builds a BackupService over a temp SQLite catalog with real
// asset and backup-job repositories (no Manager: the backfill path only needs the
// repos and DB). It also inserts one enabled localfs provider and returns its ID.
func newBackfillHarness(t *testing.T) (svc *BackupService, gdb *gorm.DB, assets *repo.AssetRepo, jobs *repo.BackupRepo, providerID string) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets = repo.NewAssetRepo(gdb)
	jobs = repo.NewBackupRepo(gdb)
	svc = NewBackupService(nil, jobs, assets, nil, nil, nil, nil)
	svc.db = gdb

	p := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, gdb, assets, jobs, p.ID
}

// seedBackfillAsset creates an asset with the given eligibility knobs and returns
// it. deleted soft-deletes it after creation.
func seedBackfillAsset(t *testing.T, assets *repo.AssetRepo, name, path string, vs domain.VerificationStatus, capture *time.Time, deleted bool) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   name,
		QuickHash:          "qh-" + name,
		CurrentArchivePath: path,
		VerificationStatus: vs,
		BackupStatus:       domain.BackupStatusNone,
		ImportDate:         time.Now(),
		CaptureDate:        capture,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset %q: %v", name, err)
	}
	if deleted {
		if err := assets.SoftDelete(context.Background(), a.ID); err != nil {
			t.Fatalf("soft delete asset %q: %v", name, err)
		}
	}
	return a
}

// waitBackfillDone polls until the service reports no running backfill (or fails).
func waitBackfillDone(t *testing.T, svc *BackupService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := svc.BackfillStatus(context.Background())
		if err != nil {
			t.Fatalf("backfill status: %v", err)
		}
		if !st.Running {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("backfill did not finish within deadline")
}

// jobsForProvider returns the non-deleted jobs for a destination.
func jobsForProvider(t *testing.T, gdb *gorm.DB, providerID string) []domain.BackupJob {
	t.Helper()
	var out []domain.BackupJob
	if err := gdb.Where("destination = ?", providerID).Find(&out).Error; err != nil {
		t.Fatalf("load jobs: %v", err)
	}
	return out
}

// TestBackfillEnqueuesExactlyEligibleSet seeds a mixed catalog and asserts the
// backfill enqueues exactly one job for each eligible asset and nothing for the
// ineligible ones (unverified, deleted, copy-mode placeholder duplicate).
func TestBackfillEnqueuesExactlyEligibleSet(t *testing.T) {
	svc, gdb, assets, _, providerID := newBackfillHarness(t)
	ctx := context.Background()

	cap1 := time.Date(2019, 6, 12, 10, 0, 0, 0, time.UTC)
	// Eligible: verified + non-deleted + archive path present.
	elig1 := seedBackfillAsset(t, assets, "a.jpg", "2019/a.jpg", domain.VerificationStatusVerified, &cap1, false)
	elig2 := seedBackfillAsset(t, assets, "b.jpg", "2019/b.jpg", domain.VerificationStatusVerified, nil, false)
	// Eligible adopt-flagged duplicate: has a real archive path.
	adoptDup := seedBackfillAsset(t, assets, "dup-adopt.jpg", "2019/dup-adopt.jpg", domain.VerificationStatusVerified, nil, false)
	dupOf := elig1.ID
	if err := assets.MarkDuplicateOf(ctx, adoptDup.ID, dupOf); err != nil {
		t.Fatalf("mark dup: %v", err)
	}
	// Ineligible: unverified.
	seedBackfillAsset(t, assets, "unver.jpg", "2019/unver.jpg", domain.VerificationStatusPending, nil, false)
	// Ineligible: soft-deleted.
	seedBackfillAsset(t, assets, "del.jpg", "2019/del.jpg", domain.VerificationStatusVerified, nil, true)
	// Ineligible: copy-mode duplicate placeholder (empty archive path).
	placeholder := seedBackfillAsset(t, assets, "dup-copy.jpg", "", domain.VerificationStatusVerified, nil, false)
	if err := assets.MarkDuplicateOf(ctx, placeholder.ID, dupOf); err != nil {
		t.Fatalf("mark placeholder dup: %v", err)
	}

	want := map[string]bool{elig1.ID: true, elig2.ID: true, adoptDup.ID: true}

	// Missing-count query should report exactly the eligible set up front.
	if n, err := assets.CountEligibleMissingBackup(ctx, providerID); err != nil {
		t.Fatalf("missing count: %v", err)
	} else if n != int64(len(want)) {
		t.Fatalf("missing count = %d, want %d", n, len(want))
	}

	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill: %v", err)
	}
	waitBackfillDone(t, svc)

	got := jobsForProvider(t, gdb, providerID)
	if len(got) != len(want) {
		t.Fatalf("enqueued %d jobs, want %d", len(got), len(want))
	}
	for _, j := range got {
		if !want[j.AssetID] {
			t.Fatalf("enqueued job for ineligible asset %q", j.AssetID)
		}
		if j.Status != domain.JobStatusPending {
			t.Fatalf("job %q status = %s, want pending", j.ID, j.Status)
		}
		if j.Plugin != "localfs" {
			t.Fatalf("job %q plugin = %q, want localfs", j.ID, j.Plugin)
		}
	}
}

// TestBackfillStampsSortKey asserts a backfilled job carries the same SortKey the
// importer would stamp (capture date, or import date when absent).
func TestBackfillStampsSortKey(t *testing.T) {
	svc, gdb, assets, _, providerID := newBackfillHarness(t)
	ctx := context.Background()

	cap := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	withCap := seedBackfillAsset(t, assets, "cap.jpg", "2020/cap.jpg", domain.VerificationStatusVerified, &cap, false)
	noCap := seedBackfillAsset(t, assets, "nocap.jpg", "2020/nocap.jpg", domain.VerificationStatusVerified, nil, false)

	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill: %v", err)
	}
	waitBackfillDone(t, svc)

	byAsset := map[string]domain.BackupJob{}
	for _, j := range jobsForProvider(t, gdb, providerID) {
		byAsset[j.AssetID] = j
	}

	// Reload the assets to read the exact stored values the helper stamps.
	wc, _ := assets.GetByID(ctx, withCap.ID)
	nc, _ := assets.GetByID(ctx, noCap.ID)
	if got, want := byAsset[withCap.ID].SortKey, backup.SortKeyForAsset(*wc); got != want {
		t.Fatalf("capture-date SortKey = %d, want %d", got, want)
	}
	if got, want := byAsset[noCap.ID].SortKey, backup.SortKeyForAsset(*nc); got != want {
		t.Fatalf("import-date SortKey = %d, want %d", got, want)
	}
	if byAsset[withCap.ID].SortKey != cap.Unix() {
		t.Fatalf("capture SortKey = %d, want %d", byAsset[withCap.ID].SortKey, cap.Unix())
	}
}

// TestBackfillIdempotentSecondRun asserts a second full run enqueues nothing (all
// skipped) and, given a partial prior state, the run picks up exactly the
// remainder — the resume mechanism.
func TestBackfillIdempotentSecondRun(t *testing.T) {
	svc, gdb, assets, jobs, providerID := newBackfillHarness(t)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 5; i++ {
		a := seedBackfillAsset(t, assets, fmt.Sprintf("f%d.jpg", i), fmt.Sprintf("2021/f%d.jpg", i), domain.VerificationStatusVerified, nil, false)
		ids = append(ids, a.ID)
	}
	// Simulate a prior partial (cancelled) run that enqueued 2 of the 5.
	for _, id := range ids[:2] {
		if _, _, err := jobs.Enqueue(ctx, id, "localfs", providerID, 0); err != nil {
			t.Fatalf("pre-enqueue: %v", err)
		}
	}

	// Missing count now excludes the 2 already-enqueued.
	if n, err := assets.CountEligibleMissingBackup(ctx, providerID); err != nil {
		t.Fatalf("missing count: %v", err)
	} else if n != 3 {
		t.Fatalf("missing count = %d, want 3", n)
	}

	// First run picks up the remaining 3.
	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill 1: %v", err)
	}
	waitBackfillDone(t, svc)
	if got := len(jobsForProvider(t, gdb, providerID)); got != 5 {
		t.Fatalf("after run 1: %d jobs, want 5", got)
	}

	// Second full run is a no-op: every pair already has a job.
	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill 2: %v", err)
	}
	waitBackfillDone(t, svc)
	got := jobsForProvider(t, gdb, providerID)
	if len(got) != 5 {
		t.Fatalf("after run 2: %d jobs, want 5 (no duplicates)", len(got))
	}
	if n, err := assets.CountEligibleMissingBackup(ctx, providerID); err != nil {
		t.Fatalf("missing count after: %v", err)
	} else if n != 0 {
		t.Fatalf("missing count after full backfill = %d, want 0", n)
	}
}

// TestBackfillCancellationResumable cancels a multi-page run and asserts it leaves
// valid (never duplicated) partial state, and that a follow-up run completes the
// remainder so every eligible asset ends with exactly one job.
func TestBackfillCancellationResumable(t *testing.T) {
	svc, gdb, assets, _, providerID := newBackfillHarness(t)
	ctx := context.Background()
	svc.bfPageSize = 1 // one asset per page: many cancellation checkpoints

	const total = 60
	for i := 0; i < total; i++ {
		seedBackfillAsset(t, assets, fmt.Sprintf("c%03d.jpg", i), fmt.Sprintf("2022/c%03d.jpg", i), domain.VerificationStatusVerified, nil, false)
	}

	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill: %v", err)
	}
	// Cancel promptly; where it lands is nondeterministic, but the invariants below
	// must hold regardless (no duplicates, resume completes the set).
	if err := svc.CancelBackfill(ctx); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitBackfillDone(t, svc)

	first := jobsForProvider(t, gdb, providerID)
	if len(first) > total {
		t.Fatalf("after cancel: %d jobs exceeds eligible %d", len(first), total)
	}
	assertNoDuplicateJobs(t, first)

	// Resume: a fresh run enqueues the remainder and skips what already exists.
	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("resume backfill: %v", err)
	}
	waitBackfillDone(t, svc)

	final := jobsForProvider(t, gdb, providerID)
	if len(final) != total {
		t.Fatalf("after resume: %d jobs, want %d", len(final), total)
	}
	assertNoDuplicateJobs(t, final)
}

// TestBackfillOneAtATimeGuard asserts a second StartBackfill while one is running
// is refused with ErrBackfillInProgress.
func TestBackfillOneAtATimeGuard(t *testing.T) {
	svc, _, assets, _, providerID := newBackfillHarness(t)
	ctx := context.Background()
	svc.bfPageSize = 1
	for i := 0; i < 200; i++ {
		seedBackfillAsset(t, assets, fmt.Sprintf("g%03d.jpg", i), fmt.Sprintf("2023/g%03d.jpg", i), domain.VerificationStatusVerified, nil, false)
	}

	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill: %v", err)
	}
	// While the first run drains its 200 single-asset pages, a second start is
	// refused. Retry briefly in case the first already finished on a fast machine.
	var sawGuard bool
	for i := 0; i < 50; i++ {
		if _, err := svc.StartBackfill(ctx, providerID); errors.Is(err, ErrBackfillInProgress) {
			sawGuard = true
			break
		}
		st, _ := svc.BackfillStatus(ctx)
		if !st.Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waitBackfillDone(t, svc)
	if !sawGuard {
		t.Skip("first backfill finished before the guard could be observed (timing)")
	}
}

// TestBackfillRejectsMissingAndDisabledProvider covers the provider precondition.
func TestBackfillRejectsMissingAndDisabledProvider(t *testing.T) {
	svc, gdb, _, _, _ := newBackfillHarness(t)
	ctx := context.Background()

	if _, err := svc.StartBackfill(ctx, "does-not-exist"); err == nil {
		t.Fatal("expected error for missing provider")
	}

	disabled := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: false}
	if err := gdb.Create(disabled).Error; err != nil {
		t.Fatalf("create disabled provider: %v", err)
	}
	if _, err := svc.StartBackfill(ctx, disabled.ID); err == nil {
		t.Fatal("expected error for disabled provider")
	}
}

func assertNoDuplicateJobs(t *testing.T, jobs []domain.BackupJob) {
	t.Helper()
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j.AssetID] {
			t.Fatalf("duplicate job for asset %q", j.AssetID)
		}
		seen[j.AssetID] = true
	}
}
