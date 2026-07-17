package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/autolinepro/paim/internal/domain"
	"gorm.io/gorm"
)

// LogRepo persists and queries LogEntry rows for the Logs page.
type LogRepo struct {
	db *gorm.DB
}

// NewLogRepo constructs a LogRepo.
func NewLogRepo(db *gorm.DB) *LogRepo { return &LogRepo{db: db} }

// WithTx binds the repo to a transaction handle.
func (r *LogRepo) WithTx(tx *gorm.DB) *LogRepo { return &LogRepo{db: tx} }

// Insert writes a single log entry.
func (r *LogRepo) Insert(ctx context.Context, e *domain.LogEntry) error {
	if err := r.db.WithContext(ctx).Create(e).Error; err != nil {
		return fmt.Errorf("repo: insert log entry: %w", err)
	}
	return nil
}

// BatchInsert writes many log entries in one round trip. It is a no-op for an
// empty slice.
func (r *LogRepo) BatchInsert(ctx context.Context, entries []domain.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := r.db.WithContext(ctx).CreateInBatches(entries, 200).Error; err != nil {
		return fmt.Errorf("repo: batch insert %d log entries: %w", len(entries), err)
	}
	return nil
}

// LogQuery filters a log listing. Empty/zero fields are ignored. Text matches the
// Message column (case-insensitive substring).
type LogQuery struct {
	Text      string
	Level     string
	Subsystem string
	Since     time.Time
	Until     time.Time
	Page      Page
}

func (r *LogRepo) filtered(ctx context.Context, q LogQuery) *gorm.DB {
	base := r.db.WithContext(ctx).Model(&domain.LogEntry{})
	if q.Text != "" {
		base = base.Where("message LIKE ?", "%"+q.Text+"%")
	}
	if q.Level != "" {
		base = base.Where("level = ?", q.Level)
	}
	if q.Subsystem != "" {
		base = base.Where("subsystem = ?", q.Subsystem)
	}
	if !q.Since.IsZero() {
		base = base.Where("timestamp >= ?", q.Since)
	}
	if !q.Until.IsZero() {
		base = base.Where("timestamp <= ?", q.Until)
	}
	return base
}

// Search returns log entries matching the query plus the total count of matches
// (ignoring pagination). Newest first.
func (r *LogRepo) Search(ctx context.Context, q LogQuery) ([]domain.LogEntry, int64, error) {
	base := r.filtered(ctx, q)

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: search logs (count): %w", err)
	}

	limit, offset := q.Page.apply()
	rows := base.Order("timestamp DESC, id DESC")
	if limit > 0 {
		rows = rows.Limit(limit)
	}
	if offset > 0 {
		rows = rows.Offset(offset)
	}

	var entries []domain.LogEntry
	if err := rows.Find(&entries).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: search logs: %w", err)
	}
	return entries, total, nil
}

// ListForExport returns all log entries matching the query in ascending
// chronological order, suitable for JSON/CSV export. Pagination on the query is
// ignored.
func (r *LogRepo) ListForExport(ctx context.Context, q LogQuery) ([]domain.LogEntry, error) {
	var entries []domain.LogEntry
	err := r.filtered(ctx, q).Order("timestamp ASC, id ASC").Find(&entries).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list logs for export: %w", err)
	}
	return entries, nil
}
