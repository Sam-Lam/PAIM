package e2e

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/backup/plugins/localfs"
	"github.com/Sam-Lam/PAIM/internal/cleanup"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/hashing"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/metadata"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/services"
	"github.com/Sam-Lam/PAIM/internal/source"
	"gorm.io/gorm"
)

// eventName is the import "event" folder label used for the copy import.
const eventName = "Big Sur Trip"

// adoptEvent is the event label used for adopt-in-place reorganization.
const adoptEvent = "Legacy"

// scenario bundles the real, main.go-style wiring shared across the subtests of
// TestFullScenario. Everything here is a real component (real SQLite, real
// exiftool extractor, real importer.Pipeline, real backup.Manager + localfs).
type scenario struct {
	t *testing.T

	srcRoot    string
	masterRoot string
	backupRoot string
	adoptRoot  string

	gdb        *gorm.DB
	assets     *repo.AssetRepo
	sessions   *repo.SessionRepo
	backupRepo *repo.BackupRepo
	extractor  metadata.Extractor
	pipeline   *importer.Pipeline
	manager    *backup.Manager
	analyzer   *cleanup.Analyzer
	identifier *source.Identifier

	provider *domain.BackupProvider
	tree     *Tree // the source ("SD card") manifest

	// dryRun is the Stage-2 dry-run prediction, retained so Stage 3 can assert the
	// prediction matched the real import exactly.
	dryRun *importer.DryRunReport

	managerStarted bool
	managerStopped bool
}

// newScenario wires the full stack the way main.go does, against fresh temp
// directories and a fresh SQLite database. It skips the whole test cleanly when
// exiftool is unavailable (metadata extraction would degrade and the EXIF-driven
// assertions could not hold).
func newScenario(t *testing.T) *scenario {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	extractor := metadata.NewExtractor(logger)
	if !extractor.Available() {
		_ = extractor.Close()
		t.Skip("exiftool not available; skipping full-stack scenario test")
	}
	t.Cleanup(func() { _ = extractor.Close() })

	root := t.TempDir()
	s := &scenario{
		t:          t,
		srcRoot:    filepath.Join(root, "SDCARD"),
		masterRoot: filepath.Join(root, "Master Library"),
		backupRoot: filepath.Join(root, "Backup"),
		adoptRoot:  filepath.Join(root, "Existing Library"),
		extractor:  extractor,
	}
	mustMkdir(t, s.masterRoot)
	mustMkdir(t, s.backupRoot)

	// Build the primary source tree ("SD card").
	bin := lookupExiftool(t)
	tree, err := BuildSourceTree(s.srcRoot, bin)
	if err != nil {
		t.Fatalf("build source tree: %v", err)
	}
	s.tree = tree

	// Database + repositories (exactly the objects main.go constructs).
	gdb, err := db.Open(filepath.Join(root, "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	s.gdb = gdb
	s.assets = repo.NewAssetRepo(gdb)
	s.sessions = repo.NewSessionRepo(gdb)
	s.backupRepo = repo.NewBackupRepo(gdb)

	// Backup subsystem: registry + localfs plugin + persisted queue + manager.
	registry := backup.NewRegistry()
	registry.Register(localfs.PluginName, localfs.New)
	jobQueue := backup.NewRepoJobQueue(gdb)
	providerStore := backup.NewRepoProviderStore(gdb)
	s.manager = backup.NewManager(jobQueue, s.assets, providerStore, registry, logger, backup.Options{
		Workers:      2,
		MaxRetries:   3,
		BaseBackoff:  5 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
		LibraryRoot:  s.masterRoot,
	})

	// Import pipeline with the backup manager wired as the atomic enqueuer.
	s.pipeline = importer.New(importer.Config{
		DB:        gdb,
		Assets:    s.assets,
		Sessions:  s.sessions,
		Extractor: extractor,
		Layout:    archive.New(s.masterRoot),
		Logger:    logger,
		Backup:    s.manager,
	})

	// Cleanup Assistant and safe-to-erase identifier.
	s.analyzer = cleanup.NewAnalyzer(s.assets, nil, nil, logger)
	s.identifier = source.NewIdentifier(nil, nil, services.Hasher{}, mediatype.IsMedia)

	// Enable a localfs backup provider BEFORE importing, so the import enqueues
	// backup jobs (EnqueueForAsset lists enabled providers).
	cfgJSON, err := json.Marshal(localfs.Config{Root: s.backupRoot})
	if err != nil {
		t.Fatalf("marshal provider config: %v", err)
	}
	s.provider = &domain.BackupProvider{
		PluginName: localfs.PluginName,
		ConfigJSON: string(cfgJSON),
		Enabled:    true,
	}
	if err := gdb.Create(s.provider).Error; err != nil {
		t.Fatalf("create backup provider: %v", err)
	}

	return s
}

// startManager launches the backup worker pool once, registering Stop cleanup.
func (s *scenario) startManager() {
	s.t.Helper()
	if s.managerStarted {
		return
	}
	if err := s.manager.Start(context.Background()); err != nil {
		s.t.Fatalf("start backup manager: %v", err)
	}
	s.managerStarted = true
	s.t.Cleanup(s.stopManager)
}

// stopManager stops the worker pool exactly once. It is called explicitly once
// backups have settled (so later stages run without a concurrent DB writer —
// avoiding SQLite writer contention between the manager and the import
// finishSession under load) and again from t.Cleanup as a backstop.
func (s *scenario) stopManager() {
	if !s.managerStarted || s.managerStopped {
		return
	}
	s.managerStopped = true
	s.manager.Stop()
}

// importedFiles returns the non-duplicate media files of the source tree (the
// files that a copy import actually lands in the archive).
func (s *scenario) importedFiles() []FixtureFile {
	var out []FixtureFile
	for _, f := range s.tree.Media {
		if !f.IsDuplicate {
			out = append(out, f)
		}
	}
	return out
}

// duplicateFile returns the single exact-duplicate file in the source tree.
func (s *scenario) duplicateFile() FixtureFile {
	for _, f := range s.tree.Media {
		if f.IsDuplicate {
			return f
		}
	}
	s.t.Fatal("source tree has no duplicate fixture")
	return FixtureFile{}
}

func TestFullScenario(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()

	// ---- Stage 2: dry run predicts, writes nothing --------------------------
	t.Run("DryRunPredictsAndWritesNothing", func(t *testing.T) {
		opts := importer.Options{Mode: importer.ModeCopy, SourceRoot: s.srcRoot, DestinationRoot: s.masterRoot, EventName: eventName}
		scan, err := s.pipeline.Scan(ctx, s.srcRoot, nil)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		// The scan excludes junk/hidden files (.DS_Store): 7 media files remain.
		if got, want := len(scan.Files), s.tree.MediaCount(); got != want {
			t.Errorf("scan found %d files, want %d (junk must be excluded)", got, want)
		}
		if got, want := len(scan.Files), 7; got != want {
			t.Errorf("scan found %d media files, want %d", got, want)
		}

		report, err := s.pipeline.DryRun(ctx, scan, opts, nil)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if report.Files != 7 {
			t.Errorf("dry run Files = %d, want 7", report.Files)
		}
		if report.Photos != 6 { // 5 JPEG (4 unique + 1 dup) + 1 RAF
			t.Errorf("dry run Photos = %d, want 6", report.Photos)
		}
		if report.Videos != 1 {
			t.Errorf("dry run Videos = %d, want 1", report.Videos)
		}
		// The dry run predicts intra-batch repeats: although the asset DB is empty
		// here, the source tree contains one exact intra-batch duplicate
		// (sub/IMG_0001_copy.JPG == IMG_0001.JPG). In copy mode the first occurrence
		// imports and the repeat is already-imported (nothing copied, no duplicate
		// row) — exactly as the real import (below) will record it.
		if report.New != 6 {
			t.Errorf("dry run New = %d, want 6 (7 files minus 1 intra-batch repeat)", report.New)
		}
		if report.Duplicates != 0 {
			t.Errorf("dry run Duplicates = %d, want 0 (copy mode records no duplicates)", report.Duplicates)
		}
		if report.AlreadyImported != 1 {
			t.Errorf("dry run AlreadyImported = %d, want 1 (the intra-batch copy)", report.AlreadyImported)
		}
		// Retain the prediction so Stage 3 can assert it matched the import exactly.
		s.dryRun = report

		// Nothing was written: master library empty, zero asset rows.
		if n := countRegularFiles(t, s.masterRoot); n != 0 {
			t.Errorf("master library has %d files after dry run, want 0", n)
		}
		if n := s.countAssets(); n != 0 {
			t.Errorf("asset rows = %d after dry run, want 0", n)
		}
	})

	// ---- Stage 3: copy import -----------------------------------------------
	t.Run("CopyImportLandsAndRecords", func(t *testing.T) {
		opts := importer.Options{Mode: importer.ModeCopy, SourceRoot: s.srcRoot, DestinationRoot: s.masterRoot, EventName: eventName}
		sess, err := s.pipeline.Run(ctx, opts, nil)
		if err != nil {
			t.Fatalf("import run: %v", err)
		}
		if sess.Status != domain.SessionStatusCompleted {
			t.Fatalf("session status = %q, want completed", sess.Status)
		}
		if sess.FilesScanned != 7 || sess.FilesImported != 6 || sess.AlreadyImported != 1 || sess.Duplicates != 0 || sess.Skipped != 0 || sess.Failures != 0 {
			t.Errorf("session counters = scanned:%d imported:%d already:%d dup:%d skipped:%d fail:%d; want 7/6/1/0/0/0",
				sess.FilesScanned, sess.FilesImported, sess.AlreadyImported, sess.Duplicates, sess.Skipped, sess.Failures)
		}

		// The Stage-2 dry run predicted the import exactly: New == the files that
		// actually imported, and AlreadyImported == the files already archived.
		if s.dryRun != nil {
			if s.dryRun.New != int(sess.FilesImported) {
				t.Errorf("dry run New = %d, but import FilesImported = %d (prediction must be exact)", s.dryRun.New, sess.FilesImported)
			}
			if s.dryRun.AlreadyImported != int(sess.AlreadyImported) {
				t.Errorf("dry run AlreadyImported = %d, but import AlreadyImported = %d (prediction must be exact)", s.dryRun.AlreadyImported, sess.AlreadyImported)
			}
		}

		// Every non-duplicate file landed at the exact expected layout path.
		for _, f := range s.importedFiles() {
			want := expectedDest(s.masterRoot, eventName, f)
			asset := s.assetByOriginalPath(f.Path)
			if asset.CurrentArchivePath != want {
				t.Errorf("%s archived at %q, want %q", filepath.Base(f.Path), asset.CurrentArchivePath, want)
			}
			if _, err := os.Stat(want); err != nil {
				t.Errorf("expected archived file missing: %v", err)
			}
			// Independently recompute the quick hash of the archived file and compare.
			gotQuick, err := hashing.QuickHash(want)
			if err != nil {
				t.Fatalf("quick hash %q: %v", want, err)
			}
			if asset.QuickHash != gotQuick {
				t.Errorf("%s asset QuickHash = %q, recomputed %q", filepath.Base(f.Path), asset.QuickHash, gotQuick)
			}
			if asset.VerificationStatus != domain.VerificationStatusVerified {
				t.Errorf("%s verification = %q, want verified", filepath.Base(f.Path), asset.VerificationStatus)
			}
			// Capture date persisted (EXIF for JPEGs, mtime for RAW/video).
			if asset.CaptureDate == nil || !asset.CaptureDate.Equal(f.CaptureDate) {
				t.Errorf("%s capture date = %v, want %v", filepath.Base(f.Path), asset.CaptureDate, f.CaptureDate)
			}
			// Camera metadata persisted for the EXIF-bearing JPEGs.
			if f.CameraMake != "" {
				if asset.CameraMake != f.CameraMake || asset.CameraModel != f.CameraModel {
					t.Errorf("%s camera = %q/%q, want %q/%q", filepath.Base(f.Path),
						asset.CameraMake, asset.CameraModel, f.CameraMake, f.CameraModel)
				}
			}
			if f.HasGPS && (asset.GPSLatitude == nil || asset.GPSLongitude == nil) {
				t.Errorf("%s expected GPS to be persisted, got lat=%v lon=%v",
					filepath.Base(f.Path), asset.GPSLatitude, asset.GPSLongitude)
			}
		}

		// The RAF and MOV used the mtime fallback: assert their day folders came
		// from mtime, not EXIF (they have no camera metadata).
		for _, f := range s.importedFiles() {
			if f.FromMtime && f.CameraMake != "" {
				t.Errorf("%s should have no camera metadata (mtime fallback)", filepath.Base(f.Path))
			}
		}

		// The intra-batch copy is already-imported in copy mode: NO row is recorded
		// for it (no placeholder), and no flagged-duplicate rows exist anywhere.
		dup := s.duplicateFile()
		var dupRowCount int64
		s.gdb.Model(&domain.Asset{}).Where("original_full_path = ?", dup.Path).Count(&dupRowCount)
		if dupRowCount != 0 {
			t.Errorf("already-imported file recorded %d asset rows, want 0 (no placeholder)", dupRowCount)
		}
		var flaggedDupCount int64
		s.gdb.Model(&domain.Asset{}).Where("duplicate_of_asset_id IS NOT NULL AND duplicate_of_asset_id <> ''").Count(&flaggedDupCount)
		if flaggedDupCount != 0 {
			t.Errorf("copy-mode import created %d duplicate rows, want 0", flaggedDupCount)
		}

		// Backup jobs enqueued for the 6 imported assets (not for the duplicate).
		if n := s.pendingJobCount(); n != 6 {
			t.Errorf("pending backup jobs = %d, want 6", n)
		}
		for _, f := range s.importedFiles() {
			a := s.assetByOriginalPath(f.Path)
			if a.BackupStatus != domain.BackupStatusPending {
				t.Errorf("%s BackupStatus = %q before manager start, want pending", filepath.Base(f.Path), a.BackupStatus)
			}
			if jobs := s.jobsForAsset(a.ID); len(jobs) != 1 {
				t.Errorf("%s has %d backup jobs, want 1", filepath.Base(f.Path), len(jobs))
			}
		}
		// Six files physically present in the master library.
		if n := countRegularFiles(t, s.masterRoot); n != 6 {
			t.Errorf("master library has %d files, want 6", n)
		}
	})

	// ---- Stage 4: backups complete ------------------------------------------
	t.Run("BackupsComplete", func(t *testing.T) {
		s.startManager()

		imported := s.importedFiles()
		eventually(t, 20*time.Second, "all imported assets backed up", func() bool {
			for _, f := range imported {
				if s.assetByOriginalPath(f.Path).BackupStatus != domain.BackupStatusComplete {
					return false
				}
			}
			return true
		})

		for _, f := range imported {
			a := s.assetByOriginalPath(f.Path)
			rel, err := filepath.Rel(s.masterRoot, a.CurrentArchivePath)
			if err != nil {
				t.Fatalf("rel path: %v", err)
			}
			backupPath := filepath.Join(s.backupRoot, rel)
			if _, err := os.Stat(backupPath); err != nil {
				t.Errorf("backup missing for %s: %v", filepath.Base(f.Path), err)
				continue
			}
			// Full BLAKE3 compare of the backup against the ORIGINAL source bytes.
			srcHash, err := hashing.FullHash(ctx, f.Path)
			if err != nil {
				t.Fatalf("hash source: %v", err)
			}
			backupHash, err := hashing.FullHash(ctx, backupPath)
			if err != nil {
				t.Fatalf("hash backup: %v", err)
			}
			if srcHash != backupHash {
				t.Errorf("%s backup does not byte-match source (hash %s vs %s)", filepath.Base(f.Path), backupHash, srcHash)
			}
		}

		// Backups have settled; stop the worker pool so the remaining stages run
		// without a concurrent DB writer (prevents SQLite lock contention under load).
		s.stopManager()
	})

	// ---- Stage 5: re-import is idempotent -----------------------------------
	t.Run("ReimportIsIdempotent", func(t *testing.T) {
		filesBefore := countRegularFiles(t, s.masterRoot)
		assetsBefore := s.countAssets()

		opts := importer.Options{Mode: importer.ModeCopy, SourceRoot: s.srcRoot, DestinationRoot: s.masterRoot, EventName: eventName}
		sess, err := s.pipeline.Run(ctx, opts, nil)
		if err != nil {
			t.Fatalf("re-import run: %v", err)
		}
		if sess.Status != domain.SessionStatusCompleted {
			t.Fatalf("re-import status = %q, want completed", sess.Status)
		}
		// Everything already imported -> already-imported; nothing new, nothing
		// duplicated, nothing skipped. All 7 source files (6 unique + the intra-batch
		// copy) resolve to archived content by hash and record no new rows.
		if sess.AlreadyImported != 7 || sess.FilesImported != 0 || sess.Duplicates != 0 || sess.Skipped != 0 {
			t.Errorf("re-import counters = already:%d imported:%d dup:%d skipped:%d; want 7/0/0/0",
				sess.AlreadyImported, sess.FilesImported, sess.Duplicates, sess.Skipped)
		}
		if got := countRegularFiles(t, s.masterRoot); got != filesBefore {
			t.Errorf("master file count changed on re-import: %d -> %d", filesBefore, got)
		}
		if got := s.countAssets(); got != assetsBefore {
			t.Errorf("asset rows changed on re-import: %d -> %d", assetsBefore, got)
		}
	})

	// ---- Stage 6: cleanup + safe-to-erase -----------------------------------
	t.Run("CleanupAndSafeToErase", func(t *testing.T) {
		report, err := s.analyzer.Analyze(ctx, s.srcRoot, nil)
		if err != nil {
			t.Fatalf("cleanup analyze: %v", err)
		}
		// The 6 originals are already_archived; the in-folder duplicate classifies
		// as `duplicate` (intra-folder duplicate detection fires before the archive
		// match). Documented reading of analyzer semantics.
		if got := report.Count(cleanup.ClassAlreadyArchived); got != 6 {
			t.Errorf("already_archived = %d, want 6", got)
		}
		if got := report.Count(cleanup.ClassDuplicate); got != 1 {
			t.Errorf("duplicate = %d, want 1 (the in-folder copy)", got)
		}
		if got := report.Count(cleanup.ClassNew); got != 0 {
			t.Errorf("new = %d, want 0", got)
		}
		if got := report.Count(cleanup.ClassVerificationFailed); got != 0 {
			t.Errorf("verification_failed = %d, want 0", got)
		}
		if report.BackupIncomplete != 0 {
			t.Errorf("BackupIncomplete = %d, want 0 (backups are complete)", report.BackupIncomplete)
		}

		// Delete-safety is about CONTENT coverage: the in-folder duplicate's bytes are
		// preserved in the archive (its content maps to the verified, fully-backed
		// IMG_0001 asset), so it does NOT block deletion — matching
		// source.EvaluateSafeToErase, which treats duplicate content as archived.
		// Every byte of the source tree is archived + verified + backed up, so the
		// folder is safe to delete.
		rec := report.Recommendation()
		if !rec.SafeToDelete {
			t.Errorf("cleanup Recommendation.SafeToDelete = false, want true (the duplicate's content is preserved in the archive); reasons=%v", rec.Reasons)
		}
		// The safe duplicate is still surfaced as an informational note, not a blocker.
		if !reasonsContain(rec.Reasons, "content preserved in archive") {
			t.Errorf("expected an informational note about the archived duplicate, got %v", rec.Reasons)
		}

		// To demonstrate the intended positive path (backups complete -> safe), run
		// the same analysis over the duplicate-free archived library itself.
		libReport, err := s.analyzer.Analyze(ctx, s.masterRoot, nil)
		if err != nil {
			t.Fatalf("cleanup analyze (library): %v", err)
		}
		if got := libReport.Count(cleanup.ClassAlreadyArchived); got != 6 {
			t.Errorf("library already_archived = %d, want 6", got)
		}
		if got := libReport.Count(cleanup.ClassDuplicate); got != 0 {
			t.Errorf("library duplicate = %d, want 0", got)
		}
		if libRec := libReport.Recommendation(); !libRec.SafeToDelete {
			t.Errorf("library Recommendation.SafeToDelete = false, want true (all archived + backed up); reasons=%v", libRec.Reasons)
		}

		// source.EvaluateSafeToErase treats duplicate CONTENT as archived (it maps
		// each file to a verified, backed-up asset regardless of intra-folder
		// duplication), so the source volume IS safe to erase.
		safe, err := s.identifier.EvaluateSafeToErase(ctx, "test-source", s.srcRoot, true, srcLookup{s.assets}, mediatype.IsMedia, nil)
		if err != nil {
			t.Fatalf("evaluate safe-to-erase: %v", err)
		}
		if !safe.Safe {
			t.Errorf("EvaluateSafeToErase Safe = false, want true; reason=%q", safe.Reason)
		}
		if safe.TotalMedia != 7 || safe.Archived != 7 {
			t.Errorf("EvaluateSafeToErase totals = total:%d archived:%d new:%d unverified:%d backupIncomplete:%d; want 7/7 archived",
				safe.TotalMedia, safe.Archived, safe.New, safe.Unverified, safe.BackupIncomplete)
		}
		if safe.Reason == "" {
			t.Errorf("EvaluateSafeToErase reason is empty")
		}
	})

	// ---- Stage 7: adopt in place --------------------------------------------
	t.Run("AdoptInPlaceReorganizes", func(t *testing.T) {
		bin := lookupExiftool(t)
		adoptTree, err := BuildAdoptTree(s.adoptRoot, bin)
		if err != nil {
			t.Fatalf("build adopt tree: %v", err)
		}

		// Capture pre-move FileInfos of the unique files so we can prove the adopt
		// reorganize MOVED (renamed, same inode) rather than copied.
		type uniq struct {
			f       FixtureFile
			preInfo os.FileInfo
		}
		var uniques []uniq
		for _, f := range adoptTree.Media {
			if f.IsDuplicate {
				continue
			}
			info, err := os.Stat(f.Path)
			if err != nil {
				t.Fatalf("stat adopt file: %v", err)
			}
			uniques = append(uniques, uniq{f: f, preInfo: info})
		}

		assetsBefore := s.countAssets()
		opts := importer.Options{Mode: importer.ModeAdopt, SourceRoot: s.adoptRoot, DestinationRoot: s.adoptRoot, EventName: adoptEvent, Reorganize: true}
		sess, err := s.pipeline.Run(ctx, opts, nil)
		if err != nil {
			t.Fatalf("adopt run: %v", err)
		}
		if sess.Status != domain.SessionStatusCompleted {
			t.Fatalf("adopt status = %q, want completed", sess.Status)
		}
		// Adopt counters mirror copy mode: the 4 files are 3 unique JPEGs + 1
		// duplicate-of-DSC_0003. The 3 uniques import; the duplicate counts ONLY under
		// Duplicates (never double-counted as Imported).
		if sess.FilesImported != 3 || sess.Duplicates != 1 || sess.Skipped != 0 {
			t.Errorf("adopt counters = imported:%d dup:%d skipped:%d; want 3/1/0",
				sess.FilesImported, sess.Duplicates, sess.Skipped)
		}

		// Four new asset rows, each with a populated FullHash (adopt baseline).
		if got := s.countAssets(); got != assetsBefore+4 {
			t.Errorf("asset rows after adopt = %d, want %d", got, assetsBefore+4)
		}
		adoptAssets := s.assetsForSession(sess.ID)
		if len(adoptAssets) != 4 {
			t.Fatalf("adopt session has %d assets, want 4", len(adoptAssets))
		}
		for _, a := range adoptAssets {
			if a.FullHash == "" {
				t.Errorf("adopt asset %q (%s) has empty FullHash, want populated baseline", a.ID, a.OriginalFilename)
			}
		}

		// The three unique files were reorganized into the layout via same-inode
		// rename (moved, not copied); their old paths are gone.
		for _, u := range uniques {
			want := expectedDest(s.adoptRoot, adoptEvent, u.f)
			if _, err := os.Stat(u.f.Path); !os.IsNotExist(err) {
				t.Errorf("old path %q still present after reorganize (err=%v)", u.f.Path, err)
			}
			newInfo, err := os.Stat(want)
			if err != nil {
				t.Errorf("reorganized file missing at %q: %v", want, err)
				continue
			}
			if !os.SameFile(u.preInfo, newInfo) {
				t.Errorf("%s was copied, not moved: inode differs between old and new path", filepath.Base(u.f.Path))
			}
			// The asset records the new path.
			a := s.assetByOriginalPath(u.f.Path)
			if a.CurrentArchivePath != want {
				t.Errorf("%s CurrentArchivePath = %q, want %q", filepath.Base(u.f.Path), a.CurrentArchivePath, want)
			}
		}

		// The duplicate was NOT moved (a flagged duplicate keeps its place) and is
		// flagged with DuplicateOfAssetID.
		for _, f := range adoptTree.Media {
			if !f.IsDuplicate {
				continue
			}
			if _, err := os.Stat(f.Path); err != nil {
				t.Errorf("duplicate was moved/removed, want left in place: %v", err)
			}
			da := s.assetByOriginalPath(f.Path)
			if da.DuplicateOfAssetID == nil || *da.DuplicateOfAssetID == "" {
				t.Errorf("adopted duplicate not flagged with DuplicateOfAssetID")
			}
		}

		// No byte copies: total file count under the adopt root is preserved.
		if got := countRegularFiles(t, s.adoptRoot); got != 4 {
			t.Errorf("adopt root has %d files, want 4 (moved, not copied)", got)
		}

		// A second adopt run is a complete no-op.
		filesBefore := countRegularFiles(t, s.adoptRoot)
		assetsBefore2 := s.countAssets()
		sess2, err := s.pipeline.Run(ctx, opts, nil)
		if err != nil {
			t.Fatalf("second adopt run: %v", err)
		}
		// A re-adopt recognizes all 4 files (3 moved uniques + the in-place flagged
		// duplicate) by content/path as already-imported; nothing new, no skips.
		if sess2.AlreadyImported != 4 || sess2.FilesImported != 0 || sess2.Duplicates != 0 || sess2.Skipped != 0 {
			t.Errorf("second adopt counters = already:%d imported:%d dup:%d skipped:%d; want 4/0/0/0",
				sess2.AlreadyImported, sess2.FilesImported, sess2.Duplicates, sess2.Skipped)
		}
		if got := countRegularFiles(t, s.adoptRoot); got != filesBefore {
			t.Errorf("second adopt changed file count: %d -> %d", filesBefore, got)
		}
		if got := s.countAssets(); got != assetsBefore2 {
			t.Errorf("second adopt changed asset rows: %d -> %d", assetsBefore2, got)
		}
	})
}

// ---- scenario query helpers -------------------------------------------------

func (s *scenario) countAssets() int64 {
	s.t.Helper()
	var n int64
	if err := s.gdb.Model(&domain.Asset{}).Count(&n).Error; err != nil {
		s.t.Fatalf("count assets: %v", err)
	}
	return n
}

func (s *scenario) assetByOriginalPath(path string) *domain.Asset {
	s.t.Helper()
	var a domain.Asset
	if err := s.gdb.Where("original_full_path = ?", path).First(&a).Error; err != nil {
		s.t.Fatalf("asset by original path %q: %v", path, err)
	}
	return &a
}

func (s *scenario) assetsForSession(sessionID string) []domain.Asset {
	s.t.Helper()
	var out []domain.Asset
	if err := s.gdb.Where("session_id = ?", sessionID).Find(&out).Error; err != nil {
		s.t.Fatalf("assets for session %q: %v", sessionID, err)
	}
	return out
}

func (s *scenario) pendingJobCount() int64 {
	s.t.Helper()
	status := domain.JobStatusPending
	_, total, err := s.backupRepo.ListJobs(context.Background(), &status, repo.Page{})
	if err != nil {
		s.t.Fatalf("list pending jobs: %v", err)
	}
	return total
}

func (s *scenario) jobsForAsset(assetID string) []domain.BackupJob {
	s.t.Helper()
	var jobs []domain.BackupJob
	if err := s.gdb.Where("asset_id = ?", assetID).Find(&jobs).Error; err != nil {
		s.t.Fatalf("jobs for asset %q: %v", assetID, err)
	}
	return jobs
}

// srcLookup adapts the asset repo to source.AssetLookup, projecting rows into the
// minimal ArchivedAsset view with backup completeness folded in (mirrors the
// unexported adapter in internal/services).
type srcLookup struct{ assets *repo.AssetRepo }

func (l srcLookup) FindByQuickHash(ctx context.Context, quickHash string) ([]source.ArchivedAsset, error) {
	rows, err := l.assets.FindByQuickHash(ctx, quickHash)
	if err != nil {
		return nil, err
	}
	return srcArchivedAssets(rows), nil
}

func (l srcLookup) FindByOriginalPath(ctx context.Context, path string) ([]source.ArchivedAsset, error) {
	rows, err := l.assets.FindByOriginalPath(ctx, path)
	if err != nil {
		return nil, err
	}
	return srcArchivedAssets(rows), nil
}

func srcArchivedAssets(rows []domain.Asset) []source.ArchivedAsset {
	out := make([]source.ArchivedAsset, 0, len(rows))
	for _, r := range rows {
		out = append(out, source.ArchivedAsset{
			ID:               r.ID,
			QuickHash:        r.QuickHash,
			FullHash:         r.FullHash,
			Verified:         r.VerificationStatus == domain.VerificationStatusVerified,
			BackupComplete:   r.BackupStatus == domain.BackupStatusComplete,
			HasArchiveCopy:   r.CurrentArchivePath != "",
			OriginalFullPath: r.OriginalFullPath,
			FileSize:         r.FileSize,
			ImportDate:       r.ImportDate,
		})
	}
	return out
}

// ---- generic test helpers ---------------------------------------------------

// expectedDest computes the archive path a file should land at, mirroring
// importer.computeDestination (archive.Layout rooted at masterRoot).
func expectedDest(masterRoot, event string, f FixtureFile) string {
	lay := archive.New(masterRoot)
	rel := lay.DestinationFor(f.CaptureDate, event, filepath.Base(f.Path), mediatype.KindOf(f.Ext))
	return filepath.Join(masterRoot, rel)
}

// countRegularFiles counts non-directory files under root (recursively),
// ignoring the localfs/importer temp-partial artifacts which should never
// linger.
func countRegularFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("count files under %q: %v", root, err)
	}
	return n
}

// reasonsContain reports whether any reason string contains sub.
func reasonsContain(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
}

// lookupExiftool resolves the exiftool binary (PATH, then the Homebrew path).
func lookupExiftool(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("exiftool"); err == nil {
		return p
	}
	const brew = "/opt/homebrew/bin/exiftool"
	if _, err := os.Stat(brew); err == nil {
		return brew
	}
	t.Skip("exiftool not found on PATH or at /opt/homebrew/bin/exiftool")
	return ""
}

// eventually polls cond until it is true or the timeout elapses.
func eventually(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, desc)
}
