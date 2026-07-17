package services

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
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
	db       *gorm.DB
	assets   *repo.AssetRepo
	settings *repo.SettingsRepo
	log      *slog.Logger
}

// NewDuplicateService constructs a DuplicateService. The db handle is used only
// to update an asset's CurrentArchivePath after a move (the AssetRepo exposes no
// setter for that column).
func NewDuplicateService(db *gorm.DB, assets *repo.AssetRepo, settings *repo.SettingsRepo, logger *slog.Logger) *DuplicateService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DuplicateService{db: db, assets: assets, settings: settings, log: logger.With(slog.String("subsystem", "duplicate"))}
}

// DuplicatePairDTO pairs a duplicate asset with the original it duplicates.
type DuplicatePairDTO struct {
	Duplicate AssetDTO `json:"duplicate"`
	Original  AssetDTO `json:"original"`
}

// ListDuplicates returns a page of duplicate/original pairs (newest first).
func (s *DuplicateService) ListDuplicates(ctx context.Context, page, pageSize int) (PageResult[DuplicatePairDTO], error) {
	limit, offset := normalizePage(page, pageSize)
	pairs, err := s.assets.ListDuplicates(ctx, repo.Page{Limit: limit, Offset: offset})
	if err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	items := make([]DuplicatePairDTO, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, DuplicatePairDTO{
			Duplicate: toAssetDTO(p.Duplicate),
			Original:  toAssetDTO(p.Original),
		})
	}
	return PageResult[DuplicatePairDTO]{Items: items, Total: int64(len(items)), Page: page, PageSize: pageSize}, nil
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
	asset, err := s.assets.GetByID(ctx, duplicateAssetID)
	if err != nil {
		return err
	}

	switch action {
	case DuplicateActionDelete:
		return s.resolveDelete(ctx, duplicateAssetID, asset.CurrentArchivePath)
	case DuplicateActionMove:
		return s.resolveMove(ctx, duplicateAssetID, asset.CurrentArchivePath, destFolder)
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

// resolveDelete soft-deletes the asset and trashes its physical file (if any).
func (s *DuplicateService) resolveDelete(ctx context.Context, assetID, archivePath string) error {
	if err := s.assets.SoftDelete(ctx, assetID); err != nil {
		return err
	}
	if archivePath == "" {
		s.log.Info("duplicate soft-deleted (no physical file)", "assetId", assetID)
		return nil
	}
	root, err := s.masterRoot(ctx)
	if err != nil {
		return err
	}
	trashRoot := root
	if trashRoot == "" {
		trashRoot = filepath.Dir(archivePath)
	}
	trashed, err := trashFile(trashRoot, archivePath)
	if err != nil {
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
	if same {
		if err := ensureDir(destFolder); err != nil {
			return err
		}
		if err := renameFile(archivePath, dst); err != nil {
			return err
		}
	} else {
		// Cross-volume: copy+verify, then trash the original (never a bare delete).
		if err := copyVerify(ctx, archivePath, dst); err != nil {
			return err
		}
		if _, err := trashFile(filepath.Dir(archivePath), archivePath); err != nil {
			return err
		}
	}

	res := s.db.WithContext(ctx).Model(&domain.Asset{}).Where("id = ?", assetID).
		Update("current_archive_path", dst)
	if res.Error != nil {
		return fmt.Errorf("services: update archive path for %q: %w", assetID, res.Error)
	}
	s.log.Info("duplicate moved", "assetId", assetID, "dest", dst)
	return nil
}

// masterRoot resolves the configured Master Library root (may be empty).
func (s *DuplicateService) masterRoot(ctx context.Context) (string, error) {
	cfg, err := LoadSettings(ctx, s.settings)
	if err != nil {
		return "", err
	}
	return cfg.MasterLibraryRoot, nil
}
