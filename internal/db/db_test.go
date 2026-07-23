package db

import (
	"path/filepath"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

func TestOpenMigratesAndSetsPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "paim.db")
	gdb, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Every model table must exist after migration.
	for _, m := range domain.AllModels() {
		if !gdb.Migrator().HasTable(m) {
			t.Errorf("expected table for %T to exist", m)
		}
	}

	// WAL journal mode is persistent and database-wide.
	var journalMode string
	if err := gdb.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	// foreign_keys must be ON (1) on the connection.
	var foreignKeys int
	if err := gdb.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	// busy_timeout must be 5000ms.
	var busyTimeout int
	if err := gdb.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	// synchronous NORMAL == 1.
	var synchronous int
	if err := gdb.Raw("PRAGMA synchronous").Scan(&synchronous).Error; err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}

func TestOpenCreatesExplicitIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paim.db")
	gdb, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	wantIndexes := []string{
		"idx_assets_quick_hash",
		"idx_assets_full_hash",
		"idx_assets_source_id",
		"idx_assets_session_id",
		"idx_assets_duplicate_of",
		"idx_assets_original_full_path",
		"idx_backup_jobs_asset_id",
		"idx_backup_jobs_status",
		"idx_log_entries_timestamp",
		"idx_log_entries_level",
		"idx_log_entries_subsystem",
	}
	for _, name := range wantIndexes {
		var count int
		err := gdb.Raw(
			"SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name,
		).Scan(&count).Error
		if err != nil {
			t.Fatalf("query index %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("index %s: found %d, want 1", name, count)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paim.db")
	if _, err := Open(path); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Re-opening must not fail (migrations and index creation are idempotent).
	if _, err := Open(path); err != nil {
		t.Fatalf("second Open: %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if filepath.Base(p) != "paim.db" {
		t.Errorf("DefaultPath base = %q, want paim.db", filepath.Base(p))
	}
	if filepath.Base(filepath.Dir(p)) != "PAIM" {
		t.Errorf("DefaultPath parent dir = %q, want PAIM", filepath.Base(filepath.Dir(p)))
	}
}
