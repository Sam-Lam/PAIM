package services

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/source"
)

func newSourcesService(t *testing.T) (*SourcesService, *captureEmitter) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "sources.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	sources := repo.NewSourceRepo(gdb)
	identifier := source.NewIdentifier(nil, sources, Hasher{}, mediatype.IsMedia)
	emitter := &captureEmitter{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewSourcesService(nil, identifier, sources, assets, nil, emitter, logger), emitter
}

func waitSafeErase(t *testing.T, svc *SourcesService, state string) ActiveSafeToEraseDTO {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dto, err := svc.ActiveSafeToErase(context.Background())
		if err != nil {
			t.Fatalf("ActiveSafeToErase: %v", err)
		}
		if dto.State == state {
			return dto
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for safe-to-erase state %q", state)
	return ActiveSafeToEraseDTO{}
}

func sourcesWaitIdle(t *testing.T, svc *SourcesService) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		active := svc.active
		svc.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for safe-to-erase to go idle")
}

// TestSafeToEraseLifecycle covers start -> progress emitted -> completed
// snapshot retrievable (re-attach), with the report present and (empty archive)
// unsafe.
func TestSafeToEraseLifecycle(t *testing.T) {
	svc, emitter := newSourcesService(t)
	root := t.TempDir()
	for _, n := range []string{"a.jpg", "b.cr3", "c.mov"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("content-"+n), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, err := svc.StartSafeToErase(context.Background(), "", root); err != nil {
		t.Fatalf("StartSafeToErase: %v", err)
	}

	done := waitSafeErase(t, svc, "completed")
	if done.Report == nil {
		t.Fatal("completed evaluation has no report")
	}
	if done.Report.TotalMedia != 3 {
		t.Fatalf("TotalMedia = %d, want 3", done.Report.TotalMedia)
	}
	if done.Report.Safe {
		t.Fatal("empty archive should make the volume unsafe to erase")
	}
	if done.Cancelled || done.Error != "" {
		t.Fatalf("unexpected terminal state cancelled=%v err=%q", done.Cancelled, done.Error)
	}

	// Determinate progress events (kind safe-to-erase, total known) were emitted.
	progress := emitter.byName(EventSourceProgress)
	if len(progress) == 0 {
		t.Fatal("no source:progress events emitted")
	}
	for _, e := range progress {
		p := e.(SourceProgress)
		if p.Kind != "safe-to-erase" {
			t.Fatalf("progress kind = %q, want safe-to-erase", p.Kind)
		}
		if p.FilesTotal != 3 {
			t.Fatalf("progress FilesTotal = %d, want 3 (determinate)", p.FilesTotal)
		}
	}
	// Exactly one terminal source:evaluated carrying the report.
	evaluated := emitter.byName(EventSourceEvaluated)
	if len(evaluated) != 1 {
		t.Fatalf("source:evaluated emitted %d times, want 1", len(evaluated))
	}
	se := evaluated[0].(SourceEvaluated)
	if se.Report == nil || se.Report.TotalMedia != 3 {
		t.Fatal("source:evaluated did not carry the report")
	}

	// Re-attach returns the retained completed report.
	again := waitSafeErase(t, svc, "completed")
	if again.Report == nil || again.Report.TotalMedia != 3 {
		t.Fatal("completed report not retained for re-attach")
	}
}

// TestSafeToEraseGuardAndCancel verifies the one-active guard and cancellation.
func TestSafeToEraseGuardAndCancel(t *testing.T) {
	svc, _ := newSourcesService(t)
	root := t.TempDir()
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(root, "f"+strconvI(i)+".jpg"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, err := svc.StartSafeToErase(context.Background(), "", root); err != nil {
		t.Fatalf("StartSafeToErase: %v", err)
	}
	if _, err := svc.StartSafeToErase(context.Background(), "", root); err != ErrSafeToEraseInProgress {
		t.Fatalf("second start err = %v, want ErrSafeToEraseInProgress", err)
	}

	if err := svc.CancelSafeToErase(context.Background()); err != nil {
		t.Fatalf("CancelSafeToErase: %v", err)
	}
	sourcesWaitIdle(t, svc)

	dto, err := svc.ActiveSafeToErase(context.Background())
	if err != nil {
		t.Fatalf("ActiveSafeToErase: %v", err)
	}
	if dto.State == "completed" && dto.Cancelled && dto.Report != nil {
		t.Fatal("cancelled run should carry no report")
	}
}
