package repo

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

func TestSessionCountsAndOrphanAdoption(t *testing.T) {
	gdb := newTestDB(t)
	sources := NewSourceRepo(gdb)

	src := &domain.ImportSource{SourceType: domain.SourceTypeSDCard, VolumeLabel: "Untitled"}
	if err := sources.Create(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	mk := func(sourceID, notes string) *domain.ImportSession {
		s := &domain.ImportSession{StartedAt: time.Now(), SourceID: sourceID, Status: domain.SessionStatusCompleted, Notes: notes}
		if err := gdb.Create(s).Error; err != nil {
			t.Fatalf("create session: %v", err)
		}
		return s
	}

	// Linked session, orphan on this mount, orphan beneath this mount,
	// orphan on a lookalike mount, orphan elsewhere.
	mk(src.ID, `{"sourceRoot":"/Volumes/Untitled"}`)
	orphanExact := mk("", `{"sourceRoot":"/Volumes/Untitled"}`)
	orphanSub := mk("", `{"sourceRoot":"/Volumes/Untitled/DCIM"}`)
	lookalike := mk("", `{"sourceRoot":"/Volumes/Untitled 1"}`)
	elsewhere := mk("", `{"sourceRoot":"/Volumes/Other"}`)

	adopted, err := sources.AdoptOrphanSessions(context.Background(), src.ID, "/Volumes/Untitled")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adopted != 2 {
		t.Fatalf("adopted = %d, want 2", adopted)
	}
	for _, tc := range []struct {
		id   string
		want string
	}{{orphanExact.ID, src.ID}, {orphanSub.ID, src.ID}, {lookalike.ID, ""}, {elsewhere.ID, ""}} {
		var got domain.ImportSession
		if err := gdb.First(&got, "id = ?", tc.id).Error; err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.SourceID != tc.want {
			t.Fatalf("session %s source = %q, want %q", tc.id, got.SourceID, tc.want)
		}
	}

	counts, err := sources.SessionCounts(context.Background())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts[src.ID] != 3 {
		t.Fatalf("count = %d, want 3 (1 linked + 2 adopted)", counts[src.ID])
	}
}
