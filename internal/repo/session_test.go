package repo

import (
	"context"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/domain"
)

func TestSessionCountersAndComplete(t *testing.T) {
	ctx := context.Background()
	r := NewSessionRepo(newTestDB(t))

	s := &domain.ImportSession{
		StartedAt: time.Now().UTC().Add(-2 * time.Second),
		Status:    domain.SessionStatusRunning,
	}
	if err := r.Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := r.IncScanned(ctx, s.ID, 5); err != nil {
		t.Fatalf("inc scanned: %v", err)
	}
	if err := r.IncImported(ctx, s.ID, 3); err != nil {
		t.Fatalf("inc imported: %v", err)
	}
	if err := r.AddCounters(ctx, s.ID, SessionCounters{Duplicates: 1, Failures: 1, Skipped: 2}); err != nil {
		t.Fatalf("add counters: %v", err)
	}
	// A second increment must accumulate (atomic add, not overwrite).
	if err := r.IncImported(ctx, s.ID, 2); err != nil {
		t.Fatalf("inc imported 2: %v", err)
	}

	got, err := r.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.FilesScanned != 5 || got.FilesImported != 5 || got.Duplicates != 1 || got.Failures != 1 || got.Skipped != 2 {
		t.Errorf("counters = scanned %d imported %d dup %d fail %d skip %d; want 5/5/1/1/2",
			got.FilesScanned, got.FilesImported, got.Duplicates, got.Failures, got.Skipped)
	}

	completedAt := got.StartedAt.Add(2 * time.Second)
	if err := r.Complete(ctx, s.ID, domain.SessionStatusCompleted, completedAt); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err = r.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get session after complete: %v", err)
	}
	if got.Status != domain.SessionStatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.CompletedAt == nil {
		t.Fatal("CompletedAt not set")
	}
	if got.DurationSeconds < 1.9 || got.DurationSeconds > 2.1 {
		t.Errorf("duration = %v, want ~2", got.DurationSeconds)
	}
}

func TestMarkInterruptedOnStartup(t *testing.T) {
	ctx := context.Background()
	r := NewSessionRepo(newTestDB(t))

	running := &domain.ImportSession{StartedAt: time.Now().UTC(), Status: domain.SessionStatusRunning}
	completed := &domain.ImportSession{StartedAt: time.Now().UTC(), Status: domain.SessionStatusCompleted}
	if err := r.Create(ctx, running); err != nil {
		t.Fatalf("create running: %v", err)
	}
	if err := r.Create(ctx, completed); err != nil {
		t.Fatalf("create completed: %v", err)
	}

	n, err := r.MarkInterruptedOnStartup(ctx)
	if err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	if n != 1 {
		t.Errorf("marked %d sessions, want 1", n)
	}

	got, err := r.GetByID(ctx, running.ID)
	if err != nil {
		t.Fatalf("get running: %v", err)
	}
	if got.Status != domain.SessionStatusInterrupted {
		t.Errorf("running session status = %q, want interrupted", got.Status)
	}

	gotDone, err := r.GetByID(ctx, completed.ID)
	if err != nil {
		t.Fatalf("get completed: %v", err)
	}
	if gotDone.Status != domain.SessionStatusCompleted {
		t.Errorf("completed session status changed to %q", gotDone.Status)
	}
}

func TestListRecentSessions(t *testing.T) {
	ctx := context.Background()
	r := NewSessionRepo(newTestDB(t))

	older := &domain.ImportSession{StartedAt: time.Now().UTC().Add(-time.Hour), Status: domain.SessionStatusCompleted}
	newer := &domain.ImportSession{StartedAt: time.Now().UTC(), Status: domain.SessionStatusCompleted}
	if err := r.Create(ctx, older); err != nil {
		t.Fatalf("create older: %v", err)
	}
	if err := r.Create(ctx, newer); err != nil {
		t.Fatalf("create newer: %v", err)
	}

	list, err := r.ListRecent(ctx, 10)
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(list) != 2 || list[0].ID != newer.ID {
		t.Errorf("ListRecent ordering wrong: %+v", list)
	}
}
