package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// ErrOperationActive is returned by mutating BrowserService methods (event
// rename) when any long operation is in flight. Renaming an archive folder while
// an import/reorganize is moving files under it is unsafe, so it is refused with
// the same one-active-operation discipline the importer uses.
var ErrOperationActive = errors.New("services: an operation is in progress; wait for it to finish, then try again")

// dateFolderRe matches a renameable date-event folder relative path:
// "YYYY/YYYY-MM-DD" optionally followed by " Label" (the label has no slash).
var dateFolderRe = regexp.MustCompile(`^\d{4}/\d{4}-\d{2}-\d{2}( [^/]+)?$`)

// activitySnapshotter is the minimal view of the activity tracker the browser
// consults before a mutating folder operation. *ActivityTracker satisfies it.
type activitySnapshotter interface {
	Snapshot() []OperationInfo
}

// Reveal targets for RevealAsset: which of an asset's two files to select in
// Finder.
const (
	RevealArchive  = "archive"  // the archived copy (resolved CurrentArchivePath)
	RevealOriginal = "original" // the original source file (OriginalFullPath)
)

// revealRunner selects a path in the platform file browser (Finder). It is
// injected so tests can assert path resolution and which-validation without
// spawning a real `open` process.
type revealRunner func(path string) error

// revealInFinder selects path in Finder via `open -R`.
func revealInFinder(path string) error {
	return exec.Command("open", "-R", path).Run()
}

// BrowserService powers the read-only asset browser: a filtered, paginated grid
// of what is archived, plus full provenance for a single asset. It is strictly
// read-only — PAIM is an integrity tool, not a DAM — and exposes no mutating
// methods. Every method is gated on an open library.
type BrowserService struct {
	gated
	db       *gorm.DB
	assets   *repo.AssetRepo
	sources  *repo.SourceRepo
	sessions *repo.SessionRepo
	log      *slog.Logger
	// root resolves stored (relative) archive paths to absolute for display.
	root string
	// reveal opens a resolved path in Finder; injectable for tests.
	reveal revealRunner
	// activity, when set, lets RenameEventFolder refuse while any long operation
	// is running. Nil (unit tests) means "never busy".
	activity activitySnapshotter
}

// SetActivity injects the shared activity tracker so RenameEventFolder can refuse
// while any long operation is in flight. Called once by main.go.
func (s *BrowserService) SetActivity(a activitySnapshotter) { s.activity = a }

// Bind wires the BrowserService to an open library's catalog in place.
func (s *BrowserService) Bind(core *AppCore) {
	s.db = core.DB
	s.assets = core.Assets
	s.sources = core.Sources
	s.sessions = core.Sessions
	s.root = core.Root
}

// NewBrowserService constructs a BrowserService.
func NewBrowserService(db *gorm.DB, assets *repo.AssetRepo, sources *repo.SourceRepo, sessions *repo.SessionRepo, logger *slog.Logger) *BrowserService {
	if logger == nil {
		logger = slog.Default()
	}
	return &BrowserService{db: db, assets: assets, sources: sources, sessions: sessions, log: logger.With(slog.String("subsystem", "browser")), reveal: revealInFinder}
}

// RevealAsset reveals one of an asset's files in Finder (`open -R`). The path is
// resolved SERVER-side from the asset row — the frontend never passes a raw
// filesystem path across the bridge. which selects the archived copy
// (RevealArchive) or the original source file (RevealOriginal). The file must
// exist; a missing file (e.g. the SD card was ejected) returns an error the
// frontend surfaces as a toast — the reveal button is normally disabled when the
// file is already known to be absent.
func (s *BrowserService) RevealAsset(ctx context.Context, assetID, which string) error {
	if err := s.guard(); err != nil {
		return err
	}
	if which != RevealArchive && which != RevealOriginal {
		return fmt.Errorf("services: reveal: unknown target %q (want %q or %q)", which, RevealArchive, RevealOriginal)
	}
	a, err := s.assets.GetByID(ctx, assetID)
	if err != nil {
		return err
	}
	var path string
	switch which {
	case RevealArchive:
		path = library.ResolvePath(s.root, a.CurrentArchivePath)
		if path == "" {
			return fmt.Errorf("services: reveal: asset %q has no archive copy", assetID)
		}
	case RevealOriginal:
		path = a.OriginalFullPath
		if path == "" {
			return fmt.Errorf("services: reveal: asset %q has no original path", assetID)
		}
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("services: reveal: file not found for asset %q: %w", assetID, err)
	}
	reveal := s.reveal
	if reveal == nil {
		reveal = revealInFinder
	}
	if err := reveal(path); err != nil {
		return fmt.Errorf("services: reveal %q: %w", path, err)
	}
	s.log.Info("revealed asset in Finder", "assetId", assetID, "which", which)
	return nil
}

// FolderEntryDTO is one immediate subdirectory in a folder listing: its display
// name, full root-relative path (for drilling in), recursive asset count, a
// representative cover asset id (for a thumbnail), and whether it is a renameable
// date-event folder.
type FolderEntryDTO struct {
	Name         string `json:"name"`
	RelPath      string `json:"relPath"`
	AssetCount   int64  `json:"assetCount"`
	CoverAssetID string `json:"coverAssetId"`
	IsDateFolder bool   `json:"isDateFolder"`
}

// FolderListingDTO is one level of the archive tree: the cleaned directory, its
// immediate subfolders, and the assets sitting directly in it (paged). For a
// date-event folder it also carries IsDateFolder=true and the current Label so
// the UI can offer Rename… prefilled.
type FolderListingDTO struct {
	RelDir       string                     `json:"relDir"`
	IsDateFolder bool                       `json:"isDateFolder"`
	Label        string                     `json:"label"`
	Subfolders   []FolderEntryDTO           `json:"subfolders"`
	Assets       PageResult[BrowseAssetDTO] `json:"assets"`
}

// ListFolder returns one level of the archive tree derived from the catalog
// (never a filesystem walk): the immediate subfolders of relDir (each with a
// recursive asset count and cover thumbnail) plus the assets sitting directly in
// relDir, paged. relDir is a root-relative forward-slash directory; "" lists the
// year folders at the root. The returned RelDir is the cleaned, breadcrumb-safe
// path.
func (s *BrowserService) ListFolder(ctx context.Context, relDir string, page, pageSize int) (FolderListingDTO, error) {
	if err := s.guard(); err != nil {
		return FolderListingDTO{}, err
	}
	clean, err := cleanRelDir(relDir)
	if err != nil {
		return FolderListingDTO{}, err
	}

	children, err := s.assets.FolderChildren(ctx, clean)
	if err != nil {
		return FolderListingDTO{}, err
	}
	subs := make([]FolderEntryDTO, 0, len(children))
	for _, c := range children {
		relPath := c.Name
		if clean != "" {
			relPath = clean + "/" + c.Name
		}
		subs = append(subs, FolderEntryDTO{
			Name:         c.Name,
			RelPath:      relPath,
			AssetCount:   c.AssetCount,
			CoverAssetID: c.CoverAssetID,
			IsDateFolder: dateFolderRe.MatchString(relPath),
		})
	}

	limit, offset := normalizePage(page, pageSize)
	rows, total, err := s.assets.FolderAssets(ctx, clean, repo.Page{Limit: limit, Offset: offset})
	if err != nil {
		return FolderListingDTO{}, err
	}
	items := make([]BrowseAssetDTO, 0, len(rows))
	for _, a := range rows {
		items = append(items, toBrowseAssetDTO(a))
	}

	return FolderListingDTO{
		RelDir:       clean,
		IsDateFolder: dateFolderRe.MatchString(clean),
		Label:        folderLabel(clean),
		Subfolders:   subs,
		Assets:       PageResult[BrowseAssetDTO]{Items: items, Total: total, Page: page, PageSize: pageSize},
	}, nil
}

// RevealFolder reveals an archive folder in Finder (`open -R`). relDir is a
// root-relative forward-slash directory resolved SERVER-side; the frontend never
// passes an absolute path. The directory must exist under the library root.
func (s *BrowserService) RevealFolder(ctx context.Context, relDir string) error {
	if err := s.guard(); err != nil {
		return err
	}
	clean, err := cleanRelDir(relDir)
	if err != nil {
		return err
	}
	if clean == "" {
		return fmt.Errorf("services: reveal folder: empty path")
	}
	abs := filepath.Join(s.root, filepath.FromSlash(clean))
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("services: reveal folder: %q not found", clean)
	}
	reveal := s.reveal
	if reveal == nil {
		reveal = revealInFinder
	}
	if err := reveal(abs); err != nil {
		return fmt.Errorf("services: reveal folder %q: %w", clean, err)
	}
	s.log.Info("revealed folder in Finder", "relDir", clean)
	return nil
}

// RenameEventFolder renames a date-event folder's human label. relDir must be a
// "YYYY/YYYY-MM-DD*" folder under the library root; the new folder name is the
// date prefix plus the sanitized newLabel (an empty label yields the bare
// "YYYY-MM-DD"). It refuses when the target folder already exists, when relDir is
// not a date folder, when the resolved path would escape the root, and when any
// long operation is running. It renames the directory on disk, then rewrites the
// CurrentArchivePath prefix of every contained asset (RAW/ subpaths ride along)
// in ONE transaction; on a DB failure it best-effort renames the directory back
// and returns the error. This is the ONLY sanctioned way to rename an archive
// folder — doing it in Finder would strand the catalog.
func (s *BrowserService) RenameEventFolder(ctx context.Context, relDir, newLabel string) (FolderListingDTO, error) {
	if err := s.guard(); err != nil {
		return FolderListingDTO{}, err
	}
	// One-active-operation guard: never rename under a running move.
	if s.activity != nil && len(s.activity.Snapshot()) > 0 {
		return FolderListingDTO{}, ErrOperationActive
	}

	oldRel, err := cleanRelDir(relDir)
	if err != nil {
		return FolderListingDTO{}, err
	}
	if !dateFolderRe.MatchString(oldRel) {
		return FolderListingDTO{}, fmt.Errorf("services: rename: %q is not a YYYY/YYYY-MM-DD event folder", oldRel)
	}

	year, base := path.Split(oldRel)          // "2019/", "2019-06-12 Trip"
	datePart := base[:10]                      // "2019-06-12" (regex guarantees length)
	newBase := datePart
	if label := archive.SanitizeEvent(newLabel); label != "" {
		newBase = datePart + " " + label
	}
	newRel := path.Join(year, newBase) // year already has trailing slash for Join

	// Resolve absolute paths and confirm neither escapes the root.
	oldAbs := filepath.Join(s.root, filepath.FromSlash(oldRel))
	newAbs := filepath.Join(s.root, filepath.FromSlash(newRel))
	if !underRoot(s.root, oldAbs) || !underRoot(s.root, newAbs) {
		return FolderListingDTO{}, fmt.Errorf("services: rename: resolved path escapes the library root")
	}

	if newRel == oldRel {
		// No change requested; return the current listing unchanged.
		return s.ListFolder(ctx, oldRel, 1, folderRenamePageSize)
	}

	// The source must exist and be a directory.
	if info, err := os.Stat(oldAbs); err != nil || !info.IsDir() {
		return FolderListingDTO{}, fmt.Errorf("services: rename: source folder %q not found", oldRel)
	}
	// Refuse if the target already exists (never merge two folders).
	if _, err := os.Stat(newAbs); err == nil {
		return FolderListingDTO{}, fmt.Errorf("services: rename: target folder %q already exists", newRel)
	} else if !os.IsNotExist(err) {
		return FolderListingDTO{}, fmt.Errorf("services: rename: stat target %q: %w", newRel, err)
	}

	if err := os.Rename(oldAbs, newAbs); err != nil {
		return FolderListingDTO{}, fmt.Errorf("services: rename %q -> %q: %w", oldRel, newRel, err)
	}

	// Rewrite every contained asset's stored path prefix in one transaction.
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, err := s.assets.WithTx(tx).UpdateArchivePathPrefix(ctx, oldRel, newRel)
		return err
	})
	if txErr != nil {
		// Roll the directory back so disk and catalog stay consistent.
		if rbErr := os.Rename(newAbs, oldAbs); rbErr != nil {
			s.log.Error("rename: DB update failed AND rename-back failed (manual repair needed)",
				"from", oldRel, "to", newRel, "dbError", txErr.Error(), "rollbackError", rbErr.Error())
			return FolderListingDTO{}, fmt.Errorf("services: rename %q: db update failed (%v) and rollback failed (%v)", oldRel, txErr, rbErr)
		}
		s.log.Warn("rename: DB update failed, rolled directory back", "from", oldRel, "to", newRel, "error", txErr.Error())
		return FolderListingDTO{}, fmt.Errorf("services: rename %q: db update failed, rolled back: %w", oldRel, txErr)
	}

	s.log.Info("renamed event folder", "from", oldRel, "to", newRel)
	return s.ListFolder(ctx, newRel, 1, folderRenamePageSize)
}

// folderRenamePageSize is the first-page size the rename/no-op paths use when
// returning the refreshed listing.
const folderRenamePageSize = 60

// cleanRelDir normalizes a caller-supplied relative directory to a
// forward-slash, root-relative, breadcrumb-safe path, rejecting any attempt to
// escape the root. "" and "." both normalize to "" (the library root).
func cleanRelDir(rel string) (string, error) {
	rel = strings.TrimSpace(filepath.ToSlash(rel))
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return "", nil
	}
	cleaned := path.Clean(rel)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("services: invalid folder path %q", rel)
	}
	return cleaned, nil
}

// folderLabel returns the human label of a date-event folder relDir (the text
// after "YYYY-MM-DD "), or "" for a bare date folder or a non-date folder.
func folderLabel(relDir string) string {
	if !dateFolderRe.MatchString(relDir) {
		return ""
	}
	base := path.Base(relDir)
	if len(base) > 10 {
		return strings.TrimSpace(base[10:])
	}
	return ""
}

// underRoot reports whether abs is root itself or lies within it.
func underRoot(root, abs string) bool {
	if root == "" {
		return true // dev escape hatch (absolute paths, no root)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// BrowseFilters are the browser grid's optional filters. Empty strings mean "no
// filter". YearMonth is "2006-01" (capture month).
type BrowseFilters struct {
	Query              string `json:"query"`
	MediaType          string `json:"mediaType"`
	VerificationStatus string `json:"verificationStatus"`
	BackupStatus       string `json:"backupStatus"`
	SessionID          string `json:"sessionId"`
	YearMonth          string `json:"yearMonth"`
}

// BrowseAssetDTO is the slim per-tile projection for the grid. It carries only
// what a tile and its badges need; full provenance comes from AssetDetail.
type BrowseAssetDTO struct {
	ID                 string     `json:"id"`
	Filename           string     `json:"filename"`
	CaptureDate        *time.Time `json:"captureDate"`
	MediaType          string     `json:"mediaType"`
	FileSize           int64      `json:"fileSize"`
	Width              int        `json:"width"`
	Height             int        `json:"height"`
	DurationSeconds    float64    `json:"durationSeconds"`
	VerificationStatus string     `json:"verificationStatus"`
	BackupStatus       string     `json:"backupStatus"`
	IsLivePhotoPair    bool       `json:"isLivePhotoPair"`
	DuplicateOfAssetID string     `json:"duplicateOfAssetId"`
	HasArchiveCopy     bool       `json:"hasArchiveCopy"`
}

// AssetRefDTO is a slim reference to a related asset (duplicate/original/Live
// Photo partner) used for in-drawer navigation.
type AssetRefDTO struct {
	ID          string     `json:"id"`
	Filename    string     `json:"filename"`
	MediaType   string     `json:"mediaType"`
	CaptureDate *time.Time `json:"captureDate"`
}

// BackupJobRefDTO is the compact per-asset backup-job view in the detail drawer.
type BackupJobRefDTO struct {
	Plugin      string     `json:"plugin"`
	Destination string     `json:"destination"`
	Status      string     `json:"status"`
	CompletedAt *time.Time `json:"completedAt"`
}

// AssetDetailDTO is the full provenance record shown in the detail drawer.
type AssetDetailDTO struct {
	ID                 string     `json:"id"`
	OriginalFilename   string     `json:"originalFilename"`
	OriginalExtension  string     `json:"originalExtension"`
	OriginalFullPath   string     `json:"originalFullPath"`
	CurrentArchivePath string     `json:"currentArchivePath"` // resolved absolute
	QuickHash          string     `json:"quickHash"`
	FullHash           string     `json:"fullHash"`
	FileSize           int64      `json:"fileSize"`
	CaptureDate        *time.Time `json:"captureDate"`
	ImportDate         time.Time  `json:"importDate"`
	MediaType          string     `json:"mediaType"`
	Width              int        `json:"width"`
	Height             int        `json:"height"`
	DurationSeconds    float64    `json:"durationSeconds"`
	CameraMake         string     `json:"cameraMake"`
	CameraModel        string     `json:"cameraModel"`
	Lens               string     `json:"lens"`
	ISO                int        `json:"iso"`
	ShutterSpeed       string     `json:"shutterSpeed"`
	Aperture           string     `json:"aperture"`
	GPSLatitude        *float64   `json:"gpsLatitude"`
	GPSLongitude       *float64   `json:"gpsLongitude"`
	VerificationStatus string     `json:"verificationStatus"`
	BackupStatus       string     `json:"backupStatus"`
	IsLivePhotoPair    bool       `json:"isLivePhotoPair"`

	SourceID    string     `json:"sourceId"`
	SourceLabel string     `json:"sourceLabel"`
	SourceType  string     `json:"sourceType"`
	SessionID   string     `json:"sessionId"`
	SessionDate *time.Time `json:"sessionDate"`

	BackupJobs       []BackupJobRefDTO `json:"backupJobs"`
	DuplicateOf      *AssetRefDTO      `json:"duplicateOf"`
	Duplicates       []AssetRefDTO     `json:"duplicates"`
	LivePhotoPartner *AssetRefDTO      `json:"livePhotoPartner"`
}

// ListAssets returns a page of slim asset tiles matching filters, sorted
// capture-date DESC (assets without a capture date sort last). Total is the true
// match count so the grid can page correctly.
func (s *BrowserService) ListAssets(ctx context.Context, filters BrowseFilters, page, pageSize int) (PageResult[BrowseAssetDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[BrowseAssetDTO]{}, err
	}
	limit, offset := normalizePage(page, pageSize)
	q := repo.AssetQuery{
		MediaType:          mediaTypeFilter(filters.MediaType),
		VerificationStatus: verificationFilter(filters.VerificationStatus),
		BackupStatus:       backupFilter(filters.BackupStatus),
		SessionID:          filters.SessionID,
		Text:               filters.Query,
		YearMonth:          filters.YearMonth,
		Page:               repo.Page{Limit: limit, Offset: offset},
	}
	rows, total, err := s.assets.List(ctx, q)
	if err != nil {
		return PageResult[BrowseAssetDTO]{}, err
	}
	items := make([]BrowseAssetDTO, 0, len(rows))
	for _, a := range rows {
		items = append(items, toBrowseAssetDTO(a))
	}
	return PageResult[BrowseAssetDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// Months returns distinct capture months with counts (newest first) for the
// filter dropdown and grid section headers.
func (s *BrowserService) Months(ctx context.Context) ([]MonthCountDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	months, err := s.assets.CaptureMonths(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MonthCountDTO, 0, len(months))
	for _, m := range months {
		out = append(out, MonthCountDTO{Month: m.Month, Count: m.Count})
	}
	return out, nil
}

// AssetDetail returns the full provenance record for one asset: original
// identity, resolved archive path, both hashes, all camera/exposure/GPS
// metadata, its source and session, its per-asset backup jobs, and its duplicate
// and Live Photo relationships (as slim refs for in-drawer navigation).
func (s *BrowserService) AssetDetail(ctx context.Context, id string) (AssetDetailDTO, error) {
	if err := s.guard(); err != nil {
		return AssetDetailDTO{}, err
	}
	a, err := s.assets.GetByID(ctx, id)
	if err != nil {
		return AssetDetailDTO{}, err
	}

	d := AssetDetailDTO{
		ID:                 a.ID,
		OriginalFilename:   a.OriginalFilename,
		OriginalExtension:  a.OriginalExtension,
		OriginalFullPath:   a.OriginalFullPath,
		CurrentArchivePath: library.ResolvePath(s.root, a.CurrentArchivePath),
		QuickHash:          a.QuickHash,
		FullHash:           a.FullHash,
		FileSize:           a.FileSize,
		CaptureDate:        a.CaptureDate,
		ImportDate:         a.ImportDate,
		MediaType:          string(a.MediaType),
		Width:              a.Width,
		Height:             a.Height,
		DurationSeconds:    a.DurationSeconds,
		CameraMake:         a.CameraMake,
		CameraModel:        a.CameraModel,
		Lens:               a.Lens,
		ISO:                a.ISO,
		ShutterSpeed:       a.ShutterSpeed,
		Aperture:           a.Aperture,
		GPSLatitude:        a.GPSLatitude,
		GPSLongitude:       a.GPSLongitude,
		VerificationStatus: string(a.VerificationStatus),
		BackupStatus:       string(a.BackupStatus),
		IsLivePhotoPair:    isLivePhotoPair(*a),
		SourceID:           a.SourceID,
		SessionID:          a.SessionID,
	}

	// Source (best-effort join): label prefers volume label, then model, then ID.
	if a.SourceID != "" && s.sources != nil {
		if src, err := s.sources.GetByID(ctx, a.SourceID); err == nil {
			d.SourceLabel = sourceLabel(src)
			d.SourceType = string(src.SourceType)
		} else if !errors.Is(err, repo.ErrNotFound) {
			return AssetDetailDTO{}, err
		}
	}

	// Session date (best-effort).
	if a.SessionID != "" && s.sessions != nil {
		if sess, err := s.sessions.GetByID(ctx, a.SessionID); err == nil {
			started := sess.StartedAt
			d.SessionDate = &started
		} else if !errors.Is(err, repo.ErrNotFound) {
			return AssetDetailDTO{}, err
		}
	}

	// Per-asset backup jobs (newest first).
	var jobs []domain.BackupJob
	if err := s.db.WithContext(ctx).Where("asset_id = ?", a.ID).
		Order("created_at DESC").Find(&jobs).Error; err != nil {
		return AssetDetailDTO{}, err
	}
	d.BackupJobs = make([]BackupJobRefDTO, 0, len(jobs))
	for _, j := range jobs {
		d.BackupJobs = append(d.BackupJobs, BackupJobRefDTO{
			Plugin:      j.Plugin,
			Destination: j.Destination,
			Status:      string(j.Status),
			CompletedAt: j.CompletedAt,
		})
	}

	// The original this asset duplicates.
	if a.DuplicateOfAssetID != nil && *a.DuplicateOfAssetID != "" {
		if orig, err := s.assets.GetByID(ctx, *a.DuplicateOfAssetID); err == nil {
			d.DuplicateOf = toAssetRefDTO(*orig)
		}
	}

	// Assets that duplicate THIS one.
	var dups []domain.Asset
	if err := s.db.WithContext(ctx).Where("duplicate_of_asset_id = ?", a.ID).
		Order("created_at DESC").Find(&dups).Error; err != nil {
		return AssetDetailDTO{}, err
	}
	d.Duplicates = make([]AssetRefDTO, 0, len(dups))
	for _, du := range dups {
		if ref := toAssetRefDTO(du); ref != nil {
			d.Duplicates = append(d.Duplicates, *ref)
		}
	}

	// Live Photo partner.
	if a.LivePhotoPartnerID != nil && *a.LivePhotoPartnerID != "" {
		if partner, err := s.assets.GetByID(ctx, *a.LivePhotoPartnerID); err == nil {
			d.LivePhotoPartner = toAssetRefDTO(*partner)
		}
	}

	return d, nil
}

// toBrowseAssetDTO projects a domain.Asset to the slim grid DTO.
func toBrowseAssetDTO(a domain.Asset) BrowseAssetDTO {
	dup := ""
	if a.DuplicateOfAssetID != nil {
		dup = *a.DuplicateOfAssetID
	}
	return BrowseAssetDTO{
		ID:                 a.ID,
		Filename:           a.OriginalFilename,
		CaptureDate:        a.CaptureDate,
		MediaType:          string(a.MediaType),
		FileSize:           a.FileSize,
		Width:              a.Width,
		Height:             a.Height,
		DurationSeconds:    a.DurationSeconds,
		VerificationStatus: string(a.VerificationStatus),
		BackupStatus:       string(a.BackupStatus),
		IsLivePhotoPair:    isLivePhotoPair(a),
		DuplicateOfAssetID: dup,
		HasArchiveCopy:     a.CurrentArchivePath != "",
	}
}

// toAssetRefDTO projects a domain.Asset to a slim reference.
func toAssetRefDTO(a domain.Asset) *AssetRefDTO {
	return &AssetRefDTO{
		ID:          a.ID,
		Filename:    a.OriginalFilename,
		MediaType:   string(a.MediaType),
		CaptureDate: a.CaptureDate,
	}
}

// isLivePhotoPair reports whether an asset represents (part of) a Live Photo.
func isLivePhotoPair(a domain.Asset) bool {
	return a.MediaType == domain.MediaTypeLivePhotoPair ||
		(a.LivePhotoPartnerID != nil && *a.LivePhotoPartnerID != "")
}

// sourceLabel derives the most human-friendly label for a source.
func sourceLabel(src *domain.ImportSource) string {
	switch {
	case src.VolumeLabel != "":
		return src.VolumeLabel
	case src.Model != "":
		return src.Model
	case src.Manufacturer != "":
		return src.Manufacturer
	default:
		return src.ID
	}
}

// mediaTypeFilter maps a filter string to a repo pointer filter (nil = no filter).
func mediaTypeFilter(v string) *domain.MediaType {
	if v == "" {
		return nil
	}
	mt := domain.MediaType(v)
	return &mt
}

func verificationFilter(v string) *domain.VerificationStatus {
	if v == "" {
		return nil
	}
	vs := domain.VerificationStatus(v)
	return &vs
}

func backupFilter(v string) *domain.BackupStatus {
	if v == "" {
		return nil
	}
	bs := domain.BackupStatus(v)
	return &bs
}
