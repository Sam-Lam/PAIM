package services

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	sleepAware
	db       *gorm.DB
	assets   *repo.AssetRepo
	sessions *repo.SessionRepo
	settings *repo.SettingsRepo
	emitter  Emitter
	log      *slog.Logger
	// root is the portable-library root used to resolve stored (relative) archive
	// paths to absolute for file operations and to relativize new paths for
	// storage. Empty (tests/dev) leaves absolute paths untouched.
	root string

	// mu guards the background bulk-resolve state below.
	mu         sync.Mutex
	bulkActive bool
	bulkCancel context.CancelFunc
	bulkRun    *bulkResolveRun
}

// Bind wires the DuplicateService to an open library's catalog in place.
func (s *DuplicateService) Bind(core *AppCore) {
	s.db = core.DB
	s.assets = core.Assets
	s.sessions = core.Sessions
	s.settings = core.Settings
	s.root = core.Root
}

// NewDuplicateService constructs a DuplicateService. The db handle is used only
// to update an asset's CurrentArchivePath after a move (the AssetRepo exposes no
// setter for that column). The sessions repo enriches import-session group labels.
func NewDuplicateService(db *gorm.DB, assets *repo.AssetRepo, sessions *repo.SessionRepo, settings *repo.SettingsRepo, emitter Emitter, logger *slog.Logger) *DuplicateService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DuplicateService{db: db, assets: assets, sessions: sessions, settings: settings, emitter: emitter, log: logger.With(slog.String("subsystem", "duplicate"))}
}

// DuplicatePairDTO pairs a duplicate asset with the original it duplicates, plus
// per-side presence flags so the UI can disable reveal actions for files that are
// not reachable right now (e.g. the archive volume is offline). Every duplicate
// managed here has its OWN physical archive copy (that is what makes it a genuine,
// reclaimable duplicate); source-only placeholder rows are excluded upstream.
type DuplicatePairDTO struct {
	Duplicate AssetDTO `json:"duplicate"`
	Original  AssetDTO `json:"original"`
	// DuplicateFileExists reports whether the duplicate's archived file exists on
	// disk right now (os.Stat of its resolved archive path).
	DuplicateFileExists bool `json:"duplicateFileExists"`
	// OriginalFileExists reports whether the original's archived file exists right
	// now (os.Stat of its resolved archive path).
	OriginalFileExists bool `json:"originalFileExists"`
}

// DuplicateStatsDTO reports the TRUE totals across all non-deleted flagged
// duplicates (never just the visible page): the pair count and the sum of the
// duplicate assets' FileSize (reclaimable "wasted" bytes).
type DuplicateStatsDTO struct {
	TotalPairs       int64 `json:"totalPairs"`
	TotalWastedBytes int64 `json:"totalWastedBytes"`
}

// DuplicateFilterDTO selects a subset of duplicates for the listing/ID methods.
// GroupBy ("" | "folder" | "session") plus GroupKey narrows to one folder or one
// import session; SortBySize orders by wasted size descending. All filtering is
// pushed into SQL.
type DuplicateFilterDTO struct {
	GroupBy    string `json:"groupBy"`
	GroupKey   string `json:"groupKey"`
	SortBySize bool   `json:"sortBySize"`
}

// toRepo maps the DTO filter to the repo filter.
func (f DuplicateFilterDTO) toRepo() repo.DuplicateFilter {
	rf := repo.DuplicateFilter{SortBySize: f.SortBySize}
	switch f.GroupBy {
	case "folder":
		rf.HasFolder = true
		rf.Folder = f.GroupKey
	case "session":
		rf.SessionID = f.GroupKey
	}
	return rf
}

// DuplicateGroupDTO is one group in the picker: its raw Key (folder path or
// session ID, echoed back as GroupKey when filtering), a human Label, and the
// flagged-duplicate Count and WastedBytes in that group.
type DuplicateGroupDTO struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Count       int64  `json:"count"`
	WastedBytes int64  `json:"wastedBytes"`
}

// DuplicateStats returns the archive-wide duplicate totals (pair count + wasted
// bytes) for the header, computed in SQL across ALL flagged duplicates.
func (s *DuplicateService) DuplicateStats(ctx context.Context) (DuplicateStatsDTO, error) {
	if err := s.guard(); err != nil {
		return DuplicateStatsDTO{}, err
	}
	count, wasted, err := s.assets.DuplicateStats(ctx, repo.DuplicateFilter{})
	if err != nil {
		return DuplicateStatsDTO{}, err
	}
	return DuplicateStatsDTO{TotalPairs: count, TotalWastedBytes: wasted}, nil
}

// CountSourceOnlyRecords returns how many legacy source-only placeholder duplicate
// rows exist (flagged duplicates with an empty archive path). These came from
// pre-v5 copy-mode re-imports of already-archived sources: nothing was copied, so
// they are "already imported", not duplicates. A non-zero count drives the
// Duplicate Manager's one-time cleanup banner.
func (s *DuplicateService) CountSourceOnlyRecords(ctx context.Context) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	return s.assets.CountSourceOnlyDuplicates(ctx)
}

// RemoveSourceOnlyRecords soft-deletes ALL source-only placeholder duplicate rows
// (flagged duplicates with an empty archive path) in one transaction and returns
// the count removed. It is record-only and reversible (soft delete): it NEVER
// touches any file — there is no library copy, and the source file on the card
// must never be touched. Archive-copy duplicates (the Duplicate Manager's real
// workload) are untouched.
func (s *DuplicateService) RemoveSourceOnlyRecords(ctx context.Context) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	n, err := s.assets.SoftDeleteSourceOnlyDuplicates(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Info("removed source-only duplicate records (already-imported re-imports)", "count", n)
	}
	return n, nil
}

// ListDuplicates returns a page of duplicate/original pairs (newest first,
// unfiltered). Retained for callers that do not filter; the Total is the true
// archive-wide count.
func (s *DuplicateService) ListDuplicates(ctx context.Context, page, pageSize int) (PageResult[DuplicatePairDTO], error) {
	return s.ListDuplicatesFiltered(ctx, DuplicateFilterDTO{}, page, pageSize)
}

// ListDuplicatesFiltered returns a page of duplicate/original pairs matching the
// filter. Total reflects the FILTERED count so pagination is correct within a
// folder/session/sort selection.
func (s *DuplicateService) ListDuplicatesFiltered(ctx context.Context, filter DuplicateFilterDTO, page, pageSize int) (PageResult[DuplicatePairDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	rf := filter.toRepo()
	limit, offset := normalizePage(page, pageSize)
	pairs, err := s.assets.ListDuplicatesFiltered(ctx, rf, repo.Page{Limit: limit, Offset: offset})
	if err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	items := make([]DuplicatePairDTO, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, s.toPairDTO(p))
	}
	total, _, err := s.assets.DuplicateStats(ctx, rf)
	if err != nil {
		return PageResult[DuplicatePairDTO]{}, err
	}
	return PageResult[DuplicatePairDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// toPairDTO maps a repo pair to its DTO with per-side presence flags. Both sides
// have an archived file (source-only rows are excluded upstream), so existence is
// always checked at the resolved archive path.
func (s *DuplicateService) toPairDTO(p repo.DuplicatePair) DuplicatePairDTO {
	dup := toAssetDTO(p.Duplicate, s.root)
	orig := toAssetDTO(p.Original, s.root)
	return DuplicatePairDTO{
		Duplicate:           dup,
		Original:            orig,
		DuplicateFileExists: pathExists(dup.CurrentArchivePath),
		OriginalFileExists:  pathExists(orig.CurrentArchivePath),
	}
}

// ListDuplicateGroups returns the distinct duplicate groups (with counts + wasted
// bytes) for a group picker. groupBy is "folder" or "session". Groups are ordered
// by wasted bytes descending.
func (s *DuplicateService) ListDuplicateGroups(ctx context.Context, groupBy string) ([]DuplicateGroupDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	groups, err := s.assets.DuplicateGroups(ctx, groupBy)
	if err != nil {
		return nil, err
	}
	out := make([]DuplicateGroupDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, DuplicateGroupDTO{
			Key:         g.Key,
			Label:       s.groupLabel(ctx, groupBy, g.Key),
			Count:       g.Count,
			WastedBytes: g.WastedBytes,
		})
	}
	return out, nil
}

// groupLabel derives a human label for a group key. Folder keys become the
// root-relative path (empty → "Library root / not copied"); session keys are
// enriched with the session's date + source label when the session is found.
func (s *DuplicateService) groupLabel(ctx context.Context, groupBy, key string) string {
	switch groupBy {
	case "folder":
		if key == "" {
			return "Library root"
		}
		return key
	case "session":
		if key == "" {
			return "No import session"
		}
		if s.sessions != nil {
			if sess, err := s.sessions.GetByID(ctx, key); err == nil && sess != nil {
				dto := toSessionDTO(*sess)
				label := dto.StartedAt.Format("Jan 2, 2006 15:04")
				if dto.SourceLabel != "" {
					label += " · " + dto.SourceLabel
				}
				return label
			}
		}
		return key
	default:
		return key
	}
}

// ListDuplicateIDs returns the full ID list of every flagged duplicate matching
// the filter, so the frontend can "select all in folder/session/filter" without
// paging thousands of rows through the UI.
func (s *DuplicateService) ListDuplicateIDs(ctx context.Context, filter DuplicateFilterDTO) ([]string, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return s.assets.DuplicateIDs(ctx, filter.toRepo())
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
	return s.applyResolution(ctx, duplicateAssetID, archiveAbs, action, destFolder)
}

// applyResolution performs one resolution action against an already-resolved
// (absolute) archive path. It is the single implementation shared by the
// per-pair ResolveDuplicate and the bulk StartBulkResolve job, so file handling
// is never reimplemented. destFolder is used only by the move action.
func (s *DuplicateService) applyResolution(ctx context.Context, assetID, archiveAbs, action, destFolder string) error {
	switch action {
	case DuplicateActionDelete:
		return s.resolveDelete(ctx, assetID, archiveAbs)
	case DuplicateActionMove:
		return s.resolveMove(ctx, assetID, archiveAbs, destFolder)
	case DuplicateActionIgnore:
		if err := s.assets.MarkDuplicateOf(ctx, assetID, ""); err != nil {
			return err
		}
		s.log.Info("duplicate ignored", "assetId", assetID, "note", "kept as-is, flag cleared by user")
		return nil
	case DuplicateActionKeepBoth:
		if err := s.assets.MarkDuplicateOf(ctx, assetID, ""); err != nil {
			return err
		}
		s.log.Info("duplicate resolved keep-both", "assetId", assetID, "note", "both copies kept intentionally")
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
