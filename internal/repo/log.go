package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
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
// Message column (case-insensitive substring); MetadataText matches the
// MetadataJSON column (case-insensitive substring) — used to gather the log
// entries that reference a particular session ID without loading a whole time
// window into memory.
type LogQuery struct {
	Text         string
	Level        string
	Subsystem    string
	MetadataText string
	Since        time.Time
	Until        time.Time
	Page         Page
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
	if q.MetadataText != "" {
		base = base.Where("metadata_json LIKE ?", "%"+q.MetadataText+"%")
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

// StreamForExport streams every log entry matching q to fn in ascending
// chronological order, in pages of pageSize rows, so a large export never holds
// the whole result set in memory. Pagination on the query itself is ignored.
// A non-nil error from fn aborts the stream and is returned.
func (r *LogRepo) StreamForExport(ctx context.Context, q LogQuery, pageSize int, fn func(domain.LogEntry) error) error {
	if pageSize <= 0 {
		pageSize = 5000
	}
	offset := 0
	for {
		var page []domain.LogEntry
		err := r.filtered(ctx, q).
			Order("timestamp ASC, id ASC").
			Limit(pageSize).Offset(offset).
			Find(&page).Error
		if err != nil {
			return fmt.Errorf("repo: stream logs for export: %w", err)
		}
		for i := range page {
			if err := fn(page[i]); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
		offset += pageSize
	}
}

// ListForSession returns up to cap+0 log entries whose MetadataJSON references
// sessionID within the query's time window, in ascending chronological order,
// plus whether the result was truncated at the cap. It pushes the sessionID
// match into SQL (metadata_json LIKE) instead of scanning a whole window in
// memory. A non-positive cap applies a sane default.
func (r *LogRepo) ListForSession(ctx context.Context, q LogQuery, cap int) ([]domain.LogEntry, bool, error) {
	if cap <= 0 {
		cap = 5000
	}
	var entries []domain.LogEntry
	// Fetch one extra row to detect truncation without a separate COUNT.
	err := r.filtered(ctx, q).
		Order("timestamp ASC, id ASC").
		Limit(cap + 1).
		Find(&entries).Error
	if err != nil {
		return nil, false, fmt.Errorf("repo: list session events: %w", err)
	}
	truncated := len(entries) > cap
	if truncated {
		entries = entries[:cap]
	}
	return entries, truncated, nil
}
