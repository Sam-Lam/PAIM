package services

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// newReconcileHarness builds a BackupService over a temp catalog with a captured
// emitter and one enabled provider scoped to the given mediaScope.
func newReconcileHarness(t *testing.T, scope string) (svc *BackupService, gdb *gorm.DB, assets *repo.AssetRepo, jobs *repo.BackupRepo, providerID string, emitter *captureEmitter) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "reconcile.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets = repo.NewAssetRepo(gdb)
	jobs = repo.NewBackupRepo(gdb)
	emitter = &captureEmitter{}
	svc = NewBackupService(nil, jobs, assets, nil, nil, emitter, nil)
	svc.db = gdb
	svc.bfPageSize = 2 // exercise multi-page paging

	p := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true, MediaScope: scope}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, gdb, assets, jobs, p.ID, emitter
}

// seedExtAsset creates a verified, archived asset with a given extension.
func seedExtAsset(t *testing.T, assets *repo.AssetRepo, ext string) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   "IMG." + ext,
		OriginalExtension:  ext,
		QuickHash:          "qh-" + t.Name() + "-" + ext + "-" + time.Now().Format("150405.000000000"),
		CurrentArchivePath: "2024/IMG-" + ext + "-" + randSuffix() + "." + ext,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
		ImportDate:         time.Now(),
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

var randCounter int

func randSuffix() string {
	randCounter++
	return time.Now().Format("150405.000000000") + "-" + itoa(randCounter)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// TestPreviewReconcileCounts verifies the dry-run counts: out-of-scope pending/
// paused jobs to cancel, and in-scope eligible assets missing a job to enqueue.
func TestPreviewReconcileCounts(t *testing.T) {
	svc, gdb, assets, jobs, providerID, _ := newReconcileHarness(t, "photos")
	ctx := context.Background()

	// In scope, already queued: not missing, not to-cancel.
	photoQueued := seedExtAsset(t, assets, "jpg")
	if _, _, err := jobs.Enqueue(ctx, photoQueued.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// In scope, missing: to-enqueue.
	seedExtAsset(t, assets, "jpg")
	seedExtAsset(t, assets, "heic")
	// Out of scope, queued (pending + paused): to-cancel.
	videoPending := seedExtAsset(t, assets, "mov")
	if _, _, err := jobs.Enqueue(ctx, videoPending.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	videoPaused := seedExtAsset(t, assets, "mov")
	if _, _, err := jobs.Enqueue(ctx, videoPaused.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := jobs.Pause(ctx, jobByAsset(t, gdb, providerID, videoPaused.ID).ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Out of scope, completed: NOT to-cancel (only pending/paused are reclaimed).
	videoDone := seedExtAsset(t, assets, "mov")
	if _, _, err := jobs.Enqueue(ctx, videoDone.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := jobs.MarkCompleted(ctx, jobByAsset(t, gdb, providerID, videoDone.ID).ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}

	preview, err := svc.PreviewReconcile(ctx, providerID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.ToCancel != 2 {
		t.Fatalf("toCancel = %d, want 2 (out-of-scope pending + paused)", preview.ToCancel)
	}
	if preview.ToEnqueue != 2 {
		t.Fatalf("toEnqueue = %d, want 2 (in-scope missing jpg + heic)", preview.ToEnqueue)
	}
}

// TestStartReconcileCancelsAndEnqueues verifies the reconcile run: it cancels
// exactly the out-of-scope pending/paused jobs (stamping the note), leaves an
// out-of-scope COMPLETED job untouched, enqueues in-scope missing assets, and
// emits a reconcile-completed event carrying {cancelled, enqueued}.
func TestStartReconcileCancelsAndEnqueues(t *testing.T) {
	svc, gdb, assets, jobs, providerID, emitter := newReconcileHarness(t, "photos")
	ctx := context.Background()

	photoQueued := seedExtAsset(t, assets, "jpg")
	if _, _, err := jobs.Enqueue(ctx, photoQueued.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	photoMissing := seedExtAsset(t, assets, "heic")

	videoPending := seedExtAsset(t, assets, "mov")
	if _, _, err := jobs.Enqueue(ctx, videoPending.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	videoDone := seedExtAsset(t, assets, "mp4")
	if _, _, err := jobs.Enqueue(ctx, videoDone.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := jobs.MarkCompleted(ctx, jobByAsset(t, gdb, providerID, videoDone.ID).ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}

	if _, err := svc.StartReconcile(ctx, providerID); err != nil {
		t.Fatalf("start reconcile: %v", err)
	}
	waitBackfillDone(t, svc)

	// Out-of-scope pending -> cancelled with note.
	vp := jobByAsset(t, gdb, providerID, videoPending.ID)
	if vp.Status != domain.JobStatusCancelled {
		t.Fatalf("out-of-scope pending status = %q, want cancelled", vp.Status)
	}
	if vp.ErrorMessage != outOfScopeNote {
		t.Fatalf("cancelled note = %q, want %q", vp.ErrorMessage, outOfScopeNote)
	}
	// Out-of-scope completed -> untouched.
	if vd := jobByAsset(t, gdb, providerID, videoDone.ID); vd.Status != domain.JobStatusCompleted {
		t.Fatalf("out-of-scope completed status = %q, want completed (untouched)", vd.Status)
	}
	// In-scope already-queued -> still pending (idempotent, not re-created).
	if pq := jobByAsset(t, gdb, providerID, photoQueued.ID); pq.Status != domain.JobStatusPending {
		t.Fatalf("in-scope queued status = %q, want pending", pq.Status)
	}
	// In-scope missing -> newly enqueued pending.
	pm := jobByAsset(t, gdb, providerID, photoMissing.ID)
	if pm.Status != domain.JobStatusPending {
		t.Fatalf("in-scope missing status = %q, want pending (enqueued)", pm.Status)
	}

	// Completion event carries the outcome counts.
	evs := emitter.byName(EventBackupReconcileCompleted)
	if len(evs) != 1 {
		t.Fatalf("reconcile-completed events = %d, want 1", len(evs))
	}
	done, ok := evs[0].(BackupReconcileCompleted)
	if !ok {
		t.Fatalf("unexpected event payload type %T", evs[0])
	}
	if done.Cancelled != 1 {
		t.Fatalf("event cancelled = %d, want 1", done.Cancelled)
	}
	if done.Enqueued != 1 {
		t.Fatalf("event enqueued = %d, want 1", done.Enqueued)
	}
}

// TestReconcileGuardVsBackfill verifies a reconcile and a backfill share the
// single-instance guard: neither can start while the other is marked running.
func TestReconcileGuardVsBackfill(t *testing.T) {
	svc, _, _, _, providerID, _ := newReconcileHarness(t, "photos")
	ctx := context.Background()

	// Simulate a backfill already running.
	svc.bfMu.Lock()
	svc.bfRunning = true
	svc.bfMu.Unlock()

	if _, err := svc.StartReconcile(ctx, providerID); !errors.Is(err, ErrBackfillInProgress) {
		t.Fatalf("StartReconcile while backfill running = %v, want ErrBackfillInProgress", err)
	}
	if _, err := svc.StartBackfill(ctx, providerID); !errors.Is(err, ErrBackfillInProgress) {
		t.Fatalf("StartBackfill while backfill running = %v, want ErrBackfillInProgress", err)
	}

	svc.bfMu.Lock()
	svc.bfRunning = false
	svc.bfMu.Unlock()
}

// jobByAsset loads the job for a destination+asset from the DB.
func jobByAsset(t *testing.T, gdb *gorm.DB, destination, assetID string) domain.BackupJob {
	t.Helper()
	var j domain.BackupJob
	if err := gdb.Where("destination = ? AND asset_id = ?", destination, assetID).First(&j).Error; err != nil {
		t.Fatalf("load job for asset %s: %v", assetID, err)
	}
	return j
}
