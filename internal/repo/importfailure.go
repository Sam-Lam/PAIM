package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// ImportFailureRepo persists and queries ImportFailure rows — the structured,
// resolvable per-file failure records produced during an import.
type ImportFailureRepo struct {
	db *gorm.DB
}

// NewImportFailureRepo constructs an ImportFailureRepo.
func NewImportFailureRepo(db *gorm.DB) *ImportFailureRepo { return &ImportFailureRepo{db: db} }

// WithTx binds the repo to a transaction handle so a failure can be recorded in
// the same transaction as the session failure-counter increment.
func (r *ImportFailureRepo) WithTx(tx *gorm.DB) *ImportFailureRepo { return &ImportFailureRepo{db: tx} }

// Create inserts a new failure record. The caller sets SessionID, Path, Op,
// ErrorMessage, and Status (open) before calling.
func (r *ImportFailureRepo) Create(ctx context.Context, f *domain.ImportFailure) error {
	if err := r.db.WithContext(ctx).Create(f).Error; err != nil {
		return fmt.Errorf("repo: create import failure: %w", err)
	}
	return nil
}

// GetByID returns the non-deleted failure with the given ID, or ErrNotFound.
func (r *ImportFailureRepo) GetByID(ctx context.Context, id string) (*domain.ImportFailure, error) {
	var f domain.ImportFailure
	err := r.db.WithContext(ctx).First(&f, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("repo: get import failure %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: get import failure %q: %w", id, err)
	}
	return &f, nil
}

// ListForSession returns a page of a session's failure records (newest first)
// and the total count of failure records for that session (all statuses). The
// window is applied in SQL so a session with many failures never loads wholesale.
func (r *ImportFailureRepo) ListForSession(ctx context.Context, sessionID string, page Page) ([]domain.ImportFailure, int64, error) {
	var total int64
	if err := r.db.WithContext(ctx).Model(&domain.ImportFailure{}).
		Where("session_id = ?", sessionID).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: count import failures for session %q: %w", sessionID, err)
	}

	limit, offset := page.apply()
	q := r.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("created_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}
	var rows []domain.ImportFailure
	if err := q.Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list import failures for session %q: %w", sessionID, err)
	}
	return rows, total, nil
}

// CountForSession returns the total number of failure records (all statuses)
// recorded for a session. It is used to distinguish a legacy session (counter >
// 0, no structured rows) from one imported after this feature.
func (r *ImportFailureRepo) CountForSession(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&domain.ImportFailure{}).
		Where("session_id = ?", sessionID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("repo: count import failures for session %q: %w", sessionID, err)
	}
	return n, nil
}

// Dismiss soft-resolves a failure: it sets Status=dismissed, records ResolvedAt,
// and stores the optional reason. It never hard-deletes the row.
func (r *ImportFailureRepo) Dismiss(ctx context.Context, id, reason string, resolvedAt time.Time) error {
	return r.update(ctx, id, map[string]any{
		"status":         domain.ImportFailureStatusDismissed,
		"resolved_at":    resolvedAt,
		"dismiss_reason": reason,
	})
}

// MarkRetried marks a failure resolved by a successful retry: Status=retried and
// ResolvedAt set. The originally-failed file now exists as a verified asset.
func (r *ImportFailureRepo) MarkRetried(ctx context.Context, id string, resolvedAt time.Time) error {
	return r.update(ctx, id, map[string]any{
		"status":      domain.ImportFailureStatusRetried,
		"resolved_at": resolvedAt,
	})
}

// RecordRetryFailure refreshes an open failure's Op and ErrorMessage after a
// retry that failed again, leaving its Status open (still resolvable). The
// counter is deliberately not touched — a retry that re-fails does not add a new
// failure; it is the same one file.
func (r *ImportFailureRepo) RecordRetryFailure(ctx context.Context, id, op, errMsg string) error {
	return r.update(ctx, id, map[string]any{
		"op":            op,
		"error_message": errMsg,
	})
}

// update applies cols to the failure with the given ID, returning ErrNotFound
// when no row matched.
func (r *ImportFailureRepo) update(ctx context.Context, id string, cols map[string]any) error {
	res := r.db.WithContext(ctx).Model(&domain.ImportFailure{}).Where("id = ?", id).Updates(cols)
	if res.Error != nil {
		return fmt.Errorf("repo: update import failure %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: update import failure %q: %w", id, ErrNotFound)
	}
	return nil
}
