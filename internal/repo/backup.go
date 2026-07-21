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

// Enqueue idempotently adds a backup job for an asset/plugin/destination. If a
// pending, running, or completed job already exists for the same triple, that
// existing job is returned and created is false; otherwise a new pending job is
// created. This makes re-enqueuing after a crash safe.
func (r *BackupRepo) Enqueue(ctx context.Context, assetID, plugin, destination string) (job *domain.BackupJob, created bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing domain.BackupJob
		findErr := tx.Where(
			"asset_id = ? AND plugin = ? AND destination = ? AND status IN ?",
			assetID, plugin, destination,
			[]domain.JobStatus{domain.JobStatusPending, domain.JobStatusRunning, domain.JobStatusCompleted},
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
			Status:      domain.JobStatusPending,
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

// NextPending atomically claims the oldest pending job, transitioning it to
// running and stamping StartedAt. It returns (nil, nil) when no pending job is
// available. The claim is a single UPDATE ... RETURNING statement, so concurrent
// callers never claim the same job.
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
