package services

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/thumbs"
)

// blockingResolver blocks Resolve until release is closed (or ctx is cancelled),
// so a warm-up stays in flight long enough to assert the single-instance guard.
type blockingResolver struct{ release chan struct{} }

func (b blockingResolver) Resolve(ctx context.Context, _ string) (string, string, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	return "", "", thumbs.ErrSourceMissing // after release, skip cleanly
}

func newThumbHarness(t *testing.T) (*ThumbnailService, *repo.AssetRepo, string) {
	t.Helper()
	root := t.TempDir()
	gdb, err := db.Open(filepath.Join(root, ".paim", "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	svc := NewThumbnailService(nil, nil, nil)
	svc.assets = assets
	svc.settings = repo.NewSettingsRepo(gdb)
	svc.root = root
	return svc, assets, root
}

func seedArchivedAsset(t *testing.T, assets *repo.AssetRepo, name string) {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   name,
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + name,
		CurrentArchivePath: "2026/" + name,
		MediaType:          domain.MediaTypePhoto,
		VerificationStatus: domain.VerificationStatusVerified,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
}

func TestWarmupSingleInstance(t *testing.T) {
	svc, assets, root := newThumbHarness(t)
	seedArchivedAsset(t, assets, "a.jpg")
	seedArchivedAsset(t, assets, "b.jpg")

	release := make(chan struct{})
	cache := thumbs.NewInDir(library.LibraryThumbsDir(root), nil)
	svc.cache = cache
	svc.warmer = thumbs.NewWarmer(cache, blockingResolver{release: release}, 1, nil)

	first, err := svc.StartWarmupAll(context.Background())
	if err != nil {
		t.Fatalf("first warm-up: %v", err)
	}
	if !first.Running || first.Total != 2 {
		t.Fatalf("first status = %+v, want running total 2", first)
	}

	// A second concurrent warm-up is refused politely.
	if _, err := svc.StartWarmupAll(context.Background()); !errors.Is(err, ErrWarmupInProgress) {
		t.Fatalf("second warm-up err = %v, want ErrWarmupInProgress", err)
	}

	// Cancel and confirm it drains back to idle.
	if err := svc.CancelWarmup(context.Background()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := svc.WarmupStatus(context.Background())
		if !st.Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("warm-up did not drain to idle after cancel")
}

func TestClearCacheGuardRejectsUnknownDir(t *testing.T) {
	svc, _, root := newThumbHarness(t)

	// A cache pointed OUTSIDE the two known roots must be refused.
	stray := filepath.Join(t.TempDir(), "stray")
	svc.cache = thumbs.NewInDir(stray, nil)
	if err := svc.ClearThumbnailCache(context.Background()); err == nil {
		t.Fatal("expected ClearThumbnailCache to refuse a non-known cache root")
	}

	// A cache at the in-library known root is cleared without error.
	svc.cache = thumbs.NewInDir(library.LibraryThumbsDir(root), nil)
	if err := svc.ClearThumbnailCache(context.Background()); err != nil {
		t.Fatalf("clear at known root: %v", err)
	}
}
