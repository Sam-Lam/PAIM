package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// newBrowserHarness builds a BrowserService over a temp SQLite DB.
func newBrowserHarness(t *testing.T) (*BrowserService, *gorm.DB, *repo.AssetRepo) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "browse.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc := NewBrowserService(gdb, repo.NewAssetRepo(gdb), repo.NewSourceRepo(gdb), repo.NewSessionRepo(gdb), nil)
	return svc, gdb, svc.assets
}

func seedBrowseAsset(t *testing.T, assets *repo.AssetRepo, filename string, mt domain.MediaType, capture *time.Time) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   filename,
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + filename,
		CurrentArchivePath: "2026/" + filename,
		CaptureDate:        capture,
		MediaType:          mt,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

func mustTime(s string) *time.Time {
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &tm
}

func TestListAssetsSortsByCaptureDateDescNullsLast(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()

	seedBrowseAsset(t, assets, "old.jpg", domain.MediaTypePhoto, mustTime("2024-01-15"))
	seedBrowseAsset(t, assets, "new.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	seedBrowseAsset(t, assets, "nodate.jpg", domain.MediaTypePhoto, nil)

	res, err := svc.ListAssets(ctx, BrowseFilters{}, 1, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
	order := []string{res.Items[0].Filename, res.Items[1].Filename, res.Items[2].Filename}
	want := []string{"new.jpg", "old.jpg", "nodate.jpg"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %s, want %s (full order %v)", i, order[i], want[i], order)
		}
	}
}

func TestListAssetsFiltersAndPaginates(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()

	seedBrowseAsset(t, assets, "p1.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	seedBrowseAsset(t, assets, "p2.jpg", domain.MediaTypePhoto, mustTime("2026-06-01"))
	seedBrowseAsset(t, assets, "v1.mov", domain.MediaTypeVideo, mustTime("2026-07-02"))

	// Media-type filter.
	res, err := svc.ListAssets(ctx, BrowseFilters{MediaType: "video"}, 1, 50)
	if err != nil {
		t.Fatalf("list video: %v", err)
	}
	if res.Total != 1 || len(res.Items) != 1 || res.Items[0].Filename != "v1.mov" {
		t.Fatalf("video filter = %+v", res)
	}

	// Year-month filter (capture month).
	res, err = svc.ListAssets(ctx, BrowseFilters{YearMonth: "2026-07"}, 1, 50)
	if err != nil {
		t.Fatalf("list month: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("2026-07 total = %d, want 2", res.Total)
	}

	// Pagination: true total across pages.
	res, err = svc.ListAssets(ctx, BrowseFilters{}, 1, 2)
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if res.Total != 3 || len(res.Items) != 2 {
		t.Errorf("page1 total=%d items=%d, want total 3 items 2", res.Total, len(res.Items))
	}

	// Text query on filename.
	res, err = svc.ListAssets(ctx, BrowseFilters{Query: "v1"}, 1, 50)
	if err != nil {
		t.Fatalf("list query: %v", err)
	}
	if res.Total != 1 || res.Items[0].Filename != "v1.mov" {
		t.Errorf("query filter = %+v", res)
	}
}

func TestMonthsReturnsCaptureMonthCounts(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()

	seedBrowseAsset(t, assets, "a.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))
	seedBrowseAsset(t, assets, "b.jpg", domain.MediaTypePhoto, mustTime("2026-07-20"))
	seedBrowseAsset(t, assets, "c.jpg", domain.MediaTypePhoto, mustTime("2026-06-01"))
	seedBrowseAsset(t, assets, "nodate.jpg", domain.MediaTypePhoto, nil)

	months, err := svc.Months(ctx)
	if err != nil {
		t.Fatalf("months: %v", err)
	}
	// Newest month first; assets without a capture date are excluded.
	if len(months) != 2 {
		t.Fatalf("months = %+v, want 2", months)
	}
	if months[0].Month != "2026-07" || months[0].Count != 2 {
		t.Errorf("months[0] = %+v, want 2026-07 count 2", months[0])
	}
	if months[1].Month != "2026-06" || months[1].Count != 1 {
		t.Errorf("months[1] = %+v, want 2026-06 count 1", months[1])
	}
}

func TestRevealAssetResolvesPathServerSide(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()

	root := t.TempDir()
	svc.root = root

	// An archived file that exists, plus an original source file that exists.
	archiveRel := filepath.Join("2026", "IMG_1.jpg")
	archiveAbs := filepath.Join(root, archiveRel)
	if err := os.MkdirAll(filepath.Dir(archiveAbs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(archiveAbs, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	origAbs := filepath.Join(t.TempDir(), "card", "IMG_1.jpg")
	if err := os.MkdirAll(filepath.Dir(origAbs), 0o755); err != nil {
		t.Fatalf("mkdir orig: %v", err)
	}
	if err := os.WriteFile(origAbs, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write orig: %v", err)
	}

	a := &domain.Asset{
		OriginalFilename:   "IMG_1.jpg",
		QuickHash:          "qh-reveal",
		CurrentArchivePath: filepath.ToSlash(archiveRel),
		OriginalFullPath:   origAbs,
		VerificationStatus: domain.VerificationStatusVerified,
	}
	if err := assets.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Capture the path the injected runner is asked to reveal (never spawn `open`).
	var revealed string
	svc.reveal = func(path string) error {
		revealed = path
		return nil
	}

	// which=archive resolves the RELATIVE stored path against the root.
	if err := svc.RevealAsset(ctx, a.ID, RevealArchive); err != nil {
		t.Fatalf("reveal archive: %v", err)
	}
	if revealed != archiveAbs {
		t.Errorf("archive reveal path = %q, want %q", revealed, archiveAbs)
	}

	// which=original reveals the original source path unchanged.
	revealed = ""
	if err := svc.RevealAsset(ctx, a.ID, RevealOriginal); err != nil {
		t.Fatalf("reveal original: %v", err)
	}
	if revealed != origAbs {
		t.Errorf("original reveal path = %q, want %q", revealed, origAbs)
	}
}

func TestRevealAssetValidatesWhich(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()

	called := false
	svc.reveal = func(string) error { called = true; return nil }

	a := &domain.Asset{OriginalFilename: "x.jpg", QuickHash: "qh-x", VerificationStatus: domain.VerificationStatusVerified}
	if err := assets.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := svc.RevealAsset(ctx, a.ID, "sideways")
	if err == nil {
		t.Fatal("expected error for unknown which")
	}
	if !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("error = %v, want unknown-target", err)
	}
	if called {
		t.Error("runner must not be invoked for an invalid which")
	}
}

func TestRevealAssetNotFoundAndMissingFile(t *testing.T) {
	svc, _, assets := newBrowserHarness(t)
	ctx := context.Background()
	svc.root = t.TempDir()

	called := false
	svc.reveal = func(string) error { called = true; return nil }

	// Unknown asset ID.
	if err := svc.RevealAsset(ctx, "no-such-id", RevealArchive); err == nil {
		t.Error("expected error for unknown asset")
	}

	// Asset with no archive copy (copy-mode duplicate) → archive reveal errors.
	dup := &domain.Asset{OriginalFilename: "d.jpg", QuickHash: "qh-d", CurrentArchivePath: "",
		OriginalFullPath: filepath.Join(t.TempDir(), "gone.jpg"), VerificationStatus: domain.VerificationStatusVerified}
	if err := assets.Create(ctx, dup); err != nil {
		t.Fatalf("create dup: %v", err)
	}
	if err := svc.RevealAsset(ctx, dup.ID, RevealArchive); err == nil {
		t.Error("expected error for asset with no archive copy")
	}

	// Original path points at a file that does not exist → not-found error.
	if err := svc.RevealAsset(ctx, dup.ID, RevealOriginal); err == nil {
		t.Error("expected not-found error for missing original file")
	}

	if called {
		t.Error("runner must not be invoked when path resolution/stat fails")
	}
}

func TestAssetDetailProvenanceAndRelationships(t *testing.T) {
	svc, gdb, assets := newBrowserHarness(t)
	ctx := context.Background()

	// A source and session to join.
	src := &domain.ImportSource{VolumeLabel: "EOS_DIGITAL", SourceType: domain.SourceTypeSDCard}
	if err := repo.NewSourceRepo(gdb).Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	sess := &domain.ImportSession{StartedAt: *mustTime("2026-07-10"), Status: domain.SessionStatusCompleted}
	if err := repo.NewSessionRepo(gdb).Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	orig := seedBrowseAsset(t, assets, "orig.jpg", domain.MediaTypePhoto, mustTime("2026-07-01"))

	// The detailed asset: linked to source/session, with a duplicate and a partner.
	a := &domain.Asset{
		OriginalFilename:   "IMG_1.heic",
		OriginalExtension:  "heic",
		QuickHash:          "qh-detail",
		FullHash:           "fh-detail",
		CurrentArchivePath: "2026/IMG_1.heic",
		CaptureDate:        mustTime("2026-07-05"),
		MediaType:          domain.MediaTypeLivePhotoPair,
		SourceID:           src.ID,
		SessionID:          sess.ID,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
	}
	if err := assets.Create(ctx, a); err != nil {
		t.Fatalf("create detail asset: %v", err)
	}
	// Mark 'a' as a duplicate of orig, and create another asset that duplicates 'a'.
	if err := assets.MarkDuplicateOf(ctx, a.ID, orig.ID); err != nil {
		t.Fatalf("mark dup: %v", err)
	}
	dupOfA := &domain.Asset{OriginalFilename: "copy.heic", QuickHash: "qh-copy", DuplicateOfAssetID: &a.ID,
		VerificationStatus: domain.VerificationStatusVerified}
	if err := assets.Create(ctx, dupOfA); err != nil {
		t.Fatalf("create dupOfA: %v", err)
	}
	// A backup job for 'a'.
	job := &domain.BackupJob{AssetID: a.ID, Plugin: "localfs", Destination: "/backup", Status: domain.JobStatusCompleted}
	if err := gdb.Create(job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}

	root := "/Volumes/Master"
	svc.root = root

	d, err := svc.AssetDetail(ctx, a.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.QuickHash != "qh-detail" || d.FullHash != "fh-detail" {
		t.Errorf("hashes = %q/%q", d.QuickHash, d.FullHash)
	}
	if d.CurrentArchivePath != filepath.Join(root, "2026/IMG_1.heic") {
		t.Errorf("resolved path = %q", d.CurrentArchivePath)
	}
	if d.SourceLabel != "EOS_DIGITAL" || d.SourceType != string(domain.SourceTypeSDCard) {
		t.Errorf("source = %q/%q", d.SourceLabel, d.SourceType)
	}
	if d.SessionDate == nil {
		t.Error("session date nil")
	}
	if !d.IsLivePhotoPair {
		t.Error("expected IsLivePhotoPair")
	}
	if d.DuplicateOf == nil || d.DuplicateOf.ID != orig.ID {
		t.Errorf("duplicateOf = %+v, want %s", d.DuplicateOf, orig.ID)
	}
	if len(d.Duplicates) != 1 || d.Duplicates[0].ID != dupOfA.ID {
		t.Errorf("duplicates = %+v, want [%s]", d.Duplicates, dupOfA.ID)
	}
	if len(d.BackupJobs) != 1 || d.BackupJobs[0].Plugin != "localfs" || d.BackupJobs[0].Status != string(domain.JobStatusCompleted) {
		t.Errorf("backup jobs = %+v", d.BackupJobs)
	}
}
