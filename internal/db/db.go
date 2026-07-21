// Package db opens and migrates the PAIM SQLite database. It configures the
// pragmas required for durability and correctness (WAL, foreign keys, busy
// timeout, synchronous=NORMAL), runs AutoMigrate for every domain model, and
// ensures explicit indexes exist on all hash and foreign-key columns.
package db

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DefaultPath returns the standard on-disk location of the PAIM database,
// ~/Library/Application Support/PAIM/paim.db. The parent directory is not
// created here; Open creates it.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("db: resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "PAIM", "paim.db"), nil
}

// Open creates the parent directory for path if needed, opens the SQLite
// database with the required pragmas, migrates all domain models, and creates
// the explicit indexes PAIM relies on. The returned *gorm.DB is safe for
// concurrent use.
func Open(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("db: create parent directory %q: %w", dir, err)
		}
	}

	// Pragmas are supplied in the DSN so they are applied to every connection in
	// the pool (foreign_keys and busy_timeout are per-connection settings). They
	// are also asserted explicitly below.
	dsn := fmt.Sprintf(
		// _txlock=immediate makes every write transaction take the write lock at
		// BEGIN instead of on first write, so concurrent writers (import pipeline
		// vs backup workers) queue on busy_timeout rather than deadlocking with
		// SQLITE_BUSY on a deferred-to-write lock upgrade.
		"file:%s?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate",
		path,
	)

	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("db: open sqlite at %q: %w", path, err)
	}

	if err := applyPragmas(gdb); err != nil {
		return nil, err
	}

	if err := gdb.AutoMigrate(domain.AllModels()...); err != nil {
		return nil, fmt.Errorf("db: auto-migrate: %w", err)
	}

	if err := createIndexes(gdb); err != nil {
		return nil, err
	}

	return gdb, nil
}

// applyPragmas asserts the durability/correctness pragmas on the active
// connection. journal_mode=WAL persists at the database level; the others are
// per-connection and also set via the DSN for pooled connections.
func applyPragmas(gdb *gorm.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if err := gdb.Exec(p).Error; err != nil {
			return fmt.Errorf("db: apply %q: %w", p, err)
		}
	}
	return nil
}

// indexSpec describes one explicit index to create if it does not already exist.
type indexSpec struct {
	name    string
	table   string
	columns string
}

// createIndexes creates the indexes PAIM depends on for hash lookups and
// foreign-key joins. AutoMigrate already creates most of these from struct
// tags; these CREATE INDEX IF NOT EXISTS statements make the guarantee explicit
// and independent of the tags.
func createIndexes(gdb *gorm.DB) error {
	specs := []indexSpec{
		{"idx_assets_quick_hash", "assets", "quick_hash"},
		{"idx_assets_full_hash", "assets", "full_hash"},
		{"idx_assets_source_id", "assets", "source_id"},
		{"idx_assets_session_id", "assets", "session_id"},
		{"idx_assets_duplicate_of", "assets", "duplicate_of_asset_id"},
		{"idx_backup_jobs_asset_id", "backup_jobs", "asset_id"},
		{"idx_backup_jobs_status", "backup_jobs", "status"},
		{"idx_log_entries_timestamp", "log_entries", "timestamp"},
		{"idx_log_entries_level", "log_entries", "level"},
		{"idx_log_entries_subsystem", "log_entries", "subsystem"},
	}
	for _, s := range specs {
		stmt := fmt.Sprintf(
			"CREATE INDEX IF NOT EXISTS %s ON %s(%s)",
			s.name, s.table, s.columns,
		)
		if err := gdb.Exec(stmt).Error; err != nil {
			return fmt.Errorf("db: create index %s: %w", s.name, err)
		}
	}
	return nil
}
