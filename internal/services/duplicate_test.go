package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
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
	sessions := repo.NewSessionRepo(gdb)
	settings := repo.NewSettingsRepo(gdb)
	svc := NewDuplicateService(gdb, assets, sessions, settings, nil, nil)
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

func TestListDuplicatesPopulatesPresenceFlags(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()

	root := t.TempDir()
	svc.root = root

	// Original: an archived file that exists on disk (stored relative to root).
	origRel := filepath.Join("2026", "orig.jpg")
	origAbs := filepath.Join(root, origRel)
	if err := os.MkdirAll(filepath.Dir(origAbs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(origAbs, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	orig := seedAsset(t, assets, "orig.jpg", filepath.ToSlash(origRel), nil)

	// Copy-mode duplicate: no archive copy; file lives only at its source path,
	// which here exists (source still attached).
	srcAbs := filepath.Join(t.TempDir(), "card", "orig.jpg")
	if err := os.MkdirAll(filepath.Dir(srcAbs), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(srcAbs, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dup := &domain.Asset{
		OriginalFilename:   "orig.jpg",
		QuickHash:          "qh-dup",
		CurrentArchivePath: "", // never copied
		OriginalFullPath:   srcAbs,
		VerificationStatus: domain.VerificationStatusVerified,
		DuplicateOfAssetID: &orig.ID,
	}
	if err := assets.Create(ctx, dup); err != nil {
		t.Fatalf("create dup: %v", err)
	}

	res, err := svc.ListDuplicates(ctx, 1, 20)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(res.Items))
	}
	it := res.Items[0]
	if it.DuplicateHasArchiveCopy {
		t.Error("copy-mode duplicate must report DuplicateHasArchiveCopy=false")
	}
	if !it.DuplicateFileExists {
		t.Error("duplicate source file exists; want DuplicateFileExists=true")
	}
	if !it.OriginalFileExists {
		t.Error("original archive file exists; want OriginalFileExists=true")
	}

	// Now remove the source file: the duplicate becomes unreachable.
	if err := os.Remove(srcAbs); err != nil {
		t.Fatalf("remove src: %v", err)
	}
	res, err = svc.ListDuplicates(ctx, 1, 20)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if res.Items[0].DuplicateFileExists {
		t.Error("source removed; want DuplicateFileExists=false")
	}
	if !res.Items[0].OriginalFileExists {
		t.Error("original still present; want OriginalFileExists=true")
	}
}

func strPtr(s string) *string { return &s }
