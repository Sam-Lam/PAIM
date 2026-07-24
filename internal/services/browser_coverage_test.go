package services

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// coverageHarness extends the browser harness with a bound backup repo (so the
// bulk queue action works) and provider/job seed helpers.
func coverageHarness(t *testing.T) (*BrowserService, *gorm.DB, *repo.AssetRepo) {
	t.Helper()
	svc, gdb, assets := newBrowserHarness(t)
	svc.jobs = repo.NewBackupRepo(gdb)
	return svc, gdb, assets
}

func seedProvider(t *testing.T, gdb *gorm.DB, plugin, configJSON string, mirror bool) *domain.BackupProvider {
	t.Helper()
	p := &domain.BackupProvider{PluginName: plugin, ConfigJSON: configJSON, Enabled: true, Mirror: mirror}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return p
}

func seedJob(t *testing.T, gdb *gorm.DB, assetID, dest, plugin string, status domain.JobStatus, note string, completed *time.Time) *domain.BackupJob {
	t.Helper()
	j := &domain.BackupJob{AssetID: assetID, Plugin: plugin, Destination: dest, Status: status, ErrorMessage: note, CompletedAt: completed}
	if err := gdb.Create(j).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	return j
}

// providerStatusOf finds the coverage status for one destination in a row, or
// "none" when the row has no entry for it (mirroring the frontend's rendering).
func providerStatusOf(row CoverageRowDTO, dest string) string {
	for _, p := range row.Providers {
		if p.ProviderID == dest {
			return p.Status
		}
	}
	return CoverageStatusNone
}

func rowByFilename(items []CoverageRowDTO, name string) (CoverageRowDTO, bool) {
	for _, r := range items {
		if r.Filename == name {
			return r, true
		}
	}
	return CoverageRowDTO{}, false
}

func TestCoverageRowDerivation(t *testing.T) {
	svc, gdb, assets := coverageHarness(t)
	ctx := context.Background()

	pOK := seedProvider(t, gdb, "localfs", `{"root":"/backup"}`, false)   // non-mirror
	pMirror := seedProvider(t, gdb, "rclone", `{"remote":"gphotos:"}`, true) // mirror

	now := time.Now().UTC()

	// verified: completed on a non-mirror destination, no note.
	aVer := seedBrowseAsset(t, assets, "verified.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	seedJob(t, gdb, aVer.ID, pOK.ID, "localfs", domain.JobStatusCompleted, "", &now)

	// uploaded_unverified via mirror: completed on a mirror destination.
	aMir := seedBrowseAsset(t, assets, "mirror.jpg", domain.MediaTypePhoto, mustTime("2026-07-02"))
	seedJob(t, gdb, aMir.ID, pMirror.ID, "rclone", domain.JobStatusCompleted, "", &now)

	// uploaded_unverified via note: completed on a non-mirror destination WITH a
	// verify-unavailable note recorded in error_message.
	aNote := seedBrowseAsset(t, assets, "note.jpg", domain.MediaTypePhoto, mustTime("2026-07-03"))
	seedJob(t, gdb, aNote.ID, pOK.ID, "localfs", domain.JobStatusCompleted, "verify unavailable", &now)

	// skipped: opted_out.
	aSkip := seedBrowseAsset(t, assets, "skip.jpg", domain.MediaTypePhoto, mustTime("2026-07-04"))
	seedJob(t, gdb, aSkip.ID, pOK.ID, "localfs", domain.JobStatusOptedOut, "", nil)

	// failed.
	aFail := seedBrowseAsset(t, assets, "fail.jpg", domain.MediaTypePhoto, mustTime("2026-07-05"))
	seedJob(t, gdb, aFail.ID, pOK.ID, "localfs", domain.JobStatusFailed, "boom", nil)

	// none: no job at all → empty providers list, "none" per column.
	seedBrowseAsset(t, assets, "none.jpg", domain.MediaTypePhoto, mustTime("2026-07-06"))

	res, err := svc.ListCoverage(ctx, BrowseFilters{}, nil, 1, 50)
	if err != nil {
		t.Fatalf("list coverage: %v", err)
	}
	if res.Total != 6 {
		t.Fatalf("total = %d, want 6", res.Total)
	}

	checks := []struct {
		file, dest, want string
	}{
		{"verified.jpg", pOK.ID, CoverageStatusVerified},
		{"mirror.jpg", pMirror.ID, CoverageStatusUploadedUnverified},
		{"note.jpg", pOK.ID, CoverageStatusUploadedUnverified},
		{"skip.jpg", pOK.ID, CoverageStatusSkipped},
		{"fail.jpg", pOK.ID, CoverageStatusFailed},
		{"none.jpg", pOK.ID, CoverageStatusNone},
	}
	for _, c := range checks {
		row, ok := rowByFilename(res.Items, c.file)
		if !ok {
			t.Errorf("row %q missing", c.file)
			continue
		}
		if got := providerStatusOf(row, c.dest); got != c.want {
			t.Errorf("%s status = %q, want %q", c.file, got, c.want)
		}
	}

	// The failed row carries the error message as its note; none row has no providers.
	if row, _ := rowByFilename(res.Items, "fail.jpg"); providerStatusOf(row, pOK.ID) == CoverageStatusFailed {
		if row.Providers[0].Note != "boom" {
			t.Errorf("failed note = %q, want boom", row.Providers[0].Note)
		}
	}
	if row, _ := rowByFilename(res.Items, "none.jpg"); len(row.Providers) != 0 {
		t.Errorf("none row providers = %+v, want empty", row.Providers)
	}
}

func TestCoverageProvidersIncludesRemoved(t *testing.T) {
	svc, gdb, assets := coverageHarness(t)
	ctx := context.Background()

	pLive := seedProvider(t, gdb, "localfs", `{"root":"/backup"}`, false)
	pMir := seedProvider(t, gdb, "rclone", `{"remote":"gphotos:"}`, true)
	pGone := seedProvider(t, gdb, "localfs", `{"root":"/old"}`, false)

	a := seedBrowseAsset(t, assets, "a.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	seedJob(t, gdb, a.ID, pLive.ID, "localfs", domain.JobStatusCompleted, "", nil)
	seedJob(t, gdb, a.ID, pMir.ID, "rclone", domain.JobStatusPending, "", nil)
	seedJob(t, gdb, a.ID, pGone.ID, "localfs", domain.JobStatusCompleted, "", nil)

	// Soft-delete the third provider AFTER its job exists — the coverage column must
	// survive and be flagged removed.
	if err := gdb.Delete(&domain.BackupProvider{}, "id = ?", pGone.ID).Error; err != nil {
		t.Fatalf("soft delete provider: %v", err)
	}

	provs, err := svc.CoverageProviders(ctx)
	if err != nil {
		t.Fatalf("coverage providers: %v", err)
	}
	if len(provs) != 3 {
		t.Fatalf("providers = %d, want 3 (%+v)", len(provs), provs)
	}
	byID := map[string]CoverageProviderDTO{}
	for _, p := range provs {
		byID[p.ProviderID] = p
	}
	if p := byID[pLive.ID]; p.Name != "/backup" || p.Removed || p.Mirror {
		t.Errorf("live provider = %+v", p)
	}
	if p := byID[pMir.ID]; p.Name != "gphotos:" || !p.Mirror || p.Removed {
		t.Errorf("mirror provider = %+v", p)
	}
	if p := byID[pGone.ID]; p.Name != "/old" || !p.Removed {
		t.Errorf("removed provider = %+v, want name /old removed=true", p)
	}
	// Live providers sort before removed ones.
	if provs[len(provs)-1].ProviderID != pGone.ID {
		t.Errorf("removed provider should sort last, got order %+v", provs)
	}

	// The removed provider's job still derives a status in the row.
	res, err := svc.ListCoverage(ctx, BrowseFilters{}, nil, 1, 50)
	if err != nil {
		t.Fatalf("list coverage: %v", err)
	}
	row, _ := rowByFilename(res.Items, "a.jpg")
	if got := providerStatusOf(row, pGone.ID); got != CoverageStatusVerified {
		t.Errorf("removed-provider status = %q, want verified", got)
	}
}

func TestCoverageProviderStatusFilterComposed(t *testing.T) {
	svc, gdb, assets := coverageHarness(t)
	ctx := context.Background()

	p := seedProvider(t, gdb, "localfs", `{"root":"/backup"}`, false)

	// Photos in various states for provider p.
	aNone := seedBrowseAsset(t, assets, "none.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	_ = aNone // no job → none
	aSkip := seedBrowseAsset(t, assets, "skip.jpg", domain.MediaTypePhoto, mustTime("2026-07-02"))
	seedJob(t, gdb, aSkip.ID, p.ID, "localfs", domain.JobStatusOptedOut, "", nil)
	aFail := seedBrowseAsset(t, assets, "fail.jpg", domain.MediaTypePhoto, mustTime("2026-07-03"))
	seedJob(t, gdb, aFail.ID, p.ID, "localfs", domain.JobStatusFailed, "boom", nil)
	aVer := seedBrowseAsset(t, assets, "ver.jpg", domain.MediaTypePhoto, mustTime("2026-07-04"))
	seedJob(t, gdb, aVer.ID, p.ID, "localfs", domain.JobStatusCompleted, "", nil)

	// A VIDEO with no job — used to prove the provider filter AND-composes with the
	// Batch-B media-type filter (it must be excluded when we scope to photos).
	seedBrowseAsset(t, assets, "clip.mov", domain.MediaTypeVideo, mustTime("2026-07-05"))

	cases := []struct {
		status string
		want   []string
	}{
		{CoverageStatusNone, []string{"none.jpg"}},   // video excluded by media filter
		{CoverageStatusSkipped, []string{"skip.jpg"}},
		{CoverageStatusFailed, []string{"fail.jpg"}},
		{CoverageStatusVerified, []string{"ver.jpg"}},
	}
	for _, c := range cases {
		filter := &CoverageProviderFilter{ProviderID: p.ID, Status: c.status}
		res, err := svc.ListCoverage(ctx, BrowseFilters{MediaType: "photo"}, filter, 1, 50)
		if err != nil {
			t.Fatalf("list coverage %s: %v", c.status, err)
		}
		got := map[string]bool{}
		for _, r := range res.Items {
			got[r.Filename] = true
		}
		if int(res.Total) != len(c.want) || len(res.Items) != len(c.want) {
			t.Errorf("%s: total=%d items=%d, want %d (%v)", c.status, res.Total, len(res.Items), len(c.want), got)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: missing %q (got %v)", c.status, w, got)
			}
		}
	}
}

func TestQueueAssetsForProviderFlipAndEnqueue(t *testing.T) {
	svc, gdb, assets := coverageHarness(t)
	ctx := context.Background()

	p := seedProvider(t, gdb, "localfs", `{"root":"/backup"}`, false)

	// opted_out → flipped to pending.
	aSkip := seedBrowseAsset(t, assets, "skip.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	seedJob(t, gdb, aSkip.ID, p.ID, "localfs", domain.JobStatusOptedOut, "", nil)
	// no job → fresh pending created.
	aFresh := seedBrowseAsset(t, assets, "fresh.jpg", domain.MediaTypePhoto, mustTime("2026-07-02"))
	// completed → untouched, not counted.
	aDone := seedBrowseAsset(t, assets, "done.jpg", domain.MediaTypePhoto, mustTime("2026-07-03"))
	seedJob(t, gdb, aDone.ID, p.ID, "localfs", domain.JobStatusCompleted, "", nil)

	n, err := svc.QueueAssetsForProvider(ctx, p.ID, []string{aSkip.ID, aFresh.ID, aDone.ID})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if n != 2 {
		t.Fatalf("queued = %d, want 2 (flip + fresh)", n)
	}

	// The opted_out job is now pending.
	var skipJob domain.BackupJob
	if err := gdb.Where("asset_id = ? AND destination = ?", aSkip.ID, p.ID).First(&skipJob).Error; err != nil {
		t.Fatalf("load skip job: %v", err)
	}
	if skipJob.Status != domain.JobStatusPending {
		t.Errorf("flipped job status = %q, want pending", skipJob.Status)
	}

	// A fresh pending job exists for the destination with the provider's plugin.
	var freshJob domain.BackupJob
	if err := gdb.Where("asset_id = ? AND destination = ?", aFresh.ID, p.ID).First(&freshJob).Error; err != nil {
		t.Fatalf("load fresh job: %v", err)
	}
	if freshJob.Status != domain.JobStatusPending || freshJob.Plugin != "localfs" {
		t.Errorf("fresh job = %+v, want pending/localfs", freshJob)
	}

	// The completed job is untouched.
	var doneJob domain.BackupJob
	if err := gdb.Where("asset_id = ? AND destination = ?", aDone.ID, p.ID).First(&doneJob).Error; err != nil {
		t.Fatalf("load done job: %v", err)
	}
	if doneJob.Status != domain.JobStatusCompleted {
		t.Errorf("done job status = %q, want completed (untouched)", doneJob.Status)
	}

	// Re-queueing the same set is now a no-op (all pending/completed).
	n2, err := svc.QueueAssetsForProvider(ctx, p.ID, []string{aSkip.ID, aFresh.ID, aDone.ID})
	if err != nil {
		t.Fatalf("re-queue: %v", err)
	}
	if n2 != 0 {
		t.Errorf("re-queue queued = %d, want 0", n2)
	}
}

func TestQueueAssetsForProviderCapAndValidation(t *testing.T) {
	svc, gdb, _ := coverageHarness(t)
	ctx := context.Background()
	p := seedProvider(t, gdb, "localfs", `{"root":"/backup"}`, false)

	// Empty provider id.
	if _, err := svc.QueueAssetsForProvider(ctx, "", []string{"x"}); err == nil {
		t.Error("expected error for empty provider id")
	}
	// Unknown provider.
	if _, err := svc.QueueAssetsForProvider(ctx, "no-such", []string{"x"}); err == nil {
		t.Error("expected error for unknown provider")
	}
	// Empty asset list is a no-op, not an error.
	if n, err := svc.QueueAssetsForProvider(ctx, p.ID, nil); err != nil || n != 0 {
		t.Errorf("empty list = (%d,%v), want (0,nil)", n, err)
	}
	// Over the cap.
	big := make([]string, coverageQueueCap+1)
	if _, err := svc.QueueAssetsForProvider(ctx, p.ID, big); err == nil {
		t.Error("expected error over the queue cap")
	}
}

func TestCoveragePaginationTotals(t *testing.T) {
	svc, _, assets := coverageHarness(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		seedBrowseAsset(t, assets, "p"+string(rune('a'+i))+".jpg", domain.MediaTypePhoto, mustTime("2026-07-0"+string(rune('1'+i))))
	}
	res, err := svc.ListCoverage(ctx, BrowseFilters{}, nil, 1, 2)
	if err != nil {
		t.Fatalf("list coverage: %v", err)
	}
	if res.Total != 5 {
		t.Errorf("total = %d, want 5", res.Total)
	}
	if len(res.Items) != 2 {
		t.Errorf("page1 items = %d, want 2", len(res.Items))
	}
	if res.Page != 1 || res.PageSize != 2 {
		t.Errorf("page/size = %d/%d, want 1/2", res.Page, res.PageSize)
	}
}
