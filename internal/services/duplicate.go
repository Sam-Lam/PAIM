package services

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// Duplicate resolution actions.
const (
	DuplicateActionDelete   = "delete"
	DuplicateActionMove     = "move"
	DuplicateActionIgnore   = "ignore"
	DuplicateActionKeepBoth = "keep_both"
)

// DuplicateService lists duplicate pairs and resolves them. Every resolving
// action is destructive-adjacent and MUST be confirmed by the frontend (via
// ConfirmDialog) before it is invoked; the service performs the action without
// re-prompting.
type DuplicateService struct {
	gated
	db       *gorm.DB
	assets   *repo.AssetRepo
	settings *repo.SettingsRepo
	emitter  Emitter
	log      *slog.Logger
	// root is the portable-library root used to resolve stored (relative) archive
	// paths to absolute for file operations and to relativize new paths for
	// storage. Empty (tests/dev) leaves absolute paths untouched.
	root string
}

// Bind wires the DuplicateService to an open library's catalog in place.
func (s *DuplicateService) Bind(core *AppCore) {
	s.db = core.DB
	s.assets = core.Assets
	s.settings = core.Settings
	s.root = core.Root
}

// NewDuplicateService constructs a DuplicateService. The db handle is used only
// to update an asset's CurrentArchivePath after a move (the AssetRepo exposes no
// setter for that column).
func NewDuplicateService(db *gorm.DB, assets *repo.AssetRepo, settings *repo.SettingsRepo, emitter Emitter, logger *slog.Logger) *DuplicateService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DuplicateService{db: db, assets: assets, settings: settings, emitter: emitter, log: logger.With(slog.String("subsystem", "duplicate"))}
}

// DuplicatePairDTO pairs a duplicate asset with the original it duplicates.
type DuplicatePairDTO struct {
	Duplicate AssetDTO `json:"duplicate"`
	Original  AssetDTO `json:"original"`
}

// ListDuplicates returns a page of duplicate/original pairs (newest first).
func (s *DuplicateService) ListDuplicates(ctx context.Context, page, pageSize int) (PageResult[DuplicatePairDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	limit, offset := normalizePage(page, pageSize)
	pairs, err := s.assets.ListDuplicates(ctx, repo.Page{Limit: limit, Offset: offset})
	if err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	items := make([]DuplicatePairDTO, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, DuplicatePairDTO{
			Duplicate: toAssetDTO(p.Duplicate, s.root),
			Original:  toAssetDTO(p.Original, s.root),
		})
	}
	total, err := s.countDuplicates(ctx)
	if err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	return PageResult[DuplicatePairDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// countDuplicates returns the true number of flagged duplicate assets (ignoring
// pagination), matching the filter used by AssetRepo.ListDuplicates. Soft-deleted
// rows are excluded by GORM's default scope.
func (s *DuplicateService) countDuplicates(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").
		Count(&total).Error
	if err != nil {
		return 0, fmt.Errorf("services: count duplicates: %w", err)
	}
	return total, nil
}

// ResolveDuplicate applies action to the duplicate asset. Actions (all require a
// prior frontend confirmation):
//
//   - delete: soft-delete the asset row AND move its physical archive file (when
//     present) into <MasterLibrary>/.paim-trash/. The file is never hard-deleted.
//   - move: physically move the duplicate's file to destFolder — a same-volume
//     atomic rename, or a cross-volume copy+verify followed by trashing the
//     original — and update CurrentArchivePath.
//   - ignore: clear the duplicate flag and record a note (in the log; the Asset
//     model has no notes column).
//   - keep_both: clear the duplicate flag, keeping both files intentionally, and
//     record a note.
//
// destFolder is used only by the move action.
func (s *DuplicateService) ResolveDuplicate(ctx context.Context, duplicateAssetID, action, destFolder string) error {
	if err := s.guard(); err != nil {
		return err
	}
	asset, err := s.assets.GetByID(ctx, duplicateAssetID)
	if err != nil {
		return err
	}
	// Stored paths are relative to the library root; resolve to absolute for the
	// filesystem operations below.
	archiveAbs := library.ResolvePath(s.root, asset.CurrentArchivePath)

	switch action {
	case DuplicateActionDelete:
		return s.resolveDelete(ctx, duplicateAssetID, archiveAbs)
	case DuplicateActionMove:
		return s.resolveMove(ctx, duplicateAssetID, archiveAbs, destFolder)
	case DuplicateActionIgnore:
		if err := s.assets.MarkDuplicateOf(ctx, duplicateAssetID, ""); err != nil {
			return err
		}
		s.log.Info("duplicate ignored", "assetId", duplicateAssetID, "note", "kept as-is, flag cleared by user")
		return nil
	case DuplicateActionKeepBoth:
		if err := s.assets.MarkDuplicateOf(ctx, duplicateAssetID, ""); err != nil {
			return err
		}
		s.log.Info("duplicate resolved keep-both", "assetId", duplicateAssetID, "note", "both copies kept intentionally")
		return nil
	default:
		return fmt.Errorf("services: unknown duplicate action %q", action)
	}
}

// resolveDelete trashes the asset's physical file (if any) and then soft-deletes
// the row, recording the trash location as the asset's current path in the same
// operation so recovery is easy. The file is trashed FIRST: if trashing fails we
// do not soft-delete, so the DB never claims a file is gone while it is still in
// place.
func (s *DuplicateService) resolveDelete(ctx context.Context, assetID, archivePath string) error {
	if archivePath == "" {
		if err := s.assets.SoftDelete(ctx, assetID); err != nil {
			return err
		}
		s.log.Info("duplicate soft-deleted (no physical file)", "assetId", assetID)
		return nil
	}
	trashRoot := s.root
	if trashRoot == "" {
		if root, err := s.masterRoot(ctx); err == nil {
			trashRoot = root
		}
	}
	if trashRoot == "" {
		trashRoot = filepath.Dir(archivePath)
	}
	trashed, err := trashFile(trashRoot, archivePath)
	if err != nil {
		return err
	}
	// Record the new (trash) location (relativized to the root) together with the
	// soft-delete so the file can be found for recovery.
	if err := s.assets.SoftDeleteWithPath(ctx, assetID, library.RelativizePath(s.root, trashed)); err != nil {
		return err
	}
	s.log.Info("duplicate deleted (file moved to trash)", "assetId", assetID, "trash", trashed)
	return nil
}

// resolveMove physically relocates the duplicate's file to destFolder and updates
// the recorded archive path.
func (s *DuplicateService) resolveMove(ctx context.Context, assetID, archivePath, destFolder string) error {
	if archivePath == "" {
		return fmt.Errorf("services: cannot move duplicate %q: it has no physical file", assetID)
	}
	if destFolder == "" {
		return fmt.Errorf("services: cannot move duplicate %q: no destination folder", assetID)
	}
	dst := filepath.Join(destFolder, filepath.Base(archivePath))

	same, err := sameVolume(archivePath, dst)
	if err != nil {
		return err
	}

	// Journal the intent BEFORE mutating disk so a crash between the file op and
	// the DB update leaves a breadcrumb for manual recovery (subsystem "duplicate").
	s.log.Info("duplicate move intent", "assetId", assetID, "from", archivePath, "to", dst, "sameVolume", same)

	// undo reverses the on-disk move, so a DB failure can leave disk and DB
	// consistent (best effort).
	var undo func() error
	if same {
		if err := ensureDir(destFolder); err != nil {
			return err
		}
		if err := renameFile(archivePath, dst); err != nil {
			return err
		}
		undo = func() error { return os.Rename(dst, archivePath) }
	} else {
		// Cross-volume: copy+verify, then trash the original (never a bare delete).
		// Emit throttled byte progress so the ConfirmDialog can show a copy bar; a
		// cancelled/closed ctx aborts the copy and the temp partial is removed by
		// copyVerify's own cleanup path.
		tr := newThrottle()
		progressFn := func(bytesDone, bytesTotal int64) {
			if bytesDone == bytesTotal || tr.allow() {
				emitSafe(s.emitter, EventDuplicateProgress, DuplicateProgress{
					AssetID:    assetID,
					BytesDone:  bytesDone,
					BytesTotal: bytesTotal,
				})
			}
		}
		if err := copyVerify(ctx, archivePath, dst, progressFn); err != nil {
			return err
		}
		trashed, err := trashFile(filepath.Dir(archivePath), archivePath)
		if err != nil {
			return err
		}
		undo = func() error {
			if e := os.Rename(trashed, archivePath); e != nil {
				return e
			}
			_ = os.Remove(dst)
			return nil
		}
	}

	// Update the recorded path immediately after the file op, retrying transient
	// DB errors. On permanent failure, roll the file back so disk and DB agree.
	// The stored path is relativized to the library root.
	if err := s.updateArchivePathWithRetry(ctx, assetID, library.RelativizePath(s.root, dst)); err != nil {
		if uerr := undo(); uerr != nil {
			s.log.Error("duplicate move: DB update failed AND file rollback failed; manual recovery needed",
				"assetId", assetID, "from", archivePath, "to", dst, "dbError", err.Error(), "rollbackError", uerr.Error())
		} else {
			s.log.Warn("duplicate move: DB update failed; rolled file back to origin",
				"assetId", assetID, "from", dst, "to", archivePath, "error", err.Error())
		}
		return fmt.Errorf("services: update archive path for %q: %w", assetID, err)
	}
	s.log.Info("duplicate moved", "assetId", assetID, "dest", dst)
	return nil
}

// updateArchivePathWithRetry updates an asset's CurrentArchivePath, retrying a
// few times to ride out transient SQLite contention (e.g. a busy lock) before
// giving up.
func (s *DuplicateService) updateArchivePathWithRetry(ctx context.Context, assetID, path string) error {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		res := s.db.WithContext(ctx).Model(&domain.Asset{}).Where("id = ?", assetID).
			Update("current_archive_path", path)
		if res.Error == nil {
			if res.RowsAffected == 0 {
				return fmt.Errorf("asset %q not found", assetID)
			}
			return nil
		}
		err = res.Error
		time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
	}
	return err
}

// masterRoot resolves the configured Master Library root (may be empty).
func (s *DuplicateService) masterRoot(ctx context.Context) (string, error) {
	cfg, err := LoadSettings(ctx, s.settings)
	if err != nil {
		return "", err
	}
	return cfg.MasterLibraryRoot, nil
}
