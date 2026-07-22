package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

func TestListSessionsTotalIsTrueCount(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "hist.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessions := repo.NewSessionRepo(gdb)
	logs := repo.NewLogRepo(gdb)
	svc := NewHistoryService(sessions, logs, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		s := &domain.ImportSession{
			StartedAt: time.Now().Add(time.Duration(i) * time.Second),
			Status:    domain.SessionStatusCompleted,
		}
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	// First page of size 2: two items, but total reflects all five sessions.
	res, err := svc.ListSessions(ctx, 1, 2)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	if res.Total != 5 {
		t.Fatalf("total = %d, want 5 (true count)", res.Total)
	}

	// Second page (1-based) returns the next window.
	res2, err := svc.ListSessions(ctx, 2, 2)
	if err != nil {
		t.Fatalf("list sessions page 2: %v", err)
	}
	if len(res2.Items) != 2 {
		t.Fatalf("page 2 items = %d, want 2", len(res2.Items))
	}
	if res2.Total != 5 {
		t.Fatalf("page 2 total = %d, want 5", res2.Total)
	}
}

// TestSessionEventsSQLFilterEquivalence verifies the SQL metadata_json filter
// returns exactly the entries whose metadata references the session ID, within
// the session's time window — the same set the old in-memory string scan would
// produce, and none belonging to other sessions or outside the window.
func TestSessionEventsSQLFilterEquivalence(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "hist2.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessions := repo.NewSessionRepo(gdb)
	logs := repo.NewLogRepo(gdb)
	svc := NewHistoryService(sessions, logs, nil)
	ctx := context.Background()

	start := time.Now()
	completed := start.Add(2 * time.Minute)
	s := &domain.ImportSession{StartedAt: start, CompletedAt: &completed, Status: domain.SessionStatusCompleted}
	if err := sessions.Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	other := &domain.ImportSession{StartedAt: start, Status: domain.SessionStatusCompleted}
	if err := sessions.Create(ctx, other); err != nil {
		t.Fatalf("create other session: %v", err)
	}

	mk := func(ts time.Time, subsystem, msg, meta string) {
		if err := logs.Insert(ctx, &domain.LogEntry{
			Timestamp: ts, Level: "INFO", Subsystem: subsystem, Message: msg, MetadataJSON: meta,
		}); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}
	inWindow := start.Add(time.Minute)
	// Matching entries (import subsystem, references this session, in window).
	mk(inWindow, "import", "copied file a", `{"sessionId":"`+s.ID+`","file":"a"}`)
	mk(inWindow, "import", "copied file b", `{"sessionId":"`+s.ID+`","file":"b"}`)
	// Non-matching: other session ID.
	mk(inWindow, "import", "copied file c", `{"sessionId":"`+other.ID+`","file":"c"}`)
	// Non-matching: outside the window.
	mk(start.Add(10*time.Minute), "import", "late", `{"sessionId":"`+s.ID+`"}`)
	// Non-matching: different subsystem.
	mk(inWindow, "backup", "backed up", `{"sessionId":"`+s.ID+`"}`)

	detail, err := svc.SessionEvents(ctx, s.ID)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(detail.Events) != 2 {
		t.Fatalf("events = %d, want 2 (only this session's import entries in window)", len(detail.Events))
	}
	for _, e := range detail.Events {
		if e.Subsystem != "import" {
			t.Fatalf("unexpected subsystem %q", e.Subsystem)
		}
	}
	if detail.Truncated {
		t.Fatalf("unexpected truncation for a small result set")
	}
}
