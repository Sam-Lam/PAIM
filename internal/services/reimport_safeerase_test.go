package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// TestReimportAlreadyImportedThenSafeToEraseGreen proves the full loop closes
// after the already-imported rework: importing a "card" and then re-importing the
// SAME card records ZERO new assets and ZERO duplicates (every file counts as
// already-imported), AND a safe-to-erase evaluation over that card still comes out
// green — because it maps the card's files to verified, backed-up archived assets
// by content, never via any placeholder duplicate row.
func TestReimportAlreadyImportedThenSafeToEraseGreen(t *testing.T) {
	svc, _ := newSourcesService(t) // SourcesService + seeded required provider
	gdb := svc.db
	assets := svc.assets
	ctx := context.Background()

	sessions := repo.NewSessionRepo(gdb)
	failures := repo.NewImportFailureRepo(gdb)
	dest := t.TempDir()
	pipe := importer.New(importer.Config{
		DB:       gdb,
		Assets:   assets,
		Sessions: sessions,
		Failures: failures,
		Layout:   archive.New(dest),
	})

	// A "card" holding one media file (mtime in the past so the safe-to-erase
	// fast-path's modtime<=importDate check is unambiguous).
	card := t.TempDir()
	cardFile := filepath.Join(card, "IMG_1.jpg")
	if err := os.WriteFile(cardFile, []byte("photo-bytes-xyz"), 0o644); err != nil {
		t.Fatalf("write card file: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cardFile, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	optsCopy := importer.Options{Mode: importer.ModeCopy, SourceRoot: card, DestinationRoot: dest, EventName: "Trip"}
	s1, err := pipe.Run(ctx, optsCopy, nil)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if s1.FilesImported != 1 {
		t.Fatalf("first import FilesImported = %d, want 1", s1.FilesImported)
	}

	// Backups run async after import; simulate them completing so the archived
	// asset is verified + fully backed up (the safe-to-erase precondition).
	if err := gdb.Model(&domain.Asset{}).Where("session_id = ?", s1.ID).
		Update("backup_status", domain.BackupStatusComplete).Error; err != nil {
		t.Fatalf("mark backups complete: %v", err)
	}

	assetsBefore := countAssets(t, gdb)

	// Re-import the SAME card: every file is already archived -> already-imported;
	// no new assets, no duplicates.
	s2, err := pipe.Run(ctx, optsCopy, nil)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if s2.FilesImported != 0 || s2.AlreadyImported != 1 || s2.Duplicates != 0 {
		t.Fatalf("re-import counters imported=%d already=%d dup=%d, want 0/1/0",
			s2.FilesImported, s2.AlreadyImported, s2.Duplicates)
	}
	if got := countAssets(t, gdb); got != assetsBefore {
		t.Fatalf("re-import created assets: %d -> %d, want unchanged", assetsBefore, got)
	}
	// No placeholder/duplicate rows exist anywhere.
	var dupCount int64
	gdb.Model(&domain.Asset{}).Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").Count(&dupCount)
	if dupCount != 0 {
		t.Fatalf("duplicate rows = %d, want 0", dupCount)
	}

	// Safe-to-erase over the card is GREEN: the file maps to a verified archived
	// asset with backups complete.
	got := evaluateGreen(t, svc, card)
	if got.Report == nil || !got.Report.Safe {
		t.Fatalf("safe-to-erase not green: %+v", got.Report)
	}
}

func countAssets(t *testing.T, gdb *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := gdb.Model(&domain.Asset{}).Count(&n).Error; err != nil {
		t.Fatalf("count assets: %v", err)
	}
	return n
}
