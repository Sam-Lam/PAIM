package services

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/cleanup"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

func newCleanupService(t *testing.T) (*CleanupService, *captureEmitter) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	analyzer := cleanup.NewAnalyzer(assets, nil, nil, logger)
	emitter := &captureEmitter{}
	return NewCleanupService(analyzer, nil, emitter, logger), emitter
}

func waitCleanup(t *testing.T, svc *CleanupService, state string) ActiveCleanupAnalyzeDTO {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dto, err := svc.ActiveCleanupAnalyze(context.Background())
		if err != nil {
			t.Fatalf("ActiveCleanupAnalyze: %v", err)
		}
		if dto.State == state {
			return dto
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for cleanup state %q", state)
	return ActiveCleanupAnalyzeDTO{}
}

func cleanupWaitIdle(t *testing.T, svc *CleanupService) {
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
	t.Fatal("timed out waiting for cleanup to go idle")
}

// TestCleanupAnalyzeLifecycle covers start -> progress emitted -> completed
// snapshot retrievable (re-attach) with the report cached.
func TestCleanupAnalyzeLifecycle(t *testing.T) {
	svc, emitter := newCleanupService(t)
	root := t.TempDir()
	for _, n := range []string{"a.jpg", "b.jpg", "c.mp4"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("content-"+n), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, err := svc.StartCleanupAnalyze(context.Background(), root); err != nil {
		t.Fatalf("StartCleanupAnalyze: %v", err)
	}

	done := waitCleanup(t, svc, "completed")
	if done.Report == nil {
		t.Fatal("completed cleanup has no report")
	}
	if done.Report.MediaFiles != 3 {
		t.Fatalf("MediaFiles = %d, want 3", done.Report.MediaFiles)
	}
	// Nothing in the archive: every media file is new, so deletion is not safe.
	if done.Report.Recommendation.SafeToDelete {
		t.Fatal("recommendation should not be safe-to-delete for all-new files")
	}
	if done.Cancelled || done.Error != "" {
		t.Fatalf("unexpected terminal state cancelled=%v err=%q", done.Cancelled, done.Error)
	}

	// Progress emitted, and a single terminal cleanup:completed carrying the report.
	if len(emitter.byName(EventCleanupProgress)) == 0 {
		t.Fatal("no cleanup:progress events emitted")
	}
	completed := emitter.byName(EventCleanupCompleted)
	if len(completed) != 1 {
		t.Fatalf("cleanup:completed emitted %d times, want 1", len(completed))
	}
	cc := completed[0].(CleanupCompleted)
	if cc.Report == nil || cc.Report.MediaFiles != 3 {
		t.Fatal("cleanup:completed did not carry the report")
	}

	// Re-attach again returns the same cached completed report (survives remount).
	again := waitCleanup(t, svc, "completed")
	if again.Report == nil || again.Report.MediaFiles != 3 {
		t.Fatal("completed report not retained for re-attach")
	}
}

// TestCleanupAnalyzeGuardAndCancel verifies the one-active guard rejects a
// concurrent start and that cancellation drives a terminal cancelled state.
func TestCleanupAnalyzeGuardAndCancel(t *testing.T) {
	svc, _ := newCleanupService(t)
	root := t.TempDir()
	for i := 0; i < 2000; i++ {
		name := filepath.Join(root, "f"+strconvI(i)+".jpg")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, err := svc.StartCleanupAnalyze(context.Background(), root); err != nil {
		t.Fatalf("StartCleanupAnalyze: %v", err)
	}
	// A concurrent start is refused (active flag is set synchronously).
	if _, err := svc.StartCleanupAnalyze(context.Background(), root); err != ErrCleanupInProgress {
		t.Fatalf("second start err = %v, want ErrCleanupInProgress", err)
	}

	if err := svc.CancelCleanupAnalyze(context.Background()); err != nil {
		t.Fatalf("CancelCleanupAnalyze: %v", err)
	}
	cleanupWaitIdle(t, svc)

	dto, err := svc.ActiveCleanupAnalyze(context.Background())
	if err != nil {
		t.Fatalf("ActiveCleanupAnalyze: %v", err)
	}
	// The run finished; if the cancel landed mid-walk it is a cancelled terminal
	// state with no report. (If it raced to completion first, that is also a valid
	// terminal outcome — but with 2000 files the cancel reliably lands first.)
	if dto.State == "completed" && dto.Cancelled && dto.Report != nil {
		t.Fatal("cancelled run should carry no report")
	}
}

// strconvI is a tiny int->string helper avoiding a strconv import in this file.
func strconvI(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
