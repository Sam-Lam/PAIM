package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/metadata"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// fakeExtractor is a test metadata.Extractor. It returns configured metadata per
// path (keyed by absolute path) and zero-value metadata (mtime fallback) for
// anything else. It never fails.
type fakeExtractor struct {
	byPath    map[string]*metadata.AssetMetadata
	available bool
}

func newFakeExtractor() *fakeExtractor {
	return &fakeExtractor{byPath: map[string]*metadata.AssetMetadata{}, available: true}
}

func (f *fakeExtractor) set(path string, m *metadata.AssetMetadata) {
	m.SourceFile = path
	f.byPath[path] = m
}

func (f *fakeExtractor) Extract(_ context.Context, path string) (*metadata.AssetMetadata, error) {
	if m, ok := f.byPath[path]; ok {
		return m, nil
	}
	return &metadata.AssetMetadata{SourceFile: path}, nil
}

func (f *fakeExtractor) ExtractBatch(_ context.Context, paths []string) (map[string]*metadata.AssetMetadata, error) {
	out := make(map[string]*metadata.AssetMetadata, len(paths))
	for _, p := range paths {
		if m, ok := f.byPath[p]; ok {
			out[p] = m
		} else {
			out[p] = &metadata.AssetMetadata{SourceFile: p}
		}
	}
	return out, nil
}

func (f *fakeExtractor) Available() bool { return f.available }
func (f *fakeExtractor) Close() error    { return nil }

// countingEnqueuer records every asset it is asked to back up and reports how
// many jobs it "created" per asset (perAsset), letting tests exercise both the
// pending path (perAsset>0) and the no-provider none path (perAsset==0).
type countingEnqueuer struct {
	ids      []string
	perAsset int
	// lastSkip records the skipProviderIDs of the most recent call so tests can
	// assert the per-import opt-out set threads through recordAsset.
	lastSkip []string
}

func (c *countingEnqueuer) EnqueueForAsset(_ context.Context, _ *gorm.DB, id string, skipProviderIDs []string) (int, error) {
	c.ids = append(c.ids, id)
	c.lastSkip = skipProviderIDs
	return c.perAsset, nil
}

// harness bundles a Pipeline with its DB, repos, and temp directories.
type harness struct {
	t         *testing.T
	db        *gorm.DB
	assets    *repo.AssetRepo
	sessions  *repo.SessionRepo
	failures  *repo.ImportFailureRepo
	extractor *fakeExtractor
	enqueuer  *countingEnqueuer
	pipe      *Pipeline
	srcRoot   string
	destRoot  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paim_test.db")
	gdb, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	failures := repo.NewImportFailureRepo(gdb)
	ext := newFakeExtractor()
	enq := &countingEnqueuer{perAsset: 1}

	src := t.TempDir()
	dest := t.TempDir()

	pipe := New(Config{
		DB:        gdb,
		Assets:    assets,
		Sessions:  sessions,
		Failures:  failures,
		Extractor: ext,
		Layout:    archive.New(dest),
		Backup:    enq,
	})
	return &harness{
		t: t, db: gdb, assets: assets, sessions: sessions, failures: failures,
		extractor: ext, enqueuer: enq, pipe: pipe, srcRoot: src, destRoot: dest,
	}
}

// writeFile creates a source file with the given relative path and content, and
// sets its mtime to when (which drives the layout when no EXIF date is present).
func (h *harness) writeFile(rel, content string, when time.Time) string {
	h.t.Helper()
	full := filepath.Join(h.srcRoot, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(full, when, when); err != nil {
		h.t.Fatalf("chtimes: %v", err)
	}
	return full
}

func (h *harness) countAssets() int64 {
	h.t.Helper()
	var n int64
	if err := h.db.Model(&domain.Asset{}).Count(&n).Error; err != nil {
		h.t.Fatalf("count assets: %v", err)
	}
	return n
}

func (h *harness) copyOpts() Options {
	return Options{Mode: ModeCopy, SourceRoot: h.srcRoot, DestinationRoot: h.destRoot, EventName: "Trip"}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %s: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected file NOT to exist: %s", path)
	}
}

var testDate = time.Date(2023, 6, 15, 12, 0, 0, 0, time.Local)

// -----------------------------------------------------------------------------

func TestCopyImportHappyPath(t *testing.T) {
	h := newHarness(t)
	h.writeFile("IMG_0001.jpg", "jpeg-content-one", testDate)
	h.writeFile("IMG_0002.cr2", "raw-canon-content", testDate)
	h.writeFile("CLIP_0003.mp4", "movie-content-here", testDate)

	ctx := context.Background()
	session, err := h.pipe.Run(ctx, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Status != domain.SessionStatusCompleted {
		t.Fatalf("status = %s, want completed", session.Status)
	}
	if session.FilesImported != 3 {
		t.Fatalf("FilesImported = %d, want 3", session.FilesImported)
	}

	// Layout: <dest>/2023/2023-06-15 Trip/... with RAW under RAW/.
	dayDir := filepath.Join(h.destRoot, "2023", "2023-06-15 Trip")
	mustExist(t, filepath.Join(dayDir, "IMG_0001.jpg"))
	mustExist(t, filepath.Join(dayDir, "CLIP_0003.mp4"))
	mustExist(t, filepath.Join(dayDir, "RAW", "IMG_0002.cr2"))

	// Every asset must be verified.
	var assets []domain.Asset
	if err := h.db.Find(&assets).Error; err != nil {
		t.Fatalf("load assets: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("assets = %d, want 3", len(assets))
	}
	for _, a := range assets {
		if a.VerificationStatus != domain.VerificationStatusVerified {
			t.Fatalf("asset %s status = %s, want verified", a.OriginalFilename, a.VerificationStatus)
		}
		if a.QuickHash == "" || a.CurrentArchivePath == "" {
			t.Fatalf("asset %s missing hash/path", a.OriginalFilename)
		}
	}
	if len(h.enqueuer.ids) != 3 {
		t.Fatalf("backup enqueues = %d, want 3", len(h.enqueuer.ids))
	}
}

func TestDuplicateHandlingSecondRunNoCopies(t *testing.T) {
	h := newHarness(t)
	h.writeFile("a.jpg", "alpha", testDate)
	h.writeFile("b.jpg", "bravo", testDate)

	ctx := context.Background()
	if _, err := h.pipe.Run(ctx, h.copyOpts(), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstCount := h.countAssets()
	if firstCount != 2 {
		t.Fatalf("after first run assets = %d, want 2", firstCount)
	}

	// Second run of the identical tree: everything is already imported.
	session, err := h.pipe.Run(ctx, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := h.countAssets(); got != firstCount {
		t.Fatalf("second run created assets: %d != %d", got, firstCount)
	}
	if session.FilesImported != 0 {
		t.Fatalf("second run FilesImported = %d, want 0", session.FilesImported)
	}
	if session.Skipped != 2 {
		t.Fatalf("second run Skipped = %d, want 2", session.Skipped)
	}
}

func TestInternalDuplicateRecordedNotCopied(t *testing.T) {
	h := newHarness(t)
	// Two files with identical content but different names.
	h.writeFile("first.jpg", "identical-bytes", testDate)
	h.writeFile("second.jpg", "identical-bytes", testDate)

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.FilesImported != 1 {
		t.Fatalf("FilesImported = %d, want 1", session.FilesImported)
	}
	if session.Duplicates != 1 {
		t.Fatalf("Duplicates = %d, want 1", session.Duplicates)
	}

	var dups []domain.Asset
	if err := h.db.Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").Find(&dups).Error; err != nil {
		t.Fatalf("load dups: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("duplicate rows = %d, want 1", len(dups))
	}
	if dups[0].CurrentArchivePath != "" {
		t.Fatalf("duplicate should not be copied, got path %q", dups[0].CurrentArchivePath)
	}
}

func TestDryRunMutatesNothingAndPredictsCounts(t *testing.T) {
	h := newHarness(t)
	h.writeFile("x.jpg", "ex-content", testDate)
	h.writeFile("y.cr2", "why-raw-content", testDate)
	h.writeFile("z.mov", "zed-movie-content", testDate)

	ctx := context.Background()
	scan, err := h.pipe.Scan(ctx, h.srcRoot, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	report, err := h.pipe.DryRun(ctx, scan, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}

	// Nothing was written: no assets, no destination files.
	if got := h.countAssets(); got != 0 {
		t.Fatalf("dry run created %d assets", got)
	}
	if entries, _ := os.ReadDir(h.destRoot); len(entries) != 0 {
		t.Fatalf("dry run wrote to destination: %v", entries)
	}
	if report.New != 3 || report.Files != 3 {
		t.Fatalf("report New=%d Files=%d, want 3/3", report.New, report.Files)
	}

	// The subsequent import must match the prediction file-for-file.
	opts := h.copyOpts()
	opts.Precomputed = report
	session, err := h.pipe.Run(ctx, opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.FilesImported != report.New {
		t.Fatalf("import FilesImported=%d != dry run New=%d", session.FilesImported, report.New)
	}
}

func TestDryRunPredictsIntraBatchDuplicate(t *testing.T) {
	h := newHarness(t)
	// Two files share identical content (an intra-batch duplicate); a third is
	// unique. The asset DB is empty, so only intra-batch detection can catch it.
	h.writeFile("orig.jpg", "same-identical-bytes", testDate)
	h.writeFile("copy.jpg", "same-identical-bytes", testDate)
	h.writeFile("other.jpg", "distinct-unique-bytes", testDate)

	ctx := context.Background()
	scan, err := h.pipe.Scan(ctx, h.srcRoot, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	report, err := h.pipe.DryRun(ctx, scan, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Files != 3 {
		t.Fatalf("Files = %d, want 3", report.Files)
	}
	// First occurrence imports (New); the later copy is predicted as a Duplicate.
	if report.New != 2 || report.Duplicates != 1 {
		t.Fatalf("dry run New=%d Duplicates=%d, want 2/1 (intra-batch duplicate predicted)",
			report.New, report.Duplicates)
	}
	// Nothing was written by the dry run.
	if got := h.countAssets(); got != 0 {
		t.Fatalf("dry run created %d assets", got)
	}

	// The prediction must match the real import exactly.
	opts := h.copyOpts()
	opts.Precomputed = report
	session, err := h.pipe.Run(ctx, opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.FilesImported != report.New || session.Duplicates != report.Duplicates {
		t.Fatalf("import imported=%d dup=%d != dry run New=%d Duplicates=%d (prediction must be exact)",
			session.FilesImported, session.Duplicates, report.New, report.Duplicates)
	}
}

func TestVerificationFailurePath(t *testing.T) {
	h := newHarness(t)
	h.writeFile("corruptme.jpg", "original-good-content", testDate)

	// Corrupt the partial after copy, before verification.
	h.pipe.afterCopyHook = func(partialPath string) {
		_ = os.WriteFile(partialPath, []byte("corrupted-different!!"), 0o644)
	}

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Failures != 1 {
		t.Fatalf("Failures = %d, want 1", session.Failures)
	}
	if session.FilesImported != 0 {
		t.Fatalf("FilesImported = %d, want 0", session.FilesImported)
	}
	if got := h.countAssets(); got != 0 {
		t.Fatalf("assets = %d, want 0 on verification failure", got)
	}
	// No partial or final file should be left behind.
	assertNoPartials(t, h.destRoot)
	mustNotExist(t, filepath.Join(h.destRoot, "2023", "2023-06-15 Trip", "corruptme.jpg"))
}

func TestResumeAfterCancel(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"f1.jpg", "f2.jpg", "f3.jpg", "f4.jpg", "f5.jpg"} {
		h.writeFile(n, "content-"+n, testDate)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once a couple of files are imported.
	prog := func(pr Progress) {
		if pr.Phase == PhaseImporting && pr.FilesDone >= 2 {
			cancel()
		}
	}
	session, err := h.pipe.Run(ctx, h.copyOpts(), prog)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Status != domain.SessionStatusCancelled {
		t.Fatalf("status = %s, want cancelled", session.Status)
	}
	importedBefore := session.FilesImported
	if importedBefore == 0 || importedBefore >= 5 {
		t.Fatalf("partial import count = %d, want between 1 and 4", importedBefore)
	}

	// Drop a stray partial that resume must delete.
	stray := filepath.Join(h.destRoot, partialPrefix+"leftover")
	if err := os.WriteFile(stray, []byte("junk"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	resumed, err := h.pipe.ResumeSession(context.Background(), session.ID, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Status != domain.SessionStatusCompleted {
		t.Fatalf("resumed status = %s, want completed", resumed.Status)
	}
	mustNotExist(t, stray)
	assertNoPartials(t, h.destRoot)

	// Exactly 5 assets, none duplicated.
	if got := h.countAssets(); got != 5 {
		t.Fatalf("assets after resume = %d, want 5", got)
	}
	var dupCount int64
	h.db.Model(&domain.Asset{}).Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").Count(&dupCount)
	if dupCount != 0 {
		t.Fatalf("duplicate assets after resume = %d, want 0", dupCount)
	}
}

func TestAdoptInPlaceRegistration(t *testing.T) {
	h := newHarness(t)
	p1 := h.writeFile("lib/one.jpg", "adopt-one", testDate)
	p2 := h.writeFile("lib/two.cr2", "adopt-two-raw", testDate)

	opts := Options{Mode: ModeAdopt, SourceRoot: h.srcRoot, DestinationRoot: h.srcRoot}
	session, err := h.pipe.Run(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("adopt run: %v", err)
	}
	if session.FilesImported != 2 {
		t.Fatalf("FilesImported = %d, want 2", session.FilesImported)
	}

	var assets []domain.Asset
	h.db.Find(&assets)
	if len(assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(assets))
	}
	for _, a := range assets {
		if a.FullHash == "" {
			t.Fatalf("adopt asset %s missing full-hash baseline", a.OriginalFilename)
		}
		if a.VerificationStatus != domain.VerificationStatusVerified {
			t.Fatalf("adopt asset %s not verified", a.OriginalFilename)
		}
	}
	// Paths unchanged and no copies were made.
	mustExist(t, p1)
	mustExist(t, p2)
	byPath := map[string]domain.Asset{}
	for _, a := range assets {
		byPath[a.CurrentArchivePath] = a
	}
	if _, ok := byPath[p1]; !ok {
		t.Fatalf("asset for %s not registered in place", p1)
	}
}

func TestAdoptDuplicateCountsOnlyAsDuplicate(t *testing.T) {
	h := newHarness(t)
	// Two identical-content files under the adopt root: the first adopts, the
	// second is a flagged in-place duplicate. The duplicate must count ONLY under
	// Duplicates (consistent with copy mode) — never inflating FilesImported.
	h.writeFile("lib/first.jpg", "identical-adopt-bytes", testDate)
	h.writeFile("lib/second.jpg", "identical-adopt-bytes", testDate)

	opts := Options{Mode: ModeAdopt, SourceRoot: h.srcRoot, DestinationRoot: h.srcRoot}
	session, err := h.pipe.Run(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("adopt run: %v", err)
	}
	if session.FilesImported != 1 {
		t.Fatalf("FilesImported = %d, want 1 (the duplicate must not count as imported)", session.FilesImported)
	}
	if session.Duplicates != 1 {
		t.Fatalf("Duplicates = %d, want 1", session.Duplicates)
	}
	// Both files are still registered (adopt never drops the duplicate).
	if got := h.countAssets(); got != 2 {
		t.Fatalf("assets = %d, want 2 (duplicate still registered in place)", got)
	}
	var dupCount int64
	h.db.Model(&domain.Asset{}).Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").Count(&dupCount)
	if dupCount != 1 {
		t.Fatalf("flagged duplicate rows = %d, want 1", dupCount)
	}
}

func TestAdoptReorganizeMovesAndSecondRunSkips(t *testing.T) {
	h := newHarness(t)
	// Adopt the library root itself; reorganize moves files into the layout.
	orig := h.writeFile("loose.jpg", "reorg-content", testDate)

	opts := Options{Mode: ModeAdopt, SourceRoot: h.srcRoot, DestinationRoot: h.srcRoot, Reorganize: true, EventName: "Vacation"}
	session, err := h.pipe.Run(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("adopt+reorganize: %v", err)
	}
	if session.FilesImported != 1 {
		t.Fatalf("FilesImported = %d, want 1", session.FilesImported)
	}

	moved := filepath.Join(h.srcRoot, "2023", "2023-06-15 Vacation", "loose.jpg")
	mustExist(t, moved)
	mustNotExist(t, orig)

	var a domain.Asset
	if err := h.db.First(&a).Error; err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if a.CurrentArchivePath != moved {
		t.Fatalf("CurrentArchivePath = %q, want %q", a.CurrentArchivePath, moved)
	}

	// Second adopt run over the same root: the moved file is recognized and
	// skipped, nothing new is registered.
	session2, err := h.pipe.Run(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("second adopt run: %v", err)
	}
	if session2.FilesImported != 0 {
		t.Fatalf("second run FilesImported = %d, want 0", session2.FilesImported)
	}
	if session2.Skipped != 1 {
		t.Fatalf("second run Skipped = %d, want 1", session2.Skipped)
	}
	if got := h.countAssets(); got != 1 {
		t.Fatalf("assets after second run = %d, want 1", got)
	}
}

func TestLivePhotoPairLinking(t *testing.T) {
	h := newHarness(t)
	still := h.writeFile("IMG_100.heic", "still-image-bytes", testDate)
	motion := h.writeFile("IMG_100.mov", "motion-video-bytes", testDate)

	// Matching ContentIdentifier confirms the pair.
	h.extractor.set(still, &metadata.AssetMetadata{ContentIdentifier: "ABC-123"})
	h.extractor.set(motion, &metadata.AssetMetadata{ContentIdentifier: "ABC-123"})

	if _, err := h.pipe.Run(context.Background(), h.copyOpts(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	var assets []domain.Asset
	h.db.Find(&assets)
	if len(assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(assets))
	}
	byName := map[string]domain.Asset{}
	for _, a := range assets {
		byName[a.OriginalFilename] = a
	}
	s := byName["IMG_100.heic"]
	m := byName["IMG_100.mov"]
	if s.LivePhotoPartnerID == nil || *s.LivePhotoPartnerID != m.ID {
		t.Fatalf("still not linked to motion")
	}
	if m.LivePhotoPartnerID == nil || *m.LivePhotoPartnerID != s.ID {
		t.Fatalf("motion not linked to still")
	}
	if s.MediaType != domain.MediaTypeLivePhotoPair || m.MediaType != domain.MediaTypeLivePhotoPair {
		t.Fatalf("pair media types = %s/%s, want live_photo_pair", s.MediaType, m.MediaType)
	}
}

func TestCancellationCleanliness(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 6; i++ {
		h.writeFile(string(rune('a'+i))+".jpg", "cancel-content", testDate)
	}
	ctx, cancel := context.WithCancel(context.Background())
	prog := func(pr Progress) {
		if pr.Phase == PhaseImporting && pr.FilesDone >= 1 {
			cancel()
		}
	}
	session, err := h.pipe.Run(ctx, h.copyOpts(), prog)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Status != domain.SessionStatusCancelled {
		t.Fatalf("status = %s, want cancelled", session.Status)
	}
	// Counters are preserved and no partial temp files remain.
	if session.FilesImported < 1 {
		t.Fatalf("expected some imports preserved, got %d", session.FilesImported)
	}
	assertNoPartials(t, h.destRoot)
}

// TestSkipProvidersThreadedAndPersistedForResume verifies per-import provider
// opt-out threads to the enqueuer during a run AND survives into the resume path:
// the skip set is persisted in the session state, so resuming re-applies it.
func TestSkipProvidersThreadedAndPersistedForResume(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"f1.jpg", "f2.jpg", "f3.jpg", "f4.jpg", "f5.jpg"} {
		h.writeFile(n, "content-"+n, testDate)
	}
	skip := []string{"prov-x"}
	opts := h.copyOpts()
	opts.SkipProviderIDs = skip

	ctx, cancel := context.WithCancel(context.Background())
	prog := func(pr Progress) {
		if pr.Phase == PhaseImporting && pr.FilesDone >= 2 {
			cancel()
		}
	}
	session, err := h.pipe.Run(ctx, opts, prog)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Status != domain.SessionStatusCancelled {
		t.Fatalf("status = %s, want cancelled", session.Status)
	}
	// The skip threaded to the enqueuer during the initial run.
	if got := h.enqueuer.lastSkip; len(got) != 1 || got[0] != "prov-x" {
		t.Fatalf("initial run skip = %v, want [prov-x]", got)
	}
	// And it is persisted in the session state for resume.
	if !strings.Contains(session.Notes, "skipProviderIds") || !strings.Contains(session.Notes, "prov-x") {
		t.Fatalf("session notes missing persisted skip: %q", session.Notes)
	}

	// Resume: the persisted skip must re-thread as the remaining files import.
	h.enqueuer.lastSkip = nil
	resumed, err := h.pipe.ResumeSession(context.Background(), session.ID, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Status != domain.SessionStatusCompleted {
		t.Fatalf("resumed status = %s, want completed", resumed.Status)
	}
	if got := h.enqueuer.lastSkip; len(got) != 1 || got[0] != "prov-x" {
		t.Fatalf("resumed skip = %v, want [prov-x] (persisted opt-out must be honored)", got)
	}
}

func TestBackupStatusReflectsEnqueueCount(t *testing.T) {
	// With at least one enabled provider (perAsset>0) the asset is pending.
	h := newHarness(t)
	h.writeFile("p.jpg", "pending-bytes", testDate)
	if _, err := h.pipe.Run(context.Background(), h.copyOpts(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	var a domain.Asset
	if err := h.db.First(&a).Error; err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if a.BackupStatus != domain.BackupStatusPending {
		t.Fatalf("BackupStatus = %s, want pending", a.BackupStatus)
	}

	// With no enabled providers (perAsset==0) nothing reconciles the status, so it
	// must stay none rather than inflating pending counts forever.
	h2 := newHarness(t)
	h2.enqueuer.perAsset = 0
	h2.writeFile("n.jpg", "none-bytes", testDate)
	if _, err := h2.pipe.Run(context.Background(), h2.copyOpts(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	var b domain.Asset
	if err := h2.db.First(&b).Error; err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if b.BackupStatus != domain.BackupStatusNone {
		t.Fatalf("BackupStatus = %s, want none (no enabled providers)", b.BackupStatus)
	}
}

func TestAllFailuresMarksSessionFailed(t *testing.T) {
	h := newHarness(t)
	h.writeFile("a.jpg", "aaa", testDate)
	h.writeFile("b.jpg", "bbb", testDate)

	// Corrupt every partial so all files fail verification.
	h.pipe.afterCopyHook = func(partialPath string) {
		_ = os.WriteFile(partialPath, []byte("corrupt"), 0o644)
	}

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.FilesImported != 0 {
		t.Fatalf("FilesImported = %d, want 0", session.FilesImported)
	}
	if session.Failures != 2 {
		t.Fatalf("Failures = %d, want 2", session.Failures)
	}
	if session.Status != domain.SessionStatusFailed {
		t.Fatalf("status = %s, want failed", session.Status)
	}
}

func TestConcurrentPublishSameFilenameNoOverwrite(t *testing.T) {
	// Two independent pipelines publish a file with the SAME basename and date into
	// the SAME destination directory concurrently. Exclusive (link+remove) publish
	// must land both distinct payloads — one as "clash.jpg", one as "clash (2).jpg"
	// — never overwriting one with the other.
	dest := t.TempDir()

	newPipe := func(src string) *Pipeline {
		dbPath := filepath.Join(t.TempDir(), "p.db")
		gdb, err := db.Open(dbPath)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		return New(Config{
			DB:        gdb,
			Assets:    repo.NewAssetRepo(gdb),
			Sessions:  repo.NewSessionRepo(gdb),
			Extractor: newFakeExtractor(),
			Layout:    archive.New(dest),
			Backup:    &countingEnqueuer{perAsset: 1},
		})
	}

	writeSrc := func(content string) string {
		dir := t.TempDir()
		full := filepath.Join(dir, "clash.jpg")
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(full, testDate, testDate); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		return dir
	}

	src1 := writeSrc("payload-one")
	src2 := writeSrc("payload-two-different")
	p1 := newPipe(src1)
	p2 := newPipe(src2)

	opts := func(src string) Options {
		return Options{Mode: ModeCopy, SourceRoot: src, DestinationRoot: dest, EventName: "Clash"}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, pc := range []struct {
		p   *Pipeline
		src string
	}{{p1, src1}, {p2, src2}} {
		pc := pc
		go func() {
			defer wg.Done()
			if _, err := pc.p.Run(context.Background(), opts(pc.src), nil); err != nil {
				t.Errorf("run: %v", err)
			}
		}()
	}
	wg.Wait()

	dayDir := filepath.Join(dest, "2023", "2023-06-15 Clash")
	first := filepath.Join(dayDir, "clash.jpg")
	second := filepath.Join(dayDir, "clash (2).jpg")
	mustExist(t, first)
	mustExist(t, second)
	assertNoPartials(t, dest)

	// Both distinct payloads must be present exactly once; neither overwrote the
	// other.
	got := map[string]bool{}
	for _, f := range []string{first, second} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		got[string(b)] = true
	}
	if !got["payload-one"] || !got["payload-two-different"] {
		t.Fatalf("expected both distinct payloads on disk, got %v", got)
	}
}

// assertNoPartials fails if any ".paim-partial-*" file remains under root.
func assertNoPartials(t *testing.T, root string) {
	t.Helper()
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if len(info.Name()) >= len(partialPrefix) && info.Name()[:len(partialPrefix)] == partialPrefix {
			t.Errorf("stray partial remains: %s", path)
		}
		return nil
	})
}
