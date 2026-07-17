package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/autolinepro/paim/internal/domain"
	"gorm.io/gorm"
)

// AssetRepo persists and queries Asset rows.
type AssetRepo struct {
	db *gorm.DB
}

// NewAssetRepo constructs an AssetRepo over the given handle.
func NewAssetRepo(db *gorm.DB) *AssetRepo { return &AssetRepo{db: db} }

// WithTx returns a copy of the repo bound to the given transaction handle so the
// operation can participate in a caller-managed transaction.
func (r *AssetRepo) WithTx(tx *gorm.DB) *AssetRepo { return &AssetRepo{db: tx} }

// Create inserts a new asset. It is transaction-aware via WithTx.
func (r *AssetRepo) Create(ctx context.Context, a *domain.Asset) error {
	if err := r.db.WithContext(ctx).Create(a).Error; err != nil {
		return fmt.Errorf("repo: create asset %q: %w", a.OriginalFilename, err)
	}
	return nil
}

// GetByID returns the non-deleted asset with the given ID, or ErrNotFound.
func (r *AssetRepo) GetByID(ctx context.Context, id string) (*domain.Asset, error) {
	var a domain.Asset
	err := r.db.WithContext(ctx).First(&a, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("repo: get asset %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: get asset %q: %w", id, err)
	}
	return &a, nil
}

// FindByQuickHash returns all non-deleted assets sharing the given quick hash.
// Soft-deleted assets are excluded by GORM's default scope.
func (r *AssetRepo) FindByQuickHash(ctx context.Context, quickHash string) ([]domain.Asset, error) {
	var assets []domain.Asset
	err := r.db.WithContext(ctx).
		Where("quick_hash = ?", quickHash).
		Find(&assets).Error
	if err != nil {
		return nil, fmt.Errorf("repo: find assets by quick hash: %w", err)
	}
	return assets, nil
}

// FindByFullHash returns all non-deleted assets whose full hash matches. An empty
// argument returns no rows (empty full hash means "not yet computed").
func (r *AssetRepo) FindByFullHash(ctx context.Context, fullHash string) ([]domain.Asset, error) {
	if fullHash == "" {
		return nil, nil
	}
	var assets []domain.Asset
	err := r.db.WithContext(ctx).
		Where("full_hash = ?", fullHash).
		Find(&assets).Error
	if err != nil {
		return nil, fmt.Errorf("repo: find assets by full hash: %w", err)
	}
	return assets, nil
}

// UpdateFullHash backfills the full hash of an asset (used when a quick-hash
// collision forces full-hash computation).
func (r *AssetRepo) UpdateFullHash(ctx context.Context, id, fullHash string) error {
	return r.updateColumns(ctx, id, map[string]any{"full_hash": fullHash})
}

// UpdateVerificationStatus sets the verification status of an asset.
func (r *AssetRepo) UpdateVerificationStatus(ctx context.Context, id string, status domain.VerificationStatus) error {
	return r.updateColumns(ctx, id, map[string]any{"verification_status": status})
}

// UpdateBackupStatus sets the aggregate backup status of an asset.
func (r *AssetRepo) UpdateBackupStatus(ctx context.Context, id string, status domain.BackupStatus) error {
	return r.updateColumns(ctx, id, map[string]any{"backup_status": status})
}

// MarkDuplicateOf records that id duplicates originalID (established by a
// full-hash match).
func (r *AssetRepo) MarkDuplicateOf(ctx context.Context, id, originalID string) error {
	return r.updateColumns(ctx, id, map[string]any{"duplicate_of_asset_id": originalID})
}

// updateColumns applies a column update to a single non-deleted asset, returning
// ErrNotFound if no row matched.
func (r *AssetRepo) updateColumns(ctx context.Context, id string, cols map[string]any) error {
	res := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("id = ?", id).
		Updates(cols)
	if res.Error != nil {
		return fmt.Errorf("repo: update asset %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repo: update asset %q: %w", id, ErrNotFound)
	}
	return nil
}

// SoftDelete marks an asset deleted without removing the row: it sets the Deleted
// flag and populates DeletedAt via GORM's soft-delete. The row remains queryable
// with Unscoped.
func (r *AssetRepo) SoftDelete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&domain.Asset{}).Where("id = ?", id).Update("deleted", true)
		if res.Error != nil {
			return fmt.Errorf("repo: soft delete asset %q: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("repo: soft delete asset %q: %w", id, ErrNotFound)
		}
		if err := tx.Delete(&domain.Asset{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("repo: soft delete asset %q: %w", id, err)
		}
		return nil
	})
}

// MediaTypeCount pairs a media type with the number of non-deleted assets of
// that type.
type MediaTypeCount struct {
	MediaType domain.MediaType `json:"mediaType"`
	Count     int64            `json:"count"`
}

// CountsByMediaType returns per-media-type counts of non-deleted assets.
func (r *AssetRepo) CountsByMediaType(ctx context.Context) ([]MediaTypeCount, error) {
	var out []MediaTypeCount
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select("media_type as media_type, count(*) as count").
		Group("media_type").
		Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repo: counts by media type: %w", err)
	}
	return out, nil
}

// TotalBytes returns the summed FileSize of all non-deleted assets.
func (r *AssetRepo) TotalBytes(ctx context.Context) (int64, error) {
	var total *int64
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select("sum(file_size)").
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("repo: total bytes: %w", err)
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}

// DuplicatePair is a duplicate asset joined with the original it duplicates.
type DuplicatePair struct {
	Duplicate domain.Asset `json:"duplicate"`
	Original  domain.Asset `json:"original"`
}

// ListDuplicates returns all non-deleted assets that reference an original via
// DuplicateOfAssetID, paired with that original.
func (r *AssetRepo) ListDuplicates(ctx context.Context, page Page) ([]DuplicatePair, error) {
	limit, offset := page.apply()

	q := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").
		Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}

	var dups []domain.Asset
	if err := q.Find(&dups).Error; err != nil {
		return nil, fmt.Errorf("repo: list duplicates: %w", err)
	}
	if len(dups) == 0 {
		return nil, nil
	}

	originalIDs := make([]string, 0, len(dups))
	for _, d := range dups {
		if d.DuplicateOfAssetID != nil {
			originalIDs = append(originalIDs, *d.DuplicateOfAssetID)
		}
	}

	var originals []domain.Asset
	if err := r.db.WithContext(ctx).Where("id IN ?", originalIDs).Find(&originals).Error; err != nil {
		return nil, fmt.Errorf("repo: list duplicates (load originals): %w", err)
	}
	byID := make(map[string]domain.Asset, len(originals))
	for _, o := range originals {
		byID[o.ID] = o
	}

	pairs := make([]DuplicatePair, 0, len(dups))
	for _, d := range dups {
		p := DuplicatePair{Duplicate: d}
		if d.DuplicateOfAssetID != nil {
			p.Original = byID[*d.DuplicateOfAssetID]
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}

// AssetQuery filters an asset listing. Nil pointer fields and empty strings are
// ignored. Text matches OriginalFilename or OriginalFullPath (case-insensitive
// substring).
type AssetQuery struct {
	MediaType          *domain.MediaType
	VerificationStatus *domain.VerificationStatus
	BackupStatus       *domain.BackupStatus
	SessionID          string
	SourceID           string
	Text               string
	Page               Page
}

// List returns assets matching the query plus the total count of matches
// (ignoring pagination).
func (r *AssetRepo) List(ctx context.Context, q AssetQuery) ([]domain.Asset, int64, error) {
	base := r.db.WithContext(ctx).Model(&domain.Asset{})
	if q.MediaType != nil {
		base = base.Where("media_type = ?", *q.MediaType)
	}
	if q.VerificationStatus != nil {
		base = base.Where("verification_status = ?", *q.VerificationStatus)
	}
	if q.BackupStatus != nil {
		base = base.Where("backup_status = ?", *q.BackupStatus)
	}
	if q.SessionID != "" {
		base = base.Where("session_id = ?", q.SessionID)
	}
	if q.SourceID != "" {
		base = base.Where("source_id = ?", q.SourceID)
	}
	if q.Text != "" {
		like := "%" + q.Text + "%"
		base = base.Where("original_filename LIKE ? OR original_full_path LIKE ?", like, like)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list assets (count): %w", err)
	}

	limit, offset := q.Page.apply()
	rows := base.Order("import_date DESC, created_at DESC")
	if limit > 0 {
		rows = rows.Limit(limit)
	}
	if offset > 0 {
		rows = rows.Offset(offset)
	}

	var assets []domain.Asset
	if err := rows.Find(&assets).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list assets: %w", err)
	}
	return assets, total, nil
}
