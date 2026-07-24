package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
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

// escapeLike escapes the SQL LIKE metacharacters (%, _) and the escape
// character itself so a stored path segment containing them is matched
// literally. Callers pair the result with `ESCAPE '\'`.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// UpdateArchivePathPrefix rewrites the leading directory prefix of every
// non-deleted asset whose CurrentArchivePath lies under oldPrefix, replacing it
// with newPrefix. Matching is on WHOLE path segments only: an asset matches iff
// its path begins with `oldPrefix + "/"`, so renaming "2019/2019-06-12 Trip"
// never touches a sibling "2019/2019-06-12 Trip2". RAW/ subpaths ride along
// (they share the prefix). It returns the number of rows updated and is
// transaction-aware via WithTx so the directory rename and the path rewrite
// commit atomically. Both prefixes are root-relative forward-slash paths with no
// trailing slash.
func (r *AssetRepo) UpdateArchivePathPrefix(ctx context.Context, oldPrefix, newPrefix string) (int64, error) {
	oldPrefix = strings.TrimRight(oldPrefix, "/")
	newPrefix = strings.TrimRight(newPrefix, "/")
	if oldPrefix == "" {
		return 0, fmt.Errorf("repo: update archive path prefix: empty old prefix")
	}
	like := escapeLike(oldPrefix) + "/%"
	// substr is 1-based: keep everything from the first char AFTER oldPrefix (the
	// leading "/rest"), and prepend newPrefix.
	res := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("current_archive_path LIKE ? ESCAPE '\\'", like).
		Update("current_archive_path",
			gorm.Expr("? || substr(current_archive_path, ?)", newPrefix, len(oldPrefix)+1))
	if res.Error != nil {
		return 0, fmt.Errorf("repo: update archive path prefix %q->%q: %w", oldPrefix, newPrefix, res.Error)
	}
	return res.RowsAffected, nil
}

// FolderChild is one immediate subdirectory of a browsed folder: its segment
// name, the number of assets anywhere beneath it (recursive), a representative
// cover asset (the newest by effective capture date), and the newest effective
// capture date anywhere beneath it. NewestCapture is nil only when the subtree
// has no datable rows (never in practice — import_date is always set).
type FolderChild struct {
	Name          string     `json:"name"`
	AssetCount    int64      `json:"assetCount"`
	CoverAssetID  string     `json:"coverAssetId"`
	NewestCapture *time.Time `json:"newestCapture"`
}

// folderChildRow is the raw scan target for FolderChildren. SQLite (via the
// mattn driver) returns a datetime EXPRESSION — max(COALESCE(...)) has no
// declared column type — as a text value, not time.Time, so newest_capture is
// scanned as a string and parsed into FolderChild.NewestCapture.
type folderChildRow struct {
	Name          string
	AssetCount    int64
	CoverAssetID  string
	NewestCapture string
}

// sqliteTimeLayouts are the datetime text formats the SQLite driver may emit
// for a stored time.Time, tried in order when parsing a raw datetime expression
// (see folderChildRow.NewestCapture). The first entry matches the driver's
// default write format (e.g. "2019-06-12 10:00:00+00:00").
var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// parseSQLiteTime parses a datetime text value emitted by the SQLite driver for
// an untyped expression, returning nil for an empty or unparseable value.
func parseSQLiteTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range sqliteTimeLayouts {
		if tm, err := time.Parse(layout, s); err == nil {
			return &tm
		}
	}
	return nil
}

// FolderChildren returns the immediate subdirectories of relDir derived from
// CurrentArchivePath, each with a recursive asset count and a cover asset. relDir
// is a root-relative forward-slash directory ("" = the library root, listing
// year folders). The child name is the path segment immediately after relDir;
// assets sitting DIRECTLY in relDir (no further segment) are files, not
// subfolders, and are excluded here (see FolderAssets).
//
// Scale: this is a single GROUP BY over the assets whose path is under relDir,
// filtered by an indexed `current_archive_path` prefix (idx on that column). At
// 250k assets a top-level listing groups the whole table once (~tens of ms in
// SQLite); deeper levels touch only the far smaller subtree. Folder navigation
// is a cold, user-driven action (not a hot loop), so a per-level prefix scan is
// the right trade-off versus materializing a separate folder table.
func (r *AssetRepo) FolderChildren(ctx context.Context, relDir string) ([]FolderChild, error) {
	relDir = strings.Trim(relDir, "/")

	var restExpr, where string
	var args []any
	if relDir == "" {
		restExpr = "current_archive_path"
		where = "deleted_at IS NULL AND current_archive_path <> ''"
	} else {
		prefix := relDir + "/"
		restExpr = "substr(current_archive_path, ?)"
		args = append(args, len(prefix)+1)
		where = "deleted_at IS NULL AND current_archive_path LIKE ? ESCAPE '\\'"
		args = append(args, escapeLike(prefix)+"%")
	}

	// eff is each row's effective capture date: the real capture date when known,
	// otherwise the import date (COALESCE fallback). import_date is always set, so
	// eff is never NULL and every subfolder gets a newest_capture.
	//
	// Inner: project each row to its (rest-of-path) relative to relDir and its eff.
	// Middle: keep only rows that go DEEPER than relDir (a subfolder) and cut the
	// first segment. Outer: group by that segment, taking the recursive count, the
	// subtree's MAX effective date, and a cover asset. With exactly one max()
	// aggregate the bare `id` column takes its value from that same max(eff) row
	// (SQLite's documented min/max bare-column rule), giving a newest-first cover
	// per subfolder.
	sql := `
SELECT name, count(*) AS asset_count, id AS cover_asset_id, max(eff) AS newest_capture
FROM (
  SELECT id, eff, substr(rest, 1, instr(rest, '/') - 1) AS name
  FROM (SELECT id, COALESCE(capture_date, import_date) AS eff, ` + restExpr + ` AS rest FROM assets WHERE ` + where + `)
  WHERE instr(rest, '/') > 0
)
GROUP BY name
ORDER BY name`

	var raw []folderChildRow
	if err := r.db.WithContext(ctx).Raw(sql, args...).Scan(&raw).Error; err != nil {
		return nil, fmt.Errorf("repo: folder children of %q: %w", relDir, err)
	}
	rows := make([]FolderChild, 0, len(raw))
	for _, rr := range raw {
		rows = append(rows, FolderChild{
			Name:          rr.Name,
			AssetCount:    rr.AssetCount,
			CoverAssetID:  rr.CoverAssetID,
			NewestCapture: parseSQLiteTime(rr.NewestCapture),
		})
	}
	return rows, nil
}

// folderAssetsOrderBy builds the ORDER BY clause for FolderAssets from a
// validated sortBy ("name"|"date") and sortDir ("asc"|"desc"). Unknown values
// fall back to the default date/desc. The clause is assembled ONLY from these
// fixed, whitelisted tokens — never interpolated caller text — so it is
// injection-safe.
//
//   - name: original_filename COLLATE NOCASE (case-insensitive), created_at tie-break.
//   - date: effective capture date = COALESCE(capture_date, import_date). Rows with
//     NO capture date are treated as undated and always sort LAST regardless of
//     direction (nulls last per direction); import_date then created_at break ties.
//     For date/desc this reproduces the prior default order exactly.
func folderAssetsOrderBy(sortBy, sortDir string) string {
	dir := "DESC"
	if strings.EqualFold(sortDir, "asc") {
		dir = "ASC"
	}
	if strings.EqualFold(sortBy, "name") {
		return "original_filename COLLATE NOCASE " + dir + ", created_at " + dir
	}
	// date (default): undated (NULL capture) last in both directions.
	return "CASE WHEN capture_date IS NULL THEN 1 ELSE 0 END ASC, " +
		"COALESCE(capture_date, import_date) " + dir + ", import_date " + dir + ", created_at " + dir
}

// FolderAssets returns the assets sitting DIRECTLY in relDir (their parent
// directory equals relDir) plus the total count. relDir is a root-relative
// forward-slash directory ("" = root). A file is "directly in" relDir when the
// remainder of its path after relDir contains no further "/". Ordering is set by
// sortBy ("name"|"date", default "date") and sortDir ("asc"|"desc", default
// "desc"); see folderAssetsOrderBy. The default date/desc matches List's
// newest-capture-first order.
func (r *AssetRepo) FolderAssets(ctx context.Context, relDir string, page Page, sortBy, sortDir string) ([]domain.Asset, int64, error) {
	relDir = strings.Trim(relDir, "/")

	base := r.db.WithContext(ctx).Model(&domain.Asset{})
	if relDir == "" {
		base = base.Where("current_archive_path <> '' AND instr(current_archive_path, '/') = 0")
	} else {
		prefix := relDir + "/"
		base = base.
			Where("current_archive_path LIKE ? ESCAPE '\\'", escapeLike(prefix)+"%").
			Where("instr(substr(current_archive_path, ?), '/') = 0", len(prefix)+1)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: folder assets of %q (count): %w", relDir, err)
	}
	limit, offset := page.apply()
	q := base.Order(folderAssetsOrderBy(sortBy, sortDir))
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}
	var assets []domain.Asset
	if err := q.Find(&assets).Error; err != nil {
		return nil, 0, fmt.Errorf("repo: folder assets of %q: %w", relDir, err)
	}
	return assets, total, nil
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

// scopeExtWhere adds a provider-scope extension predicate to a query. scope is a
// provider MediaScope CSV (see internal/mediatype); an empty/"all" scope adds no
// predicate (every media kind is eligible). A restricting scope filters to exactly
// its kinds' extensions via an indexed `lower(original_extension) IN (...)` clause
// built from the fixed registry — so a photos-only provider never even loads RAW
// rows. Matching is by FILE extension, so each Live Photo component (still vs MOV)
// is judged independently despite the shared live_photo_pair MediaType.
func scopeExtWhere(q *gorm.DB, scope string) *gorm.DB {
	exts := mediatype.ScopedExtensions(scope)
	if exts == nil {
		return q // empty/all scope: no restriction
	}
	return q.Where("lower(original_extension) IN ?", exts)
}

// EligibleForBackupPage returns up to limit non-deleted, verified assets that
// have an archive copy on disk (CurrentArchivePath <> ”), whose ID sorts after
// afterID, ordered by ID ascending. It is the stable, resumable keyset page the
// backup backfill iterates: copy-mode duplicate PLACEHOLDERS (empty archive path)
// are excluded because there is nothing to back up, while adopt-flagged duplicates
// (which carry a real path) are INCLUDED — exactly the set the importer enqueues at
// import time. When scope restricts the provider's media kinds, out-of-scope
// extensions are filtered in SQL (see scopeExtWhere) so a 237k-row catalog never
// loads out-of-scope rows into Go. Pass afterID="" to start from the first page.
// Because the order is a stable total order over the ID key and Enqueue is
// idempotent, a cancelled backfill resumes correctly on a fresh from-the-start run
// (already-enqueued pairs are skipped).
func (r *AssetRepo) EligibleForBackupPage(ctx context.Context, afterID string, limit int, scope string) ([]domain.Asset, error) {
	if limit <= 0 {
		limit = 1000
	}
	var assets []domain.Asset
	q := r.db.WithContext(ctx).
		Where("verification_status = ?", domain.VerificationStatusVerified).
		Where("current_archive_path <> ''").
		Where("id > ?", afterID)
	err := scopeExtWhere(q, scope).
		Order("id ASC").
		Limit(limit).
		Find(&assets).Error
	if err != nil {
		return nil, fmt.Errorf("repo: eligible-for-backup page (after %q): %w", afterID, err)
	}
	return assets, nil
}

// CountEligibleForBackup counts every non-deleted, verified asset that has an
// archive copy on disk AND falls within the provider's scope — the total set a
// scoped backfill scans (the progress-bar denominator). It is the same eligibility
// EligibleForBackupPage pages over. An empty scope counts every eligible asset.
func (r *AssetRepo) CountEligibleForBackup(ctx context.Context, scope string) (int64, error) {
	var n int64
	q := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("verification_status = ?", domain.VerificationStatusVerified).
		Where("current_archive_path <> ''")
	if err := scopeExtWhere(q, scope).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("repo: count eligible for backup: %w", err)
	}
	return n, nil
}

// CountEligibleMissingBackup counts non-deleted, verified assets that have an
// archive copy, fall WITHIN the provider's scope, but have NO pending, running,
// completed, or opted-out backup job for the given provider (destination). The
// status set matches BackupRepo.Enqueue's idempotency exactly, so the result is
// precisely how many jobs a scoped backfill for this provider would create — the
// "N assets aren't queued for this destination yet" count the Providers UI shows.
// Out-of-scope assets are excluded (they are never a gap for this provider — the
// exclusion is a DERIVED policy, not a recorded opt-out). Opted-out jobs count as
// present (NOT missing): a deliberately-excluded asset is surfaced separately as
// "skipped by choice". It is a single indexed NOT EXISTS scan.
func (r *AssetRepo) CountEligibleMissingBackup(ctx context.Context, providerID, scope string) (int64, error) {
	var n int64
	q := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("verification_status = ?", domain.VerificationStatusVerified).
		Where("current_archive_path <> ''").
		Where("NOT EXISTS (SELECT 1 FROM backup_jobs j WHERE j.asset_id = assets.id AND j.destination = ? AND j.deleted_at IS NULL AND j.status IN ?)",
			providerID,
			[]domain.JobStatus{domain.JobStatusPending, domain.JobStatusRunning, domain.JobStatusCompleted, domain.JobStatusOptedOut})
	if err := scopeExtWhere(q, scope).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("repo: count eligible missing backup (provider %q): %w", providerID, err)
	}
	return n, nil
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

// dupWhere is the predicate identifying a flagged duplicate asset (references an
// original via DuplicateOfAssetID). Soft-deleted rows are excluded by GORM's
// default scope on domain.Asset.
const dupWhere = "duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''"

// dupParentDirExpr is a SQLite expression yielding the parent directory of
// current_archive_path (library-root-relative, forward slashes): everything
// before the last '/'. It uses the standard rtrim idiom — rtrim(path, <all
// non-slash characters of path>) strips the trailing filename up to (and
// including) the last slash, then rtrim(..., '/') drops that slash. A root-level
// file with no slash (or a copy-mode duplicate with an empty archive path)
// yields the empty string, which the service labels distinctly.
const dupParentDirExpr = "rtrim(rtrim(current_archive_path, replace(current_archive_path, '/', '')), '/')"

// DuplicateFilter constrains a duplicate listing/aggregation. The zero value
// matches every flagged duplicate. Folder and SessionID filter in SQL (never in
// Go); SortBySize orders by wasted size (the duplicate's FileSize) descending.
type DuplicateFilter struct {
	// HasFolder activates the Folder predicate (distinguishing "no folder filter"
	// from a filter on the empty/root folder).
	HasFolder  bool
	Folder     string
	SessionID  string
	SortBySize bool
}

// duplicateBase builds the filtered base query over flagged duplicates.
func duplicateBase(db *gorm.DB, f DuplicateFilter) *gorm.DB {
	q := db.Model(&domain.Asset{}).Where(dupWhere)
	if f.HasFolder {
		q = q.Where(dupParentDirExpr+" = ?", f.Folder)
	}
	if f.SessionID != "" {
		q = q.Where("session_id = ?", f.SessionID)
	}
	return q
}

// duplicateOrder applies the stable ordering for a filter: by wasted size
// descending when requested, otherwise newest first.
func duplicateOrder(q *gorm.DB, f DuplicateFilter) *gorm.DB {
	if f.SortBySize {
		return q.Order("file_size DESC, created_at DESC")
	}
	return q.Order("created_at DESC")
}

// DuplicateStats returns the true count of flagged duplicates matching f and the
// sum of their FileSize (wasted bytes) — computed entirely in SQL, independent of
// any pagination. A zero filter yields the archive-wide totals.
func (r *AssetRepo) DuplicateStats(ctx context.Context, f DuplicateFilter) (count, wastedBytes int64, err error) {
	var row struct {
		Cnt    int64
		Wasted int64
	}
	if err := duplicateBase(r.db.WithContext(ctx), f).
		Select("COUNT(*) AS cnt, COALESCE(SUM(file_size), 0) AS wasted").
		Scan(&row).Error; err != nil {
		return 0, 0, fmt.Errorf("repo: duplicate stats: %w", err)
	}
	return row.Cnt, row.Wasted, nil
}

// DuplicateIDs returns the IDs of every flagged duplicate matching f, ordered the
// same way ListDuplicatesFiltered pages them — so "select all in filter" yields a
// stable, complete set without paging rows through the caller.
func (r *AssetRepo) DuplicateIDs(ctx context.Context, f DuplicateFilter) ([]string, error) {
	var ids []string
	q := duplicateOrder(duplicateBase(r.db.WithContext(ctx), f), f)
	if err := q.Pluck("id", &ids).Error; err != nil {
		return nil, fmt.Errorf("repo: duplicate ids: %w", err)
	}
	return ids, nil
}

// DuplicateGroup is one distinct group (by folder or session) with its flagged-
// duplicate count and wasted bytes.
type DuplicateGroup struct {
	Key         string
	Count       int64
	WastedBytes int64
}

// DuplicateGroups returns the distinct groups of flagged duplicates by "folder"
// (parent directory of the archive path) or "session" (import session), each with
// its count and wasted bytes, ordered by wasted bytes descending. All grouping is
// done in SQL.
func (r *AssetRepo) DuplicateGroups(ctx context.Context, groupBy string) ([]DuplicateGroup, error) {
	var keyExpr string
	switch groupBy {
	case "folder":
		keyExpr = dupParentDirExpr
	case "session":
		keyExpr = "session_id"
	default:
		return nil, fmt.Errorf("repo: duplicate groups: unknown groupBy %q", groupBy)
	}
	var groups []DuplicateGroup
	if err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where(dupWhere).
		Select(keyExpr + " AS key, COUNT(*) AS count, COALESCE(SUM(file_size), 0) AS wasted_bytes").
		Group(keyExpr).
		Order("wasted_bytes DESC").
		Scan(&groups).Error; err != nil {
		return nil, fmt.Errorf("repo: duplicate groups: %w", err)
	}
	return groups, nil
}

// ListDuplicates returns all non-deleted assets that reference an original via
// DuplicateOfAssetID, paired with that original (newest first, unfiltered).
func (r *AssetRepo) ListDuplicates(ctx context.Context, page Page) ([]DuplicatePair, error) {
	return r.ListDuplicatesFiltered(ctx, DuplicateFilter{}, page)
}

// ListDuplicatesFiltered returns a page of duplicate/original pairs matching f,
// ordered per the filter (by wasted size or newest first).
func (r *AssetRepo) ListDuplicatesFiltered(ctx context.Context, f DuplicateFilter, page Page) ([]DuplicatePair, error) {
	limit, offset := page.apply()

	q := duplicateOrder(duplicateBase(r.db.WithContext(ctx), f), f)
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
// ignored. Text matches OriginalFilename, OriginalFullPath, CameraMake,
// CameraModel, or Lens (case-insensitive substring, LIKE metacharacters escaped).
// YearMonth ("2006-01") restricts to a single capture month. CaptureFrom/CaptureTo
// are inclusive bounds on the EFFECTIVE date (COALESCE(capture_date, import_date)),
// so an undated asset is placed by its import date — consistent with the
// dashboard's assets-over-time axis. CameraMake/CameraModel are EXACT-match
// (equality, not LIKE) so a metacharacter in a camera name matches literally.
type AssetQuery struct {
	MediaType          *domain.MediaType
	VerificationStatus *domain.VerificationStatus
	BackupStatus       *domain.BackupStatus
	SessionID          string
	SourceID           string
	Text               string
	YearMonth          string
	CaptureFrom        *time.Time
	CaptureTo          *time.Time
	CameraMake         string
	CameraModel        string
	// ProviderJob, when non-nil, restricts the result to assets whose backup-job
	// state for one destination (provider) matches a coverage status — an
	// EXISTS/NOT EXISTS subquery over backup_jobs AND-ed with every other
	// predicate. It backs the Backup Coverage view's per-provider status filter.
	ProviderJob *ProviderJobFilter
	Page        Page
}

// ProviderJobMatch is the coverage-status vocabulary the Backup Coverage view
// filters on. The values are identical to the services-layer coverage status
// strings so the browser service can pass them straight through.
type ProviderJobMatch string

// ProviderJobMatch values. They map to EXISTS/NOT EXISTS predicates over a
// destination's backup_jobs (see applyProviderJobFilter).
const (
	ProviderJobNone               ProviderJobMatch = "none"
	ProviderJobSkipped            ProviderJobMatch = "skipped"
	ProviderJobPending            ProviderJobMatch = "pending"
	ProviderJobRunning            ProviderJobMatch = "running"
	ProviderJobFailed             ProviderJobMatch = "failed"
	ProviderJobCancelled          ProviderJobMatch = "cancelled"
	ProviderJobVerified           ProviderJobMatch = "verified"
	ProviderJobUploadedUnverified ProviderJobMatch = "uploaded_unverified"
)

// ProviderJobFilter restricts a listing to assets whose backup-job state for one
// destination matches a coverage status. Mirror carries whether the destination
// is a mirror provider, which decides whether a completed job reads as verified
// (non-mirror, no note) or uploaded_unverified (mirror, or a verify-unavailable
// note) — matching the browser service's per-cell derivation.
type ProviderJobFilter struct {
	Destination string
	Match       ProviderJobMatch
	Mirror      bool
}

// applyProviderJobFilter composes f into base as an EXISTS/NOT EXISTS subquery
// over backup_jobs for f.Destination. The predicate is assembled from fixed,
// caller-independent SQL and domain status constants (never interpolated caller
// text), so it is injection-safe; the destination and statuses are bound
// parameters. Because Enqueue's idempotency prevents a destination from holding
// more than one active job per asset, a per-status EXISTS matches the same job
// the coverage cell derivation displays.
func applyProviderJobFilter(base *gorm.DB, f ProviderJobFilter) *gorm.DB {
	const existsBase = "SELECT 1 FROM backup_jobs j WHERE j.asset_id = assets.id AND j.destination = ? AND j.deleted_at IS NULL"
	dest := f.Destination
	switch f.Match {
	case ProviderJobNone:
		// No job at all (an opted_out job counts as 'skipped', not 'none').
		return base.Where("NOT EXISTS ("+existsBase+")", dest)
	case ProviderJobSkipped:
		return base.Where("EXISTS ("+existsBase+" AND j.status = ?)", dest, domain.JobStatusOptedOut)
	case ProviderJobPending:
		return base.Where("EXISTS ("+existsBase+" AND j.status IN ?)", dest,
			[]domain.JobStatus{domain.JobStatusPending, domain.JobStatusPaused})
	case ProviderJobRunning:
		return base.Where("EXISTS ("+existsBase+" AND j.status = ?)", dest, domain.JobStatusRunning)
	case ProviderJobFailed:
		return base.Where("EXISTS ("+existsBase+" AND j.status = ?)", dest, domain.JobStatusFailed)
	case ProviderJobCancelled:
		return base.Where("EXISTS ("+existsBase+" AND j.status = ?)", dest, domain.JobStatusCancelled)
	case ProviderJobVerified:
		if f.Mirror {
			return base.Where("1 = 0") // a mirror destination is never independently verified
		}
		return base.Where("EXISTS ("+existsBase+" AND j.status = ? AND j.error_message = '')",
			dest, domain.JobStatusCompleted)
	case ProviderJobUploadedUnverified:
		if f.Mirror {
			return base.Where("EXISTS ("+existsBase+" AND j.status = ?)", dest, domain.JobStatusCompleted)
		}
		return base.Where("EXISTS ("+existsBase+" AND j.status = ? AND j.error_message <> '')",
			dest, domain.JobStatusCompleted)
	default:
		return base
	}
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
	if q.CameraMake != "" {
		base = base.Where("camera_make = ?", q.CameraMake)
	}
	if q.CameraModel != "" {
		base = base.Where("camera_model = ?", q.CameraModel)
	}
	if q.Text != "" {
		// One grouped OR across identity + camera metadata columns, with LIKE
		// metacharacters escaped so a literal % or _ in the query (or in a stored
		// value) matches literally. GORM wraps this whole Where in parentheses, so
		// it composes correctly with the AND-ed predicates above.
		like := "%" + escapeLike(q.Text) + "%"
		base = base.Where(
			"original_filename LIKE ? ESCAPE '\\' OR original_full_path LIKE ? ESCAPE '\\' "+
				"OR camera_make LIKE ? ESCAPE '\\' OR camera_model LIKE ? ESCAPE '\\' OR lens LIKE ? ESCAPE '\\'",
			like, like, like, like, like)
	}
	if q.YearMonth != "" {
		base = base.Where("strftime('%Y-%m', capture_date) = ?", q.YearMonth)
	}
	if q.ProviderJob != nil && q.ProviderJob.Destination != "" {
		base = applyProviderJobFilter(base, *q.ProviderJob)
	}
	if q.CaptureFrom != nil {
		base = base.Where(effectiveDateExpr+" >= ?", *q.CaptureFrom)
	}
	if q.CaptureTo != nil {
		base = base.Where(effectiveDateExpr+" <= ?", *q.CaptureTo)
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

// effectiveDateExpr is COALESCE(capture_date, import_date): a row's capture time,
// falling back to its import time when no capture date was extracted. import_date
// is always set, so the expression is never NULL. It is the honest "when did this
// happen" axis the dashboard's assets-over-time rollup groups on.
const effectiveDateExpr = "COALESCE(capture_date, import_date)"

// photoMediaIn is the SQL IN-list of the media types the dashboard counts as
// "photos" (everything that is not a video). Built from the domain enum constants,
// never from caller input, so it is safe to interpolate.
var photoMediaIn = "'" + string(domain.MediaTypePhoto) + "','" +
	string(domain.MediaTypeRawPhoto) + "','" + string(domain.MediaTypeLivePhotoPair) + "'"

// bucketExpr returns the SQLite strftime expression mapping effectiveDateExpr to a
// bucket key for the given concrete granularity ("day"|"month"|"year"|"5year").
// The expression is a fixed, caller-independent string (never user text), so it is
// injection-safe. 5year floors the year to a multiple of five (2017 -> "2015").
func bucketExpr(gran string) (string, error) {
	switch gran {
	case "day":
		return "strftime('%Y-%m-%d', " + effectiveDateExpr + ")", nil
	case "month":
		return "strftime('%Y-%m', " + effectiveDateExpr + ")", nil
	case "year":
		return "strftime('%Y', " + effectiveDateExpr + ")", nil
	case "5year":
		return "CAST((CAST(strftime('%Y', " + effectiveDateExpr + ") AS INTEGER) / 5) * 5 AS TEXT)", nil
	default:
		return "", fmt.Errorf("repo: unknown granularity %q", gran)
	}
}

// BucketCount is one raw effective-capture-time bucket from AssetsByBucket: the
// strftime bucket key and the photo/video split within it.
type BucketCount struct {
	Bucket string `json:"bucket"`
	Photos int64  `json:"photos"`
	Videos int64  `json:"videos"`
}

// AssetsByBucket groups non-deleted assets into effective-capture-time buckets at
// the given granularity, returning the photo/video split per bucket ordered by key
// ascending. Photos are photo + raw_photo + live_photo_pair; videos are video.
// Buckets are keyed on COALESCE(capture_date, import_date), so an asset with no
// capture date lands in its import-date bucket. Only buckets that actually contain
// rows are returned — the caller zero-fills the gaps for an honest time axis.
func (r *AssetRepo) AssetsByBucket(ctx context.Context, gran string) ([]BucketCount, error) {
	expr, err := bucketExpr(gran)
	if err != nil {
		return nil, err
	}
	sel := expr + " AS bucket, " +
		"SUM(CASE WHEN media_type = '" + string(domain.MediaTypeVideo) + "' THEN 1 ELSE 0 END) AS videos, " +
		"SUM(CASE WHEN media_type IN (" + photoMediaIn + ") THEN 1 ELSE 0 END) AS photos"
	var rows []BucketCount
	err = r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select(sel).
		Group("bucket").
		Order("bucket ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repo: assets by bucket (%s): %w", gran, err)
	}
	return rows, nil
}

// EffectiveDateRange returns the earliest and latest effective capture time
// (COALESCE(capture_date, import_date)) across non-deleted assets. Both are nil
// when the library holds no live assets.
func (r *AssetRepo) EffectiveDateRange(ctx context.Context) (minEff, maxEff *time.Time, err error) {
	var row struct {
		MinEff string
		MaxEff string
	}
	err = r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select("min(" + effectiveDateExpr + ") AS min_eff, max(" + effectiveDateExpr + ") AS max_eff").
		Scan(&row).Error
	if err != nil {
		return nil, nil, fmt.Errorf("repo: effective date range: %w", err)
	}
	return parseSQLiteTime(row.MinEff), parseSQLiteTime(row.MaxEff), nil
}

// CountUndatedFallback counts non-deleted assets that have NO capture date and so
// fall back to their import date in the effective-capture-time rollups. It is the
// honest "these bars are placed by import date, not capture date" footnote count.
func (r *AssetRepo) CountUndatedFallback(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("capture_date IS NULL").
		Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("repo: count undated fallback: %w", err)
	}
	return n, nil
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

// YearCount pairs a capture year ("2006") with the number of non-deleted assets
// captured in it.
type YearCount struct {
	Year  string `json:"year"`
	Count int64  `json:"count"`
}

// RollupYears aggregates per-month capture counts ("YYYY-MM") into per-year
// counts ("YYYY"), newest year first. It is a pure function over CaptureMonths
// data (which already excludes undated assets) so the browser's year-level Date
// filter and its month-level picker share one capture-date basis. Malformed or
// short month keys are skipped.
func RollupYears(months []CaptureMonth) []YearCount {
	counts := make(map[string]int64, len(months))
	order := make([]string, 0, len(months))
	for _, m := range months {
		if len(m.Month) < 4 {
			continue
		}
		y := m.Month[:4]
		if _, seen := counts[y]; !seen {
			order = append(order, y)
		}
		counts[y] += m.Count
	}
	sort.Slice(order, func(i, j int) bool { return order[i] > order[j] }) // newest year first
	out := make([]YearCount, 0, len(order))
	for _, y := range order {
		out = append(out, YearCount{Year: y, Count: counts[y]})
	}
	return out
}

// CameraCount pairs a distinct camera (make + model) with the number of
// non-deleted assets that carry it.
type CameraCount struct {
	Make  string `json:"make"`
	Model string `json:"model"`
	Count int64  `json:"count"`
}

// Cameras returns distinct (make, model) camera pairs with per-pair asset counts
// across non-deleted assets, most-used first (ties broken by make then model).
// Rows with neither a make nor a model are excluded — they carry no camera
// identity to filter on. Backs the browser's Camera filter dropdown.
func (r *AssetRepo) Cameras(ctx context.Context) ([]CameraCount, error) {
	var rows []CameraCount
	err := r.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select("camera_make as make, camera_model as model, count(*) as count").
		Where("camera_make <> '' OR camera_model <> ''").
		Group("camera_make, camera_model").
		Order("count DESC, camera_make, camera_model").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repo: cameras: %w", err)
	}
	return rows, nil
}
