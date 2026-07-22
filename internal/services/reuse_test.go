package services

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// captureHandler is a slog.Handler that records every emitted record so a test
// can assert on the pipeline's structured "reusing precomputed hashes" summary.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first captured record with the given message, and whether one
// was found.
func (h *captureHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// int64Attr reads a named int64 attribute off a record (0 if absent).
func int64Attr(r slog.Record, key string) int64 {
	var out int64
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.Int64()
			return false
		}
		return true
	})
	return out
}

// TestStartImportReusesAnalyzeHashes is the service-level proof: a completed
// StartAnalyze primes the scan/report cache, and a following StartImport for the
// same root reuses every precomputed hash (reused=N, new=0) and still imports
// every file. Reuse is asserted through the pipeline's log summary — the same
// seam the importer emits it on — captured here.
func TestStartImportReusesAnalyzeHashes(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	master := filepath.Join(root, "master")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(master, 0o755); err != nil {
		t.Fatalf("mkdir master: %v", err)
	}

	gdb, err := db.Open(filepath.Join(root, "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	settings := repo.NewSettingsRepo(gdb)

	cap := &captureHandler{}
	logger := slog.New(cap)
	pipe := importer.New(importer.Config{
		DB:       gdb,
		Assets:   assets,
		Sessions: sessions,
		Layout:   archive.New(master),
		Logger:   logger,
	})
	emitter := &captureEmitter{}
	svc := NewImportService(pipe, sessions, settings, nil, emitter, logger)

	// Reuse the analyzeHarness helpers (waitAnalyze/waitIdle) against this service.
	h := &analyzeHarness{t: t, svc: svc, emitter: emitter, src: src, master: master, gdb: gdb}
	h.write("a.jpg", "alpha-content")
	h.write("b.jpg", "bravo-content")
	h.write("c.jpg", "charlie-content")

	// 1) Analyze primes the scan+report cache.
	if _, err := svc.StartAnalyze(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartAnalyze: %v", err)
	}
	h.waitAnalyze("completed")
	h.waitIdle()

	// 2) Import reuses the cached report's hashes.
	if _, err := svc.StartImport(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	h.waitIdle()

	rec, ok := cap.find("reusing precomputed hashes")
	if !ok {
		t.Fatal("no 'reusing precomputed hashes' log: StartImport did not thread the analyze report")
	}
	if reused, newN, stale := int64Attr(rec, "reused"), int64Attr(rec, "new"), int64Attr(rec, "stale"); reused != 3 || newN != 0 || stale != 0 {
		t.Fatalf("reuse summary reused=%d new=%d stale=%d, want 3/0/0", reused, newN, stale)
	}

	var count int64
	if err := gdb.Model(&domain.Asset{}).Count(&count).Error; err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 3 {
		t.Fatalf("assets imported = %d, want 3", count)
	}
}

// TestStartImportWithoutAnalyzeStillImports guards that a StartImport with no
// prior analyze (empty cache -> nil precomputed) still imports correctly and does
// not emit a reuse summary (nothing to reuse).
func TestStartImportWithoutAnalyzeStillImports(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	master := filepath.Join(root, "master")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(master, 0o755); err != nil {
		t.Fatalf("mkdir master: %v", err)
	}

	gdb, err := db.Open(filepath.Join(root, "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	settings := repo.NewSettingsRepo(gdb)

	cap := &captureHandler{}
	logger := slog.New(cap)
	pipe := importer.New(importer.Config{DB: gdb, Assets: assets, Sessions: sessions, Layout: archive.New(master), Logger: logger})
	emitter := &captureEmitter{}
	svc := NewImportService(pipe, sessions, settings, nil, emitter, logger)

	h := &analyzeHarness{t: t, svc: svc, emitter: emitter, src: src, master: master, gdb: gdb}
	h.write("a.jpg", "alpha-content")
	h.write("b.jpg", "bravo-content")

	if _, err := svc.StartImport(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	h.waitIdle()

	if _, ok := cap.find("reusing precomputed hashes"); ok {
		t.Fatal("unexpected reuse summary with no prior analyze")
	}
	var count int64
	if err := gdb.Model(&domain.Asset{}).Count(&count).Error; err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 2 {
		t.Fatalf("assets imported = %d, want 2", count)
	}
}

// TestPrecomputedForRespectsTTL asserts the cache freshness gate: a report older
// than analyzeReportTTL is not reused.
func TestPrecomputedForRespectsTTL(t *testing.T) {
	svc := &ImportService{cache: map[string]scanEntry{}}
	root := "/vol/src"
	report := &importer.DryRunReport{}

	svc.putScan(root, scanEntry{report: report, at: time.Now()})
	if got := svc.precomputedFor(root); got != report {
		t.Fatalf("fresh report not returned: %v", got)
	}

	svc.putScan(root, scanEntry{report: report, at: time.Now().Add(-analyzeReportTTL - time.Minute)})
	if got := svc.precomputedFor(root); got != nil {
		t.Fatalf("stale report returned; want nil")
	}

	if got := svc.precomputedFor("/no/such/root"); got != nil {
		t.Fatalf("missing entry returned %v; want nil", got)
	}
}
