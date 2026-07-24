package backup_test

import (
	"context"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// addScopedProvider creates an enabled provider with the given media scope.
func (h *testHarness) addScopedProvider(t *testing.T, plugin, scope string) *domain.BackupProvider {
	t.Helper()
	p := &domain.BackupProvider{PluginName: plugin, ConfigJSON: "{}", Enabled: true, MediaScope: scope}
	if err := h.db.Create(p).Error; err != nil {
		t.Fatalf("create scoped provider: %v", err)
	}
	return p
}

// addAssetExt creates a verified, archived asset with the given extension/media
// type so scope decisions (which key on the extension) can be exercised.
func (h *testHarness) addAssetExt(t *testing.T, ext string, mt domain.MediaType) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   "IMG." + ext,
		OriginalExtension:  ext,
		QuickHash:          "qh-" + t.Name() + "-" + ext,
		FileSize:           1234,
		MediaType:          mt,
		CurrentArchivePath: "/library/2024/2024-01-01/IMG." + ext,
		ImportDate:         time.Now(),
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
	}
	if err := h.assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

// TestEnqueueForAsset_ScopeSkipsOutOfScope verifies the derived scope exclusion:
// a provider whose scope does not accept the asset's kind gets NO job at all — not
// a pending one, and not an opted_out one (that distinction is the whole point:
// scope exclusion is derived from the provider's config, never recorded as a row).
// In-scope providers get their pending jobs as usual, and the returned "created"
// count reflects only real pending work.
func TestEnqueueForAsset_ScopeSkipsOutOfScope(t *testing.T) {
	h := newHarness(t)
	all := h.addScopedProvider(t, "localfs", "")               // all kinds
	photos := h.addScopedProvider(t, "localfs", "photos")      // photos only
	videos := h.addScopedProvider(t, "localfs", "videos")      // videos only
	raws := h.addScopedProvider(t, "localfs", "photos,videos") // no RAW

	// A photo (jpg): in scope for all, photos, and photos+videos; out of scope for videos.
	photo := h.addAssetExt(t, "jpg", domain.MediaTypePhoto)

	ctx := context.Background()
	var created int
	err := h.db.Transaction(func(tx *gorm.DB) error {
		m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
		var e error
		created, e = m.EnqueueForAsset(ctx, tx, photo.ID, nil)
		return e
	})
	if err != nil {
		t.Fatalf("enqueue for asset: %v", err)
	}

	byDest := jobsByDestination(t, h, photo.ID)
	// videos-only provider must have NO job row for a photo.
	if _, ok := byDest[videos.ID]; ok {
		t.Fatalf("videos-only provider got a job for a photo; expected none (derived exclusion, not a row)")
	}
	// The three in-scope providers each get a pending job.
	for _, p := range []*domain.BackupProvider{all, photos, raws} {
		j, ok := byDest[p.ID]
		if !ok {
			t.Fatalf("in-scope provider %q got no job", p.ID)
		}
		if j.Status != domain.JobStatusPending {
			t.Fatalf("in-scope provider %q job status = %q, want pending", p.ID, j.Status)
		}
	}
	if len(byDest) != 3 {
		t.Fatalf("want 3 job rows (one per in-scope provider), got %d", len(byDest))
	}
	if created != 3 {
		t.Fatalf("created = %d, want 3 (only in-scope pending work)", created)
	}
}

// TestEnqueueForAsset_ScopeAndOptOut verifies scope composes with per-import
// opt-out: an out-of-scope provider gets nothing even if it is also in the skip
// list (no meaningless opted_out row), while an in-scope skipped provider still
// records its durable opted_out marker.
func TestEnqueueForAsset_ScopeAndOptOut(t *testing.T) {
	h := newHarness(t)
	photos := h.addScopedProvider(t, "localfs", "photos") // in scope for a photo
	videos := h.addScopedProvider(t, "localfs", "videos") // out of scope for a photo

	photo := h.addAssetExt(t, "jpg", domain.MediaTypePhoto)

	ctx := context.Background()
	err := h.db.Transaction(func(tx *gorm.DB) error {
		m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
		// Skip BOTH providers: the in-scope one becomes opted_out; the out-of-scope
		// one is excluded by scope and records nothing.
		_, e := m.EnqueueForAsset(ctx, tx, photo.ID, []string{photos.ID, videos.ID})
		return e
	})
	if err != nil {
		t.Fatalf("enqueue for asset: %v", err)
	}

	byDest := jobsByDestination(t, h, photo.ID)
	if got := byDest[photos.ID].Status; got != domain.JobStatusOptedOut {
		t.Fatalf("in-scope skipped provider status = %q, want opted_out", got)
	}
	if _, ok := byDest[videos.ID]; ok {
		t.Fatalf("out-of-scope provider recorded a row despite being excluded by scope")
	}
	if len(byDest) != 1 {
		t.Fatalf("want exactly 1 row (the in-scope opt-out), got %d", len(byDest))
	}
}

// TestCountEligibleMissingBackup_ScopeAware verifies the missing-count excludes
// out-of-scope assets: with a photos+videos scope, a RAW asset is not counted as
// "missing" for the provider (it is not a gap for a destination that never accepts
// RAW), while photos and videos are.
func TestCountEligibleMissingBackup_ScopeAware(t *testing.T) {
	h := newHarness(t)
	provider := h.addScopedProvider(t, "localfs", "photos,videos")

	h.addAssetExt(t, "jpg", domain.MediaTypePhoto)
	h.addAssetExt(t, "mov", domain.MediaTypeVideo)
	h.addAssetExt(t, "dng", domain.MediaTypeRawPhoto)

	ctx := context.Background()
	// Scoped: RAW excluded -> 2 missing (photo + video).
	n, err := h.assets.CountEligibleMissingBackup(ctx, provider.ID, provider.MediaScope)
	if err != nil {
		t.Fatalf("count scoped: %v", err)
	}
	if n != 2 {
		t.Fatalf("scoped missing = %d, want 2 (RAW excluded from photos+videos)", n)
	}
	// Empty scope: all three counted.
	nAll, err := h.assets.CountEligibleMissingBackup(ctx, provider.ID, "")
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	if nAll != 3 {
		t.Fatalf("all-scope missing = %d, want 3", nAll)
	}
}
