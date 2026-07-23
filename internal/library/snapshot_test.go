package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/hashing"
	"gorm.io/gorm"
)

// openTempCatalog opens a fresh SQLite catalog in a temp dir and returns the live
// handle and its file path. The handle stays open so Snapshot can checkpoint it.
func openTempCatalog(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "paim.db")
	gdb, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := gdb.Exec("CREATE TABLE t (v TEXT)").Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := gdb.Exec("INSERT INTO t (v) VALUES ('hello')").Error; err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb, dbPath
}

func TestSnapshotCreatesVerifiedCopy(t *testing.T) {
	gdb, dbPath := openTempCatalog(t)
	dest := t.TempDir()

	res, err := Snapshot(context.Background(), gdb, dbPath, "My Photos", dest, DefaultSnapshotKeep)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}
	// The snapshot must be a byte-faithful copy (BLAKE3 of the live file == snapshot).
	srcHash, err := hashing.FullHash(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("hash source: %v", err)
	}
	dstHash, err := hashing.FullHash(context.Background(), res.Path)
	if err != nil {
		t.Fatalf("hash snapshot: %v", err)
	}
	if srcHash != dstHash {
		t.Errorf("snapshot hash mismatch: src=%s dst=%s", srcHash, dstHash)
	}
	// Name shape: paim-<sanitized>-<stamp>.db.
	base := filepath.Base(res.Path)
	if got := base[:len("paim-My-Photos-")]; got != "paim-My-Photos-" {
		t.Errorf("snapshot name %q does not start with sanitized prefix", base)
	}
}

func TestPruneKeepsNewestAndSpareNonMatching(t *testing.T) {
	dest := t.TempDir()
	name := "lib"

	// Create 5 matching snapshots with distinct, sortable stamps.
	stamps := []string{
		"20260101-000000", "20260102-000000", "20260103-000000",
		"20260104-000000", "20260105-000000",
	}
	for _, s := range stamps {
		p := filepath.Join(dest, "paim-"+name+"-"+s+".db")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Files that must NEVER be touched: another library's snapshot, and unrelated
	// files.
	untouched := []string{
		filepath.Join(dest, "paim-otherlib-20260101-000000.db"),
		filepath.Join(dest, "keepme.txt"),
		filepath.Join(dest, "important.db"),
	}
	for _, p := range untouched {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pruned := pruneSnapshots(dest, name, 2)
	if pruned != 3 {
		t.Errorf("pruned = %d, want 3", pruned)
	}
	// Newest two of this library survive.
	for _, s := range stamps[3:] {
		if _, err := os.Stat(filepath.Join(dest, "paim-"+name+"-"+s+".db")); err != nil {
			t.Errorf("newest snapshot %s was pruned", s)
		}
	}
	// Oldest three of this library are gone.
	for _, s := range stamps[:3] {
		if _, err := os.Stat(filepath.Join(dest, "paim-"+name+"-"+s+".db")); err == nil {
			t.Errorf("old snapshot %s should have been pruned", s)
		}
	}
	// Non-matching files are all intact.
	for _, p := range untouched {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("non-matching file %s was deleted: %v", p, err)
		}
	}
}

func TestSnapshotErrorsOnMissingDest(t *testing.T) {
	gdb, dbPath := openTempCatalog(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := Snapshot(context.Background(), gdb, dbPath, "lib", missing, 3); err == nil {
		t.Fatal("expected error for missing destination")
	}
}

func TestSnapshotIntervalDuration(t *testing.T) {
	if d, ok := SnapshotIntervalDuration(SnapshotInterval6h); !ok || d != 6*time.Hour {
		t.Errorf("6h => %v,%v", d, ok)
	}
	if d, ok := SnapshotIntervalDuration(SnapshotIntervalDaily); !ok || d != 24*time.Hour {
		t.Errorf("daily => %v,%v", d, ok)
	}
	if _, ok := SnapshotIntervalDuration(SnapshotIntervalQuit); ok {
		t.Error("quit should have no timer duration")
	}
	if _, ok := SnapshotIntervalDuration(SnapshotIntervalOff); ok {
		t.Error("off should have no timer duration")
	}
}
