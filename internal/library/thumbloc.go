package library

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Thumbnail-cache location tokens stored in Config.ThumbnailCacheLocation.
const (
	// ThumbLocationLibrary keeps the cache inside the library at
	// "<root>/.paim/thumbs" (the default; empty is treated as this).
	ThumbLocationLibrary = "library"
	// ThumbLocationLocal keeps the cache on this Mac's internal disk under the
	// app-support directory, per library id.
	ThumbLocationLocal = "local"
)

// ThumbsDirName is the thumbnail cache subdirectory name (inside .paim for the
// library location, and per-library-id for the local location).
const ThumbsDirName = "thumbs"

// AppSupportDir returns "~/Library/Application Support/PAIM", the per-machine
// PAIM directory that also holds config.json.
func AppSupportDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("library: resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "PAIM"), nil
}

// StableID derives a stable, filesystem-safe identifier for the library rooted at
// root. No library id is persisted today, so it is a truncated SHA-256 of the
// cleaned absolute root path — stable across mounts as long as the path is stable
// (the local thumbnail cache is disposable, so a changed id merely re-generates
// thumbnails, never loses data).
func StableID(root string) string {
	abs := root
	if a, err := filepath.Abs(root); err == nil {
		abs = a
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(sum[:8]) // 16 hex chars is plenty to avoid collisions
}

// LibraryThumbsDir returns the in-library thumbnail cache dir,
// "<root>/.paim/thumbs".
func LibraryThumbsDir(root string) string {
	return filepath.Join(MetaDir(root), ThumbsDirName)
}

// LocalThumbsDir returns the on-this-Mac thumbnail cache dir for the library,
// "~/Library/Application Support/PAIM/thumbs/<libraryID>".
func LocalThumbsDir(root string) (string, error) {
	base, err := AppSupportDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, ThumbsDirName, StableID(root)), nil
}

// ResolveThumbCacheDir returns the thumbnail cache directory for root given the
// per-machine location preference. Any value other than ThumbLocationLocal
// (including "") resolves to the in-library location.
func ResolveThumbCacheDir(root, location string) (string, error) {
	if location == ThumbLocationLocal {
		return LocalThumbsDir(root)
	}
	return LibraryThumbsDir(root), nil
}

// KnownThumbCacheDirs returns both cache roots PAIM ever uses for a library —
// the in-library dir and this Mac's local dir. Callers that delete cache contents
// (Clear cache) verify the active dir is one of these before removing anything.
func KnownThumbCacheDirs(root string) ([]string, error) {
	local, err := LocalThumbsDir(root)
	if err != nil {
		return nil, err
	}
	return []string{LibraryThumbsDir(root), local}, nil
}
