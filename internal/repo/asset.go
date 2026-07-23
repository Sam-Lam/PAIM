package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sam-Lam/PAIM/internal/domain"
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

// FindByOriginalPath returns all non-deleted assets whose recorded original
// source path equals path. It backs the safe-to-erase fast path, which trusts
// import-time verification for a source file still sitting at its imported path
// with an unchanged size and mtime. An empty argument returns no rows.
func (r *AssetRepo) FindByOriginalPath(ctx context.Context, path string) ([]domain.Asset, error) {
	if path == "" {
		return nil, nil
	}
	var assets []domain.Asset
	err := r.db.WithContext(ctx).
		Where("original_full_path = ?", path).
		Find(&assets).Error
	if err != nil {
		return nil, fmt.Errorf("repo: find assets by original path: %w", err)
	}
	return assets, nil
}

// SessionBackupCounts returns, for the assets imported under sessionID that have
// an archive copy (backup-eligible), how many exist in total and how many have a
// complete aggregate backup status. It powers the import completion panel's live
// "N of M backed up" indicator, aligned with the same BackupStatusComplete signal
// safe-to-erase uses per asset.
func (r *AssetRepo) SessionBackupCounts(ctx context.Context, sessionID string) (total, complete int64, err error) {
	if err = r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("session_id = ?", sessionID).
		Where("current_archive_path <> ''").
		Count(&total).Error; err != nil {
		return 0, 0, fmt.Errorf("repo: count session assets: %w", err)
	}
	if err = r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("session_id = ?", sessionID).
		Where("current_archive_path <> ''").
		Where("backup_status = ?", domain.BackupStatusComplete).
		Count(&complete).Error; err != nil {
		return 0, 0, fmt.Errorf("repo: count session backed-up assets: %w", err)
	}
	return total, complete, nil
}

// UpdateFullHash backfills the full hash of an asset (used when a quick-hash
// collision forces full-hash computation).
func (r *AssetRepo) UpdateFullHash(ctx context.Context, id, fullHash string) error {
	return r.updateColumns(ctx, id, map[string]any{"full_hash": fullHash})
}

// UpdateArchivePath records a new current archive path for an asset (used by the
// reorganize maintenance operation after a same-volume move). It is
// transaction-aware via WithTx so the path update and session counter can commit
// atomically.
func (r *AssetRepo) UpdateArchivePath(ctx context.Context, id, archivePath string) error {
	return r.updateColumns(ctx, id, map[string]any{"current_archive_path": archivePath})
}

// ListActiveArchived returns every non-deleted, verified asset that has a
// recorded archive path (ordered by that path). It intentionally INCLUDES assets
// flagged as duplicates that were adopted in place (they carry a path): the
// reorganize planner reports them as skipped rather than moving them. Copy-mode
// duplicate placeholders (empty path) are excluded — there is nothing to move.
func (r *AssetRepo) ListActiveArchived(ctx context.Context) ([]domain.Asset, error) {
	var assets []domain.Asset
	err := r.db.WithContext(ctx).
		Where("verification_status = ?", domain.VerificationStatusVerified).
		Where("current_archive_path <> ''").
		Order("current_archive_path").
		Find(&assets).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list active archived assets: %w", err)
	}
	return assets, nil
}

// ArchivedIDs returns the IDs of non-deleted assets that have an archive copy on
// disk (a non-empty CurrentArchivePath), ordered oldest-import-first so a
// thumbnail warm-up walks the library in a stable, resumable order. Duplicates
// with no physical copy are excluded (there is nothing to render).
func (r *AssetRepo) ArchivedIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("current_archive_path <> ''").
		Order("import_date ASC, created_at ASC").
		Pluck("id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list archived asset ids: %w", err)
	}
	return ids, nil
}

// IDsForSession returns the IDs of non-deleted assets imported under sessionID
// that have an archive copy on disk, ordered oldest-import-first. Used to warm
// thumbnails for exactly the assets a just-completed import added.
func (r *AssetRepo) IDsForSession(ctx context.Context, sessionID string) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("session_id = ?", sessionID).
		Where("current_archive_path <> ''").
		Order("import_date ASC, created_at ASC").
		Pluck("id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("repo: list session asset ids: %w", err)
	}
	return ids, nil
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

// SoftDeleteWithPath records archivePath as the asset's current archive path and
// soft-deletes it in one transaction, so a trashed file's new location is
// preserved on the row for recovery. Like SoftDelete, the row remains queryable
// with Unscoped.
func (r *AssetRepo) SoftDeleteWithPath(ctx context.Context, id, archivePath string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&domain.Asset{}).Where("id = ?", id).Updates(map[string]any{
			"deleted":              true,
			"current_archive_path": archivePath,
		})
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
// substring). YearMonth ("2006-01") restricts to a single capture month.
type AssetQuery struct {
	MediaType          *domain.MediaType
	VerificationStatus *domain.VerificationStatus
	BackupStatus       *domain.BackupStatus
	SessionID          string
	SourceID           string
	Text               string
	YearMonth          string
	Page               Page
}

// applyFilters adds every non-empty AssetQuery predicate to a base query. It is
// shared by List and the month-rollup query so the filter semantics stay
// identical across the two.
func (q AssetQuery) applyFilters(base *gorm.DB) *gorm.DB {
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
	if q.YearMonth != "" {
		base = base.Where("strftime('%Y-%m', capture_date) = ?", q.YearMonth)
	}
	return base
}

// List returns assets matching the query plus the total count of matches
// (ignoring pagination). Results are ordered newest-capture-first; assets with no
// capture date sort last (SQLite orders NULL below any value, so DESC places them
// at the end), with import date and creation order as stable tie-breakers.
func (r *AssetRepo) List(ctx context.Context, q AssetQuery) ([]domain.Asset, int64, error) {
	base := q.applyFilters(r.db.WithContext(ctx).Model(&domain.Asset{}))

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: list assets (count): %w", err)
	}

	limit, offset := q.Page.apply()
	rows := base.Order("capture_date DESC, import_date DESC, created_at DESC")
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

// CaptureMonth pairs a capture month ("2006-01") with the number of non-deleted
// assets captured in it.
type CaptureMonth struct {
	Month string `json:"month"`
	Count int64  `json:"count"`
}

// CaptureMonths returns per-month asset counts keyed by capture date, newest
// month first. Assets without a capture date are excluded (they have no month to
// group under). Used to populate the browser's month filter and section headers.
func (r *AssetRepo) CaptureMonths(ctx context.Context) ([]CaptureMonth, error) {
	var rows []CaptureMonth
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select("strftime('%Y-%m', capture_date) as month, count(*) as count").
		Where("capture_date IS NOT NULL").
		Group("month").
		Order("month DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repo: capture months: %w", err)
	}
	return rows, nil
}
