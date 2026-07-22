package services

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

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
}

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
	return &BrowserService{db: db, assets: assets, sources: sources, sessions: sessions, log: logger.With(slog.String("subsystem", "browser"))}
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
