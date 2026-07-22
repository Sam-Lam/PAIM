package importer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/hashing"
)

// hashCounter counts identity-hash computations that flow through the pipeline's
// hashing seams. It lets a test prove that precomputed dry-run hashes are reused
// (count stays low) rather than recomputed at import time.
type hashCounter struct {
	quick atomic.Int64
	full  atomic.Int64
}

// installCounter wraps the pipeline's hashing seams with counters, delegating to
// the real hashing functions so results stay correct. Returns the live counter.
func (h *harness) installCounter() *hashCounter {
	c := &hashCounter{}
	realQuick := h.pipe.quickHash
	realFull := h.pipe.fullHash
	h.pipe.quickHash = func(path string) (string, error) {
		c.quick.Add(1)
		return realQuick(path)
	}
	h.pipe.fullHash = func(ctx context.Context, path string) (string, error) {
		c.full.Add(1)
		return realFull(ctx, path)
	}
	return c
}

// TestPrecomputedReuseSkipsRehashing proves the analyze->import handoff: after a
// dry run, importing with the report as Precomputed recomputes ZERO hashes when
// nothing changed — every quick hash is reused via the size+mtime gate.
func TestPrecomputedReuseSkipsRehashing(t *testing.T) {
	h := newHarness(t)
	h.writeFile("a.jpg", "alpha-content", testDate)
	h.writeFile("b.jpg", "bravo-content", testDate)
	h.writeFile("c.mp4", "charlie-video-content", testDate)

	ctx := context.Background()
	c := h.installCounter()

	scan, err := h.pipe.Scan(ctx, h.srcRoot, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	report, err := h.pipe.DryRun(ctx, scan, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := c.quick.Load(); got != 3 {
		t.Fatalf("dry run quick hashes = %d, want 3 (one per file)", got)
	}

	// Reset: from here we measure ONLY the import's hashing work.
	c.quick.Store(0)
	c.full.Store(0)

	opts := h.copyOpts()
	opts.Precomputed = report
	session, err := h.pipe.Run(ctx, opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := c.quick.Load(); got != 0 {
		t.Fatalf("import recomputed %d quick hashes, want 0 (all reused from dry run)", got)
	}
	if got := c.full.Load(); got != 0 {
		t.Fatalf("import recomputed %d full hashes, want 0", got)
	}
	if session.FilesImported != 3 {
		t.Fatalf("FilesImported = %d, want 3", session.FilesImported)
	}
}

// TestPrecomputedReuseStaleFileRehashed proves the staleness gate: a file whose
// bytes AND mtime changed between dry run and import is the ONLY one re-hashed,
// and the NEW (correct) hash of that file is what gets recorded.
func TestPrecomputedReuseStaleFileRehashed(t *testing.T) {
	h := newHarness(t)
	pA := h.writeFile("a.jpg", "alpha-original", testDate)
	h.writeFile("b.jpg", "bravo-unchanged", testDate)

	ctx := context.Background()
	scan, err := h.pipe.Scan(ctx, h.srcRoot, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	report, err := h.pipe.DryRun(ctx, scan, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}

	// Edit a.jpg after the dry run: different content (size) AND a different mtime.
	later := testDate.Add(48 * time.Hour)
	if err := os.WriteFile(pA, []byte("alpha-CHANGED-and-longer-content"), 0o644); err != nil {
		t.Fatalf("rewrite a.jpg: %v", err)
	}
	if err := os.Chtimes(pA, later, later); err != nil {
		t.Fatalf("chtimes a.jpg: %v", err)
	}
	wantHash, err := hashing.QuickHash(pA)
	if err != nil {
		t.Fatalf("hash changed a.jpg: %v", err)
	}

	c := h.installCounter()
	opts := h.copyOpts()
	opts.Precomputed = report
	if _, err := h.pipe.Run(ctx, opts, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := c.quick.Load(); got != 1 {
		t.Fatalf("import quick hashes = %d, want 1 (only the changed a.jpg re-hashed)", got)
	}

	// The freshly computed hash — not the stale dry-run one — must be recorded.
	var a domain.Asset
	if err := h.db.Where("original_filename = ?", "a.jpg").First(&a).Error; err != nil {
		t.Fatalf("load a.jpg asset: %v", err)
	}
	if a.QuickHash != wantHash {
		t.Fatalf("a.jpg recorded quick hash %q, want fresh %q", a.QuickHash, wantHash)
	}
	if stale := report.QuickHashes[pA]; a.QuickHash == stale {
		t.Fatalf("a.jpg recorded the STALE dry-run hash %q", stale)
	}
}

// TestResumeWithoutPrecomputedRehashesAll proves the crash-resume path is
// unaffected: ResumeSession with no precomputed report re-hashes every file
// (correct behavior — a crash has no trustworthy in-memory report).
func TestResumeWithoutPrecomputedRehashesAll(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		h.writeFile(n, "content-"+n, testDate)
	}

	ctx := context.Background()
	session, err := h.pipe.Run(ctx, h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("initial run: %v", err)
	}

	// Now resume the session with NO precomputed hashes. Every file is re-hashed
	// for classification (all resolve to already-imported).
	c := h.installCounter()
	if _, err := h.pipe.ResumeSession(ctx, session.ID, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := c.quick.Load(); got != 3 {
		t.Fatalf("resume without precomputed hashed %d quick, want 3 (all re-hashed)", got)
	}
}

// TestPrecomputedAdoptReusesFullHashes proves full-hash reuse (requirement 3):
// files the dry run full-hashed (an intra-batch collision group) are NOT
// full-hashed again at import — only the unique New file needs a fresh baseline.
func TestPrecomputedAdoptReusesFullHashes(t *testing.T) {
	h := newHarness(t)
	// dup1 and dup2 are identical -> the dry run full-hashes both to confirm the
	// intra-batch duplicate. uniq is unique -> quick-hashed only.
	h.writeFile("dup1.jpg", "identical-adopt-bytes", testDate)
	h.writeFile("dup2.jpg", "identical-adopt-bytes", testDate)
	h.writeFile("uniq.jpg", "unique-adopt-bytes", testDate)

	ctx := context.Background()
	opts := Options{Mode: ModeAdopt, SourceRoot: h.srcRoot, DestinationRoot: h.srcRoot}
	scan, err := h.pipe.Scan(ctx, h.srcRoot, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	report, err := h.pipe.DryRun(ctx, scan, opts, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// Sanity: the collision group carries full hashes; the unique file does not.
	if report.FullHashes[filepath.Join(h.srcRoot, "dup1.jpg")] == "" ||
		report.FullHashes[filepath.Join(h.srcRoot, "dup2.jpg")] == "" {
		t.Fatalf("dry run did not carry full hashes for the collision group: %+v", report.FullHashes)
	}

	c := h.installCounter()
	opts.Precomputed = report
	session, err := h.pipe.Run(ctx, opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// dup1 reuses its full hash as the adopt baseline; dup2 reuses its full hash
	// for Stage-2 duplicate confirmation; only uniq computes a fresh baseline.
	if got := c.full.Load(); got != 1 {
		t.Fatalf("import full hashes = %d, want 1 (only uniq.jpg's baseline; collision full hashes reused)", got)
	}
	if got := c.quick.Load(); got != 0 {
		t.Fatalf("import quick hashes = %d, want 0 (all reused)", got)
	}
	if session.FilesImported != 2 || session.Duplicates != 1 {
		t.Fatalf("adopt imported=%d dup=%d, want 2/1", session.FilesImported, session.Duplicates)
	}
}
