// Package library implements PAIM's portable "library-on-drive" support: the
// per-machine config that remembers recently opened libraries, the single-writer
// lock that guards a library while it is open, the legacy-catalog migration, and
// the path-resolution helpers that let Asset.CurrentArchivePath be stored
// relative to the library root so the catalog travels with the photos.
//
// The path helpers (ResolvePath / RelativizePath) are the ONE place every
// producer and consumer of a stored archive path must funnel through. Storing
// paths relative to the root (forward slashes) means the catalog survives volume
// renames and "/Volumes/Name 1" mount fallbacks; resolving them against the
// current root yields a real absolute path for filesystem operations and for the
// UI.
package library

import (
	"path/filepath"
	"strings"
)

// ResolvePath turns a stored Asset.CurrentArchivePath into an absolute path for
// filesystem operations and display. Stored paths are normally relative to the
// library root (forward slashes); this joins them with root. The rules:
//
//   - stored == ""        -> "" (no physical file, e.g. a not-copied duplicate).
//   - stored is absolute  -> returned unchanged (a legacy path, or a path that
//     lived outside the root and was intentionally kept absolute).
//   - stored is relative  -> filepath.Join(root, stored) with OS separators.
//
// A relative stored path with an empty root is returned as an OS-native path
// (best effort); production always resolves against a real root.
func ResolvePath(root, stored string) string {
	if stored == "" {
		return ""
	}
	native := filepath.FromSlash(stored)
	if filepath.IsAbs(native) {
		return native
	}
	if root == "" {
		return native
	}
	return filepath.Join(root, native)
}

// RelativizePath converts an absolute archive path into the value to STORE in
// Asset.CurrentArchivePath: relative to root with forward slashes when the path
// is inside root, otherwise the original absolute path (paths outside the library
// are legitimately kept absolute — e.g. an adopted file left in place on another
// volume). The rules:
//
//   - abs == ""                 -> "".
//   - root == ""                -> abs unchanged (no root to relativize against).
//   - abs already relative      -> forward-slashed unchanged (idempotent).
//   - abs inside root           -> forward-slashed path relative to root.
//   - abs outside root          -> abs unchanged.
func RelativizePath(root, abs string) string {
	if abs == "" {
		return ""
	}
	if !filepath.IsAbs(abs) {
		// Already relative (or unrooted); normalize to forward slashes.
		return filepath.ToSlash(abs)
	}
	if root == "" {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Outside the root: keep it absolute.
		return abs
	}
	return filepath.ToSlash(rel)
}
