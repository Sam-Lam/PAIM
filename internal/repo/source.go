package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/autolinepro/paim/internal/domain"
	"gorm.io/gorm"
)

// SourceRepo persists and queries ImportSource rows.
type SourceRepo struct {
	db *gorm.DB
}

// NewSourceRepo constructs a SourceRepo.
func NewSourceRepo(db *gorm.DB) *SourceRepo { return &SourceRepo{db: db} }

// WithTx binds the repo to a transaction handle.
func (r *SourceRepo) WithTx(tx *gorm.DB) *SourceRepo { return &SourceRepo{db: tx} }

// Create inserts a new import source.
func (r *SourceRepo) Create(ctx context.Context, s *domain.ImportSource) error {
	if err := r.db.WithContext(ctx).Create(s).Error; err != nil {
		return fmt.Errorf("repo: create source: %w", err)
	}
	return nil
}

// Update saves all fields of an existing source.
func (r *SourceRepo) Update(ctx context.Context, s *domain.ImportSource) error {
	if err := r.db.WithContext(ctx).Save(s).Error; err != nil {
		return fmt.Errorf("repo: update source %q: %w", s.ID, err)
	}
	return nil
}

// GetByID returns the non-deleted source with the given ID, or ErrNotFound.
func (r *SourceRepo) GetByID(ctx context.Context, id string) (*domain.ImportSource, error) {
	var s domain.ImportSource
	err := r.db.WithContext(ctx).First(&s, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("repo: get source %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: get source %q: %w", id, err)
	}
	return &s, nil
}

// findByColumn returns the first non-deleted source where column == value. An
// empty value or no match returns (nil, nil).
func (r *SourceRepo) findByColumn(ctx context.Context, column, value string) (*domain.ImportSource, error) {
	if value == "" {
		return nil, nil
	}
	var s domain.ImportSource
	err := r.db.WithContext(ctx).Where(column+" = ?", value).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("repo: find source by %s: %w", column, err)
	}
	return &s, nil
}

// FindByHardwareSerial returns the source with the given hardware serial, if any.
func (r *SourceRepo) FindByHardwareSerial(ctx context.Context, serial string) (*domain.ImportSource, error) {
	return r.findByColumn(ctx, "hardware_serial", serial)
}

// FindByVolumeUUID returns the source with the given volume UUID, if any.
func (r *SourceRepo) FindByVolumeUUID(ctx context.Context, volumeUUID string) (*domain.ImportSource, error) {
	return r.findByColumn(ctx, "volume_uuid", volumeUUID)
}

// FindByFilesystemUUID returns the source with the given filesystem UUID, if any.
func (r *SourceRepo) FindByFilesystemUUID(ctx context.Context, fsUUID string) (*domain.ImportSource, error) {
	return r.findByColumn(ctx, "filesystem_uuid", fsUUID)
}

// ListRecent returns the most recently seen sources, newest first.
func (r *SourceRepo) ListRecent(ctx context.Context, limit int) ([]domain.ImportSource, error) {
	q := r.db.WithContext(ctx).Order("last_seen_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var sources []domain.ImportSource
	if err := q.Find(&sources).Error; err != nil {
		return nil, fmt.Errorf("repo: list recent sources: %w", err)
	}
	return sources, nil
}

// UpdateLastSeen sets LastSeenAt for a source.
func (r *SourceRepo) UpdateLastSeen(ctx context.Context, id string, seenAt time.Time) error {
	res := r.db.WithContext(ctx).Model(&domain.ImportSource{}).Where("id = ?", id).Update("last_seen_at", seenAt)
	if res.Error != nil {
		return fmt.Errorf("repo: update source last seen %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: update source last seen %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetSafeToErase records whether a source is safe to erase and the human-readable
// reason for the conclusion.
func (r *SourceRepo) SetSafeToErase(ctx context.Context, id string, safe bool, reason string) error {
	res := r.db.WithContext(ctx).Model(&domain.ImportSource{}).Where("id = ?", id).Updates(map[string]any{
		"safe_to_erase":        safe,
		"safe_to_erase_reason": reason,
	})
	if res.Error != nil {
		return fmt.Errorf("repo: set safe-to-erase %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: set safe-to-erase %q: %w", id, ErrNotFound)
	}
	return nil
}
