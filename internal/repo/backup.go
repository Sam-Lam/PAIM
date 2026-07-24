package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// BackupRepo persists and queries the BackupJob queue. The BackupJob rows are
// the queue, which makes the backup system restart-safe by construction.
type BackupRepo struct {
	db *gorm.DB
}

// NewBackupRepo constructs a BackupRepo.
func NewBackupRepo(db *gorm.DB) *BackupRepo { return &BackupRepo{db: db} }

// WithTx binds the repo to a transaction handle.
func (r *BackupRepo) WithTx(tx *gorm.DB) *BackupRepo { return &BackupRepo{db: tx} }

// enqueueIdempotencyStatuses is the set of job statuses that make a
// (asset, plugin, destination) triple "already enqueued", so a re-enqueue is a
// no-op. It includes opted_out: a deliberate per-import opt-out is a durable
// record, so a later Enqueue (import resume, backfill) must NOT silently create
// a competing pending job for a destination the user excluded — un-skipping is
// the explicit RequeueOptedOut path only.
var enqueueIdempotencyStatuses = []domain.JobStatus{
	domain.JobStatusPending,
	domain.JobStatusRunning,
	domain.JobStatusCompleted,
	domain.JobStatusOptedOut,
}

// Enqueue idempotently adds a PENDING backup job for an asset/plugin/destination.
// If a pending, running, completed, or opted-out job already exists for the same
// triple, that existing job is returned and created is false; otherwise a new
// pending job is created with the given sortKey (the per-provider upload-order
// key). This makes re-enqueuing after a crash safe.
func (r *BackupRepo) Enqueue(ctx context.Context, assetID, plugin, destination string, sortKey int64) (job *domain.BackupJob, created bool, err error) {
	return r.enqueue(ctx, assetID, plugin, destination, sortKey, domain.JobStatusPending)
}

// EnqueueOptedOut idempotently records an OPTED-OUT backup job for an
// asset/plugin/destination — the durable "the user deliberately excluded this
// destination for this asset at import time" marker. It shares Enqueue's
// idempotency set, so it never displaces an existing pending/running/completed
// job (an already-real backup is never downgraded to opted-out) and is itself a
// no-op when an opted-out job already exists (safe on import resume).
func (r *BackupRepo) EnqueueOptedOut(ctx context.Context, assetID, plugin, destination string, sortKey int64) (job *domain.BackupJob, created bool, err error) {
	return r.enqueue(ctx, assetID, plugin, destination, sortKey, domain.JobStatusOptedOut)
}

// enqueue is the shared idempotent insert for Enqueue/EnqueueOptedOut: it creates
// a job in the given status only when no job in enqueueIdempotencyStatuses
// already exists for the triple.
func (r *BackupRepo) enqueue(ctx context.Context, assetID, plugin, destination string, sortKey int64, status domain.JobStatus) (job *domain.BackupJob, created bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing domain.BackupJob
		findErr := tx.Where(
			"asset_id = ? AND plugin = ? AND destination = ? AND status IN ?",
			assetID, plugin, destination,
			enqueueIdempotencyStatuses,
		).First(&existing).Error
		if findErr == nil {
			job = &existing
			created = false
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}

		newJob := &domain.BackupJob{
			AssetID:     assetID,
			Plugin:      plugin,
			Destination: destination,
			Status:      status,
			SortKey:     sortKey,
		}
		if createErr := tx.Create(newJob).Error; createErr != nil {
			return createErr
		}
		job = newJob
		created = true
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("repo: enqueue backup job (asset %q, plugin %q): %w", assetID, plugin, err)
	}
	return job, created, nil
}

// NextPending atomically claims the oldest pending job (across all providers),
// transitioning it to running and stamping StartedAt. It returns (nil, nil) when
// no pending job is available. The claim is a single UPDATE ... RETURNING
// statement, so concurrent callers never claim the same job. It is retained for
// callers that do not need per-provider ordering; the Manager uses
// ClaimNextForProvider so it can honor each provider's UploadOrder and skip
// providers in cooldown.
func (r *BackupRepo) NextPending(ctx context.Context) (*domain.BackupJob, error) {
	now := time.Now().UTC()
	const claimSQL = `
UPDATE backup_jobs
SET status = ?, started_at = ?, updated_at = ?
WHERE id = (
    SELECT id FROM backup_jobs
    WHERE status = ? AND deleted_at IS NULL
    ORDER BY created_at ASC, id ASC
    LIMIT 1
)
RETURNING id`

	var claimedID string
	res := r.db.WithContext(ctx).Raw(
		claimSQL,
		domain.JobStatusRunning, now, now,
		domain.JobStatusPending,
	).Scan(&claimedID)
	if res.Error != nil {
		return nil, fmt.Errorf("repo: claim next pending backup job: %w", res.Error)
	}
	if claimedID == "" {
		return nil, nil
	}
	return r.GetByID(ctx, claimedID)
}

// ClaimNextForProvider atomically claims the next pending job for one provider
// (identified by destination = provider ID), transitioning it to running and
// stamping StartedAt. Ordering honors the provider's upload order: newestFirst
// claims the highest SortKey first (newest media), otherwise the oldest job first
// (FIFO). It returns (nil, nil) when the provider has no pending job. Restricting
// the claim to a single provider is what lets the Manager both round-robin across
// providers and skip any provider currently in quota cooldown.
func (r *BackupRepo) ClaimNextForProvider(ctx context.Context, destination string, newestFirst bool) (*domain.BackupJob, error) {
	now := time.Now().UTC()
	// oldest_first preserves strict FIFO (creation order); newest_first drains the
	// highest SortKey (newest media) first, so new imports jump the queue.
	order := "created_at ASC, id ASC"
	if newestFirst {
		order = "sort_key DESC, created_at DESC, id DESC"
	}
	claimSQL := `
UPDATE backup_jobs
SET status = ?, started_at = ?, updated_at = ?
WHERE id = (
    SELECT id FROM backup_jobs
    WHERE status = ? AND deleted_at IS NULL AND destination = ?
    ORDER BY ` + order + `
    LIMIT 1
)
RETURNING id`

	var claimedID string
	res := r.db.WithContext(ctx).Raw(
		claimSQL,
		domain.JobStatusRunning, now, now,
		domain.JobStatusPending, destination,
	).Scan(&claimedID)
	if res.Error != nil {
		return nil, fmt.Errorf("repo: claim next pending backup job for provider %q: %w", destination, res.Error)
	}
	if claimedID == "" {
		return nil, nil
	}
	return r.GetByID(ctx, claimedID)
}

// GetByID returns the non-deleted job with the given ID, or ErrNotFound.
func (r *BackupRepo) GetByID(ctx context.Context, id string) (*domain.BackupJob, error) {
	var j domain.BackupJob
	err := r.db.WithContext(ctx).First(&j, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("repo: get backup job %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: get backup job %q: %w", id, err)
	}
	return &j, nil
}

// MarkCompleted transitions a job to completed and stamps CompletedAt, clearing
// any prior error message.
func (r *BackupRepo) MarkCompleted(ctx context.Context, id string) error {
	now := time.Now().UTC()
	return r.updateJob(ctx, id, map[string]any{
		"status":        domain.JobStatusCompleted,
		"completed_at":  now,
		"error_message": "",
	})
}

// MarkCompletedWithNote transitions a job to completed and stamps CompletedAt,
// recording a non-fatal note in ErrorMessage (used for the best-effort mirror
// verification path, where a failed/unavailable Verify still completes the job
// with an explanatory note rather than failing it).
func (r *BackupRepo) MarkCompletedWithNote(ctx context.Context, id, note string) error {
	now := time.Now().UTC()
	return r.updateJob(ctx, id, map[string]any{
		"status":        domain.JobStatusCompleted,
		"completed_at":  now,
		"error_message": note,
	})
}

// RevertToPending transitions a job from running back to pending WITHOUT
// incrementing its retry counter, clearing StartedAt. It is used when an upload
// attempt is abandoned for a reason that is not the job's fault — a provider
// quota cooldown — so the job simply waits and is re-claimed once the provider's
// cooldown expires.
func (r *BackupRepo) RevertToPending(ctx context.Context, id string) error {
	return r.transition(ctx, id, []domain.JobStatus{domain.JobStatusRunning}, domain.JobStatusPending, map[string]any{
		"started_at": nil,
	})
}

// MarkFailed transitions a job to failed, records the error message, and
// increments the retry counter.
func (r *BackupRepo) MarkFailed(ctx context.Context, id, errMsg string) error {
	return r.updateJob(ctx, id, map[string]any{
		"status":        domain.JobStatusFailed,
		"error_message": errMsg,
		"retries":       gorm.Expr("retries + 1"),
	})
}

// Requeue transitions a failed job back to pending so a worker can retry it.
func (r *BackupRepo) Requeue(ctx context.Context, id string) error {
	return r.transition(ctx, id, []domain.JobStatus{domain.JobStatusFailed}, domain.JobStatusPending, map[string]any{
		"started_at":   nil,
		"completed_at": nil,
	})
}

// RequeueAllFailed transitions every failed job back to pending in a single
// UPDATE (clearing StartedAt/CompletedAt) and returns the number requeued. It is
// the bulk form of Requeue used by "Retry all failed"; the status guard in the
// WHERE clause keeps it a valid failed→pending transition for every row.
func (r *BackupRepo) RequeueAllFailed(ctx context.Context) (int64, error) {
	res := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("status = ?", domain.JobStatusFailed).
		Updates(map[string]any{
			"status":       domain.JobStatusPending,
			"started_at":   nil,
			"completed_at": nil,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("repo: requeue all failed backup jobs: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// RequeueOptedOut flips opted-out jobs for one destination (provider ID) back to
// pending so they upload — the reversal of a per-import opt-out ("Queue anyway").
// When sessionID is non-empty the flip is scoped to the assets imported under
// that session; an empty sessionID requeues every opted-out job for the
// destination. It returns the number of jobs requeued. The status guard keeps it
// a valid opted_out→pending transition for every row.
func (r *BackupRepo) RequeueOptedOut(ctx context.Context, destination, sessionID string) (int64, error) {
	q := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("destination = ? AND status = ?", destination, domain.JobStatusOptedOut)
	if sessionID != "" {
		q = q.Where("asset_id IN (?)",
			r.db.WithContext(ctx).Model(&domain.Asset{}).Select("id").Where("session_id = ?", sessionID))
	}
	res := q.Updates(map[string]any{
		"status":       domain.JobStatusPending,
		"started_at":   nil,
		"completed_at": nil,
	})
	if res.Error != nil {
		return 0, fmt.Errorf("repo: requeue opted-out backup jobs (dest %q): %w", destination, res.Error)
	}
	return res.RowsAffected, nil
}

// CountOutOfScopePending counts this provider's pending or paused jobs whose
// asset's file extension falls OUTSIDE the given in-scope extension whitelist —
// the jobs a scope-narrowing reconcile would cancel. scopedExts is the whitelist
// of in-scope extensions (mediatype.ScopedExtensions of the provider's new scope);
// an empty whitelist means the scope imposes no restriction, so nothing is out of
// scope and the count is 0. Only non-deleted assets are considered. Completed,
// failed, cancelled, and opted-out jobs are untouched — a reconcile only reclaims
// still-queued work.
func (r *BackupRepo) CountOutOfScopePending(ctx context.Context, destination string, scopedExts []string) (int64, error) {
	if len(scopedExts) == 0 {
		return 0, nil
	}
	var n int64
	err := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("destination = ? AND status IN ?", destination,
			[]domain.JobStatus{domain.JobStatusPending, domain.JobStatusPaused}).
		Where("asset_id IN (SELECT id FROM assets WHERE deleted_at IS NULL AND lower(original_extension) NOT IN ?)", scopedExts).
		Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("repo: count out-of-scope pending jobs (dest %q): %w", destination, err)
	}
	return n, nil
}

// CancelOutOfScopePending transitions this provider's pending/paused jobs whose
// asset is out of scope (see CountOutOfScopePending) to cancelled in one UPDATE,
// stamping note into ErrorMessage, and returns the number cancelled. The status
// guard keeps it a valid pending|paused -> cancelled transition for every row. A
// cancelled job is excluded from an asset's aggregate BackupStatus, so the derived
// scope exclusion is honored downstream. scopedExts empty -> no-op (nothing out of
// scope).
func (r *BackupRepo) CancelOutOfScopePending(ctx context.Context, destination, note string, scopedExts []string) (int64, error) {
	if len(scopedExts) == 0 {
		return 0, nil
	}
	res := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("destination = ? AND status IN ?", destination,
			[]domain.JobStatus{domain.JobStatusPending, domain.JobStatusPaused}).
		Where("asset_id IN (SELECT id FROM assets WHERE deleted_at IS NULL AND lower(original_extension) NOT IN ?)", scopedExts).
		Updates(map[string]any{
			"status":        domain.JobStatusCancelled,
			"error_message": note,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("repo: cancel out-of-scope pending jobs (dest %q): %w", destination, res.Error)
	}
	return res.RowsAffected, nil
}

// Pause transitions a pending job to paused.
func (r *BackupRepo) Pause(ctx context.Context, id string) error {
	return r.transition(ctx, id, []domain.JobStatus{domain.JobStatusPending}, domain.JobStatusPaused, nil)
}

// Resume transitions a paused job back to pending.
func (r *BackupRepo) Resume(ctx context.Context, id string) error {
	return r.transition(ctx, id, []domain.JobStatus{domain.JobStatusPaused}, domain.JobStatusPending, nil)
}

// Cancel transitions a job to cancelled from any non-terminal state.
func (r *BackupRepo) Cancel(ctx context.Context, id string) error {
	return r.transition(ctx, id,
		[]domain.JobStatus{domain.JobStatusPending, domain.JobStatusRunning, domain.JobStatusPaused, domain.JobStatusFailed},
		domain.JobStatusCancelled, nil)
}

// transition atomically moves a job from any of the fromStatuses to toStatus,
// applying extraCols. It returns ErrNotFound if the job does not exist or is not
// in an allowed source state.
func (r *BackupRepo) transition(ctx context.Context, id string, fromStatuses []domain.JobStatus, toStatus domain.JobStatus, extraCols map[string]any) error {
	cols := map[string]any{"status": toStatus}
	for k, v := range extraCols {
		cols[k] = v
	}
	res := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("id = ? AND status IN ?", id, fromStatuses).
		Updates(cols)
	if res.Error != nil {
		return fmt.Errorf("repo: transition backup job %q -> %s: %w", id, toStatus, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: transition backup job %q -> %s: %w", id, toStatus, ErrNotFound)
	}
	return nil
}

func (r *BackupRepo) updateJob(ctx context.Context, id string, cols map[string]any) error {
	res := r.db.WithContext(ctx).Model(&domain.BackupJob{}).Where("id = ?", id).Updates(cols)
	if res.Error != nil {
		return fmt.Errorf("repo: update backup job %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: update backup job %q: %w", id, ErrNotFound)
	}
	return nil
}

// StatusCount pairs a job status with a count.
type StatusCount struct {
	Status domain.JobStatus `json:"status"`
	Count  int64            `json:"count"`
}

// QueueSummary returns the number of non-deleted jobs in each status.
func (r *BackupRepo) QueueSummary(ctx context.Context) ([]StatusCount, error) {
	var out []StatusCount
	err := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Select("status as status, count(*) as count").
		Group("status").
		Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repo: backup queue summary: %w", err)
	}
	return out, nil
}

// CountOptedOutForProvider counts the non-deleted opted-out backup jobs for one
// destination (provider ID) — how many assets the user deliberately excluded from
// this destination. It powers the Providers card's "N skipped by choice" line and
// the "Queue anyway" reversal affordance.
func (r *BackupRepo) CountOptedOutForProvider(ctx context.Context, destination string) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("destination = ? AND status = ?", destination, domain.JobStatusOptedOut).
		Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("repo: count opted-out backup jobs (dest %q): %w", destination, err)
	}
	return n, nil
}

// ListJobs returns jobs optionally filtered by status, plus the total count of
// matches (ignoring pagination). Newest first.
func (r *BackupRepo) ListJobs(ctx context.Context, status *domain.JobStatus, page Page) ([]domain.BackupJob, int64, error) {
	base := r.db.WithContext(ctx).Model(&domain.BackupJob{})
	if status != nil {
		base = base.Where("status = ?", *status)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list backup jobs (count): %w", err)
	}

	limit, offset := page.apply()
	q := base.Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}

	var jobs []domain.BackupJob
	if err := q.Find(&jobs).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list backup jobs: %w", err)
	}
	return jobs, total, nil
}

// ResetRunningOnStartup transitions any job left running to pending so it is
// retried after a restart. It returns the number of jobs updated.
func (r *BackupRepo) ResetRunningOnStartup(ctx context.Context) (int64, error) {
	res := r.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Where("status = ?", domain.JobStatusRunning).
		Updates(map[string]any{
			"status":     domain.JobStatusPending,
			"started_at": nil,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("repo: reset running backup jobs on startup: %w", res.Error)
	}
	return res.RowsAffected, nil
}
