package library

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/hashing"
	"gorm.io/gorm"
)

// LegacyBackupSuffix is appended to the original per-machine DB after a
// successful legacy migration, so the source is preserved, never deleted.
const LegacyBackupSuffix = ".pre-library-backup"

// LegacyDBPath returns the pre-library per-machine catalog location,
// "~/Library/Application Support/PAIM/paim.db".
func LegacyDBPath() (string, error) { return db.DefaultPath() }

// LegacyExists reports whether a pre-library per-machine catalog exists on this
// machine (so Welcome can offer to migrate it).
func LegacyExists() bool {
	path, err := LegacyDBPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// InstallLegacy copies the pre-library per-machine catalog into a portable
// library at targetRoot WITHOUT opening it: it copies the legacy DB into
// "<targetRoot>/.paim/paim.db" (verifying the copy with BLAKE3) and renames the
// original to "paim.db.pre-library-backup" (never deleting the source). The
// caller then opens the library normally, which runs the migration framework —
// including the absolute→relative archive-path conversion. It refuses to
// overwrite an existing library at targetRoot.
func InstallLegacy(ctx context.Context, legacyDBPath, targetRoot string) error {
	if legacyDBPath == "" {
		var err error
		if legacyDBPath, err = LegacyDBPath(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(legacyDBPath); err != nil {
		return fmt.Errorf("library: legacy catalog %q not found: %w", legacyDBPath, err)
	}
	if HasLibrary(targetRoot) {
		return fmt.Errorf("library: a library already exists at %q — open it instead of migrating", targetRoot)
	}
	if _, err := EnsureMetaDir(targetRoot); err != nil {
		return err
	}
	dest := DBPath(targetRoot)

	// Fold any WAL back into the main DB file so the byte copy is self-contained.
	if err := checkpointLegacy(legacyDBPath); err != nil {
		return err
	}
	if err := copyVerify(ctx, legacyDBPath, dest); err != nil {
		return err
	}

	// Preserve the original, renamed — never delete the source.
	backup := legacyDBPath + LegacyBackupSuffix
	if err := os.Rename(legacyDBPath, backup); err != nil {
		return fmt.Errorf("library: preserve original catalog as %q: %w", backup, err)
	}
	// Clean up the now-orphaned legacy sidecars (their contents were folded in).
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(legacyDBPath + suffix)
	}
	return nil
}

// MigrateLegacy installs the legacy catalog into targetRoot (see InstallLegacy)
// and opens it, returning the opened catalog and its metadata. It is a
// convenience for standalone/test use; the app installs then opens via its normal
// open path.
func MigrateLegacy(ctx context.Context, legacyDBPath, targetRoot, appVersion string) (*gorm.DB, *db.Meta, error) {
	if err := InstallLegacy(ctx, legacyDBPath, targetRoot); err != nil {
		return nil, nil, err
	}
	return db.OpenLibrary(targetRoot, appVersion)
}

// checkpointLegacy opens the legacy DB just long enough to fold its WAL into the
// main file, then closes it, so the subsequent copy is a complete snapshot.
func checkpointLegacy(path string) error {
	gdb, err := db.Open(path)
	if err != nil {
		return fmt.Errorf("library: open legacy catalog for checkpoint: %w", err)
	}
	_ = gdb.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error
	sqlDB, err := gdb.DB()
	if err != nil {
		return fmt.Errorf("library: access legacy sql handle: %w", err)
	}
	return sqlDB.Close()
}

// copyVerify copies src to dst and confirms the copy with a BLAKE3 full-hash
// comparison before returning. dst must not already exist.
func copyVerify(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("library: open legacy catalog %q: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("library: create library catalog %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("library: copy legacy catalog: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("library: fsync library catalog: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("library: close library catalog: %w", err)
	}

	srcHash, err := hashing.FullHash(ctx, src)
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("library: hash legacy catalog: %w", err)
	}
	dstHash, err := hashing.FullHash(ctx, dst)
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("library: hash library catalog: %w", err)
	}
	if srcHash != dstHash {
		_ = os.Remove(dst)
		return fmt.Errorf("library: legacy catalog copy verification failed (hash mismatch)")
	}
	return nil
}
