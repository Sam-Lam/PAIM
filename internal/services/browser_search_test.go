package services

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// seedCameraAsset inserts a browse-visible asset with camera metadata and an
// effective date (capture nil => import date is used).
func seedCameraAsset(t *testing.T, assets *repo.AssetRepo, name string, mt domain.MediaType, capture *time.Time, imp time.Time, make, model string) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   name,
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + name,
		CurrentArchivePath: "2020/" + name,
		CaptureDate:        capture,
		ImportDate:         imp,
		MediaType:          mt,
		CameraMake:         make,
		CameraModel:        model,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

func TestListAssetsDateRangeFilter(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()
	imp := *mustTime("2025-05-05")

	seedCameraAsset(t, assets, "a2016.jpg", domain.MediaTypePhoto, mustTime("2016-05-01"), imp, "", "")
	seedCameraAsset(t, assets, "a2018.jpg", domain.MediaTypePhoto, mustTime("2018-12-31"), imp, "", "")
	seedCameraAsset(t, assets, "a2019.jpg", domain.MediaTypePhoto, mustTime("2019-01-01"), imp, "", "")

	// "photos between 2015 and 2018": inclusive day boundaries as the frontend sends them.
	res, err := svc.ListAssets(ctx, BrowseFilters{
		CaptureFrom: "2015-01-01T00:00:00",
		CaptureTo:   "2018-12-31T23:59:59",
	}, 1, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("2015..2018 total = %d, want 2", res.Total)
	}

	// A bare date bound parses too (midnight UTC).
	if res, err = svc.ListAssets(ctx, BrowseFilters{CaptureFrom: "2019-01-01"}, 1, 50); err != nil {
		t.Fatalf("list bare: %v", err)
	} else if res.Total != 1 {
		t.Errorf("from 2019-01-01 total = %d, want 1", res.Total)
	}

	// Invalid date is a surfaced error, never a silent no-op.
	if _, err = svc.ListAssets(ctx, BrowseFilters{CaptureFrom: "nonsense"}, 1, 50); err == nil {
		t.Errorf("expected error for invalid CaptureFrom, got nil")
	}
}

func TestListAssetsCameraFilter(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()
	imp := *mustTime("2025-05-05")

	seedCameraAsset(t, assets, "f1.jpg", domain.MediaTypePhoto, nil, imp, "FUJIFILM", "X100VI")
	seedCameraAsset(t, assets, "f2.jpg", domain.MediaTypePhoto, nil, imp, "FUJIFILM", "X100VI")
	seedCameraAsset(t, assets, "c1.jpg", domain.MediaTypePhoto, nil, imp, "Canon", "EOS R5")

	// "everything from the X100VI"
	res, err := svc.ListAssets(ctx, BrowseFilters{CameraMake: "FUJIFILM", CameraModel: "X100VI"}, 1, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("X100VI total = %d, want 2", res.Total)
	}
}

func TestCamerasAndYearsService(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()
	imp := *mustTime("2025-05-05")

	seedCameraAsset(t, assets, "f1.jpg", domain.MediaTypePhoto, mustTime("2019-06-01"), imp, "FUJIFILM", "X100VI")
	seedCameraAsset(t, assets, "f2.jpg", domain.MediaTypePhoto, mustTime("2019-07-01"), imp, "FUJIFILM", "X100VI")
	seedCameraAsset(t, assets, "c1.jpg", domain.MediaTypePhoto, mustTime("2021-01-01"), imp, "Canon", "EOS R5")

	cams, err := svc.Cameras(ctx)
	if err != nil {
		t.Fatalf("cameras: %v", err)
	}
	if len(cams) != 2 {
		t.Fatalf("cameras = %d, want 2 (%+v)", len(cams), cams)
	}
	if cams[0].Label != "FUJIFILM X100VI" || cams[0].Count != 2 {
		t.Errorf("top camera = %+v, want label 'FUJIFILM X100VI' count 2", cams[0])
	}

	years, err := svc.Years(ctx)
	if err != nil {
		t.Fatalf("years: %v", err)
	}
	if len(years) != 2 || years[0].Year != "2021" || years[1].Year != "2019" || years[1].Count != 2 {
		t.Fatalf("years = %+v, want 2021(1) then 2019(2)", years)
	}
}
