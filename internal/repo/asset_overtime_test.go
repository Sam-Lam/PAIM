package repo

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm.UTC()
}

// seedTimed inserts an asset with the given capture date (nil = undated), import
// date, and media type. Distinct quick hashes keep the rows independent.
func seedTimed(t *testing.T, r *AssetRepo, qh string, capture *time.Time, imp time.Time, mt domain.MediaType) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   qh + ".jpg",
		OriginalExtension:  "jpg",
		QuickHash:          qh,
		CaptureDate:        capture,
		ImportDate:         imp,
		MediaType:          mt,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
	}
	if err := r.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset %q: %v", qh, err)
	}
	return a
}

// seedOverTimeDataset lays down a fixed multi-year dataset used by the bucketing
// tests. import is a single instant (2025-05-05) for every row, so only the
// undated row's import-date FALLBACK ever affects a bucket.
//
//	capture 2019-06-12  photo            -> 2019 / 2019-06 / 2015
//	capture 2019-06-15  video            -> 2019 / 2019-06 / 2015
//	capture 2021-03-01  photo            -> 2021 / 2021-03 / 2020
//	capture 2024-01-10  raw_photo        -> 2024 / 2024-01 / 2020
//	capture 2024-01-10  live_photo_pair  -> 2024 / 2024-01 / 2020
//	NO capture (import 2022-08-08) photo -> 2022 / 2022-08 / 2020  (fallback)
func seedOverTimeDataset(t *testing.T, r *AssetRepo) {
	t.Helper()
	imp := mustDay(t, "2025-05-05")
	seedTimed(t, r, "p2019", ptr(mustDay(t, "2019-06-12")), imp, domain.MediaTypePhoto)
	seedTimed(t, r, "v2019", ptr(mustDay(t, "2019-06-15")), imp, domain.MediaTypeVideo)
	seedTimed(t, r, "p2021", ptr(mustDay(t, "2021-03-01")), imp, domain.MediaTypePhoto)
	seedTimed(t, r, "raw2024", ptr(mustDay(t, "2024-01-10")), imp, domain.MediaTypeRawPhoto)
	seedTimed(t, r, "live2024", ptr(mustDay(t, "2024-01-10")), imp, domain.MediaTypeLivePhotoPair)
	seedTimed(t, r, "undated", nil, mustDay(t, "2022-08-08"), domain.MediaTypePhoto)
}

func bucketMap(t *testing.T, r *AssetRepo, gran string) map[string]BucketCount {
	t.Helper()
	rows, err := r.AssetsByBucket(context.Background(), gran)
	if err != nil {
		t.Fatalf("AssetsByBucket(%s): %v", gran, err)
	}
	m := make(map[string]BucketCount, len(rows))
	for _, b := range rows {
		m[b.Bucket] = b
	}
	return m
}

func wantBucket(t *testing.T, m map[string]BucketCount, key string, photos, videos int64) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Fatalf("bucket %q missing; have %v", key, m)
	}
	if got.Photos != photos || got.Videos != videos {
		t.Errorf("bucket %q = photos %d / videos %d, want %d / %d", key, got.Photos, got.Videos, photos, videos)
	}
}

func TestAssetsByBucketYear(t *testing.T) {
	r := NewAssetRepo(newTestDB(t))
	seedOverTimeDataset(t, r)
	m := bucketMap(t, r, "year")
	if len(m) != 4 {
		t.Fatalf("year buckets = %d, want 4: %v", len(m), m)
	}
	wantBucket(t, m, "2019", 1, 1)
	wantBucket(t, m, "2021", 1, 0)
	wantBucket(t, m, "2022", 1, 0) // undated row via import-date fallback
	wantBucket(t, m, "2024", 2, 0) // raw_photo + live_photo_pair both count as photos
}

func TestAssetsByBucketMonth(t *testing.T) {
	r := NewAssetRepo(newTestDB(t))
	seedOverTimeDataset(t, r)
	m := bucketMap(t, r, "month")
	wantBucket(t, m, "2019-06", 1, 1)
	wantBucket(t, m, "2021-03", 1, 0)
	wantBucket(t, m, "2022-08", 1, 0)
	wantBucket(t, m, "2024-01", 2, 0)
}

func TestAssetsByBucketDay(t *testing.T) {
	r := NewAssetRepo(newTestDB(t))
	seedOverTimeDataset(t, r)
	m := bucketMap(t, r, "day")
	wantBucket(t, m, "2019-06-12", 1, 0)
	wantBucket(t, m, "2019-06-15", 0, 1)
	wantBucket(t, m, "2024-01-10", 2, 0)
	wantBucket(t, m, "2022-08-08", 1, 0)
}

func TestAssetsByBucket5Year(t *testing.T) {
	r := NewAssetRepo(newTestDB(t))
	seedOverTimeDataset(t, r)
	m := bucketMap(t, r, "5year")
	if len(m) != 2 {
		t.Fatalf("5year buckets = %d, want 2: %v", len(m), m)
	}
	// 2019 floors to 2015; 2021/2022/2024 all floor to 2020.
	wantBucket(t, m, "2015", 1, 1)
	wantBucket(t, m, "2020", 4, 0) // 2021 photo + 2022 fallback + 2024 raw + 2024 live
}

func TestAssetsByBucketExcludesSoftDeleted(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	seedOverTimeDataset(t, r)
	del := seedTimed(t, r, "del2019", ptr(mustDay(t, "2019-06-12")), mustDay(t, "2025-05-05"), domain.MediaTypePhoto)
	if err := r.SoftDelete(ctx, del.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	m := bucketMap(t, r, "year")
	wantBucket(t, m, "2019", 1, 1) // deleted 2019 photo must not inflate the count
}

func TestAssetsByBucketUnknownGranularity(t *testing.T) {
	r := NewAssetRepo(newTestDB(t))
	if _, err := r.AssetsByBucket(context.Background(), "week"); err == nil {
		t.Fatal("AssetsByBucket(week): expected error for unknown granularity")
	}
}

func TestEffectiveDateRange(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	// Empty library -> nil, nil.
	minEff, maxEff, err := r.EffectiveDateRange(ctx)
	if err != nil {
		t.Fatalf("EffectiveDateRange(empty): %v", err)
	}
	if minEff != nil || maxEff != nil {
		t.Fatalf("empty range = %v..%v, want nil..nil", minEff, maxEff)
	}

	seedOverTimeDataset(t, r)
	minEff, maxEff, err = r.EffectiveDateRange(ctx)
	if err != nil {
		t.Fatalf("EffectiveDateRange: %v", err)
	}
	if minEff == nil || maxEff == nil {
		t.Fatalf("range = %v..%v, want non-nil", minEff, maxEff)
	}
	if got := minEff.UTC().Format("2006-01-02"); got != "2019-06-12" {
		t.Errorf("min effective = %s, want 2019-06-12", got)
	}
	if got := maxEff.UTC().Format("2006-01-02"); got != "2024-01-10" {
		t.Errorf("max effective = %s, want 2024-01-10", got)
	}
}

func TestCountUndatedFallback(t *testing.T) {
	r := NewAssetRepo(newTestDB(t))
	seedOverTimeDataset(t, r)
	n, err := r.CountUndatedFallback(context.Background())
	if err != nil {
		t.Fatalf("CountUndatedFallback: %v", err)
	}
	if n != 1 {
		t.Errorf("undated fallback = %d, want 1", n)
	}
}
