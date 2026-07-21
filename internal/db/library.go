package db

import (
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
)

// Meta describes an opened library's catalog: where it lives and the versioning
// metadata recorded in library_meta.
type Meta struct {
	Root                   string
	DBPath                 string
	SchemaVersion          int
	CreatedByAppVersion    string
	CreatedAt              time.Time
	LastOpenedByAppVersion string
	LastOpenedAt           time.Time
}

// OpenLibrary opens (or creates) the catalog for the library rooted at root:
// "<root>/.paim/paim.db". It applies the standard pragmas and indexes (via Open),
// ensures the library_meta row, runs any pending migrations (backing up first),
// and stamps LastOpenedByAppVersion/At. A catalog from a newer schema than
// appVersion supports is refused (see Migrate).
//
// It is additive over Open: plain db.Open is untouched and continues to serve the
// dev escape hatch and all existing tests.
func OpenLibrary(root, appVersion string) (*gorm.DB, *Meta, error) {
	metaDir := filepath.Join(root, ".paim")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return nil, nil, wrap("create library meta dir", err)
	}
	dbPath := filepath.Join(metaDir, DBFileName())
	existed := fileExists(dbPath)

	gdb, err := Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureMetaTable(gdb); err != nil {
		return nil, nil, err
	}

	latest := LatestSchemaVersion()
	if !existed {
		// A brand-new library: Open already built the current schema, so mark it at
		// the latest version without running (or backing up for) historical
		// migrations. Migration 2 on an empty catalog would be a no-op anyway.
		if err := gdb.Transaction(func(tx *gorm.DB) error { return bumpSchemaVersion(tx, latest) }); err != nil {
			return nil, nil, err
		}
		if err := stampCreated(gdb, appVersion); err != nil {
			return nil, nil, err
		}
	} else {
		if _, err := Migrate(gdb, dbPath, root, LibraryMigrations(root)); err != nil {
			return nil, nil, err
		}
		if err := stampCreatedIfEmpty(gdb, appVersion); err != nil {
			return nil, nil, err
		}
	}

	if err := stampOpened(gdb, appVersion); err != nil {
		return nil, nil, err
	}

	meta, err := loadMeta(gdb)
	if err != nil {
		return nil, nil, err
	}
	m := &Meta{
		Root:                   root,
		DBPath:                 dbPath,
		SchemaVersion:          meta.SchemaVersion,
		CreatedByAppVersion:    meta.CreatedByAppVersion,
		CreatedAt:              meta.CreatedAt,
		LastOpenedByAppVersion: meta.LastOpenedByAppVersion,
		LastOpenedAt:           meta.LastOpenedAt,
	}
	return gdb, m, nil
}

// DBFileName is the catalog file name inside "<root>/.paim".
func DBFileName() string { return "paim.db" }

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
