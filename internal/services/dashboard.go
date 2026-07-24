package services

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// DashboardService aggregates the headline metrics shown on the dashboard. It
// uses the repositories where they expose the needed rollups and falls back to
// direct GORM read-model queries for aggregates the repos do not cover
// (per-month growth, status counts, recent activity).
type DashboardService struct {
	gated
	db      *gorm.DB
	assets  *repo.AssetRepo
	backups *repo.BackupRepo
	sources *repo.SourceRepo
	manager *backup.Manager
	log     *slog.Logger
}

// Bind wires the DashboardService to an open library's catalog in place.
func (s *DashboardService) Bind(core *AppCore) {
	s.db = core.DB
	s.assets = core.Assets
	s.backups = core.Backups
	s.sources = core.Sources
	s.manager = core.Manager
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

// MonthCountDTO pairs a capture month ("YYYY-MM") with an asset count. It backs
// the Library browser's month filter (BrowserService.Months).
type MonthCountDTO struct {
	Month string `json:"month"` // YYYY-MM
	Count int64  `json:"count"`
}

// YearCountDTO pairs a capture year ("YYYY") with an asset count. It backs the
// Library browser's Date filter year level (BrowserService.Years).
type YearCountDTO struct {
	Year  string `json:"year"` // YYYY
	Count int64  `json:"count"`
}

// CameraCountDTO is one distinct camera in the library with its asset count. It
// backs the Library browser's Camera filter (BrowserService.Cameras). Make and
// Model are the exact stored values the grid filters on; Label is the display
// "Make Model".
type CameraCountDTO struct {
	Make  string `json:"make"`
	Model string `json:"model"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// BackupSummaryDTO is the dashboard's compact backup-queue view. Pending/Failed
// are the HEADLINE numbers and count required (non-mirror) backups only, so a
// convenience mirror lagging behind never inflates the failure count. MirrorPending
// / MirrorFailed are a separate soft count the dashboard shows as "mirror uploads
// pending: N".
type BackupSummaryDTO struct {
	Pending       int64 `json:"pending"`
	Failed        int64 `json:"failed"`
	MirrorPending int64 `json:"mirrorPending"`
	MirrorFailed  int64 `json:"mirrorFailed"`

	// Live rate/ETA for the dashboard backup card ("11,402 pending → done
	// ~Thursday"). JobsPerMinute is the rolling completion rate; BytesRemaining is
	// the outstanding upload workload; EtaSeconds/EtaAt estimate when the whole
	// active queue (all providers, pending+running) drains; LastCompletedAt is the
	// most recent completion; Paused is true when backups are yielding to foreground
	// work or a provider is cooling (the card shows the paused state, not an ETA).
	JobsPerMinute   float64    `json:"jobsPerMinute"`
	BytesRemaining  int64      `json:"bytesRemaining"`
	EtaSeconds      int64      `json:"etaSeconds"`
	EtaAt           *time.Time `json:"etaAt"`
	LastCompletedAt *time.Time `json:"lastCompletedAt"`
	Paused          bool       `json:"paused"`
}

// DashboardStats is the full dashboard payload.
type DashboardStats struct {
	Totals         TotalsDTO        `json:"totals"`
	PendingImports int64            `json:"pendingImports"`
	BackupQueue    BackupSummaryDTO `json:"backupQueue"`
	// EnabledRequiredProviders is the number of enabled, non-mirror (required)
	// backup destinations configured. Zero while assets exist means the archive is
	// the only copy — the dashboard renders a prominent "no backup destination"
	// warning. Mirrors are excluded: a mirror-only setup still has no custody copy.
	EnabledRequiredProviders int64         `json:"enabledRequiredProviders"`
	DuplicateCount           int64         `json:"duplicateCount"`
	RecentSources            []SourceDTO   `json:"recentSources"`
	SafeToEraseSources       []SourceDTO   `json:"safeToEraseSources"`
	RecentActivity           []LogEntryDTO `json:"recentActivity"`
}

// GetStats computes the dashboard metrics.
func (s *DashboardService) GetStats(ctx context.Context) (DashboardStats, error) {
	if err := s.guard(); err != nil {
		return DashboardStats{}, err
	}
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

	// Pending imports: sessions left interrupted or running (resumable).
	if err := s.db.WithContext(ctx).Model(&domain.ImportSession{}).
		Where("status IN ?", []domain.SessionStatus{domain.SessionStatusInterrupted, domain.SessionStatusRunning}).
		Count(&out.PendingImports).Error; err != nil {
		return DashboardStats{}, err
	}

	// Backup queue summary, split so mirror (quality-of-life) providers do not
	// inflate the headline pending/failed numbers. Mirror jobs are counted
	// separately as a soft indicator.
	if out.BackupQueue, err = s.backupSummary(ctx); err != nil {
		return DashboardStats{}, err
	}
	s.fillBackupEta(ctx, &out.BackupQueue)

	// Enabled required (non-mirror) backup destinations. Zero with assets present
	// means the archive has only one copy — the dashboard warns prominently.
	if err := s.db.WithContext(ctx).Model(&domain.BackupProvider{}).
		Where("enabled = ? AND mirror = ?", true, false).
		Count(&out.EnabledRequiredProviders).Error; err != nil {
		return DashboardStats{}, err
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

// backupSummary computes the dashboard's backup counts, splitting mirror-provider
// jobs (a soft count) from required-provider jobs (the headline). A job maps to
// its provider by destination = provider ID.
func (s *DashboardService) backupSummary(ctx context.Context) (BackupSummaryDTO, error) {
	var mirrorIDs []string
	if err := s.db.WithContext(ctx).
		Model(&domain.BackupProvider{}).
		Where("mirror = ?", true).
		Pluck("id", &mirrorIDs).Error; err != nil {
		return BackupSummaryDTO{}, err
	}

	countJobs := func(status domain.JobStatus, mirror bool) (int64, error) {
		q := s.db.WithContext(ctx).Model(&domain.BackupJob{}).Where("status = ?", status)
		if len(mirrorIDs) == 0 {
			if mirror {
				return 0, nil // no mirror providers ⇒ no mirror jobs
			}
		} else if mirror {
			q = q.Where("destination IN ?", mirrorIDs)
		} else {
			q = q.Where("destination NOT IN ?", mirrorIDs)
		}
		var n int64
		return n, q.Count(&n).Error
	}

	var out BackupSummaryDTO
	var err error
	if out.Pending, err = countJobs(domain.JobStatusPending, false); err != nil {
		return BackupSummaryDTO{}, err
	}
	if out.Failed, err = countJobs(domain.JobStatusFailed, false); err != nil {
		return BackupSummaryDTO{}, err
	}
	if out.MirrorPending, err = countJobs(domain.JobStatusPending, true); err != nil {
		return BackupSummaryDTO{}, err
	}
	if out.MirrorFailed, err = countJobs(domain.JobStatusFailed, true); err != nil {
		return BackupSummaryDTO{}, err
	}
	return out, nil
}

// fillBackupEta populates the live rate/ETA fields on the dashboard backup card
// from the Manager's rolling completion stats and the repo's byte/queue
// aggregates, so the card can show "N pending → done ~Thursday". The ETA covers
// the whole active queue (all providers, pending+running); it is suppressed when
// backups are paused (yielding to foreground work) or a provider is cooling. A nil
// Manager (before a library binds) leaves the fields zero. Best-effort: any query
// error simply leaves the corresponding field unset.
func (s *DashboardService) fillBackupEta(ctx context.Context, sum *BackupSummaryDTO) {
	if s.manager == nil || s.backups == nil {
		return
	}
	counts, err := s.backups.QueueSummary(ctx)
	if err != nil {
		return
	}
	var pending, running int64
	for _, c := range counts {
		switch c.Status {
		case domain.JobStatusPending:
			pending = c.Count
		case domain.JobStatusRunning:
			running = c.Count
		}
	}
	jpm, last := s.manager.CompletionStats()
	sum.JobsPerMinute = jpm
	if !last.IsZero() {
		lc := last
		sum.LastCompletedAt = &lc
	}
	if br, berr := s.backups.BytesRemaining(ctx); berr == nil {
		sum.BytesRemaining = br
	}
	paused := s.manager.Yielding() || len(s.manager.Cooldowns()) > 0
	sum.Paused = paused
	if secs, at, ok := backupETA(time.Now(), pending+running, jpm, paused); ok {
		sum.EtaSeconds = secs
		ea := at
		sum.EtaAt = &ea
	}
}

// AssetsOverTime chart tuning constants.
const (
	// dayWindow caps the Day granularity to its most recent N buckets: a 20-year
	// library at day resolution is ~7,300 bars, so Day always windows.
	dayWindow = 120
	// maxBuckets is the hard ceiling on emitted bars. 20 years of months is 240,
	// so honest zero-filled axes stay well under it; the cap is a safety net that
	// trims to the most recent buckets (marking the result windowed) rather than
	// coarsening.
	maxBuckets = 400
)

// AssetsOverTimeBucketDTO is one bar of the assets-over-time chart: a labeled
// effective-capture-time bucket with its photo/video split.
type AssetsOverTimeBucketDTO struct {
	Label  string `json:"label"`  // human bucket label ("2026-07", "2020–2024", …)
	Start  string `json:"start"`  // ISO date (YYYY-MM-DD) at the bucket's start
	Photos int64  `json:"photos"` // photo + raw_photo + live_photo_pair
	Videos int64  `json:"videos"`
}

// AssetsOverTimeDTO is the capture-date distribution shown on the dashboard,
// bucketed at the resolved granularity. Buckets are keyed on
// COALESCE(capture_date, import_date), so assets with no capture date land in their
// import-date bucket — TotalUndatedFallback counts how many, surfaced as a footnote.
// Windowed is true when Buckets cover only part of [RangeStart, RangeEnd] (the Day
// view's most-recent-120-days window, or the maxBuckets safety trim); the UI shows
// the full range so the window is honest.
type AssetsOverTimeDTO struct {
	Granularity          string                    `json:"granularity"` // resolved concrete granularity
	Buckets              []AssetsOverTimeBucketDTO `json:"buckets"`
	Windowed             bool                      `json:"windowed"`
	RangeStart           string                    `json:"rangeStart"` // ISO date, "" when empty
	RangeEnd             string                    `json:"rangeEnd"`
	TotalUndatedFallback int64                     `json:"totalUndatedFallback"`
}

// AssetsOverTime returns the library's asset distribution over effective capture
// time, bucketed at the given granularity ("day"|"month"|"year"|"5year"|"all").
// "all" (and any unrecognized value) auto-picks from the data span; see
// resolveGranularity. Gaps between the earliest and latest bucket are zero-filled
// so the time axis is honest; the Day view windows to its most recent dayWindow
// buckets. Excludes soft-deleted assets.
func (s *DashboardService) AssetsOverTime(ctx context.Context, granularity string) (*AssetsOverTimeDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}

	minEff, maxEff, err := s.assets.EffectiveDateRange(ctx)
	if err != nil {
		return nil, err
	}
	undated, err := s.assets.CountUndatedFallback(ctx)
	if err != nil {
		return nil, err
	}

	out := &AssetsOverTimeDTO{
		Buckets:              []AssetsOverTimeBucketDTO{},
		TotalUndatedFallback: undated,
	}
	if minEff == nil || maxEff == nil {
		// Empty library: no range, no bars. Still echo a concrete granularity.
		out.Granularity = resolveGranularity(granularity, 0)
		return out, nil
	}
	out.RangeStart = minEff.Format("2006-01-02")
	out.RangeEnd = maxEff.Format("2006-01-02")

	gran := resolveGranularity(granularity, maxEff.Sub(*minEff))
	out.Granularity = gran

	raw, err := s.assets.AssetsByBucket(ctx, gran)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]repo.BucketCount, len(raw))
	for _, b := range raw {
		counts[b.Bucket] = b
	}

	// Build the zero-filled, ordered bucket axis over [start, end].
	start, end := *minEff, *maxEff
	if gran == "day" {
		if w := end.AddDate(0, 0, -(dayWindow - 1)); w.After(start) {
			start = w
			out.Windowed = true
		}
	}
	keys := bucketKeys(gran, start, end)
	if len(keys) > maxBuckets {
		keys = keys[len(keys)-maxBuckets:]
		out.Windowed = true
	}
	for _, k := range keys {
		bc := counts[k.key]
		out.Buckets = append(out.Buckets, AssetsOverTimeBucketDTO{
			Label:  k.label,
			Start:  k.start.Format("2006-01-02"),
			Photos: bc.Photos,
			Videos: bc.Videos,
		})
	}
	return out, nil
}

// resolveGranularity maps a requested granularity to a concrete one. A recognized
// concrete value passes through; "all" (and anything else) auto-picks from the data
// span:
//   - ≤ 3 months  -> day
//   - ≤ 3 years   -> month
//   - ≤ 15 years  -> year
//   - otherwise   -> 5year
//
// Thresholds are expressed in days (≈ the calendar spans) so the boundaries are
// stable and cheap to compare.
func resolveGranularity(requested string, span time.Duration) string {
	switch requested {
	case "day", "month", "year", "5year":
		return requested
	}
	days := span.Hours() / 24
	switch {
	case days <= 92: // ~3 months
		return "day"
	case days <= 1096: // ~3 years
		return "month"
	case days <= 5479: // ~15 years
		return "year"
	default:
		return "5year"
	}
}

// bucketKey is one entry on the time axis: its strftime key (matching AssetsByBucket
// output), a human label, and the bucket's start instant (UTC).
type bucketKey struct {
	key   string
	label string
	start time.Time
}

// bucketKeys enumerates every bucket from start to end inclusive at the given
// granularity, so gaps with no assets become explicit zero bars. Keys are computed
// in UTC to match the SQLite driver's UTC-normalized datetime text (strftime reads
// the stored value in UTC), keeping these labels aligned with AssetsByBucket keys.
func bucketKeys(gran string, start, end time.Time) []bucketKey {
	start, end = start.UTC(), end.UTC()
	var keys []bucketKey
	switch gran {
	case "day":
		d := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		last := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
		for !d.After(last) {
			k := d.Format("2006-01-02")
			keys = append(keys, bucketKey{key: k, label: k, start: d})
			d = d.AddDate(0, 0, 1)
		}
	case "month":
		d := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
		last := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
		for !d.After(last) {
			k := d.Format("2006-01")
			keys = append(keys, bucketKey{key: k, label: k, start: d})
			d = d.AddDate(0, 1, 0)
		}
	case "year":
		for y := start.Year(); y <= end.Year(); y++ {
			d := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
			k := strconv.Itoa(y)
			keys = append(keys, bucketKey{key: k, label: k, start: d})
		}
	case "5year":
		for y := (start.Year() / 5) * 5; y <= (end.Year()/5)*5; y += 5 {
			d := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
			keys = append(keys, bucketKey{key: strconv.Itoa(y), label: fmt.Sprintf("%d–%d", y, y+4), start: d})
		}
	}
	return keys
}
