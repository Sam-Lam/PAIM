package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

func newDashboardHarness(t *testing.T) (*DashboardService, *repo.AssetRepo) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "dash.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	svc := NewDashboardService(gdb, assets, repo.NewBackupRepo(gdb), repo.NewSourceRepo(gdb), nil)
	return svc, assets
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm.UTC()
}

func seedTimedAsset(t *testing.T, assets *repo.AssetRepo, qh string, capture *time.Time, imp time.Time, mt domain.MediaType) {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   qh + ".jpg",
		QuickHash:          qh,
		CaptureDate:        capture,
		ImportDate:         imp,
		MediaType:          mt,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset %q: %v", qh, err)
	}
}

func ptrTime(tm time.Time) *time.Time { return &tm }

// bucketByLabel indexes a DTO's buckets by label for assertions.
func bucketByLabel(dto *AssetsOverTimeDTO) map[string]AssetsOverTimeBucketDTO {
	m := make(map[string]AssetsOverTimeBucketDTO, len(dto.Buckets))
	for _, b := range dto.Buckets {
		m[b.Label] = b
	}
	return m
}

func TestAssetsOverTimeYearZeroFill(t *testing.T) {
	svc, assets := newDashboardHarness(t)
	imp := day(t, "2025-05-05")
	// Captures in 2019 and 2022 only — 2020 and 2021 must appear as zero bars.
	seedTimedAsset(t, assets, "a", ptrTime(day(t, "2019-06-12")), imp, domain.MediaTypePhoto)
	seedTimedAsset(t, assets, "b", ptrTime(day(t, "2019-06-15")), imp, domain.MediaTypeVideo)
	seedTimedAsset(t, assets, "c", ptrTime(day(t, "2022-08-01")), imp, domain.MediaTypePhoto)

	dto, err := svc.AssetsOverTime(context.Background(), "year")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	if dto.Granularity != "year" {
		t.Fatalf("granularity = %q, want year", dto.Granularity)
	}
	if len(dto.Buckets) != 4 {
		t.Fatalf("buckets = %d, want 4 (2019..2022 inclusive): %+v", len(dto.Buckets), dto.Buckets)
	}
	labels := []string{"2019", "2020", "2021", "2022"}
	for i, want := range labels {
		if dto.Buckets[i].Label != want {
			t.Errorf("bucket[%d].Label = %q, want %q", i, dto.Buckets[i].Label, want)
		}
	}
	m := bucketByLabel(dto)
	if b := m["2019"]; b.Photos != 1 || b.Videos != 1 {
		t.Errorf("2019 = %d/%d, want 1/1", b.Photos, b.Videos)
	}
	if b := m["2020"]; b.Photos != 0 || b.Videos != 0 {
		t.Errorf("2020 (gap) = %d/%d, want 0/0", b.Photos, b.Videos)
	}
	if b := m["2021"]; b.Photos != 0 || b.Videos != 0 {
		t.Errorf("2021 (gap) = %d/%d, want 0/0", b.Photos, b.Videos)
	}
	if b := m["2022"]; b.Photos != 1 || b.Videos != 0 {
		t.Errorf("2022 = %d/%d, want 1/0", b.Photos, b.Videos)
	}
	if dto.Windowed {
		t.Error("year view over a 3-year span must not be windowed")
	}
	if dto.RangeStart != "2019-06-12" || dto.RangeEnd != "2022-08-01" {
		t.Errorf("range = %s..%s, want 2019-06-12..2022-08-01", dto.RangeStart, dto.RangeEnd)
	}
	// Bucket start ISO dates track the bucket boundary.
	if m["2020"].Start != "2020-01-01" {
		t.Errorf("2020 start = %q, want 2020-01-01", m["2020"].Start)
	}
}

func TestAssetsOverTimeUndatedFallbackFootnote(t *testing.T) {
	svc, assets := newDashboardHarness(t)
	imp := day(t, "2023-04-04")
	seedTimedAsset(t, assets, "dated", ptrTime(day(t, "2023-01-01")), imp, domain.MediaTypePhoto)
	seedTimedAsset(t, assets, "undated", nil, imp, domain.MediaTypePhoto)

	dto, err := svc.AssetsOverTime(context.Background(), "month")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	if dto.TotalUndatedFallback != 1 {
		t.Errorf("TotalUndatedFallback = %d, want 1", dto.TotalUndatedFallback)
	}
}

func TestAssetsOverTime5YearBucketing(t *testing.T) {
	svc, assets := newDashboardHarness(t)
	imp := day(t, "2025-05-05")
	seedTimedAsset(t, assets, "a", ptrTime(day(t, "2003-06-12")), imp, domain.MediaTypePhoto) // -> 2000
	seedTimedAsset(t, assets, "b", ptrTime(day(t, "2019-06-15")), imp, domain.MediaTypeVideo) // -> 2015

	dto, err := svc.AssetsOverTime(context.Background(), "5year")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	// 2000, 2005, 2010, 2015 — zero-filled across the span.
	wantLabels := []string{"2000–2004", "2005–2009", "2010–2014", "2015–2019"}
	if len(dto.Buckets) != len(wantLabels) {
		t.Fatalf("buckets = %d, want %d: %+v", len(dto.Buckets), len(wantLabels), dto.Buckets)
	}
	for i, want := range wantLabels {
		if dto.Buckets[i].Label != want {
			t.Errorf("bucket[%d].Label = %q, want %q", i, dto.Buckets[i].Label, want)
		}
	}
	m := bucketByLabel(dto)
	if b := m["2000–2004"]; b.Photos != 1 || b.Videos != 0 {
		t.Errorf("2000–2004 = %d/%d, want 1/0", b.Photos, b.Videos)
	}
	if b := m["2015–2019"]; b.Photos != 0 || b.Videos != 1 {
		t.Errorf("2015–2019 = %d/%d, want 0/1", b.Photos, b.Videos)
	}
	if m["2000–2004"].Start != "2000-01-01" {
		t.Errorf("2000–2004 start = %q, want 2000-01-01", m["2000–2004"].Start)
	}
}

func TestAssetsOverTimeDayWindowing(t *testing.T) {
	svc, assets := newDashboardHarness(t)
	imp := day(t, "2025-05-05")
	// One old capture and one recent capture, both real. Day view windows to the
	// most recent dayWindow days ending at the max effective date, so the old bar
	// falls outside the window and the result is flagged windowed.
	seedTimedAsset(t, assets, "old", ptrTime(day(t, "2020-01-01")), imp, domain.MediaTypePhoto)
	seedTimedAsset(t, assets, "recent", ptrTime(day(t, "2020-06-15")), imp, domain.MediaTypeVideo)

	dto, err := svc.AssetsOverTime(context.Background(), "day")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	if dto.Granularity != "day" {
		t.Fatalf("granularity = %q, want day", dto.Granularity)
	}
	if !dto.Windowed {
		t.Error("day view spanning >120 days must be windowed")
	}
	if len(dto.Buckets) != dayWindow {
		t.Fatalf("day buckets = %d, want %d", len(dto.Buckets), dayWindow)
	}
	// Window ends at the max effective date (2020-06-15) and the recent video is
	// inside it; the old 2020-01-01 photo is not.
	last := dto.Buckets[len(dto.Buckets)-1]
	if last.Start != "2020-06-15" {
		t.Errorf("last bucket start = %q, want 2020-06-15", last.Start)
	}
	if last.Videos != 1 {
		t.Errorf("last bucket videos = %d, want 1", last.Videos)
	}
	// Full range still reported honestly even though bars are windowed.
	if dto.RangeStart != "2020-01-01" || dto.RangeEnd != "2020-06-15" {
		t.Errorf("range = %s..%s, want 2020-01-01..2020-06-15", dto.RangeStart, dto.RangeEnd)
	}
	var total int64
	for _, b := range dto.Buckets {
		total += b.Photos + b.Videos
	}
	if total != 1 {
		t.Errorf("windowed total = %d, want 1 (old photo excluded)", total)
	}
}

func TestAssetsOverTimeDayNotWindowedWhenNarrow(t *testing.T) {
	svc, assets := newDashboardHarness(t)
	imp := day(t, "2025-05-05")
	seedTimedAsset(t, assets, "a", ptrTime(day(t, "2020-06-10")), imp, domain.MediaTypePhoto)
	seedTimedAsset(t, assets, "b", ptrTime(day(t, "2020-06-15")), imp, domain.MediaTypePhoto)

	dto, err := svc.AssetsOverTime(context.Background(), "day")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	if dto.Windowed {
		t.Error("6-day span must not be windowed")
	}
	if len(dto.Buckets) != 6 { // 2020-06-10 .. 2020-06-15 inclusive
		t.Fatalf("day buckets = %d, want 6", len(dto.Buckets))
	}
}

func TestAssetsOverTimeEmptyLibrary(t *testing.T) {
	svc, _ := newDashboardHarness(t)
	dto, err := svc.AssetsOverTime(context.Background(), "all")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	if len(dto.Buckets) != 0 {
		t.Errorf("empty library buckets = %d, want 0", len(dto.Buckets))
	}
	if dto.Buckets == nil {
		t.Error("Buckets must be non-nil (empty slice) for JSON")
	}
	if dto.RangeStart != "" || dto.RangeEnd != "" {
		t.Errorf("empty range = %q..%q, want empty", dto.RangeStart, dto.RangeEnd)
	}
	if dto.Granularity == "" {
		t.Error("granularity must still be resolved for an empty library")
	}
}

func TestResolveGranularity(t *testing.T) {
	dayD := 24 * time.Hour
	cases := []struct {
		name      string
		requested string
		span      time.Duration
		want      string
	}{
		{"explicit day passes through", "day", 9999 * dayD, "day"},
		{"explicit month passes through", "month", 1 * dayD, "month"},
		{"explicit year passes through", "year", 1 * dayD, "year"},
		{"explicit 5year passes through", "5year", 1 * dayD, "5year"},
		{"all: 2 months -> day", "all", 60 * dayD, "day"},
		{"all: exactly 3 months boundary -> day", "all", 92 * dayD, "day"},
		{"all: 1 year -> month", "all", 365 * dayD, "month"},
		{"all: just over 3 months -> month", "all", 93 * dayD, "month"},
		{"all: 3 years boundary -> month", "all", 1096 * dayD, "month"},
		{"all: 5 years -> year", "all", 5 * 365 * dayD, "year"},
		{"all: 15 years boundary -> year", "all", 5479 * dayD, "year"},
		{"all: 20 years -> 5year", "all", 20 * 365 * dayD, "5year"},
		{"unknown value auto-picks", "week", 20 * 365 * dayD, "5year"},
		{"empty span -> day", "all", 0, "day"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveGranularity(tc.requested, tc.span); got != tc.want {
				t.Errorf("resolveGranularity(%q, %v) = %q, want %q", tc.requested, tc.span, got, tc.want)
			}
		})
	}
}

func TestAssetsOverTimeAutoPicksFromSpan(t *testing.T) {
	svc, assets := newDashboardHarness(t)
	imp := day(t, "2025-05-05")
	// ~20 year span -> auto should resolve to 5year.
	seedTimedAsset(t, assets, "old", ptrTime(day(t, "2001-01-01")), imp, domain.MediaTypePhoto)
	seedTimedAsset(t, assets, "new", ptrTime(day(t, "2021-01-01")), imp, domain.MediaTypePhoto)

	dto, err := svc.AssetsOverTime(context.Background(), "all")
	if err != nil {
		t.Fatalf("AssetsOverTime: %v", err)
	}
	if dto.Granularity != "5year" {
		t.Errorf("auto granularity = %q, want 5year", dto.Granularity)
	}
}
