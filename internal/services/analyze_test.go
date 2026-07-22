package services

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// captureEmitter records every emitted event for later assertions. It is safe
// for concurrent use because progress is emitted from the background goroutine.
type captureEmitter struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	name string
	data any
}

func (c *captureEmitter) Emit(name string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{name: name, data: data})
}

func (c *captureEmitter) byName(name string) []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []any
	for _, e := range c.events {
		if e.name == name {
			out = append(out, e.data)
		}
	}
	return out
}

// analyzeHarness wires a real ImportService (real SQLite, real importer.Pipeline)
// for the analyze lifecycle tests. Metadata extraction is left nil (degrades to
// mtime capture dates), which keeps the tests independent of exiftool.
type analyzeHarness struct {
	t       *testing.T
	svc     *ImportService
	emitter *captureEmitter
	src     string
	master  string
	gdb     *gorm.DB
}

func newAnalyzeHarness(t *testing.T) *analyzeHarness {
	t.Helper()
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

	pipe := importer.New(importer.Config{
		DB:       gdb,
		Assets:   assets,
		Sessions: sessions,
		Layout:   archive.New(master),
	})

	emitter := &captureEmitter{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewImportService(pipe, sessions, settings, nil, emitter, logger)

	return &analyzeHarness{t: t, svc: svc, emitter: emitter, src: src, master: master, gdb: gdb}
}

func (h *analyzeHarness) write(name, content string) {
	h.t.Helper()
	full := filepath.Join(h.src, name)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", name, err)
	}
}

// copyOpts builds a copy-mode ImportOptions with an explicit destination so the
// tests do not depend on a persisted Master Library setting.
func (h *analyzeHarness) copyOpts() ImportOptions {
	return ImportOptions{Root: h.src, DestinationRoot: h.master, Mode: string(importer.ModeCopy)}
}

// waitAnalyze polls ActiveAnalyze until it reports state, failing on timeout.
func (h *analyzeHarness) waitAnalyze(state string) ActiveAnalyzeDTO {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dto, err := h.svc.ActiveAnalyze(context.Background())
		if err != nil {
			h.t.Fatalf("ActiveAnalyze: %v", err)
		}
		if dto.State == state {
			return dto
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for analyze state %q", state)
	return ActiveAnalyzeDTO{}
}

func (h *analyzeHarness) waitIdle() {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.svc.mu.Lock()
		active := h.svc.active
		h.svc.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("timed out waiting for the operation to go idle")
}

// TestAnalyzeLifecycle covers the full happy path: start -> progress emitted ->
// completed snapshot retrievable -> the scan/report cache is populated so a
// subsequent StartImport reuses it and imports every file.
func TestAnalyzeLifecycle(t *testing.T) {
	h := newAnalyzeHarness(t)
	h.write("a.jpg", "alpha-content")
	h.write("b.jpg", "bravo-content")
	h.write("c.mp4", "charlie-video-content")

	if _, err := h.svc.StartAnalyze(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartAnalyze: %v", err)
	}

	done := h.waitAnalyze("completed")
	if done.Report == nil {
		t.Fatal("completed analyze has no report")
	}
	if done.Report.Files != 3 {
		t.Fatalf("report.Files = %d, want 3", done.Report.Files)
	}
	if done.Report.New != 3 {
		t.Fatalf("report.New = %d, want 3", done.Report.New)
	}
	if done.Cancelled || done.Error != "" {
		t.Fatalf("unexpected terminal state cancelled=%v err=%q", done.Cancelled, done.Error)
	}

	// Progress was emitted on import:progress with an empty sessionId and at least
	// one recognizable analyze phase.
	progressEvents := h.emitter.byName(EventImportProgress)
	if len(progressEvents) == 0 {
		t.Fatal("no import:progress events emitted during analyze")
	}
	sawPhase := false
	for _, e := range progressEvents {
		p := e.(ImportProgress)
		if p.SessionID != "" {
			t.Fatalf("analyze progress carried a sessionId %q; want empty", p.SessionID)
		}
		switch p.Phase {
		case string(importer.PhaseScanning), string(importer.PhaseHashing), string(importer.PhaseClassifying):
			sawPhase = true
		}
	}
	if !sawPhase {
		t.Fatal("no scanning/hashing/classifying phase seen in analyze progress")
	}

	// analyze:completed emitted exactly once, carrying the report and opts echo.
	completedEvents := h.emitter.byName(EventAnalyzeCompleted)
	if len(completedEvents) != 1 {
		t.Fatalf("analyze:completed emitted %d times, want 1", len(completedEvents))
	}
	ac := completedEvents[0].(AnalyzeCompleted)
	if ac.Report == nil || ac.Report.Files != 3 {
		t.Fatalf("analyze:completed report mismatch: %+v", ac.Report)
	}
	if ac.Opts.Mode != string(importer.ModeCopy) {
		t.Fatalf("opts echo mode = %q, want copy", ac.Opts.Mode)
	}

	// Cache handoff: the scan+report cache StartImport relies on is populated.
	absSrc, _ := filepath.Abs(h.src)
	entry, ok := h.svc.getScan(absSrc)
	if !ok || entry.scan == nil || entry.report == nil {
		t.Fatalf("scan/report cache not populated after analyze: ok=%v entry=%+v", ok, entry)
	}

	// StartImport after the completed analyze imports every file end to end.
	if _, err := h.svc.StartImport(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartImport after analyze: %v", err)
	}
	h.waitIdle()

	var count int64
	if err := h.gdb.Model(&domain.Asset{}).Count(&count).Error; err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 3 {
		t.Fatalf("assets imported = %d, want 3", count)
	}
}

// TestAnalyzeBlocksImportAndViceVersa verifies the shared one-active-operation
// guard: an in-flight analyze refuses a StartImport, and an active operation
// refuses a StartAnalyze.
func TestAnalyzeBlocksImportAndViceVersa(t *testing.T) {
	h := newAnalyzeHarness(t)
	// Enough files that the analyze is still hashing when we race a StartImport.
	for i := 0; i < 40; i++ {
		h.write(filepathName(i), "content-"+filepathName(i))
	}

	if _, err := h.svc.StartAnalyze(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartAnalyze: %v", err)
	}

	// While the analyze holds the slot, an import start is refused. (If the
	// analyze already finished, this still holds via the completed-but-active
	// window only briefly; guard against flakiness by asserting on the error type
	// only when still active.)
	h.svc.mu.Lock()
	stillActive := h.svc.active
	h.svc.mu.Unlock()
	if stillActive {
		if _, err := h.svc.StartImport(context.Background(), h.copyOpts()); !errors.Is(err, ErrImportInProgress) {
			t.Fatalf("StartImport during analyze: got %v, want ErrImportInProgress", err)
		}
	}

	h.waitAnalyze("completed")
	h.waitIdle()

	// Reverse direction: with the active slot held, StartAnalyze is refused.
	h.svc.mu.Lock()
	h.svc.active = true
	h.svc.mu.Unlock()
	if _, err := h.svc.StartAnalyze(context.Background(), h.copyOpts()); !errors.Is(err, ErrImportInProgress) {
		t.Fatalf("StartAnalyze while active: got %v, want ErrImportInProgress", err)
	}
	h.svc.mu.Lock()
	h.svc.active = false
	h.svc.mu.Unlock()
}

// TestAnalyzeCancel verifies CancelImport cancels a running analyze, which
// resolves to a cancelled analyze:completed (no report).
func TestAnalyzeCancel(t *testing.T) {
	h := newAnalyzeHarness(t)
	for i := 0; i < 200; i++ {
		h.write(filepathName(i), "content-"+filepathName(i))
	}

	if _, err := h.svc.StartAnalyze(context.Background(), h.copyOpts()); err != nil {
		t.Fatalf("StartAnalyze: %v", err)
	}
	// Cancel immediately; the run observes the cancelled context.
	if err := h.svc.CancelImport(context.Background()); err != nil {
		t.Fatalf("CancelImport: %v", err)
	}
	h.waitIdle()

	dto, err := h.svc.ActiveAnalyze(context.Background())
	if err != nil {
		t.Fatalf("ActiveAnalyze: %v", err)
	}
	// A cancelled analyze may finish either cancelled (Report nil, Cancelled true)
	// or, if it beat the cancel, completed with a report. Both are valid; assert
	// it is not stuck running and never reports a spurious error.
	if dto.State == "running" {
		t.Fatal("analyze still running after cancel + idle")
	}
	if dto.Error != "" {
		t.Fatalf("cancelled analyze reported an error: %q", dto.Error)
	}
	if dto.Cancelled && dto.Report != nil {
		t.Fatal("cancelled analyze should carry no report")
	}
}

// filepathName produces a stable, unique media filename for the i-th generated
// file (unique content so each is classified New, not an intra-batch duplicate).
func filepathName(i int) string {
	return "f" + strconv.Itoa(i) + ".jpg"
}
