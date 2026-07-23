package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// folderHarness is a BrowserService rooted at a real temp directory so the
// folder-view and rename operations touch actual files.
type folderHarness struct {
	t      *testing.T
	svc    *BrowserService
	gdb    *gorm.DB
	assets *repo.AssetRepo
	root   string
}

func newFolderHarness(t *testing.T) *folderHarness {
	t.Helper()
	root := t.TempDir()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "folder.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc := NewBrowserService(gdb, repo.NewAssetRepo(gdb), repo.NewSourceRepo(gdb), repo.NewSessionRepo(gdb), nil)
	svc.root = root
	return &folderHarness{t: t, svc: svc, gdb: gdb, assets: repo.NewAssetRepo(gdb), root: root}
}

// seed writes a file at relPath under the root (forward-slash) and records an
// asset row whose CurrentArchivePath is that relative path.
func (h *folderHarness) seed(relPath string) *domain.Asset {
	h.t.Helper()
	abs := filepath.Join(h.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	a := &domain.Asset{
		OriginalFilename:   filepath.Base(relPath),
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + relPath,
		CurrentArchivePath: relPath,
		MediaType:          domain.MediaTypePhoto,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
	}
	if err := h.assets.Create(context.Background(), a); err != nil {
		h.t.Fatalf("create asset: %v", err)
	}
	return a
}

func (h *folderHarness) pathOf(id string) string {
	h.t.Helper()
	a, err := h.assets.GetByID(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get asset: %v", err)
	}
	return a.CurrentArchivePath
}

func TestListFolderRootAndDrillIn(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	h.seed("2019/2019-06-12 Trip/IMG_1.jpg")
	h.seed("2019/2019-06-12 Trip/RAW/IMG_1.cr3")
	h.seed("2019/2019-08-01/IMG_2.jpg")
	h.seed("2020/2020-01-01/IMG_3.jpg")

	// Root lists the two year folders.
	root, err := h.svc.ListFolder(ctx, "", 1, 60)
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(root.Subfolders) != 2 {
		t.Fatalf("root subfolders = %d, want 2", len(root.Subfolders))
	}
	if root.Subfolders[0].Name != "2019" || root.Subfolders[0].AssetCount != 3 {
		t.Errorf("2019 folder = %+v, want name 2019 count 3", root.Subfolders[0])
	}
	if root.Subfolders[0].IsDateFolder {
		t.Errorf("year folder must not be flagged a date folder")
	}
	if root.Assets.Total != 0 {
		t.Errorf("root direct assets = %d, want 0", root.Assets.Total)
	}

	// Into 2019: two date folders, one of which is renameable.
	y2019, err := h.svc.ListFolder(ctx, "2019", 1, 60)
	if err != nil {
		t.Fatalf("list 2019: %v", err)
	}
	if len(y2019.Subfolders) != 2 {
		t.Fatalf("2019 subfolders = %d, want 2", len(y2019.Subfolders))
	}
	var trip *FolderEntryDTO
	for i := range y2019.Subfolders {
		if y2019.Subfolders[i].Name == "2019-06-12 Trip" {
			trip = &y2019.Subfolders[i]
		}
	}
	if trip == nil {
		t.Fatal("did not find 2019-06-12 Trip subfolder")
	}
	if !trip.IsDateFolder {
		t.Errorf("date-event folder must be flagged renameable")
	}
	if trip.AssetCount != 2 { // IMG_1.jpg + RAW/IMG_1.cr3 (recursive)
		t.Errorf("Trip asset count = %d, want 2 (recursive incl RAW/)", trip.AssetCount)
	}
	if trip.CoverAssetID == "" {
		t.Errorf("Trip should have a cover asset id")
	}

	// Into the Trip date folder: the JPEG is directly present; the RAW is in a
	// subfolder, not a direct asset.
	tripList, err := h.svc.ListFolder(ctx, "2019/2019-06-12 Trip", 1, 60)
	if err != nil {
		t.Fatalf("list trip: %v", err)
	}
	if !tripList.IsDateFolder || tripList.Label != "Trip" {
		t.Errorf("trip listing IsDateFolder=%v label=%q, want true/Trip", tripList.IsDateFolder, tripList.Label)
	}
	if tripList.Assets.Total != 1 || tripList.Assets.Items[0].Filename != "IMG_1.jpg" {
		t.Errorf("trip direct assets = %d (%v), want 1 IMG_1.jpg", tripList.Assets.Total, tripList.Assets.Items)
	}
	if len(tripList.Subfolders) != 1 || tripList.Subfolders[0].Name != "RAW" {
		t.Errorf("trip subfolders = %v, want [RAW]", tripList.Subfolders)
	}
}

func TestRenameEventFolderHappyPath(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	jpg := h.seed("2019/2019-06-12 Trip/IMG_1.jpg")
	raw := h.seed("2019/2019-06-12 Trip/RAW/IMG_1.cr3")
	// A boundary sibling that must NOT be touched.
	sib := h.seed("2019/2019-06-12 Trip2/OTHER.jpg")

	listing, err := h.svc.RenameEventFolder(ctx, "2019/2019-06-12 Trip", "Beach Day")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if listing.RelDir != "2019/2019-06-12 Beach Day" || listing.Label != "Beach Day" {
		t.Errorf("returned listing relDir=%q label=%q", listing.RelDir, listing.Label)
	}

	// Directory moved on disk.
	if _, err := os.Stat(filepath.Join(h.root, "2019", "2019-06-12 Trip")); !os.IsNotExist(err) {
		t.Errorf("old dir still present (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "2019", "2019-06-12 Beach Day", "RAW", "IMG_1.cr3")); err != nil {
		t.Errorf("RAW file not at new path: %v", err)
	}

	// Asset paths rewritten (RAW subpath rides along).
	if got := h.pathOf(jpg.ID); got != "2019/2019-06-12 Beach Day/IMG_1.jpg" {
		t.Errorf("jpg path = %q", got)
	}
	if got := h.pathOf(raw.ID); got != "2019/2019-06-12 Beach Day/RAW/IMG_1.cr3" {
		t.Errorf("raw path = %q", got)
	}
	// The boundary sibling is untouched.
	if got := h.pathOf(sib.ID); got != "2019/2019-06-12 Trip2/OTHER.jpg" {
		t.Errorf("boundary sibling path changed to %q", got)
	}
}

func TestRenameEventFolderEmptyLabelToBareDate(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	a := h.seed("2019/2019-06-12 Trip/IMG_1.jpg")

	if _, err := h.svc.RenameEventFolder(ctx, "2019/2019-06-12 Trip", "  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := h.pathOf(a.ID); got != "2019/2019-06-12/IMG_1.jpg" {
		t.Errorf("path = %q, want bare date folder", got)
	}
}

func TestRenameEventFolderTargetExistsRefused(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	h.seed("2019/2019-06-12 Trip/IMG_1.jpg")
	h.seed("2019/2019-06-12 Beach/IMG_2.jpg") // target already exists

	_, err := h.svc.RenameEventFolder(ctx, "2019/2019-06-12 Trip", "Beach")
	if err == nil {
		t.Fatal("expected refusal when target folder exists")
	}
	// Nothing moved.
	if _, err := os.Stat(filepath.Join(h.root, "2019", "2019-06-12 Trip", "IMG_1.jpg")); err != nil {
		t.Errorf("source disturbed after refusal: %v", err)
	}
}

func TestRenameEventFolderNonDateDirRefused(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	h.seed("2019/2019-06-12 Trip/IMG_1.jpg")

	if _, err := h.svc.RenameEventFolder(ctx, "2019", "Anything"); err == nil {
		t.Fatal("expected refusal for a non-date directory (year folder)")
	}
	if _, err := h.svc.RenameEventFolder(ctx, "2019/2019-06-12 Trip/RAW", "Anything"); err == nil {
		t.Fatal("expected refusal for a non-date directory (RAW subfolder)")
	}
}

func TestRenameEventFolderEscapeRootRefused(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	if _, err := h.svc.RenameEventFolder(ctx, "../evil", "x"); err == nil {
		t.Fatal("expected refusal for a path escaping the root")
	}
	if _, err := h.svc.RenameEventFolder(ctx, "2019/../../evil", "x"); err == nil {
		t.Fatal("expected refusal for a traversal path")
	}
}

func TestRenameEventFolderRefusedWhileBusy(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	h.seed("2019/2019-06-12 Trip/IMG_1.jpg")
	h.svc.activity = busyActivity{}

	_, err := h.svc.RenameEventFolder(ctx, "2019/2019-06-12 Trip", "Beach")
	if !errors.Is(err, ErrOperationActive) {
		t.Fatalf("err = %v, want ErrOperationActive", err)
	}
	// Untouched on disk.
	if _, err := os.Stat(filepath.Join(h.root, "2019", "2019-06-12 Trip")); err != nil {
		t.Errorf("source disturbed while busy: %v", err)
	}
}

// TestRenameEventFolderRollsBackOnDBFailure injects a DB error during the path
// rewrite and asserts the directory is renamed back so disk and catalog stay
// consistent.
func TestRenameEventFolderRollsBackOnDBFailure(t *testing.T) {
	h := newFolderHarness(t)
	ctx := context.Background()
	a := h.seed("2019/2019-06-12 Trip/IMG_1.jpg")

	// Force every UPDATE on the assets table to fail for the duration of the call.
	cb := h.gdb.Callback().Update().Before("gorm:update")
	if err := cb.Register("test_fail_asset_update", func(tx *gorm.DB) {
		if tx.Statement.Table == "assets" {
			tx.AddError(errors.New("injected update failure"))
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	_, err := h.svc.RenameEventFolder(ctx, "2019/2019-06-12 Trip", "Beach")
	// Remove the fault regardless of outcome.
	_ = h.gdb.Callback().Update().Remove("test_fail_asset_update")

	if err == nil {
		t.Fatal("expected rename to fail on injected DB error")
	}
	// Directory rolled back to its original name.
	if _, err := os.Stat(filepath.Join(h.root, "2019", "2019-06-12 Trip", "IMG_1.jpg")); err != nil {
		t.Errorf("directory not rolled back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "2019", "2019-06-12 Beach")); !os.IsNotExist(err) {
		t.Errorf("target dir lingered after rollback (err=%v)", err)
	}
	// Asset path unchanged.
	if got := h.pathOf(a.ID); got != "2019/2019-06-12 Trip/IMG_1.jpg" {
		t.Errorf("asset path changed despite rollback: %q", got)
	}
}

// busyActivity reports a single active operation.
type busyActivity struct{}

func (busyActivity) Snapshot() []OperationInfo {
	return []OperationInfo{{Kind: "import", Label: "Importing files"}}
}
