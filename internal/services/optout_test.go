package services

import (
	"context"
	"strings"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// TestValidateSkipProvidersRejectsUnknown covers the skip-provider-ID guard:
// known IDs pass (and are de-duplicated); an unknown ID is rejected so a stale UI
// can never silently suppress backups to a destination that was never opted out.
func TestValidateSkipProvidersRejectsUnknown(t *testing.T) {
	h := newAnalyzeHarness(t)
	h.svc.db = h.gdb
	p := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true}
	if err := h.gdb.Create(p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	ctx := context.Background()

	out, err := h.svc.validateSkipProviders(ctx, []string{p.ID, p.ID, ""})
	if err != nil {
		t.Fatalf("validate known: %v", err)
	}
	if len(out) != 1 || out[0] != p.ID {
		t.Fatalf("validate known = %v, want [%s] (deduped, empties dropped)", out, p.ID)
	}

	if _, err := h.svc.validateSkipProviders(ctx, []string{"nope"}); err == nil {
		t.Fatal("expected error for unknown provider id")
	}
}

// TestStartImportRejectsUnknownSkipProvider verifies the guard fires end-to-end
// through StartImport's buildOptions before any session/goroutine is created.
func TestStartImportRejectsUnknownSkipProvider(t *testing.T) {
	h := newAnalyzeHarness(t)
	h.svc.db = h.gdb
	h.write("a.jpg", "alpha")

	opts := h.copyOpts()
	opts.SkipProviderIds = []string{"ghost-provider"}
	_, err := h.svc.StartImport(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "unknown backup provider") {
		t.Fatalf("StartImport error = %v, want unknown-provider rejection", err)
	}
}

// TestBackfillSkipsOptedOut verifies a backfill does NOT re-queue an asset the
// user deliberately opted out of: the opted_out job satisfies the idempotency
// "exists" check, so no competing pending job is created and the missing-count
// excludes it.
func TestBackfillSkipsOptedOut(t *testing.T) {
	svc, gdb, assets, jobs, providerID := newBackfillHarness(t)
	ctx := context.Background()

	optedOut := seedBackfillAsset(t, assets, "skip.jpg", "2019/skip.jpg", domain.VerificationStatusVerified, nil, false)
	normal := seedBackfillAsset(t, assets, "keep.jpg", "2019/keep.jpg", domain.VerificationStatusVerified, nil, false)
	if _, _, err := jobs.EnqueueOptedOut(ctx, optedOut.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	// Missing count excludes the opted-out asset (only the normal one is a gap).
	if n, err := assets.CountEligibleMissingBackup(ctx, providerID); err != nil {
		t.Fatalf("missing count: %v", err)
	} else if n != 1 {
		t.Fatalf("missing count = %d, want 1 (opted_out excluded)", n)
	}

	if _, err := svc.StartBackfill(ctx, providerID); err != nil {
		t.Fatalf("start backfill: %v", err)
	}
	waitBackfillDone(t, svc)

	byAsset := map[string]domain.BackupJob{}
	for _, j := range jobsForProvider(t, gdb, providerID) {
		byAsset[j.AssetID] = j
	}
	if got := byAsset[optedOut.ID].Status; got != domain.JobStatusOptedOut {
		t.Fatalf("opted-out asset job = %q, want unchanged opted_out (backfill must skip it)", got)
	}
	if got := byAsset[normal.ID].Status; got != domain.JobStatusPending {
		t.Fatalf("normal asset job = %q, want pending (backfill fills the real gap)", got)
	}
}

// TestRequeueOptedOutService verifies the reversal service method flips opted_out
// jobs to pending and reports the count.
func TestRequeueOptedOutService(t *testing.T) {
	svc, gdb, assets, jobs, providerID := newBackfillHarness(t)
	ctx := context.Background()

	a := seedBackfillAsset(t, assets, "x.jpg", "2019/x.jpg", domain.VerificationStatusVerified, nil, false)
	if _, _, err := jobs.EnqueueOptedOut(ctx, a.ID, "localfs", providerID, 0); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	n, err := svc.RequeueOptedOut(ctx, providerID, "")
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 1 {
		t.Fatalf("requeued %d, want 1", n)
	}
	var j domain.BackupJob
	if err := gdb.Where("asset_id = ? AND destination = ?", a.ID, providerID).First(&j).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	if j.Status != domain.JobStatusPending {
		t.Fatalf("job status = %q, want pending after Queue anyway", j.Status)
	}
}
