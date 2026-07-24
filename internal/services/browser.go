package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
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
	// jobs is the backup queue repo, used by QueueAssetsForProvider to create
	// fresh pending jobs (via the exported idempotent Enqueue). Bound in place.
	jobs *repo.BackupRepo
	log  *slog.Logger
	// root resolves stored (relative) archive paths to absolute for display.
	root string
	// reveal opens a resolved path in Finder; injectable for tests.
	reveal revealRunner
	// activity, when set, lets RenameEventFolder refuse while any long operation
	// is running. Nil (unit tests) means "never busy".
	activity activitySnapshotter
	// emitter, when set, delivers backup:queue-changed after QueueAssetsForProvider
	// creates or un-skips jobs so the Backup Queue and provider cards refresh. Nil
	// (unit tests) is tolerated by emitSafe.
	emitter Emitter
}

// SetActivity injects the shared activity tracker so RenameEventFolder can refuse
// while any long operation is in flight. Called once by main.go.
func (s *BrowserService) SetActivity(a activitySnapshotter) { s.activity = a }

// SetEmitter injects the shared event emitter so the coverage view's bulk queue
// action can emit backup:queue-changed. Called once by main.go (mirrors how the
// backup services receive their emitter at construction).
func (s *BrowserService) SetEmitter(e Emitter) { s.emitter = e }

// Bind wires the BrowserService to an open library's catalog in place.
func (s *BrowserService) Bind(core *AppCore) {
	s.db = core.DB
	s.assets = core.Assets
	s.sources = core.Sources
	s.sessions = core.Sessions
	s.jobs = core.Backups
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
// representative cover asset id (for a thumbnail), the newest effective capture
// date anywhere beneath it (nullable ISO string; capture date, or import date as
// fallback), and whether it is a renameable date-event folder.
type FolderEntryDTO struct {
	Name          string     `json:"name"`
	RelPath       string     `json:"relPath"`
	AssetCount    int64      `json:"assetCount"`
	CoverAssetID  string     `json:"coverAssetId"`
	NewestCapture *time.Time `json:"newestCapture"`
	IsDateFolder  bool       `json:"isDateFolder"`
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

// normalizeFolderSort validates the ListFolder sort parameters, mapping any
// unrecognized value to the default (date/desc — newest first). sortBy is
// "name" or "date"; sortDir is "asc" or "desc". The Items column is a
// folder-only, client-side ordering and never reaches the server, so it is not a
// valid sortBy here.
func normalizeFolderSort(sortBy, sortDir string) (by, dir string) {
	by = strings.ToLower(strings.TrimSpace(sortBy))
	if by != "name" && by != "date" {
		by = "date"
	}
	dir = strings.ToLower(strings.TrimSpace(sortDir))
	if dir != "asc" && dir != "desc" {
		dir = "desc"
	}
	return by, dir
}

// ListFolder returns one level of the archive tree derived from the catalog
// (never a filesystem walk): the immediate subfolders of relDir (each with a
// recursive asset count, cover thumbnail, and newest capture date) plus the
// assets sitting directly in relDir, paged. relDir is a root-relative
// forward-slash directory; "" lists the year folders at the root. The returned
// RelDir is the cleaned, breadcrumb-safe path. sortBy ("name"|"date") and
// sortDir ("asc"|"desc") order the directly-contained assets; unknown values
// default to date/desc (newest first). Subfolders are always returned in name
// order and re-sorted client-side.
func (s *BrowserService) ListFolder(ctx context.Context, relDir string, page, pageSize int, sortBy, sortDir string) (FolderListingDTO, error) {
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
			Name:          c.Name,
			RelPath:       relPath,
			AssetCount:    c.AssetCount,
			CoverAssetID:  c.CoverAssetID,
			NewestCapture: c.NewestCapture,
			IsDateFolder:  dateFolderRe.MatchString(relPath),
		})
	}

	by, dir := normalizeFolderSort(sortBy, sortDir)
	limit, offset := normalizePage(page, pageSize)
	rows, total, err := s.assets.FolderAssets(ctx, clean, repo.Page{Limit: limit, Offset: offset}, by, dir)
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

	year, base := path.Split(oldRel) // "2019/", "2019-06-12 Trip"
	datePart := base[:10]            // "2019-06-12" (regex guarantees length)
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
		return s.ListFolder(ctx, oldRel, 1, folderRenamePageSize, "date", "desc")
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
	return s.ListFolder(ctx, newRel, 1, folderRenamePageSize, "date", "desc")
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
// filter". YearMonth is "2006-01" (capture month). CaptureFrom/CaptureTo are
// inclusive ISO date/datetime bounds on the effective date
// (COALESCE(capture_date, import_date)); the frontend supplies whole-day/whole-
// year boundaries. CameraMake/CameraModel are exact-match camera identity. The
// month, the from/to range, and the camera are independent AND-ed predicates.
type BrowseFilters struct {
	Query              string `json:"query"`
	MediaType          string `json:"mediaType"`
	VerificationStatus string `json:"verificationStatus"`
	BackupStatus       string `json:"backupStatus"`
	SessionID          string `json:"sessionId"`
	YearMonth          string `json:"yearMonth"`
	CaptureFrom        string `json:"captureFrom"`
	CaptureTo          string `json:"captureTo"`
	CameraMake         string `json:"cameraMake"`
	CameraModel        string `json:"cameraModel"`
}

// filterTimeLayouts are the ISO forms the frontend may send for a date-range
// bound, tried in order. A bare date is treated as midnight UTC; a zoneless
// datetime as UTC — matching how capture dates are stored in tests and by the
// SQLite driver.
var filterTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// parseFilterTime parses a caller-supplied date-range bound. An empty string
// yields (nil, nil) — "no bound".
func parseFilterTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, layout := range filterTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("services: invalid date filter %q", s)
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
	from, err := parseFilterTime(filters.CaptureFrom)
	if err != nil {
		return PageResult[BrowseAssetDTO]{}, err
	}
	to, err := parseFilterTime(filters.CaptureTo)
	if err != nil {
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
		CaptureFrom:        from,
		CaptureTo:          to,
		CameraMake:         filters.CameraMake,
		CameraModel:        filters.CameraModel,
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

/* -------------------------------------------------------------------------- */
/* Backup Coverage                                                            */
/* -------------------------------------------------------------------------- */

// Coverage status vocabulary — one asset's backup state for one destination.
// These strings are shared with the frontend chips and are identical to the
// repo's ProviderJobMatch values so the per-provider status filter passes
// straight through.
const (
	// CoverageStatusVerified: a completed job on a non-mirror destination with no
	// verify-unavailable note — an independently proven backup.
	CoverageStatusVerified = "verified"
	// CoverageStatusUploadedUnverified: a completed job that PAIM cannot prove
	// (a mirror destination, or a non-mirror destination whose Verify was
	// unavailable and recorded a note). The upload happened; verification did not.
	CoverageStatusUploadedUnverified = "uploaded_unverified"
	CoverageStatusPending            = "pending"   // pending or paused (queued, not yet uploaded)
	CoverageStatusRunning            = "running"   // uploading now
	CoverageStatusFailed             = "failed"    // last attempt failed
	CoverageStatusSkipped            = "skipped"   // opted out by choice (opted_out)
	CoverageStatusCancelled          = "cancelled" // cancelled
	CoverageStatusNone               = "none"      // no job for this destination
)

// ProviderCoverageDTO is one asset's backup state for one destination in the
// coverage table: the coverage status, when the job completed (nil unless it
// completed), and a note (a verify-unavailable explanation on an unverifiable
// destination, or the error message on a failure; empty otherwise).
type ProviderCoverageDTO struct {
	ProviderID  string     `json:"providerId"`
	Status      string     `json:"status"`
	CompletedAt *time.Time `json:"completedAt"`
	Note        string     `json:"note"`
}

// CoverageRowDTO is one asset row of the Backup Coverage table: its identity and
// provenance (name, resolved archive location, where it came from, taken/imported
// dates) plus its per-destination backup state. Providers carries ONLY the
// destinations this asset actually has a job for; a column in the table with no
// matching entry renders as "none".
type CoverageRowDTO struct {
	AssetID        string                `json:"assetId"`
	Filename       string                `json:"filename"`
	ArchivePath    string                `json:"archivePath"` // resolved absolute
	SourceLabel    string                `json:"sourceLabel"`
	CaptureDate    *time.Time            `json:"captureDate"`
	ImportDate     time.Time             `json:"importDate"`
	MediaType      string                `json:"mediaType"`
	HasArchiveCopy bool                  `json:"hasArchiveCopy"`
	Providers      []ProviderCoverageDTO `json:"providers"`
}

// CoverageProviderDTO describes one column of the coverage table: a destination
// ever referenced by a backup job. It survives provider removal (Removed=true,
// named from the soft-deleted row) so the table stays honest about where past
// backups went. Mirror marks a destination whose completed jobs are
// uploaded-but-unverifiable.
type CoverageProviderDTO struct {
	ProviderID string `json:"providerId"`
	Name       string `json:"name"`
	Mirror     bool   `json:"mirror"`
	Removed    bool   `json:"removed"`
}

// CoverageProviderFilter is the optional per-provider status filter for
// ListCoverage: restrict to assets whose backup state for ProviderID matches
// Status (a coverage status string). A nil pointer (or empty fields) applies no
// provider filter.
type CoverageProviderFilter struct {
	ProviderID string `json:"providerId"`
	Status     string `json:"status"`
}

// coverageProviderMeta is the resolved display metadata for one destination.
type coverageProviderMeta struct {
	name    string
	mirror  bool
	removed bool
}

// coverageQueueCap bounds one QueueAssetsForProvider call so a runaway
// select-all can never queue an unbounded set in a single request.
const coverageQueueCap = 10000

// coverageQueueBatch is how many asset IDs QueueAssetsForProvider processes per
// transaction (bounded IN-clause size, one commit per batch).
const coverageQueueBatch = 500

// ListCoverage returns a page of the Backup Coverage table: one row per asset
// (matching the same Batch-B filters ListAssets accepts) with its provenance and
// its per-destination backup state. When providerStatus is set, the page is
// restricted to assets whose backup state for that destination matches the given
// coverage status (composed as an EXISTS/NOT EXISTS predicate on the jobs table,
// AND-ed with every other filter).
//
// Query shape / scale: the page of assets is fetched first via AssetRepo.List
// (the same filtered, capture-date-ordered, indexed query the grid uses). Then a
// SINGLE query loads every non-deleted backup job for that page's asset IDs (an
// IN clause over the indexed asset_id), grouped in Go into asset -> destination
// -> newest job. Provider metadata (mirror/name/removed) is resolved with one
// Unscoped query over the destinations seen on the page, and per-row source
// labels are memoized by session. So a page costs O(1) queries regardless of page
// size — no per-asset or per-provider round-trips in the row loop.
func (s *BrowserService) ListCoverage(ctx context.Context, filters BrowseFilters, providerStatus *CoverageProviderFilter, page, pageSize int) (PageResult[CoverageRowDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[CoverageRowDTO]{}, err
	}
	from, err := parseFilterTime(filters.CaptureFrom)
	if err != nil {
		return PageResult[CoverageRowDTO]{}, err
	}
	to, err := parseFilterTime(filters.CaptureTo)
	if err != nil {
		return PageResult[CoverageRowDTO]{}, err
	}
	limit, offset := normalizePage(page, pageSize)
	q := repo.AssetQuery{
		MediaType:          mediaTypeFilter(filters.MediaType),
		VerificationStatus: verificationFilter(filters.VerificationStatus),
		BackupStatus:       backupFilter(filters.BackupStatus),
		SessionID:          filters.SessionID,
		Text:               filters.Query,
		YearMonth:          filters.YearMonth,
		CaptureFrom:        from,
		CaptureTo:          to,
		CameraMake:         filters.CameraMake,
		CameraModel:        filters.CameraModel,
		Page:               repo.Page{Limit: limit, Offset: offset},
	}
	// Provider-status filter: the destination's mirror flag decides how a completed
	// job is classified (verified vs uploaded_unverified), so resolve it Unscoped —
	// a removed provider is still a valid filter target.
	if providerStatus != nil && providerStatus.ProviderID != "" && providerStatus.Status != "" {
		mirror := false
		var p domain.BackupProvider
		err := s.db.WithContext(ctx).Unscoped().First(&p, "id = ?", providerStatus.ProviderID).Error
		if err == nil {
			mirror = p.Mirror
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return PageResult[CoverageRowDTO]{}, fmt.Errorf("services: coverage filter provider %q: %w", providerStatus.ProviderID, err)
		}
		q.ProviderJob = &repo.ProviderJobFilter{
			Destination: providerStatus.ProviderID,
			Match:       repo.ProviderJobMatch(providerStatus.Status),
			Mirror:      mirror,
		}
	}

	rows, total, err := s.assets.List(ctx, q)
	if err != nil {
		return PageResult[CoverageRowDTO]{}, err
	}

	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	newestByAsset, dests, err := s.coverageJobsForAssets(ctx, ids)
	if err != nil {
		return PageResult[CoverageRowDTO]{}, err
	}
	meta, err := s.coverageProviderMetaMap(ctx, dests)
	if err != nil {
		return PageResult[CoverageRowDTO]{}, err
	}

	sessLabels := make(map[string]string)
	items := make([]CoverageRowDTO, 0, len(rows))
	for i := range rows {
		a := rows[i]
		byDest := newestByAsset[a.ID]
		provs := make([]ProviderCoverageDTO, 0, len(byDest))
		for dest, job := range byDest {
			m := meta[dest]
			provs = append(provs, ProviderCoverageDTO{
				ProviderID:  dest,
				Status:      coverageStatusForJob(job, m.mirror),
				CompletedAt: job.CompletedAt,
				Note:        job.ErrorMessage,
			})
		}
		// Stable order so the frontend's per-column lookup is deterministic.
		sort.Slice(provs, func(i, j int) bool { return provs[i].ProviderID < provs[j].ProviderID })
		items = append(items, CoverageRowDTO{
			AssetID:        a.ID,
			Filename:       a.OriginalFilename,
			ArchivePath:    library.ResolvePath(s.root, a.CurrentArchivePath),
			SourceLabel:    s.coverageSourceLabel(ctx, a, sessLabels),
			CaptureDate:    a.CaptureDate,
			ImportDate:     a.ImportDate,
			MediaType:      string(a.MediaType),
			HasArchiveCopy: a.CurrentArchivePath != "",
			Providers:      provs,
		})
	}
	return PageResult[CoverageRowDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// coverageJobsForAssets loads every non-deleted backup job for the given asset
// IDs in one query and folds them into asset -> destination -> NEWEST job, plus
// the distinct set of destinations seen. Jobs are ordered oldest-first so a later
// map assignment overwrites with the newest job per (asset, destination).
func (s *BrowserService) coverageJobsForAssets(ctx context.Context, assetIDs []string) (map[string]map[string]domain.BackupJob, []string, error) {
	byAsset := make(map[string]map[string]domain.BackupJob, len(assetIDs))
	if len(assetIDs) == 0 {
		return byAsset, nil, nil
	}
	var jobs []domain.BackupJob
	if err := s.db.WithContext(ctx).
		Where("asset_id IN ?", assetIDs).
		Order("created_at ASC, id ASC").
		Find(&jobs).Error; err != nil {
		return nil, nil, fmt.Errorf("services: coverage jobs: %w", err)
	}
	destSeen := make(map[string]struct{})
	for i := range jobs {
		j := jobs[i]
		m := byAsset[j.AssetID]
		if m == nil {
			m = make(map[string]domain.BackupJob)
			byAsset[j.AssetID] = m
		}
		m[j.Destination] = j
		destSeen[j.Destination] = struct{}{}
	}
	dests := make([]string, 0, len(destSeen))
	for d := range destSeen {
		dests = append(dests, d)
	}
	return byAsset, dests, nil
}

// coverageProviderMetaMap resolves display metadata for a set of destination IDs
// with ONE Unscoped query (soft-deleted providers included so removed
// destinations still get a name and are flagged Removed). A destination with no
// provider row at all is simply absent from the map; callers fall back to the raw
// ID and treat it as removed.
func (s *BrowserService) coverageProviderMetaMap(ctx context.Context, ids []string) (map[string]coverageProviderMeta, error) {
	out := make(map[string]coverageProviderMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var provs []domain.BackupProvider
	if err := s.db.WithContext(ctx).Unscoped().Where("id IN ?", ids).Find(&provs).Error; err != nil {
		return nil, fmt.Errorf("services: coverage provider meta: %w", err)
	}
	for _, p := range provs {
		out[p.ID] = coverageProviderMeta{
			name:    coverageProviderName(p),
			mirror:  p.Mirror,
			removed: p.DeletedAt.Valid,
		}
	}
	return out, nil
}

// CoverageProviders lists every destination ever referenced by a backup job
// (including removed providers, resolved Unscoped) for the coverage table's
// column set and the per-provider status filter dropdown. Live destinations sort
// first, then removed ones; within each group, by name — so the primary columns
// are the destinations still in use.
func (s *BrowserService) CoverageProviders(ctx context.Context) ([]CoverageProviderDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var dests []string
	if err := s.db.WithContext(ctx).
		Model(&domain.BackupJob{}).
		Distinct().
		Where("deleted_at IS NULL AND destination <> ''").
		Order("destination").
		Pluck("destination", &dests).Error; err != nil {
		return nil, fmt.Errorf("services: coverage providers: %w", err)
	}
	meta, err := s.coverageProviderMetaMap(ctx, dests)
	if err != nil {
		return nil, err
	}
	out := make([]CoverageProviderDTO, 0, len(dests))
	for _, d := range dests {
		if m, ok := meta[d]; ok {
			out = append(out, CoverageProviderDTO{ProviderID: d, Name: m.name, Mirror: m.mirror, Removed: m.removed})
			continue
		}
		// No provider row at all: name by ID and mark removed so the column is honest.
		out = append(out, CoverageProviderDTO{ProviderID: d, Name: d, Removed: true})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Removed != out[j].Removed {
			return !out[i].Removed // live providers first
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// QueueAssetsForProvider queues the given assets for one destination, returning
// how many were newly queued. Per asset: an existing opted_out job for the
// destination is un-skipped (opted_out -> pending); an asset with NO job for the
// destination gets a fresh pending job; an asset that already has a
// pending/running/completed job is left untouched (and not counted). It emits
// backup:queue-changed when anything was queued. assetIds is capped at
// coverageQueueCap per call.
//
// Seam decision: the fresh-enqueue path reuses the exported, idempotent
// BackupRepo.Enqueue (owned elsewhere; called, not modified). There is no
// asset-scoped un-skip primitive — RequeueOptedOut is provider/session-scoped —
// so the opted_out -> pending flip is a guarded direct update in this file
// (WHERE destination = ? AND status = opted_out AND asset_id IN ?), mirroring
// RequeueOptedOut's transition exactly. TODO: consolidate the asset-scoped
// requeue into repo/backup.go once ownership allows.
func (s *BrowserService) QueueAssetsForProvider(ctx context.Context, providerID string, assetIds []string) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	if providerID == "" {
		return 0, fmt.Errorf("services: queue for provider: empty provider id")
	}
	if len(assetIds) == 0 {
		return 0, nil
	}
	if len(assetIds) > coverageQueueCap {
		return 0, fmt.Errorf("services: queue for provider: too many assets (%d > %d)", len(assetIds), coverageQueueCap)
	}
	// The destination must be a live (non-deleted) provider: queueing to a removed
	// destination would create unrunnable work. Its plugin name stamps new jobs.
	var provider domain.BackupProvider
	if err := s.db.WithContext(ctx).First(&provider, "id = ?", providerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("services: queue for provider: provider %q not found", providerID)
		}
		return 0, fmt.Errorf("services: queue for provider: load provider %q: %w", providerID, err)
	}

	queued := 0
	for start := 0; start < len(assetIds); start += coverageQueueBatch {
		end := start + coverageQueueBatch
		if end > len(assetIds) {
			end = len(assetIds)
		}
		n, err := s.queueBatch(ctx, provider, assetIds[start:end])
		if err != nil {
			return queued, err
		}
		queued += n
	}

	if queued > 0 && s.jobs != nil {
		if counts, err := s.jobs.QueueSummary(ctx); err == nil {
			statuses := make([]domain.JobStatus, len(counts))
			values := make([]int64, len(counts))
			for i, c := range counts {
				statuses[i] = c.Status
				values[i] = c.Count
			}
			emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summaryFromCounts(statuses, values)})
		}
	}
	s.log.Info("queued assets for provider", "provider", providerID, "requested", len(assetIds), "queued", queued)
	return queued, nil
}

// queueBatch queues one batch of assets for the provider inside a single
// transaction. It un-skips existing opted_out jobs (guarded update) and enqueues
// a fresh pending job for assets with none, stamping each with the same
// capture/import-date SortKey the importer uses so it honors the provider's
// upload order.
func (s *BrowserService) queueBatch(ctx context.Context, provider domain.BackupProvider, ids []string) (int, error) {
	if s.jobs == nil {
		return 0, fmt.Errorf("services: queue batch: backup repo unbound")
	}
	queued := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) Which of these assets currently have an opted_out job for the dest?
		var optedIDs []string
		if err := tx.Model(&domain.BackupJob{}).
			Where("destination = ? AND status = ? AND asset_id IN ?", provider.ID, domain.JobStatusOptedOut, ids).
			Distinct().
			Pluck("asset_id", &optedIDs).Error; err != nil {
			return err
		}
		optedSet := make(map[string]struct{}, len(optedIDs))
		for _, id := range optedIDs {
			optedSet[id] = struct{}{}
		}
		// 2) Un-skip them (opted_out -> pending), guarded on the opted_out state.
		if len(optedIDs) > 0 {
			res := tx.Model(&domain.BackupJob{}).
				Where("destination = ? AND status = ? AND asset_id IN ?", provider.ID, domain.JobStatusOptedOut, optedIDs).
				Updates(map[string]any{
					"status":       domain.JobStatusPending,
					"started_at":   nil,
					"completed_at": nil,
				})
			if res.Error != nil {
				return res.Error
			}
			queued += int(res.RowsAffected)
		}
		// 3) Enqueue a fresh pending job for assets that were NOT opted out. Load
		//    them (existing rows only) to stamp the upload-order SortKey; Enqueue is
		//    idempotent, so an asset already pending/running/completed is a no-op.
		fresh := make([]string, 0, len(ids))
		for _, id := range ids {
			if _, skip := optedSet[id]; !skip {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) > 0 {
			var assets []domain.Asset
			if err := tx.Where("id IN ?", fresh).Find(&assets).Error; err != nil {
				return err
			}
			q := s.jobs.WithTx(tx)
			for i := range assets {
				a := assets[i]
				_, created, err := q.Enqueue(ctx, a.ID, provider.PluginName, provider.ID, backup.SortKeyForAsset(a))
				if err != nil {
					return err
				}
				if created {
					queued++
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("services: queue batch for provider %q: %w", provider.ID, err)
	}
	return queued, nil
}

// coverageStatusForJob maps a single backup job (the newest for its destination)
// to a coverage status. A completed job is verified only on a non-mirror
// destination with no verify-unavailable note; otherwise it is
// uploaded_unverified. Paused folds into pending (still queued); opted_out is
// skipped.
func coverageStatusForJob(j domain.BackupJob, mirror bool) string {
	switch j.Status {
	case domain.JobStatusCompleted:
		if mirror || j.ErrorMessage != "" {
			return CoverageStatusUploadedUnverified
		}
		return CoverageStatusVerified
	case domain.JobStatusPending, domain.JobStatusPaused:
		return CoverageStatusPending
	case domain.JobStatusRunning:
		return CoverageStatusRunning
	case domain.JobStatusFailed:
		return CoverageStatusFailed
	case domain.JobStatusOptedOut:
		return CoverageStatusSkipped
	case domain.JobStatusCancelled:
		return CoverageStatusCancelled
	default:
		return CoverageStatusNone
	}
}

// coverageProviderName derives a short human label for a destination: its rclone
// remote or localfs root from the config JSON, else the plugin name, else the raw
// ID. Best-effort — any parse failure degrades to the next fallback.
func coverageProviderName(p domain.BackupProvider) string {
	var cfg struct {
		Remote string `json:"remote"`
		Root   string `json:"root"`
	}
	if json.Unmarshal([]byte(p.ConfigJSON), &cfg) == nil {
		if cfg.Remote != "" {
			return cfg.Remote
		}
		if cfg.Root != "" {
			return cfg.Root
		}
	}
	if p.PluginName != "" {
		return p.PluginName
	}
	return p.ID
}

// coverageSourceLabel resolves "where this asset came from" via its session,
// memoized by session ID for the page. An asset with no session yields "".
func (s *BrowserService) coverageSourceLabel(ctx context.Context, a domain.Asset, cache map[string]string) string {
	if a.SessionID == "" {
		return ""
	}
	if lbl, ok := cache[a.SessionID]; ok {
		return lbl
	}
	lbl := s.coverageSessionSourceLabel(ctx, a.SessionID)
	cache[a.SessionID] = lbl
	return lbl
}

// coverageSessionSourceLabel names an import session's origin, reusing History's
// enrichment approach: a linked copy-mode source is named by its volume label +
// type; otherwise it falls back to "Library (adopt)" for adopt runs or the
// source-root folder's basename recorded in the session notes.
func (s *BrowserService) coverageSessionSourceLabel(ctx context.Context, sessionID string) string {
	if s.sessions == nil {
		return ""
	}
	sess, err := s.sessions.GetByID(ctx, sessionID)
	if err != nil || sess == nil {
		return ""
	}
	st, _ := decodeResumeState(sess.Notes)
	mode := "copy"
	if st.Mode != "" {
		mode = st.Mode
	}
	if sess.SourceID != "" && mode != string(importer.ModeAdopt) && s.sources != nil {
		if src, err := s.sources.GetByID(ctx, sess.SourceID); err == nil && src != nil {
			if lbl := coverageSourceDisplay(src); lbl != "" {
				return lbl
			}
		}
	}
	return defaultSourceLabel(mode, st.SourceRoot)
}

// coverageSourceDisplay formats a linked source as "<label> (<type>)", falling
// through volume label / model / manufacturer / serial for the label.
func coverageSourceDisplay(src *domain.ImportSource) string {
	label := firstNonEmptyStr(src.VolumeLabel, src.Model, src.Manufacturer, src.HardwareSerial)
	typ := string(src.SourceType)
	switch {
	case label != "" && typ != "":
		return fmt.Sprintf("%s (%s)", label, typ)
	case label != "":
		return label
	default:
		return typ
	}
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

// Years returns distinct capture years with counts (newest first) for the Date
// filter's year level. It is the CaptureMonths data rolled up to years, so years
// and months share one capture-date basis (undated assets excluded).
func (s *BrowserService) Years(ctx context.Context) ([]YearCountDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	months, err := s.assets.CaptureMonths(ctx)
	if err != nil {
		return nil, err
	}
	years := repo.RollupYears(months)
	out := make([]YearCountDTO, 0, len(years))
	for _, y := range years {
		out = append(out, YearCountDTO{Year: y.Year, Count: y.Count})
	}
	return out, nil
}

// Cameras returns the distinct cameras (make + model) present in the library
// with per-camera asset counts, most-used first, for the Camera filter dropdown.
// Label is the display "Make Model" (collapsed whitespace); the frontend filters
// on the exact Make/Model pair.
func (s *BrowserService) Cameras(ctx context.Context) ([]CameraCountDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	cams, err := s.assets.Cameras(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CameraCountDTO, 0, len(cams))
	for _, c := range cams {
		out = append(out, CameraCountDTO{
			Make:  c.Make,
			Model: c.Model,
			Label: strings.TrimSpace(c.Make + " " + c.Model),
			Count: c.Count,
		})
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
