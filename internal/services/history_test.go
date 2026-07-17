package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
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
