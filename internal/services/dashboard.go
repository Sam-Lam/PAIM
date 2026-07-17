package services

import (
	"context"
	"log/slog"

	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
	"gorm.io/gorm"
)

// DashboardService aggregates the headline metrics shown on the dashboard. It
// uses the repositories where they expose the needed rollups and falls back to
// direct GORM read-model queries for aggregates the repos do not cover
// (per-month growth, status counts, recent activity).
type DashboardService struct {
	db      *gorm.DB
	assets  *repo.AssetRepo
	backups *repo.BackupRepo
	sources *repo.SourceRepo
	log     *slog.Logger
}

// NewDashboardService constructs a DashboardService.
func NewDashboardService(db *gorm.DB, assets *repo.AssetRepo, backups *repo.BackupRepo, sources *repo.SourceRepo, logger *slog.Logger) *DashboardService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DashboardService{db: db, assets: assets, backups: backups, sources: sources, log: logger}
}

// TotalsDTO holds the library totals.
type TotalsDTO struct {
	Assets       int64 `json:"assets"`
	Photos       int64 `json:"photos"`
	Videos       int64 `json:"videos"`
	LivePhotos   int64 `json:"livePhotos"`
	StorageBytes int64 `json:"storageBytes"`
}

// MonthCountDTO is one point in the library growth series.
type MonthCountDTO struct {
	Month string `json:"month"` // YYYY-MM
	Count int64  `json:"count"`
}

// BackupSummaryDTO is the dashboard's compact backup-queue view.
type BackupSummaryDTO struct {
	Pending int64 `json:"pending"`
	Failed  int64 `json:"failed"`
}

// DashboardStats is the full dashboard payload.
type DashboardStats struct {
	Totals             TotalsDTO        `json:"totals"`
	LibraryGrowth      []MonthCountDTO  `json:"libraryGrowth"`
	PendingImports     int64            `json:"pendingImports"`
	BackupQueue        BackupSummaryDTO `json:"backupQueue"`
	DuplicateCount     int64            `json:"duplicateCount"`
	RecentSources      []SourceDTO      `json:"recentSources"`
	SafeToEraseSources []SourceDTO      `json:"safeToEraseSources"`
	RecentActivity     []LogEntryDTO    `json:"recentActivity"`
}

// GetStats computes the dashboard metrics.
func (s *DashboardService) GetStats(ctx context.Context) (DashboardStats, error) {
	var out DashboardStats

	// Totals by media type.
	counts, err := s.assets.CountsByMediaType(ctx)
	if err != nil {
		return DashboardStats{}, err
	}
	for _, c := range counts {
		out.Totals.Assets += c.Count
		switch c.MediaType {
		case domain.MediaTypePhoto, domain.MediaTypeRawPhoto:
			out.Totals.Photos += c.Count
		case domain.MediaTypeVideo:
			out.Totals.Videos += c.Count
		case domain.MediaTypeLivePhotoPair:
			out.Totals.LivePhotos += c.Count
		}
	}
	if out.Totals.StorageBytes, err = s.assets.TotalBytes(ctx); err != nil {
		return DashboardStats{}, err
	}

	if out.LibraryGrowth, err = s.libraryGrowth(ctx); err != nil {
		return DashboardStats{}, err
	}

	// Pending imports: sessions left interrupted or running (resumable).
	if err := s.db.WithContext(ctx).Model(&domain.ImportSession{}).
		Where("status IN ?", []domain.SessionStatus{domain.SessionStatusInterrupted, domain.SessionStatusRunning}).
		Count(&out.PendingImports).Error; err != nil {
		return DashboardStats{}, err
	}

	// Backup queue summary.
	summary, err := s.backups.QueueSummary(ctx)
	if err != nil {
		return DashboardStats{}, err
	}
	for _, c := range summary {
		switch c.Status {
		case domain.JobStatusPending:
			out.BackupQueue.Pending = c.Count
		case domain.JobStatusFailed:
			out.BackupQueue.Failed = c.Count
		}
	}

	// Duplicate count.
	if err := s.db.WithContext(ctx).Model(&domain.Asset{}).
		Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").
		Count(&out.DuplicateCount).Error; err != nil {
		return DashboardStats{}, err
	}

	// Recent sources.
	recent, err := s.sources.ListRecent(ctx, 8)
	if err != nil {
		return DashboardStats{}, err
	}
	out.RecentSources = make([]SourceDTO, 0, len(recent))
	for _, r := range recent {
		out.RecentSources = append(out.RecentSources, toSourceDTO(r))
	}

	// Safe-to-erase sources.
	var safe []domain.ImportSource
	if err := s.db.WithContext(ctx).Where("safe_to_erase = ?", true).
		Order("last_seen_at DESC").Limit(20).Find(&safe).Error; err != nil {
		return DashboardStats{}, err
	}
	out.SafeToEraseSources = make([]SourceDTO, 0, len(safe))
	for _, r := range safe {
		out.SafeToEraseSources = append(out.SafeToEraseSources, toSourceDTO(r))
	}

	// Recent activity: last 20 info+ entries from import/backup subsystems.
	var activity []domain.LogEntry
	if err := s.db.WithContext(ctx).Model(&domain.LogEntry{}).
		Where("subsystem IN ?", []string{"import", "backup"}).
		Where("level IN ?", []string{domain.LogLevelInfo, domain.LogLevelWarn, domain.LogLevelError}).
		Order("timestamp DESC, id DESC").Limit(20).Find(&activity).Error; err != nil {
		return DashboardStats{}, err
	}
	out.RecentActivity = make([]LogEntryDTO, 0, len(activity))
	for _, e := range activity {
		out.RecentActivity = append(out.RecentActivity, toLogEntryDTO(e))
	}

	return out, nil
}

// libraryGrowth returns per-month asset counts (by import date) for the last 12
// months, oldest first.
func (s *DashboardService) libraryGrowth(ctx context.Context) ([]MonthCountDTO, error) {
	var rows []MonthCountDTO
	err := s.db.WithContext(ctx).
		Model(&domain.Asset{}).
		Select("strftime('%Y-%m', import_date) as month, count(*) as count").
		Where("import_date >= date('now', '-12 months')").
		Group("month").
		Order("month ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
