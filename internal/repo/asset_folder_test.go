package repo

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// seedDated inserts an asset at relPath with an explicit capture date (nil =
// no capture date) and import date, so folder date-ordering and newest-capture
// aggregation can be exercised deterministically.
func seedDated(t *testing.T, r *AssetRepo, relPath string, capture *time.Time, importDate time.Time) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   relPath,
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + relPath,
		CurrentArchivePath: relPath,
		MediaType:          domain.MediaTypePhoto,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
		CaptureDate:        capture,
		ImportDate:         importDate,
	}
	if err := r.Create(context.Background(), a); err != nil {
		t.Fatalf("create %q: %v", relPath, err)
	}
	return a
}

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
	assets, total, err := r.FolderAssets(ctx, "2019/2019-06-12 Trip", Page{}, "date", "desc")
	if err != nil {
		t.Fatalf("folder assets: %v", err)
	}
	if total != 1 || len(assets) != 1 || assets[0].OriginalFilename != "2019/2019-06-12 Trip/IMG_1.jpg" {
		t.Fatalf("direct assets = %d %v, want 1 (the JPEG)", total, assets)
	}
}

// TestFolderChildrenNewestCapture proves each subfolder's newestCapture is the
// MAX effective date over its whole subtree, and that a NULL capture date falls
// back to the import date (COALESCE).
func TestFolderChildrenNewestCapture(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	may := time.Date(2020, 5, 1, 12, 0, 0, 0, time.UTC)
	jul := time.Date(2020, 7, 15, 9, 30, 0, 0, time.UTC)
	mar := time.Date(2020, 3, 10, 8, 0, 0, 0, time.UTC)
	imp := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)

	// Folder A: two dated assets; newest is July.
	seedDated(t, r, "2020/A/img1.jpg", &may, imp)
	seedDated(t, r, "2020/A/img2.jpg", &jul, imp)
	// Folder B: a NULL-capture asset must fall back to its import date (March).
	seedDated(t, r, "2020/B/img3.jpg", nil, mar)

	// At "2020": A -> July, B -> March (import fallback).
	kids, err := r.FolderChildren(ctx, "2020")
	if err != nil {
		t.Fatalf("children 2020: %v", err)
	}
	got := map[string]*time.Time{}
	for _, k := range kids {
		got[k.Name] = k.NewestCapture
	}
	if got["A"] == nil || !got["A"].Equal(jul) {
		t.Errorf("A newestCapture = %v, want %v (subtree max)", got["A"], jul)
	}
	if got["B"] == nil || !got["B"].Equal(mar) {
		t.Errorf("B newestCapture = %v, want %v (COALESCE import fallback)", got["B"], mar)
	}

	// At root: 2020 rolls up to the overall max (July).
	roots, err := r.FolderChildren(ctx, "")
	if err != nil {
		t.Fatalf("children root: %v", err)
	}
	if len(roots) != 1 || roots[0].Name != "2020" {
		t.Fatalf("roots = %+v, want single 2020", roots)
	}
	if roots[0].NewestCapture == nil || !roots[0].NewestCapture.Equal(jul) {
		t.Errorf("2020 newestCapture = %v, want %v (subtree max over A and B)", roots[0].NewestCapture, jul)
	}
}

// TestFolderAssetsOrderBy exercises the name and date ORDER BY variants:
// case-insensitive name asc/desc, and date ordering that keeps NULL-capture
// (undated) rows last in BOTH directions.
func TestFolderAssetsOrderBy(t *testing.T) {
	ctx := context.Background()
	r := NewAssetRepo(newTestDB(t))

	jan := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	jun := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	impMar := time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)
	imp := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// Filenames chosen so a case-SENSITIVE sort would differ from NOCASE:
	// ASCII-wise 'B' (66) and 'C' (67) precede 'a' (97).
	seedDated(t, r, "f/Bravo.jpg", &jan, imp)     // capture Jan
	seedDated(t, r, "f/alpha.jpg", &jun, imp)     // capture Jun
	seedDated(t, r, "f/Charlie.jpg", nil, impMar) // undated (import Mar)

	names := func(sortBy, sortDir string) []string {
		assets, _, err := r.FolderAssets(ctx, "f", Page{}, sortBy, sortDir)
		if err != nil {
			t.Fatalf("folder assets %s/%s: %v", sortBy, sortDir, err)
		}
		out := make([]string, len(assets))
		for i, a := range assets {
			out[i] = a.OriginalFilename
		}
		return out
	}

	if got := names("name", "asc"); !equalStrs(got, []string{"f/alpha.jpg", "f/Bravo.jpg", "f/Charlie.jpg"}) {
		t.Errorf("name asc = %v, want case-insensitive alpha, Bravo, Charlie", got)
	}
	if got := names("name", "desc"); !equalStrs(got, []string{"f/Charlie.jpg", "f/Bravo.jpg", "f/alpha.jpg"}) {
		t.Errorf("name desc = %v, want Charlie, Bravo, alpha", got)
	}
	// date desc: newest capture first, undated (Charlie) last.
	if got := names("date", "desc"); !equalStrs(got, []string{"f/alpha.jpg", "f/Bravo.jpg", "f/Charlie.jpg"}) {
		t.Errorf("date desc = %v, want alpha(Jun), Bravo(Jan), Charlie(undated last)", got)
	}
	// date asc: oldest capture first, undated (Charlie) STILL last.
	if got := names("date", "asc"); !equalStrs(got, []string{"f/Bravo.jpg", "f/alpha.jpg", "f/Charlie.jpg"}) {
		t.Errorf("date asc = %v, want Bravo(Jan), alpha(Jun), Charlie(undated last)", got)
	}
	// An unknown sortBy/sortDir falls back to the default date/desc.
	if got := names("bogus", "sideways"); !equalStrs(got, []string{"f/alpha.jpg", "f/Bravo.jpg", "f/Charlie.jpg"}) {
		t.Errorf("default fallback = %v, want date/desc order", got)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
