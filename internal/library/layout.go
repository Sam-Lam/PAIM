package library

import (
	"fmt"
	"os"
	"path/filepath"
)

// On-disk layout of a portable library. The catalog and all PAIM bookkeeping
// live under "<root>/.paim/"; the photos themselves live in the root tree.
const (
	// MetaDirName is the per-library PAIM directory inside the library root.
	MetaDirName = ".paim"
	// DBFileName is the SQLite catalog file inside MetaDirName.
	DBFileName = "paim.db"
	// LockFileName is the single-writer lock file inside MetaDirName.
	LockFileName = "lock"
	// BackupsDirName holds pre-migration DB backups inside MetaDirName.
	BackupsDirName = "backups"
)

// MetaDir returns "<root>/.paim".
func MetaDir(root string) string { return filepath.Join(root, MetaDirName) }

// DBPath returns "<root>/.paim/paim.db".
func DBPath(root string) string { return filepath.Join(MetaDir(root), DBFileName) }

// LockPath returns "<root>/.paim/lock".
func LockPath(root string) string { return filepath.Join(MetaDir(root), LockFileName) }

// BackupsDir returns "<root>/.paim/backups".
func BackupsDir(root string) string { return filepath.Join(MetaDir(root), BackupsDirName) }

// EnsureMetaDir creates "<root>/.paim" if needed and returns its path.
func EnsureMetaDir(root string) (string, error) {
	dir := MetaDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("library: create meta dir %q: %w", dir, err)
	}
	return dir, nil
}

// HasLibrary reports whether root already contains a PAIM catalog
// ("<root>/.paim/paim.db"). It is used to validate "Open existing" requests.
func HasLibrary(root string) bool {
	info, err := os.Stat(DBPath(root))
	return err == nil && !info.IsDir()
}

// DefaultName derives a human-friendly library name from its root path (the base
// directory name), falling back to the full path when the base is empty.
func DefaultName(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return root
	}
	return base
}
