package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// SessionRepo persists and queries ImportSession rows.
type SessionRepo struct {
	db *gorm.DB
}

// NewSessionRepo constructs a SessionRepo.
func NewSessionRepo(db *gorm.DB) *SessionRepo { return &SessionRepo{db: db} }

// WithTx binds the repo to a transaction handle.
func (r *SessionRepo) WithTx(tx *gorm.DB) *SessionRepo { return &SessionRepo{db: tx} }

// Create inserts a new session.
func (r *SessionRepo) Create(ctx context.Context, s *domain.ImportSession) error {
	if err := r.db.WithContext(ctx).Create(s).Error; err != nil {
		return fmt.Errorf("repo: create session: %w", err)
	}
	return nil
}

// GetByID returns the non-deleted session with the given ID, or ErrNotFound.
func (r *SessionRepo) GetByID(ctx context.Context, id string) (*domain.ImportSession, error) {
	var s domain.ImportSession
	err := r.db.WithContext(ctx).First(&s, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("repo: get session %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: get session %q: %w", id, err)
	}
	return &s, nil
}

// SessionCounters is a set of atomic increments to apply to a session's rolling
// counters. Zero fields are not touched.
type SessionCounters struct {
	Scanned    int
	Imported   int
	Duplicates int
	Failures   int
	Skipped    int
}

// AddCounters atomically adds the given deltas to a session's counters using SQL
// expressions (no read-modify-write race).
func (r *SessionRepo) AddCounters(ctx context.Context, id string, c SessionCounters) error {
	cols := map[string]any{}
	if c.Scanned != 0 {
		cols["files_scanned"] = gorm.Expr("files_scanned + ?", c.Scanned)
	}
	if c.Imported != 0 {
		cols["files_imported"] = gorm.Expr("files_imported + ?", c.Imported)
	}
	if c.Duplicates != 0 {
		cols["duplicates"] = gorm.Expr("duplicates + ?", c.Duplicates)
	}
	if c.Failures != 0 {
		cols["failures"] = gorm.Expr("failures + ?", c.Failures)
	}
	if c.Skipped != 0 {
		cols["skipped"] = gorm.Expr("skipped + ?", c.Skipped)
	}
	if len(cols) == 0 {
		return nil
	}
	res := r.db.WithContext(ctx).Model(&domain.ImportSession{}).Where("id = ?", id).Updates(cols)
	if res.Error != nil {
		return fmt.Errorf("repo: add session counters %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: add session counters %q: %w", id, ErrNotFound)
	}
	return nil
}

// IncScanned atomically increments FilesScanned by n.
func (r *SessionRepo) IncScanned(ctx context.Context, id string, n int) error {
	return r.AddCounters(ctx, id, SessionCounters{Scanned: n})
}

// IncImported atomically increments FilesImported by n.
func (r *SessionRepo) IncImported(ctx context.Context, id string, n int) error {
	return r.AddCounters(ctx, id, SessionCounters{Imported: n})
}

// IncDuplicates atomically increments Duplicates by n.
func (r *SessionRepo) IncDuplicates(ctx context.Context, id string, n int) error {
	return r.AddCounters(ctx, id, SessionCounters{Duplicates: n})
}

// IncFailures atomically increments Failures by n.
func (r *SessionRepo) IncFailures(ctx context.Context, id string, n int) error {
	return r.AddCounters(ctx, id, SessionCounters{Failures: n})
}

// IncSkipped atomically increments Skipped by n.
func (r *SessionRepo) IncSkipped(ctx context.Context, id string, n int) error {
	return r.AddCounters(ctx, id, SessionCounters{Skipped: n})
}

// SetStatus updates the session status.
func (r *SessionRepo) SetStatus(ctx context.Context, id string, status domain.SessionStatus) error {
	res := r.db.WithContext(ctx).Model(&domain.ImportSession{}).Where("id = ?", id).Update("status", status)
	if res.Error != nil {
		return fmt.Errorf("repo: set session status %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: set session status %q: %w", id, ErrNotFound)
	}
	return nil
}

// Complete finalizes a session: it records the completion time, computes the
// duration from StartedAt, and sets the terminal status (completed, failed,
// cancelled, ...).
func (r *SessionRepo) Complete(ctx context.Context, id string, status domain.SessionStatus, completedAt time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var s domain.ImportSession
		if err := tx.First(&s, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("repo: complete session %q: %w", id, ErrNotFound)
			}
			return fmt.Errorf("repo: complete session %q: %w", id, err)
		}
		duration := completedAt.Sub(s.StartedAt).Seconds()
		if duration < 0 {
			duration = 0
		}
		err := tx.Model(&domain.ImportSession{}).Where("id = ?", id).Updates(map[string]any{
			"completed_at":     completedAt,
			"duration_seconds": duration,
			"status":           status,
		}).Error
		if err != nil {
			return fmt.Errorf("repo: complete session %q: %w", id, err)
		}
		return nil
	})
}

// ListRecent returns the most recently started sessions, newest first.
func (r *SessionRepo) ListRecent(ctx context.Context, limit int) ([]domain.ImportSession, error) {
	q := r.db.WithContext(ctx).Order("started_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var sessions []domain.ImportSession
	if err := q.Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("repo: list recent sessions: %w", err)
	}
	return sessions, nil
}

// Count returns the total number of import sessions.
func (r *SessionRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&domain.ImportSession{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("repo: count sessions: %w", err)
	}
	return n, nil
}

// MarkInterruptedOnStartup transitions any session still marked running to
// interrupted (so it becomes resumable). It returns the number of sessions
// updated. Intended to run once at application startup.
func (r *SessionRepo) MarkInterruptedOnStartup(ctx context.Context) (int64, error) {
	res := r.db.WithContext(ctx).
		Model(&domain.ImportSession{}).
		Where("status = ?", domain.SessionStatusRunning).
		Update("status", domain.SessionStatusInterrupted)
	if res.Error != nil {
		return 0, fmt.Errorf("repo: mark interrupted on startup: %w", res.Error)
	}
	return res.RowsAffected, nil
}
