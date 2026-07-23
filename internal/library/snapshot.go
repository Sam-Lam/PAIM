package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Snapshot naming/retention defaults.
const (
	// snapshotPrefix begins every snapshot file name: "paim-<library>-<stamp>.db".
	snapshotPrefix = "paim-"
	// snapshotExt is the snapshot file extension.
	snapshotExt = ".db"
	// snapshotStampFormat is the timestamp embedded in a snapshot file name.
	snapshotStampFormat = "20060102-150405"
	// DefaultSnapshotKeep is how many snapshots per library are retained by default.
	DefaultSnapshotKeep = 7
)

// Snapshot-interval tokens stored in Config.SnapshotInterval.
const (
	SnapshotIntervalOff   = "off"
	SnapshotIntervalQuit  = "quit"
	SnapshotInterval6h    = "6h"
	SnapshotIntervalDaily = "daily"
)

// SnapshotResult describes a completed snapshot for logging and UI status.
type SnapshotResult struct {
	Path      string
	Bytes     int64
	Pruned    int
	CreatedAt time.Time
}

// safeNameRE matches characters not allowed in the library-name portion of a
// snapshot file name; they are replaced with '-' so the name is filesystem-safe
// and the prune glob is well-formed.
var safeNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeName makes a library name safe for a file name and non-empty.
func sanitizeName(name string) string {
	s := safeNameRE.ReplaceAllString(strings.TrimSpace(name), "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		return "library"
	}
	return s
}

// Snapshot writes a one-way, disaster-recovery copy of the live catalog at dbPath
// into dest as "paim-<libraryName>-<yyyymmdd-hhmmss>.db", then prunes older
// snapshots of the SAME library beyond keepN. It is insurance only — the result
// is never opened as a live catalog.
//
// It first folds the WAL back into the main DB via the OPEN gorm handle
// (PRAGMA wal_checkpoint(TRUNCATE)) so the byte copy is self-contained, then
// copies with a BLAKE3 verify (the same helper the legacy install uses). A
// missing/unplugged destination surfaces as an error the caller logs and retries
// next interval; it never touches anything but this library's snapshot files.
func Snapshot(ctx context.Context, gdb *gorm.DB, dbPath, libraryName, dest string, keepN int) (SnapshotResult, error) {
	if dest == "" {
		return SnapshotResult{}, fmt.Errorf("library: snapshot destination not configured")
	}
	if keepN < 1 {
		keepN = DefaultSnapshotKeep
	}
	info, err := os.Stat(dest)
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("library: snapshot destination %q unavailable: %w", dest, err)
	}
	if !info.IsDir() {
		return SnapshotResult{}, fmt.Errorf("library: snapshot destination %q is not a directory", dest)
	}

	// Fold the WAL into the main DB file so the copy is a complete snapshot.
	if err := gdb.WithContext(ctx).Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		return SnapshotResult{}, fmt.Errorf("library: snapshot wal checkpoint: %w", err)
	}

	name := sanitizeName(libraryName)
	stamp := time.Now().Format(snapshotStampFormat)
	out := filepath.Join(dest, fmt.Sprintf("%s%s-%s%s", snapshotPrefix, name, stamp, snapshotExt))

	if err := copyVerify(ctx, dbPath, out); err != nil {
		return SnapshotResult{}, err
	}
	res := SnapshotResult{Path: out, CreatedAt: time.Now()}
	if st, err := os.Stat(out); err == nil {
		res.Bytes = st.Size()
	}
	res.Pruned = pruneSnapshots(dest, name, keepN)
	return res, nil
}

// pruneSnapshots deletes snapshots of the named library in dest beyond the newest
// keepN, matching ONLY the "paim-<name>-*.db" naming pattern so nothing else in
// the destination folder is ever removed. It returns the number deleted; delete
// errors are ignored (a stuck old snapshot is harmless).
func pruneSnapshots(dest, name string, keepN int) int {
	glob := filepath.Join(dest, fmt.Sprintf("%s%s-*%s", snapshotPrefix, name, snapshotExt))
	matches, err := filepath.Glob(glob)
	if err != nil || len(matches) <= keepN {
		return 0
	}
	// Names embed a sortable yyyymmdd-hhmmss stamp, so lexical sort is chronological.
	sort.Strings(matches)
	victims := matches[:len(matches)-keepN]
	pruned := 0
	for _, p := range victims {
		if err := os.Remove(p); err == nil {
			pruned++
		}
	}
	return pruned
}

// SnapshotIntervalDuration maps an interval token to its timer duration, or ok
// false for intervals with no timer (off / quit-only).
func SnapshotIntervalDuration(interval string) (time.Duration, bool) {
	switch interval {
	case SnapshotInterval6h:
		return 6 * time.Hour, true
	case SnapshotIntervalDaily:
		return 24 * time.Hour, true
	default:
		return 0, false
	}
}
