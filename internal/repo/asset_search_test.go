package repo

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// seedCamera inserts an asset with the given effective-date inputs, media type,
// and camera metadata. capture nil = undated (import date becomes the effective
// date). Distinct quick hashes keep rows independent.
func seedCamera(t *testing.T, r *AssetRepo, qh string, capture *time.Time, imp time.Time, mt domain.MediaType, make, model, lens string) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   qh + ".jpg",
		OriginalExtension:  "jpg",
		OriginalFullPath:   "/src/" + qh + ".jpg",
		QuickHash:          qh,
		CaptureDate:        capture,
		ImportDate:         imp,
		MediaType:          mt,
		CameraMake:         make,
		CameraModel:        model,
		Lens:               lens,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
	}
	if err := r.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset %q: %v", qh, err)
	}
	return a
}

func TestListDateRangeInclusiveBounds(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	imp := mustDay(t, "2025-05-05")

	seedTimed(t, r, "y2015", ptr(mustDay(t, "2015-06-01")), imp, domain.MediaTypePhoto)
	seedTimed(t, r, "y2016", ptr(mustDay(t, "2016-06-01")), imp, domain.MediaTypePhoto)
	seedTimed(t, r, "y2018", ptr(mustDay(t, "2018-12-31")), imp, domain.MediaTypePhoto)
	seedTimed(t, r, "y2019", ptr(mustDay(t, "2019-01-01")), imp, domain.MediaTypePhoto)

	from := mustDay(t, "2015-06-01") // exactly the lower bound row
	to := mustDay(t, "2018-12-31")   // exactly the upper bound row
	_, total, err := r.List(ctx, AssetQuery{CaptureFrom: &from, CaptureTo: &to})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 { // 2015, 2016, 2018 inclusive; 2019 excluded
		t.Errorf("inclusive range total = %d, want 3", total)
	}

	// Open-ended lower bound only.
	if _, total, err = r.List(ctx, AssetQuery{CaptureFrom: &to}); err != nil {
		t.Fatalf("list from: %v", err)
	} else if total != 2 { // 2018-12-31 and 2019-01-01
		t.Errorf("from-only total = %d, want 2", total)
	}
}

func TestListDateRangeUsesImportDateFallback(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	// Undated asset: effective date is its import date (2020), NOT excluded by a
	// 2020 range even though capture_date is NULL.
	seedTimed(t, r, "undated2020", nil, mustDay(t, "2020-07-15"), domain.MediaTypePhoto)
	// Dated asset in 2022, outside the window.
	seedTimed(t, r, "dated2022", ptr(mustDay(t, "2022-03-03")), mustDay(t, "2025-01-01"), domain.MediaTypePhoto)

	from := mustDay(t, "2020-01-01")
	to := mustDay(t, "2020-12-31")
	rows, total, err := r.List(ctx, AssetQuery{CaptureFrom: &from, CaptureTo: &to})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].QuickHash != "undated2020" {
		t.Fatalf("fallback range = total %d rows %d, want the undated 2020 import-date row", total, len(rows))
	}
}

func TestListDateRangeComposesWithTypeAndStatus(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	imp := mustDay(t, "2025-05-05")

	// Two 2019 assets: one photo (verified), one video (verified). One 2021 photo.
	p := seedTimed(t, r, "p2019", ptr(mustDay(t, "2019-06-01")), imp, domain.MediaTypePhoto)
	seedTimed(t, r, "v2019", ptr(mustDay(t, "2019-06-02")), imp, domain.MediaTypeVideo)
	seedTimed(t, r, "p2021", ptr(mustDay(t, "2021-06-01")), imp, domain.MediaTypePhoto)
	_ = p

	from := mustDay(t, "2019-01-01")
	to := mustDay(t, "2019-12-31")

	// "all videos from 2019"
	rows, total, err := r.List(ctx, AssetQuery{
		MediaType:   ptr(domain.MediaTypeVideo),
		CaptureFrom: &from,
		CaptureTo:   &to,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].QuickHash != "v2019" {
		t.Fatalf("videos-from-2019 = total %d rows %v, want the one 2019 video", total, rows)
	}

	// Range + verification status still composes.
	if _, total, err = r.List(ctx, AssetQuery{
		VerificationStatus: ptr(domain.VerificationStatusVerified),
		CaptureFrom:        &from,
		CaptureTo:          &to,
	}); err != nil {
		t.Fatalf("list verified: %v", err)
	} else if total != 2 {
		t.Errorf("verified-in-2019 total = %d, want 2", total)
	}
}

func TestListCameraExactMatchLiteralMetachars(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	imp := mustDay(t, "2025-05-05")

	// Camera model containing LIKE metacharacters (% and _). Exact match must
	// treat them literally and NOT match the sibling.
	seedCamera(t, r, "meta", nil, imp, domain.MediaTypePhoto, "ACME", "X_100%", "")
	seedCamera(t, r, "sibling", nil, imp, domain.MediaTypePhoto, "ACME", "X0100Z", "")
	seedCamera(t, r, "fuji", nil, imp, domain.MediaTypePhoto, "FUJIFILM", "X100VI", "")

	rows, total, err := r.List(ctx, AssetQuery{CameraMake: "ACME", CameraModel: "X_100%"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].QuickHash != "meta" {
		t.Fatalf("exact camera match = total %d rows %v, want only the literal X_100%% row", total, rows)
	}

	// Make-only exact match returns both ACME rows.
	if _, total, err = r.List(ctx, AssetQuery{CameraMake: "ACME"}); err != nil {
		t.Fatalf("list make: %v", err)
	} else if total != 2 {
		t.Errorf("make-only total = %d, want 2", total)
	}
}

func TestListTextSearchReachesCameraAndLens(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	imp := mustDay(t, "2025-05-05")

	seedCamera(t, r, "a", nil, imp, domain.MediaTypePhoto, "FUJIFILM", "X100VI", "23mm f/2")
	seedCamera(t, r, "b", nil, imp, domain.MediaTypePhoto, "Canon", "EOS R5", "RF 50mm")
	seedCamera(t, r, "c", nil, imp, domain.MediaTypePhoto, "SONY", "A7IV", "FE 35mm")

	// Text search hits camera model.
	if _, total, err := r.List(ctx, AssetQuery{Text: "X100"}); err != nil {
		t.Fatalf("list model text: %v", err)
	} else if total != 1 {
		t.Errorf("text 'X100' total = %d, want 1", total)
	}

	// Text search hits camera make.
	if _, total, err := r.List(ctx, AssetQuery{Text: "canon"}); err != nil {
		t.Fatalf("list make text: %v", err)
	} else if total != 1 {
		t.Errorf("text 'canon' total = %d, want 1", total)
	}

	// Text search hits lens (a text-search column, not a filter).
	if _, total, err := r.List(ctx, AssetQuery{Text: "35mm"}); err != nil {
		t.Fatalf("list lens text: %v", err)
	} else if total != 1 {
		t.Errorf("text '35mm' total = %d, want 1", total)
	}

	// Filename still matches (original two columns preserved).
	if _, total, err := r.List(ctx, AssetQuery{Text: "a.jpg"}); err != nil {
		t.Fatalf("list filename text: %v", err)
	} else if total != 1 {
		t.Errorf("text 'a.jpg' total = %d, want 1", total)
	}
}

func TestListTextSearchEscapesMetacharacters(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	imp := mustDay(t, "2025-05-05")

	// A literal '%' in a query must NOT act as a wildcard.
	seedCamera(t, r, "pct", nil, imp, domain.MediaTypePhoto, "ACME", "50% Grey", "")
	seedCamera(t, r, "plain", nil, imp, domain.MediaTypePhoto, "ACME", "Neutral", "")

	if _, total, err := r.List(ctx, AssetQuery{Text: "50%"}); err != nil {
		t.Fatalf("list: %v", err)
	} else if total != 1 {
		t.Errorf("escaped '50%%' total = %d, want 1 (literal, not wildcard)", total)
	}
	// A bare '%' should match nothing by content here (no model literally contains '%'
	// other than "50% Grey"); it must not match every row as a wildcard would.
	if _, total, err := r.List(ctx, AssetQuery{Text: "%"}); err != nil {
		t.Fatalf("list pct: %v", err)
	} else if total != 1 {
		t.Errorf("bare '%%' total = %d, want 1 (literal), not all rows", total)
	}
}

func TestCamerasAggregatesWithCounts(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	imp := mustDay(t, "2025-05-05")

	seedCamera(t, r, "f1", nil, imp, domain.MediaTypePhoto, "FUJIFILM", "X100VI", "")
	seedCamera(t, r, "f2", nil, imp, domain.MediaTypePhoto, "FUJIFILM", "X100VI", "")
	seedCamera(t, r, "f3", nil, imp, domain.MediaTypePhoto, "FUJIFILM", "X-T5", "")
	seedCamera(t, r, "c1", nil, imp, domain.MediaTypePhoto, "Canon", "EOS R5", "")
	// No-camera row is excluded entirely.
	seedCamera(t, r, "none", nil, imp, domain.MediaTypePhoto, "", "", "")
	// Soft-deleted row must not be counted.
	del := seedCamera(t, r, "del", nil, imp, domain.MediaTypePhoto, "FUJIFILM", "X100VI", "")
	if err := r.SoftDelete(ctx, del.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	cams, err := r.Cameras(ctx)
	if err != nil {
		t.Fatalf("cameras: %v", err)
	}
	if len(cams) != 3 {
		t.Fatalf("distinct cameras = %d, want 3 (%+v)", len(cams), cams)
	}
	// Most-used first: FUJIFILM X100VI has 2 live rows (deleted excluded).
	if cams[0].Make != "FUJIFILM" || cams[0].Model != "X100VI" || cams[0].Count != 2 {
		t.Errorf("top camera = %+v, want FUJIFILM X100VI count 2", cams[0])
	}
	// Every returned pair has at least a make or model.
	for _, c := range cams {
		if c.Make == "" && c.Model == "" {
			t.Errorf("empty camera pair leaked into results: %+v", c)
		}
	}
}

func TestRollupYearsAggregatesMonths(t *testing.T) {
	// CaptureMonths yields newest-first months; RollupYears folds to years.
	months := []CaptureMonth{
		{Month: "2019-06", Count: 3},
		{Month: "2019-01", Count: 2},
		{Month: "2018-12", Count: 5},
		{Month: "2016-07", Count: 1},
		{Month: "bad", Count: 99}, // malformed, skipped
	}
	years := RollupYears(months)
	if len(years) != 3 {
		t.Fatalf("years = %d, want 3 (%+v)", len(years), years)
	}
	// Newest year first.
	want := []YearCount{
		{Year: "2019", Count: 5},
		{Year: "2018", Count: 5},
		{Year: "2016", Count: 1},
	}
	for i, w := range want {
		if years[i] != w {
			t.Errorf("years[%d] = %+v, want %+v", i, years[i], w)
		}
	}

	if got := RollupYears(nil); len(got) != 0 {
		t.Errorf("RollupYears(nil) = %+v, want empty", got)
	}
}
