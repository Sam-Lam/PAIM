package repo

import (
	"context"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// TestEnqueueSkipsWhenOptedOutExists verifies opted_out is part of Enqueue's
// idempotency set: once an asset+destination is opted out, a later Enqueue does
// NOT create a competing pending job (so a backfill / resume honors the opt-out;
// un-skipping is the explicit RequeueOptedOut path only).
func TestEnqueueSkipsWhenOptedOutExists(t *testing.T) {
	ctx := context.Background()
	r := NewBackupRepo(newTestDB(t))

	oo, created, err := r.EnqueueOptedOut(ctx, "asset-1", "localfs", "prov-a", 7)
	if err != nil {
		t.Fatalf("enqueue opted-out: %v", err)
	}
	if !created || oo.Status != domain.JobStatusOptedOut {
		t.Fatalf("expected a created opted_out job, got created=%v status=%q", created, oo.Status)
	}

	same, created2, err := r.Enqueue(ctx, "asset-1", "localfs", "prov-a", 7)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if created2 {
		t.Fatalf("Enqueue must not create a pending job when opted_out already exists")
	}
	if same.ID != oo.ID || same.Status != domain.JobStatusOptedOut {
		t.Fatalf("Enqueue should return the existing opted_out job, got id=%q status=%q", same.ID, same.Status)
	}
}

// TestRequeueOptedOut_ScopedAndUnscoped verifies the reversal path: opted_out
// jobs flip to pending, scoped to a session when requested and across all
// sessions when not.
func TestRequeueOptedOut_ScopedAndUnscoped(t *testing.T) {
	ctx := context.Background()
	gdb := newTestDB(t)
	r := NewBackupRepo(gdb)

	// Two assets in different sessions, both opted out of prov-a.
	a1 := &domain.Asset{SessionID: "sess-1", OriginalFilename: "a1", QuickHash: "q1",
		VerificationStatus: domain.VerificationStatusVerified, CurrentArchivePath: "p1"}
	a2 := &domain.Asset{SessionID: "sess-2", OriginalFilename: "a2", QuickHash: "q2",
		VerificationStatus: domain.VerificationStatusVerified, CurrentArchivePath: "p2"}
	if err := gdb.Create(a1).Error; err != nil {
		t.Fatalf("create a1: %v", err)
	}
	if err := gdb.Create(a2).Error; err != nil {
		t.Fatalf("create a2: %v", err)
	}
	if _, _, err := r.EnqueueOptedOut(ctx, a1.ID, "localfs", "prov-a", 0); err != nil {
		t.Fatalf("opt out a1: %v", err)
	}
	if _, _, err := r.EnqueueOptedOut(ctx, a2.ID, "localfs", "prov-a", 0); err != nil {
		t.Fatalf("opt out a2: %v", err)
	}

	// Scoped to sess-1: only a1's job flips.
	n, err := r.RequeueOptedOut(ctx, "prov-a", "sess-1")
	if err != nil {
		t.Fatalf("requeue scoped: %v", err)
	}
	if n != 1 {
		t.Fatalf("scoped requeue affected %d jobs, want 1", n)
	}
	assertJobStatus(t, r, a1.ID, "prov-a", domain.JobStatusPending)
	assertJobStatus(t, r, a2.ID, "prov-a", domain.JobStatusOptedOut)

	// Unscoped: the remaining opted_out job (a2) flips.
	n, err = r.RequeueOptedOut(ctx, "prov-a", "")
	if err != nil {
		t.Fatalf("requeue unscoped: %v", err)
	}
	if n != 1 {
		t.Fatalf("unscoped requeue affected %d jobs, want 1", n)
	}
	assertJobStatus(t, r, a2.ID, "prov-a", domain.JobStatusPending)
}

func assertJobStatus(t *testing.T, r *BackupRepo, assetID, dest string, want domain.JobStatus) {
	t.Helper()
	var j domain.BackupJob
	if err := r.db.Where("asset_id = ? AND destination = ?", assetID, dest).First(&j).Error; err != nil {
		t.Fatalf("load job for asset %q dest %q: %v", assetID, dest, err)
	}
	if j.Status != want {
		t.Fatalf("job (asset %q) status = %q, want %q", assetID, j.Status, want)
	}
}
