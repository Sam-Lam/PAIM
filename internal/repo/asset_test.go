package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/autolinepro/paim/internal/domain"
)

func TestAssetSoftDeleteKeepsRowAndHidesFromQueries(t *testing.T) {
	ctx := context.Background()
	gdb := newTestDB(t)
	r := NewAssetRepo(gdb)

	a := mustCreateAsset(t, r, "hash-a", 100, domain.MediaTypePhoto)

	if err := r.SoftDelete(ctx, a.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Default scope must hide the row.
	if _, err := r.GetByID(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID after soft delete: got %v, want ErrNotFound", err)
	}

	// The row must still physically exist (Unscoped) and carry the Deleted flag.
	var count int64
	if err := gdb.Unscoped().Model(&domain.Asset{}).Where("id = ?", a.ID).Count(&count).Error; err != nil {
		t.Fatalf("unscoped count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row physically removed: unscoped count = %d, want 1", count)
	}

	var got domain.Asset
	if err := gdb.Unscoped().First(&got, "id = ?", a.ID).Error; err != nil {
		t.Fatalf("unscoped fetch: %v", err)
	}
	if !got.Deleted {
		t.Error("expected Deleted flag to be true after SoftDelete")
	}
	if !got.DeletedAt.Valid {
		t.Error("expected DeletedAt to be set after SoftDelete")
	}
}

func TestFindByQuickHashExcludesSoftDeleted(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	keep := mustCreateAsset(t, r, "dup-hash", 10, domain.MediaTypePhoto)
	del := mustCreateAsset(t, r, "dup-hash", 10, domain.MediaTypePhoto)

	if err := r.SoftDelete(ctx, del.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	got, err := r.FindByQuickHash(ctx, "dup-hash")
	if err != nil {
		t.Fatalf("find by quick hash: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FindByQuickHash returned %d assets, want 1", len(got))
	}
	if got[0].ID != keep.ID {
		t.Errorf("FindByQuickHash returned %q, want the non-deleted asset %q", got[0].ID, keep.ID)
	}
}

func TestUpdateFullHashAndFindByFullHash(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	a := mustCreateAsset(t, r, "q1", 10, domain.MediaTypePhoto)

	// Empty full hash never matches.
	if got, err := r.FindByFullHash(ctx, ""); err != nil || len(got) != 0 {
		t.Fatalf("FindByFullHash(empty) = %v, %v; want nil, nil", got, err)
	}

	if err := r.UpdateFullHash(ctx, a.ID, "full-1"); err != nil {
		t.Fatalf("update full hash: %v", err)
	}
	got, err := r.FindByFullHash(ctx, "full-1")
	if err != nil {
		t.Fatalf("find by full hash: %v", err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("FindByFullHash = %+v, want asset %q", got, a.ID)
	}
}

func TestStatusUpdatesAndMarkDuplicate(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	orig := mustCreateAsset(t, r, "orig", 10, domain.MediaTypePhoto)
	dup := mustCreateAsset(t, r, "orig", 10, domain.MediaTypePhoto)

	if err := r.UpdateVerificationStatus(ctx, dup.ID, domain.VerificationStatusFailed); err != nil {
		t.Fatalf("update verification: %v", err)
	}
	if err := r.UpdateBackupStatus(ctx, dup.ID, domain.BackupStatusComplete); err != nil {
		t.Fatalf("update backup: %v", err)
	}
	if err := r.MarkDuplicateOf(ctx, dup.ID, orig.ID); err != nil {
		t.Fatalf("mark duplicate: %v", err)
	}

	got, err := r.GetByID(ctx, dup.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.VerificationStatus != domain.VerificationStatusFailed {
		t.Errorf("verification status = %q", got.VerificationStatus)
	}
	if got.BackupStatus != domain.BackupStatusComplete {
		t.Errorf("backup status = %q", got.BackupStatus)
	}
	if got.DuplicateOfAssetID == nil || *got.DuplicateOfAssetID != orig.ID {
		t.Errorf("duplicate-of = %v, want %q", got.DuplicateOfAssetID, orig.ID)
	}

	// Updating a missing asset returns ErrNotFound.
	if err := r.UpdateBackupStatus(ctx, "nope", domain.BackupStatusFailed); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing asset: got %v, want ErrNotFound", err)
	}
}

func TestCountsAndTotals(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	mustCreateAsset(t, r, "p1", 100, domain.MediaTypePhoto)
	mustCreateAsset(t, r, "p2", 200, domain.MediaTypePhoto)
	mustCreateAsset(t, r, "v1", 1000, domain.MediaTypeVideo)
	deleted := mustCreateAsset(t, r, "v2", 5000, domain.MediaTypeVideo)
	if err := r.SoftDelete(ctx, deleted.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	total, err := r.TotalBytes(ctx)
	if err != nil {
		t.Fatalf("total bytes: %v", err)
	}
	if total != 1300 { // deleted 5000 excluded
		t.Errorf("TotalBytes = %d, want 1300", total)
	}

	counts, err := r.CountsByMediaType(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	got := map[domain.MediaType]int64{}
	for _, c := range counts {
		got[c.MediaType] = c.Count
	}
	if got[domain.MediaTypePhoto] != 2 {
		t.Errorf("photo count = %d, want 2", got[domain.MediaTypePhoto])
	}
	if got[domain.MediaTypeVideo] != 1 {
		t.Errorf("video count = %d, want 1 (deleted excluded)", got[domain.MediaTypeVideo])
	}
}

func TestListDuplicatesAndList(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	orig := mustCreateAsset(t, r, "o", 10, domain.MediaTypePhoto)
	dup := mustCreateAsset(t, r, "o", 10, domain.MediaTypePhoto)
	if err := r.MarkDuplicateOf(ctx, dup.ID, orig.ID); err != nil {
		t.Fatalf("mark duplicate: %v", err)
	}

	pairs, err := r.ListDuplicates(ctx, Page{})
	if err != nil {
		t.Fatalf("list duplicates: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("ListDuplicates returned %d pairs, want 1", len(pairs))
	}
	if pairs[0].Duplicate.ID != dup.ID || pairs[0].Original.ID != orig.ID {
		t.Errorf("pair = dup %q / orig %q, want %q / %q",
			pairs[0].Duplicate.ID, pairs[0].Original.ID, dup.ID, orig.ID)
	}

	// List with pagination and a media-type filter.
	assets, total, err := r.List(ctx, AssetQuery{
		MediaType: ptr(domain.MediaTypePhoto),
		Page:      Page{Limit: 1},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Errorf("List total = %d, want 2", total)
	}
	if len(assets) != 1 {
		t.Errorf("List returned %d assets, want 1 (limited)", len(assets))
	}
}
