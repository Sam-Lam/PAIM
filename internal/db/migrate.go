package db

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// wrap is the standard error wrapper for this package.
func wrap(op string, err error) error { return fmt.Errorf("db: %s: %w", op, err) }

// Migration is one ordered, transactional schema/data upgrade. Run executes
// inside a transaction; the framework bumps SchemaVersion to ID when it commits.
// A Run error rolls the transaction back and aborts the open.
type Migration struct {
	ID          int
	Description string
	Run         func(tx *gorm.DB) error
}

// LibraryMigrations returns the ordered migration list for a library rooted at
// root. Migration 1 is the baseline (today's schema: AutoMigrate + indexes).
// Migration 2 converts absolute Asset.CurrentArchivePath values under root to
// root-relative forward-slash paths — the first real exercise of the framework
// and what makes libraries portable.
func LibraryMigrations(root string) []Migration {
	return []Migration{
		{
			ID:          1,
			Description: "baseline schema (AutoMigrate + indexes)",
			Run: func(tx *gorm.DB) error {
				if err := tx.AutoMigrate(domain.AllModels()...); err != nil {
					return wrap("baseline auto-migrate", err)
				}
				return createIndexes(tx)
			},
		},
		{
			ID:          2,
			Description: "convert archive paths to library-relative",
			Run:         migrateRelativePaths(root),
		},
		{
			ID:          3,
			Description: "add backup provider media_scope column",
			Run: func(tx *gorm.DB) error {
				// AutoMigrate is additive and idempotent: it adds the new
				// BackupProvider.MediaScope column on existing catalogs (fresh ones
				// already have it from migration 1). Existing rows default to the empty
				// scope, which means "all kinds" — so scoping is backward compatible.
				if err := tx.AutoMigrate(&domain.BackupProvider{}); err != nil {
					return wrap("add media_scope column", err)
				}
				return nil
			},
		},
		{
			ID:          4,
			Description: "add import_failures table (structured per-file import failures)",
			Run: func(tx *gorm.DB) error {
				// Additive and idempotent: AutoMigrate creates the import_failures table
				// on existing catalogs (fresh ones already have it from migration 1,
				// which migrates domain.AllModels()). Sessions that predate this table
				// carry a Failures counter but no rows — the UI keeps the log-only view
				// for them. Recreate the explicit indexes so an upgraded catalog matches
				// a fresh one regardless of struct-tag drift.
				if err := tx.AutoMigrate(&domain.ImportFailure{}); err != nil {
					return wrap("add import_failures table", err)
				}
				return createImportFailureIndexes(tx)
			},
		},
	}
}

// LatestSchemaVersion is the highest migration ID PAIM knows about.
func LatestSchemaVersion() int { return maxMigrationID(LibraryMigrations("")) }

func maxMigrationID(migs []Migration) int {
	max := 0
	for _, m := range migs {
		if m.ID > max {
			max = m.ID
		}
	}
	return max
}

// Migrate brings gdb up to the latest of migs, backing up first. It:
//   - ensures the meta table exists and reads the current schema version;
//   - REFUSES (error, no changes) if the DB is newer than migs cover;
//   - when behind, checkpoints the WAL and copies the DB (plus -wal/-shm) into
//     "<root>/.paim/backups/paim-v<current>-<timestamp>.db" BEFORE any change;
//   - runs each pending migration in its own transaction, bumping SchemaVersion
//     as each commits. A failed migration rolls back and aborts with the backup
//     path in the error.
//
// It returns the backup path it created (empty when the DB was already current).
func Migrate(gdb *gorm.DB, dbPath, root string, migs []Migration) (string, error) {
	if err := ensureMetaTable(gdb); err != nil {
		return "", err
	}
	current, _, err := readSchemaVersion(gdb)
	if err != nil {
		return "", err
	}
	latest := maxMigrationID(migs)
	if current > latest {
		return "", fmt.Errorf(
			"db: this library was written by a newer version of PAIM (schema v%d, this app supports v%d) — update the app to open it",
			current, latest)
	}
	if current >= latest {
		return "", nil
	}

	backupPath, err := backupDatabase(gdb, dbPath, root, current)
	if err != nil {
		return "", fmt.Errorf("db: pre-migration backup failed; database unchanged: %w", err)
	}

	for _, m := range migs {
		if m.ID <= current {
			continue
		}
		if err := runMigration(gdb, m); err != nil {
			return backupPath, fmt.Errorf(
				"db: migration %d (%s) failed and was rolled back; database left at schema v%d, backup at %q: %w",
				m.ID, m.Description, current, backupPath, err)
		}
		slog.Default().Info("db: migration applied", "subsystem", "db", "id", m.ID, "description", m.Description)
		current = m.ID
	}
	return backupPath, nil
}

// runMigration runs one migration's Run and bumps the schema version to its ID,
// all in a single transaction so a failure leaves the version untouched.
func runMigration(gdb *gorm.DB, m Migration) error {
	return gdb.Transaction(func(tx *gorm.DB) error {
		if err := m.Run(tx); err != nil {
			return err
		}
		return bumpSchemaVersion(tx, m.ID)
	})
}

// backupDatabase checkpoints the WAL and copies the database file (and any
// lingering -wal/-shm sidecars) to a timestamped file under
// "<root>/.paim/backups". Old backups are never auto-deleted.
func backupDatabase(gdb *gorm.DB, dbPath, root string, fromVersion int) (string, error) {
	// Fold the WAL back into the main DB file so the copy is self-contained.
	if err := gdb.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		return "", wrap("wal checkpoint before backup", err)
	}
	backupsDir := filepath.Join(root, ".paim", "backups")
	if err := os.MkdirAll(backupsDir, 0o755); err != nil {
		return "", wrap("create backups dir", err)
	}
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(backupsDir, fmt.Sprintf("paim-v%d-%s.db", fromVersion, stamp))
	if err := copyFile(dbPath, dst); err != nil {
		return "", err
	}
	// Best-effort copy of sidecars (they should be empty after the checkpoint).
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			_ = copyFile(dbPath+suffix, dst+suffix)
		}
	}
	return dst, nil
}

// copyFile copies src to dst, fsyncing the destination.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return wrap("open db for backup", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return wrap("create backup file", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return wrap("copy db to backup", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return wrap("fsync backup", err)
	}
	if err := out.Close(); err != nil {
		return wrap("close backup", err)
	}
	return nil
}

// migrateRelativePaths returns the Run for migration 2: rewrite every absolute
// Asset.CurrentArchivePath under root to a root-relative forward-slash path.
// Paths outside root are left absolute and counted. Soft-deleted rows (trashed
// duplicates whose files live under "<root>/.paim-trash") are included so their
// recorded locations remain resolvable.
func migrateRelativePaths(root string) func(tx *gorm.DB) error {
	return func(tx *gorm.DB) error {
		var assets []domain.Asset
		if err := tx.Unscoped().Where("current_archive_path <> ''").Find(&assets).Error; err != nil {
			return wrap("load assets for path conversion", err)
		}

		// Compute every new (relative) path in Go — the conversion result is
		// byte-for-byte identical to the previous per-row loop — then flush the
		// updates in batches so the whole migration is a handful of statements
		// instead of one UPDATE per asset (the minute-plus tail on large catalogs).
		type update struct {
			id      string
			newPath string
		}
		var updates []update
		kept := 0
		for _, a := range assets {
			p := a.CurrentArchivePath
			if !filepath.IsAbs(p) {
				continue // already relative
			}
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				kept++
				continue
			}
			updates = append(updates, update{id: a.ID, newPath: filepath.ToSlash(rel)})
		}

		const chunk = 500
		for start := 0; start < len(updates); start += chunk {
			end := start + chunk
			if end > len(updates) {
				end = len(updates)
			}
			batch := updates[start:end]

			// One statement per chunk:
			//   UPDATE assets SET current_archive_path = CASE id
			//       WHEN ? THEN ? ... END
			//   WHERE id IN (?, ?, ...)
			var sb strings.Builder
			sb.WriteString("UPDATE assets SET current_archive_path = CASE id")
			args := make([]interface{}, 0, len(batch)*3)
			for _, u := range batch {
				sb.WriteString(" WHEN ? THEN ?")
				args = append(args, u.id, u.newPath)
			}
			sb.WriteString(" END WHERE id IN (")
			for i, u := range batch {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString("?")
				args = append(args, u.id)
			}
			sb.WriteString(")")

			if err := tx.Exec(sb.String(), args...).Error; err != nil {
				return wrap(fmt.Sprintf("batch update archive paths [%d:%d]", start, end), err)
			}
		}

		slog.Default().Info("db: archive paths converted to relative",
			"subsystem", "db", "root", root, "converted", len(updates), "keptAbsolute", kept)
		return nil
	}
}
