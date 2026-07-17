package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
	"gorm.io/gorm"
)

// newDuplicateHarness builds a DuplicateService over a temp SQLite DB.
func newDuplicateHarness(t *testing.T) (*DuplicateService, *gorm.DB, *repo.AssetRepo) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "dup.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	settings := repo.NewSettingsRepo(gdb)
	svc := NewDuplicateService(gdb, assets, settings, nil)
	return svc, gdb, assets
}

func seedAsset(t *testing.T, assets *repo.AssetRepo, filename, path string, dupOf *string) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   filename,
		QuickHash:          "qh-" + filename,
		CurrentArchivePath: path,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
		DuplicateOfAssetID: dupOf,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

func TestResolveDeleteTrashesFileFirstAndRecordsPath(t *testing.T) {
	svc, gdb, assets := newDuplicateHarness(t)
	ctx := context.Background()

	root := t.TempDir()
	archivePath := filepath.Join(root, "2023", "dup.jpg")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(archivePath, []byte("dup-bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	orig := seedAsset(t, assets, "orig.jpg", filepath.Join(root, "orig.jpg"), nil)
	dup := seedAsset(t, assets, "dup.jpg", archivePath, &orig.ID)

	if err := svc.ResolveDuplicate(ctx, dup.ID, DuplicateActionDelete, ""); err != nil {
		t.Fatalf("resolve delete: %v", err)
	}

	// The original file was moved into the trash (not left in place, not removed).
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("archive file should have been moved to trash, stat err = %v", err)
	}

	// The row is soft-deleted and its recorded path points at the trash location.
	var row domain.Asset
	if err := gdb.Unscoped().First(&row, "id = ?", dup.ID).Error; err != nil {
		t.Fatalf("load soft-deleted row: %v", err)
	}
	if !row.Deleted {
		t.Fatalf("asset should be soft-deleted")
	}
	if !strings.Contains(row.CurrentArchivePath, trashDirName) {
		t.Fatalf("current path %q should point into trash", row.CurrentArchivePath)
	}
	if _, err := os.Stat(row.CurrentArchivePath); err != nil {
		t.Fatalf("trashed file should exist at recorded path: %v", err)
	}
}

func TestResolveDeleteTrashFailureDoesNotSoftDelete(t *testing.T) {
	svc, gdb, assets := newDuplicateHarness(t)
	ctx := context.Background()

	// Archive path points at a file that does not exist, so trashing fails.
	missing := filepath.Join(t.TempDir(), "gone.jpg")
	dup := seedAsset(t, assets, "gone.jpg", missing, strPtr("orig"))

	if err := svc.ResolveDuplicate(ctx, dup.ID, DuplicateActionDelete, ""); err == nil {
		t.Fatalf("expected error when trashing a missing file")
	}

	// The row must NOT be soft-deleted (DB never claims a gone file).
	var count int64
	gdb.Model(&domain.Asset{}).Where("id = ?", dup.ID).Count(&count)
	if count != 1 {
		t.Fatalf("asset should remain live after trash failure, count = %d", count)
	}
}

func TestResolveMoveUpdatesPath(t *testing.T) {
	svc, gdb, assets := newDuplicateHarness(t)
	ctx := context.Background()

	root := t.TempDir()
	archivePath := filepath.Join(root, "src", "move.jpg")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(archivePath, []byte("move-bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dup := seedAsset(t, assets, "move.jpg", archivePath, strPtr("orig"))

	destFolder := filepath.Join(root, "dest")
	if err := svc.ResolveDuplicate(ctx, dup.ID, DuplicateActionMove, destFolder); err != nil {
		t.Fatalf("resolve move: %v", err)
	}

	moved := filepath.Join(destFolder, "move.jpg")
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("moved file should exist: %v", err)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("original should be gone after same-volume move, err = %v", err)
	}

	var row domain.Asset
	if err := gdb.First(&row, "id = ?", dup.ID).Error; err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.CurrentArchivePath != moved {
		t.Fatalf("CurrentArchivePath = %q, want %q", row.CurrentArchivePath, moved)
	}
}

func TestListDuplicatesTotalIsTrueCount(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()

	orig := seedAsset(t, assets, "orig.jpg", "/x/orig.jpg", nil)
	for i := 0; i < 3; i++ {
		seedAsset(t, assets, "dup.jpg", "", &orig.ID)
	}

	// Page size 1 returns one item but the total must reflect all three duplicates.
	res, err := svc.ListDuplicates(ctx, 1, 1)
	if err != nil {
		t.Fatalf("list duplicates: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %d, want 1 (page size)", len(res.Items))
	}
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3 (true count)", res.Total)
	}
}

func strPtr(s string) *string { return &s }
