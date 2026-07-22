package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/metadata"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// reorgHarness bundles a Pipeline rooted at a portable library root so archive
// paths are stored root-relative and the layout targets that root — exactly the
// production configuration the reorganize maintenance operation runs under.
type reorgHarness struct {
	t        *testing.T
	assets   *repo.AssetRepo
	sessions *repo.SessionRepo
	pipe     *Pipeline
	root     string
}

// newReorgHarness adopts an existing (unorganized) tree under a library root so
// the catalog is populated with in-place assets ready to be reorganized.
func newReorgHarness(t *testing.T) *reorgHarness {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "paim_test.db")
	gdb, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	pipe := New(Config{
		DB:          gdb,
		Assets:      assets,
		Sessions:    sessions,
		Extractor:   newFakeExtractor(),
		Layout:      archive.New(root),
		Backup:      &countingEnqueuer{perAsset: 1},
		LibraryRoot: root,
	})
	return &reorgHarness{t: t, assets: assets, sessions: sessions, pipe: pipe, root: root}
}

// write creates a file at a path relative to the library root with a set mtime.
func (h *reorgHarness) write(rel, content string, when time.Time) string {
	h.t.Helper()
	full := filepath.Join(h.root, rel)
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

// adopt registers every media file under the library root in place (no
// reorganize), populating the catalog with assets at their current paths.
func (h *reorgHarness) adopt() {
	h.t.Helper()
	opts := Options{Mode: ModeAdopt, SourceRoot: h.root, DestinationRoot: h.root}
	if _, err := h.pipe.Run(context.Background(), opts, nil); err != nil {
		h.t.Fatalf("adopt: %v", err)
	}
}

func (h *reorgHarness) assetByFilename(name string) domain.Asset {
	h.t.Helper()
	var a domain.Asset
	if err := h.pipe.db.Where("original_filename = ?", name).First(&a).Error; err != nil {
		h.t.Fatalf("load asset %q: %v", name, err)
	}
	return a
}

// reorg2023 is the fixed capture date used across reorganize tests.
var reorg2023 = time.Date(2023, 6, 15, 12, 0, 0, 0, time.Local)

func TestReorganizePlanMixed(t *testing.T) {
	h := newReorgHarness(t)
	// loose.jpg is out of place; already/2023/... simulates an in-place file.
	h.write("loose.jpg", "loose-bytes", reorg2023)
	h.write(filepath.Join("2023", "2023-06-15", "settled.jpg"), "settled-bytes", reorg2023)
	// A distinct file that is an intra-batch duplicate of nothing but will be a
	// content duplicate: two identical-content files → second is flagged duplicate.
	h.write("dup-a.jpg", "same-dup-bytes", reorg2023)
	h.write("dup-b.jpg", "same-dup-bytes", reorg2023)
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// settled.jpg is already at 2023/2023-06-15/ → in place.
	// loose.jpg and the non-duplicate of the pair move; the flagged duplicate is
	// skipped.
	if plan.TotalAssets != 4 {
		t.Fatalf("TotalAssets = %d, want 4", plan.TotalAssets)
	}
	if plan.InPlace != 1 {
		t.Fatalf("InPlace = %d, want 1", plan.InPlace)
	}
	if plan.Moves != 2 {
		t.Fatalf("Moves = %d, want 2 (loose + first of dup pair)", plan.Moves)
	}
	if plan.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (flagged duplicate)", plan.Skipped)
	}

	// Verify the skip is the duplicate with the right reason.
	var sawDupSkip bool
	for _, e := range plan.Entries {
		if e.Kind == MoveSkip && e.Reason == ReorgSkipDuplicate {
			sawDupSkip = true
		}
	}
	if !sawDupSkip {
		t.Fatalf("expected a duplicate skip entry")
	}

	// The plan must not have touched anything on disk.
	mustExist(t, filepath.Join(h.root, "loose.jpg"))
}

func TestReorganizePlanMissingFileSkipped(t *testing.T) {
	h := newReorgHarness(t)
	kept := h.write("keep.jpg", "keep-bytes", reorg2023)
	gone := h.write("gone.jpg", "gone-bytes", reorg2023)
	h.adopt()

	// Delete one file after adoption: the plan must report it as missing, not fail.
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var missing int
	for _, e := range plan.Entries {
		if e.Kind == MoveSkip && e.Reason == ReorgSkipMissing {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("missing skips = %d, want 1", missing)
	}
	mustExist(t, kept)
}

func TestReorganizePlanCollisionAmongPlannedTargets(t *testing.T) {
	h := newReorgHarness(t)
	// Two DISTINCT files with the SAME basename in different source dirs and the
	// same capture date resolve to the same destination directory. Their planned
	// targets must be distinct names.
	h.write(filepath.Join("a", "clash.jpg"), "payload-one", reorg2023)
	h.write(filepath.Join("b", "clash.jpg"), "payload-two-different", reorg2023)
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	targets := map[string]bool{}
	moves := 0
	for _, e := range plan.Entries {
		if e.Kind != MoveMove {
			continue
		}
		moves++
		if targets[e.To] {
			t.Fatalf("two planned moves share target %q", e.To)
		}
		targets[e.To] = true
	}
	if moves != 2 {
		t.Fatalf("moves = %d, want 2", moves)
	}
	dayDir := filepath.Join(h.root, "2023", "2023-06-15")
	if !targets[filepath.Join(dayDir, "clash.jpg")] || !targets[filepath.Join(dayDir, "clash (2).jpg")] {
		t.Fatalf("expected clash.jpg and clash (2).jpg targets, got %v", targets)
	}
}

func TestReorganizeRunMovesAndUpdatesPathsAndSecondRunNoop(t *testing.T) {
	h := newReorgHarness(t)
	orig := h.write("loose.jpg", "reorg-run-bytes", reorg2023)
	h.write(filepath.Join("nested", "deep", "buried.cr2"), "raw-bytes", reorg2023)
	h.adopt()

	// Capture the source inode to prove a rename (not a copy) later.
	origInfo, err := os.Stat(orig)
	if err != nil {
		t.Fatalf("stat orig: %v", err)
	}

	session, err := h.pipe.RunReorganize(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("run reorganize: %v", err)
	}
	if session.Status != domain.SessionStatusCompleted {
		t.Fatalf("status = %s, want completed", session.Status)
	}
	if session.FilesImported != 2 {
		t.Fatalf("moved (FilesImported) = %d, want 2", session.FilesImported)
	}
	if session.FilesScanned != 2 {
		t.Fatalf("FilesScanned = %d, want 2", session.FilesScanned)
	}

	// The JPEG moved into the date folder; the RAW into the RAW subfolder.
	movedJPG := filepath.Join(h.root, "2023", "2023-06-15", "loose.jpg")
	movedRAW := filepath.Join(h.root, "2023", "2023-06-15", "RAW", "buried.cr2")
	mustExist(t, movedJPG)
	mustExist(t, movedRAW)
	mustNotExist(t, orig)

	// os.SameFile proves the move was a rename that preserved the inode.
	movedInfo, err := os.Stat(movedJPG)
	if err != nil {
		t.Fatalf("stat moved: %v", err)
	}
	if !os.SameFile(origInfo, movedInfo) {
		t.Fatalf("moved file is not the same inode as the original (was it copied?)")
	}

	// CurrentArchivePath is updated and stored ROOT-RELATIVE.
	a := h.assetByFilename("loose.jpg")
	wantRel := "2023/2023-06-15/loose.jpg"
	if a.CurrentArchivePath != wantRel {
		t.Fatalf("CurrentArchivePath = %q, want %q (root-relative)", a.CurrentArchivePath, wantRel)
	}
	if library.ResolvePath(h.root, a.CurrentArchivePath) != movedJPG {
		t.Fatalf("resolved path mismatch")
	}

	// A second reorganize finds everything in place: no moves.
	session2, err := h.pipe.RunReorganize(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if session2.FilesImported != 0 {
		t.Fatalf("second run moved = %d, want 0 (all in place)", session2.FilesImported)
	}
}

func TestReorganizeRunCancellationMidRun(t *testing.T) {
	h := newReorgHarness(t)
	for _, n := range []string{"f1.jpg", "f2.jpg", "f3.jpg", "f4.jpg", "f5.jpg"} {
		h.write(n, "content-"+n, reorg2023)
	}
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	prog := func(pr Progress) {
		if pr.Phase == PhaseReorganizing && pr.FilesDone >= 2 {
			cancel()
		}
	}
	session, err := h.pipe.RunReorganize(ctx, plan, prog)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Status != domain.SessionStatusCancelled {
		t.Fatalf("status = %s, want cancelled", session.Status)
	}
	if session.FilesImported == 0 || session.FilesImported >= 5 {
		t.Fatalf("partial move count = %d, want between 1 and 4", session.FilesImported)
	}
	assertNoPartials(t, h.root)
}

func TestReorganizeEmptyDirSweep(t *testing.T) {
	h := newReorgHarness(t)
	// A file buried deep; after it moves out, the empty ancestor dirs must be
	// swept. A sibling non-empty dir and the .paim* dirs must be preserved.
	h.write(filepath.Join("old", "deep", "buried.jpg"), "buried-bytes", reorg2023)
	// A non-media stray keeps keepdir/ non-empty; reorganize never tracks or moves
	// it, so the directory must be preserved by the sweep.
	h.write(filepath.Join("keepdir", "notes.txt"), "not-media", reorg2023)
	h.adopt()

	// Simulate PAIM bookkeeping dirs that must never be swept.
	dotDir := filepath.Join(h.root, ".paim")
	if err := os.MkdirAll(filepath.Join(dotDir, "backups"), 0o755); err != nil {
		t.Fatalf("mkdir dot: %v", err)
	}

	if _, err := h.pipe.RunReorganize(context.Background(), nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	// old/ and old/deep/ became empty → removed.
	mustNotExist(t, filepath.Join(h.root, "old", "deep"))
	mustNotExist(t, filepath.Join(h.root, "old"))
	// keepdir/ still holds notes.txt → preserved.
	mustExist(t, filepath.Join(h.root, "keepdir", "notes.txt"))
	// The library root and its dotted bookkeeping dir survive.
	mustExist(t, h.root)
	mustExist(t, dotDir)
	mustExist(t, filepath.Join(dotDir, "backups"))
}

func TestReorganizeLivePhotoPairMovesTogether(t *testing.T) {
	h := newReorgHarness(t)
	still := h.write(filepath.Join("shots", "IMG_1.heic"), "still-bytes", reorg2023)
	motion := h.write(filepath.Join("shots", "IMG_1.mov"), "motion-bytes", reorg2023)
	// Matching ContentIdentifier confirms the Live Photo pair during adoption.
	h.pipe.extractor.(*fakeExtractor).set(still, &metadata.AssetMetadata{ContentIdentifier: "PAIR-1"})
	h.pipe.extractor.(*fakeExtractor).set(motion, &metadata.AssetMetadata{ContentIdentifier: "PAIR-1"})
	h.adopt()

	// Confirm the pair is linked before reorganizing.
	s := h.assetByFilename("IMG_1.heic")
	m := h.assetByFilename("IMG_1.mov")
	if s.LivePhotoPartnerID == nil || *s.LivePhotoPartnerID != m.ID {
		t.Fatalf("pair not linked before reorganize")
	}

	if _, err := h.pipe.RunReorganize(context.Background(), nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	dayDir := filepath.Join(h.root, "2023", "2023-06-15")
	mustExist(t, filepath.Join(dayDir, "IMG_1.heic"))
	mustExist(t, filepath.Join(dayDir, "IMG_1.mov"))

	// Both moved and the partner links are preserved (consistency across the move).
	s2 := h.assetByFilename("IMG_1.heic")
	m2 := h.assetByFilename("IMG_1.mov")
	if s2.CurrentArchivePath != "2023/2023-06-15/IMG_1.heic" {
		t.Fatalf("still path = %q", s2.CurrentArchivePath)
	}
	if m2.CurrentArchivePath != "2023/2023-06-15/IMG_1.mov" {
		t.Fatalf("motion path = %q", m2.CurrentArchivePath)
	}
	if s2.LivePhotoPartnerID == nil || *s2.LivePhotoPartnerID != m2.ID {
		t.Fatalf("still→motion link lost after move")
	}
	if m2.LivePhotoPartnerID == nil || *m2.LivePhotoPartnerID != s2.ID {
		t.Fatalf("motion→still link lost after move")
	}
}

// TestReorganizePlanProgress verifies PlanReorganize drives the progress
// callback with the total known up front and advancing to completion.
func TestReorganizePlanProgress(t *testing.T) {
	h := newReorgHarness(t)
	h.write("loose1.jpg", "b1", reorg2023)
	h.write("loose2.jpg", "b2", reorg2023)
	h.write("loose3.jpg", "b3", reorg2023)
	h.adopt()

	type sample struct{ done, total int }
	var samples []sample
	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, func(done, total int) {
		samples = append(samples, sample{done, total})
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no progress callbacks from PlanReorganize")
	}
	for i, s := range samples {
		if s.total != plan.TotalAssets {
			t.Fatalf("sample %d total = %d, want %d (known up front)", i, s.total, plan.TotalAssets)
		}
	}
	last := samples[len(samples)-1]
	if last.done != plan.TotalAssets {
		t.Fatalf("final progress done = %d, want %d", last.done, plan.TotalAssets)
	}
}
