package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autolinepro/paim/internal/domain"
	"gorm.io/gorm"
)

// openAt opens a catalog at "<root>/.paim/paim.db" and returns it plus the path.
func openAt(t *testing.T, root string) (*gorm.DB, string) {
	t.Helper()
	dbPath := filepath.Join(root, ".paim", "paim.db")
	gdb, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ensureMetaTable(gdb); err != nil {
		t.Fatalf("ensureMetaTable: %v", err)
	}
	return gdb, dbPath
}

func setVersion(t *testing.T, gdb *gorm.DB, v int) {
	t.Helper()
	if err := gdb.Transaction(func(tx *gorm.DB) error { return bumpSchemaVersion(tx, v) }); err != nil {
		t.Fatalf("set version: %v", err)
	}
}

func insertAsset(t *testing.T, gdb *gorm.DB, path string) string {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   filepath.Base(path),
		QuickHash:          "qh-" + path,
		CurrentArchivePath: path,
		VerificationStatus: domain.VerificationStatusVerified,
	}
	if err := gdb.Create(a).Error; err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a.ID
}

func assetPath(t *testing.T, gdb *gorm.DB, id string) string {
	t.Helper()
	var a domain.Asset
	if err := gdb.Unscoped().Take(&a, "id = ?", id).Error; err != nil {
		t.Fatalf("load asset: %v", err)
	}
	return a.CurrentArchivePath
}

func TestMigrateBehindRunsPendingAndBacksUp(t *testing.T) {
	root := t.TempDir()
	gdb, dbPath := openAt(t, root)
	setVersion(t, gdb, 1) // one behind latest (2)

	inside := filepath.Join(root, "2024", "2024-01-01", "IMG.JPG")
	outside := filepath.Join(t.TempDir(), "elsewhere.JPG")
	insideID := insertAsset(t, gdb, inside)
	outsideID := insertAsset(t, gdb, outside)

	backup, err := Migrate(gdb, dbPath, root, LibraryMigrations(root))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A backup was written before migrating.
	if backup == "" {
		t.Fatalf("expected a backup path")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup file should exist: %v", err)
	}
	if !strings.Contains(backup, filepath.Join(".paim", "backups")) {
		t.Fatalf("backup should live under .paim/backups: %q", backup)
	}

	// Version advanced to latest.
	v, _, _ := readSchemaVersion(gdb)
	if v != LatestSchemaVersion() {
		t.Fatalf("schema version = %d, want %d", v, LatestSchemaVersion())
	}

	// Inside-root path converted to relative (forward slash); outside kept absolute.
	if got := assetPath(t, gdb, insideID); got != "2024/2024-01-01/IMG.JPG" {
		t.Fatalf("inside path = %q, want relative", got)
	}
	if got := assetPath(t, gdb, outsideID); got != outside {
		t.Fatalf("outside path = %q, want kept absolute %q", got, outside)
	}
}

func TestMigrateAheadRefuses(t *testing.T) {
	root := t.TempDir()
	gdb, dbPath := openAt(t, root)
	setVersion(t, gdb, 99) // newer than this app knows

	_, err := Migrate(gdb, dbPath, root, LibraryMigrations(root))
	if err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("expected newer-version refusal, got: %v", err)
	}
	// No backup directory should have been created (refused before backup).
	if _, statErr := os.Stat(filepath.Join(root, ".paim", "backups")); statErr == nil {
		t.Fatalf("no backup should be created when refusing an ahead schema")
	}
}

func TestMigrateFailedRollsBack(t *testing.T) {
	root := t.TempDir()
	gdb, dbPath := openAt(t, root)
	setVersion(t, gdb, 1)

	id := insertAsset(t, gdb, filepath.Join(root, "keep.JPG"))

	boom := errors.New("boom")
	migs := []Migration{
		{ID: 1, Description: "base", Run: func(*gorm.DB) error { return nil }},
		{ID: 2, Description: "boom", Run: func(tx *gorm.DB) error {
			// Mutate then fail: the mutation must roll back with the transaction.
			if err := tx.Model(&domain.Asset{}).Where("id = ?", id).Update("current_archive_path", "MUTATED").Error; err != nil {
				return err
			}
			return boom
		}},
	}

	backup, err := Migrate(gdb, dbPath, root, migs)
	if err == nil {
		t.Fatalf("expected migration failure")
	}
	if !strings.Contains(err.Error(), backup) || backup == "" {
		t.Fatalf("error should carry the backup path %q: %v", backup, err)
	}
	// Version stayed at 1; the partial write rolled back.
	if v, _, _ := readSchemaVersion(gdb); v != 1 {
		t.Fatalf("version = %d, want 1 after rollback", v)
	}
	if got := assetPath(t, gdb, id); got == "MUTATED" {
		t.Fatalf("failed migration write should have rolled back, path = %q", got)
	}
}

func TestOpenLibraryFreshAndReopen(t *testing.T) {
	root := t.TempDir()
	gdb, meta, err := OpenLibrary(root, "0.1.0")
	if err != nil {
		t.Fatalf("OpenLibrary: %v", err)
	}
	if meta.SchemaVersion != LatestSchemaVersion() {
		t.Fatalf("fresh schema = %d, want %d", meta.SchemaVersion, LatestSchemaVersion())
	}
	if meta.CreatedByAppVersion != "0.1.0" {
		t.Fatalf("CreatedByAppVersion = %q", meta.CreatedByAppVersion)
	}
	if meta.LastOpenedByAppVersion != "0.1.0" {
		t.Fatalf("LastOpenedByAppVersion = %q", meta.LastOpenedByAppVersion)
	}
	sqlDB, _ := gdb.DB()
	_ = sqlDB.Close()

	// A fresh library creates no pre-migration backup.
	if entries, _ := os.ReadDir(filepath.Join(root, ".paim", "backups")); len(entries) != 0 {
		t.Fatalf("fresh library should not create a backup, found %d", len(entries))
	}

	// Reopen is clean and idempotent.
	gdb2, meta2, err := OpenLibrary(root, "0.1.0")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if meta2.SchemaVersion != LatestSchemaVersion() {
		t.Fatalf("reopen schema = %d", meta2.SchemaVersion)
	}
	sqlDB2, _ := gdb2.DB()
	_ = sqlDB2.Close()
}

func TestOpenLibraryMigratesLegacyCatalog(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, ".paim", "paim.db")

	// Simulate a legacy (pre-versioning) catalog: plain Open, an absolute path, no
	// meta row.
	legacy, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	inside := filepath.Join(root, "2024", "IMG.JPG")
	id := insertAsset(t, legacy, inside)
	sqlDB, _ := legacy.DB()
	_ = sqlDB.Close()

	// Opening it as a library runs the framework, converting paths to relative.
	gdb, meta, err := OpenLibrary(root, "0.1.0")
	if err != nil {
		t.Fatalf("OpenLibrary legacy: %v", err)
	}
	if meta.SchemaVersion != LatestSchemaVersion() {
		t.Fatalf("migrated schema = %d", meta.SchemaVersion)
	}
	if got := assetPath(t, gdb, id); got != "2024/IMG.JPG" {
		t.Fatalf("legacy path not converted: %q", got)
	}
}
