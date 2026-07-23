package repo

import (
	"context"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

func seedPath(t *testing.T, r *AssetRepo, relPath string) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   relPath,
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + relPath,
		CurrentArchivePath: relPath,
		MediaType:          domain.MediaTypePhoto,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
	}
	if err := r.Create(context.Background(), a); err != nil {
		t.Fatalf("create %q: %v", relPath, err)
	}
	return a
}

func pathOf(t *testing.T, r *AssetRepo, id string) string {
	t.Helper()
	a, err := r.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get %q: %v", id, err)
	}
	return a.CurrentArchivePath
}

// TestUpdateArchivePathPrefixWholeSegment proves the prefix rewrite matches only
// whole path segments: renaming "2019/2019-06-12 Trip" touches its files and RAW
// subpaths but never the sibling "2019/2019-06-12 Trip2".
func TestUpdateArchivePathPrefixWholeSegment(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	jpg := seedPath(t, r, "2019/2019-06-12 Trip/IMG_1.jpg")
	raw := seedPath(t, r, "2019/2019-06-12 Trip/RAW/IMG_1.cr3")
	sib := seedPath(t, r, "2019/2019-06-12 Trip2/OTHER.jpg")

	n, err := r.UpdateArchivePathPrefix(ctx, "2019/2019-06-12 Trip", "2019/2019-06-12 Beach")
	if err != nil {
		t.Fatalf("update prefix: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows updated = %d, want 2", n)
	}
	if got := pathOf(t, r, jpg.ID); got != "2019/2019-06-12 Beach/IMG_1.jpg" {
		t.Errorf("jpg = %q", got)
	}
	if got := pathOf(t, r, raw.ID); got != "2019/2019-06-12 Beach/RAW/IMG_1.cr3" {
		t.Errorf("raw = %q", got)
	}
	if got := pathOf(t, r, sib.ID); got != "2019/2019-06-12 Trip2/OTHER.jpg" {
		t.Errorf("sibling changed: %q", got)
	}
}

// TestUpdateArchivePathPrefixLikeMetachars ensures a label containing SQL LIKE
// wildcards (_ and %) is matched literally, not as a wildcard.
func TestUpdateArchivePathPrefixLikeMetachars(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	target := seedPath(t, r, "2019/2019-06-12 50%_off/IMG.jpg")
	// A path that a naive (unescaped) LIKE '%_off/%' pattern could also match.
	other := seedPath(t, r, "2019/2019-06-12 5aXoff/IMG.jpg")

	n, err := r.UpdateArchivePathPrefix(ctx, "2019/2019-06-12 50%_off", "2019/2019-06-12 Sale")
	if err != nil {
		t.Fatalf("update prefix: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows updated = %d, want 1 (metachars must be literal)", n)
	}
	if got := pathOf(t, r, target.ID); got != "2019/2019-06-12 Sale/IMG.jpg" {
		t.Errorf("target = %q", got)
	}
	if got := pathOf(t, r, other.ID); got != "2019/2019-06-12 5aXoff/IMG.jpg" {
		t.Errorf("other row wrongly matched: %q", got)
	}
}

func TestFolderChildrenAndAssets(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))
	seedPath(t, r, "2019/2019-06-12 Trip/IMG_1.jpg")
	seedPath(t, r, "2019/2019-06-12 Trip/RAW/IMG_1.cr3")
	seedPath(t, r, "2019/2019-08-01/IMG_2.jpg")
	seedPath(t, r, "2020/2020-01-01/IMG_3.jpg")
	// A not-copied duplicate (empty path) must be ignored by the folder view.
	seedPath(t, r, "")

	// Root → years.
	roots, err := r.FolderChildren(ctx, "")
	if err != nil {
		t.Fatalf("children root: %v", err)
	}
	if len(roots) != 2 || roots[0].Name != "2019" || roots[0].AssetCount != 3 {
		t.Fatalf("root children = %+v, want 2019(3) and 2020(1)", roots)
	}

	// Direct assets in the Trip folder = only the JPEG (RAW is nested).
	assets, total, err := r.FolderAssets(ctx, "2019/2019-06-12 Trip", Page{})
	if err != nil {
		t.Fatalf("folder assets: %v", err)
	}
	if total != 1 || len(assets) != 1 || assets[0].OriginalFilename != "2019/2019-06-12 Trip/IMG_1.jpg" {
		t.Fatalf("direct assets = %d %v, want 1 (the JPEG)", total, assets)
	}
}
