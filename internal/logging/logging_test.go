package logging

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
	"gorm.io/gorm"
)

func newTestLogRepo(t *testing.T) (*repo.LogRepo, *gorm.DB) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return repo.NewLogRepo(gdb), gdb
}

func countLogs(t *testing.T, gdb *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := gdb.Model(&domain.LogEntry{}).Count(&n).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return n
}

func TestHandlerFlushesOnClose(t *testing.T) {
	logRepo, gdb := newTestLogRepo(t)
	h, closeFn := New(logRepo, slog.LevelInfo)
	logger := slog.New(h)

	const n = 5
	for i := 0; i < n; i++ {
		logger.Info("import event", slog.String(subsystemKey, "importer"), slog.Int("index", i))
	}

	// Close must flush everything still buffered.
	closeFn()
	closeFn() // idempotent

	if got := countLogs(t, gdb); got != n {
		t.Errorf("persisted %d log entries, want %d", got, n)
	}

	var first domain.LogEntry
	if err := gdb.Order("id ASC").First(&first).Error; err != nil {
		t.Fatalf("fetch first log: %v", err)
	}
	if first.Subsystem != "importer" {
		t.Errorf("subsystem = %q, want importer", first.Subsystem)
	}
	if first.Level != "INFO" {
		t.Errorf("level = %q, want INFO", first.Level)
	}
	if first.MetadataJSON == "" {
		t.Error("expected non-subsystem attrs to be captured in MetadataJSON")
	}
}

func TestHandlerFlushesOnBatchThreshold(t *testing.T) {
	logRepo, gdb := newTestLogRepo(t)
	h, closeFn := New(logRepo, slog.LevelInfo)
	defer closeFn()
	logger := slog.New(h).With(slog.String(subsystemKey, "bulk"))

	// Emit more than one batch worth to exercise the size-based flush.
	const n = flushBatch + 25
	for i := 0; i < n; i++ {
		logger.Info("bulk", slog.Int("i", i))
	}

	// Wait for the async flushes to land (batch flush + ticker), without relying
	// on Close.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countLogs(t, gdb) >= int64(n) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := countLogs(t, gdb); got != int64(n) {
		t.Errorf("persisted %d entries, want %d", got, n)
	}
}

func TestHandlerRespectsLevel(t *testing.T) {
	logRepo, gdb := newTestLogRepo(t)
	h, closeFn := New(logRepo, slog.LevelWarn)
	logger := slog.New(h)

	logger.Info("below threshold", slog.String(subsystemKey, "x"))  // dropped
	logger.Warn("at threshold", slog.String(subsystemKey, "x"))     // kept
	logger.Error("above threshold", slog.String(subsystemKey, "x")) // kept
	closeFn()

	if got := countLogs(t, gdb); got != 2 {
		t.Errorf("persisted %d entries, want 2 (info filtered out)", got)
	}
}

func TestForUsesDefaultLogger(t *testing.T) {
	logRepo, gdb := newTestLogRepo(t)
	h, closeFn := New(logRepo, slog.LevelInfo)
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	logging := For("cleanup")
	logging.Info("analyzed folder")
	closeFn()

	var e domain.LogEntry
	if err := gdb.First(&e).Error; err != nil {
		t.Fatalf("fetch log: %v", err)
	}
	if e.Subsystem != "cleanup" {
		t.Errorf("For() subsystem = %q, want cleanup", e.Subsystem)
	}
}
