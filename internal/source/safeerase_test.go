package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeLookup maps quick hash -> archived assets (the hashing path) and original
// source path -> archived assets (the fast path). byPath is nil in tests that
// only exercise hashing, so FindByOriginalPath returns nothing and every file
// falls back to the hash path.
type fakeLookup struct {
	byQuick map[string][]ArchivedAsset
	byPath  map[string][]ArchivedAsset
	err     error
}

func (l fakeLookup) FindByQuickHash(_ context.Context, quick string) ([]ArchivedAsset, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.byQuick[quick], nil
}

func (l fakeLookup) FindByOriginalPath(_ context.Context, path string) ([]ArchivedAsset, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.byPath[path], nil
}

// countingHasher wraps fakeHasher and records how many QuickHash/FullHash calls
// were made, so a test can assert the fast path hashed nothing.
type countingHasher struct {
	fakeHasher
	quick int
	full  int
}

func (h *countingHasher) QuickHash(path string) (string, error) {
	h.quick++
	return h.fakeHasher.QuickHash(path)
}

func (h *countingHasher) FullHash(path string) (string, error) {
	h.full++
	return h.fakeHasher.FullHash(path)
}

// quickOf and fullOf replicate fakeHasher's hashing over raw content so tests
// can predict the hash a file with that content will produce.
func quickOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "q:" + hex.EncodeToString(sum[:8])
}

func fullOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "f:" + hex.EncodeToString(sum[:])
}

func newIdentifierForErase() *Identifier {
	return NewIdentifier(nil, nil, fakeHasher{}, nil)
}

func TestEvaluateSafeToErase_AllArchivedSafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")
	writeFile(t, filepath.Join(root, "b.cr3"), "two")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", QuickHash: quickOf("one"), Verified: true, BackupComplete: true}},
		quickOf("two"): {{ID: "b", QuickHash: quickOf("two"), Verified: true, BackupComplete: true}},
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true (reason: %s)", rep.Reason)
	}
	if rep.Archived != 2 || rep.TotalMedia != 2 {
		t.Errorf("Archived=%d TotalMedia=%d, want 2/2", rep.Archived, rep.TotalMedia)
	}
	if rep.New != 0 || rep.Unverified != 0 || rep.BackupIncomplete != 0 {
		t.Errorf("unexpected problem counts: %+v", rep)
	}
}

func TestEvaluateSafeToErase_NewFileUnsafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")
	writeFile(t, filepath.Join(root, "new.jpg"), "brand-new") // not in archive

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", Verified: true, BackupComplete: true}},
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false")
	}
	if rep.New != 1 {
		t.Errorf("New = %d, want 1", rep.New)
	}
	if !contains(rep.Reason, "not yet imported") {
		t.Errorf("reason %q should mention not-yet-imported", rep.Reason)
	}
}

func TestEvaluateSafeToErase_UnverifiedUnsafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", Verified: false, BackupComplete: true}}, // matched but not verified
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false")
	}
	if rep.Unverified != 1 {
		t.Errorf("Unverified = %d, want 1", rep.Unverified)
	}
	if !contains(rep.Reason, "not verified") {
		t.Errorf("reason %q should mention verification", rep.Reason)
	}
}

func TestEvaluateSafeToErase_BackupIncompleteUnsafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", Verified: true, BackupComplete: false}},
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false")
	}
	if rep.BackupIncomplete != 1 {
		t.Errorf("BackupIncomplete = %d, want 1", rep.BackupIncomplete)
	}
	if !contains(rep.Reason, "incomplete backups") {
		t.Errorf("reason %q should mention backups", rep.Reason)
	}
}

// TestEvaluateSafeToErase_QuickHashCollisionResolvedByFullHash covers the case
// where several archived assets share a quick hash and the file's full hash
// disambiguates.
func TestEvaluateSafeToErase_QuickHashCollisionResolvedByFullHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "collide")

	h := fakeHasher{}
	quick := quickOf("collide")
	full := fullOf("collide")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quick: {
			{ID: "other", QuickHash: quick, FullHash: "f:not-this-one", Verified: true, BackupComplete: true},
			{ID: "match", QuickHash: quick, FullHash: full, Verified: true, BackupComplete: true},
		},
	}}

	id := NewIdentifier(nil, nil, h, nil)
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true (collision should resolve): %+v", rep)
	}
	if rep.Archived != 1 {
		t.Errorf("Archived = %d, want 1", rep.Archived)
	}
}

func TestEvaluateSafeToErase_EmptyVolumeSafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "readme.txt"), "no media here")
	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, fakeLookup{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true for no-media volume")
	}
	if rep.TotalMedia != 0 {
		t.Errorf("TotalMedia = %d, want 0", rep.TotalMedia)
	}
}

// statOf returns a file's size and mtime for building fast-path assets.
func statOf(t *testing.T, path string) (int64, time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info.Size(), info.ModTime()
}

// TestEvaluateSafeToErase_FastPathNoHashing verifies that files matched by the
// catalog fast path (same original path + size, unmodified since import) are
// classified WITHOUT any hash calls, and reported under FastPath.
func TestEvaluateSafeToErase_FastPathNoHashing(t *testing.T) {
	root := t.TempDir()
	pa := filepath.Join(root, "a.jpg")
	pb := filepath.Join(root, "b.cr3")
	writeFile(t, pa, "one")
	writeFile(t, pb, "two")

	sa, ma := statOf(t, pa)
	sb, mb := statOf(t, pb)

	lookup := fakeLookup{byPath: map[string][]ArchivedAsset{
		pa: {{ID: "a", OriginalFullPath: pa, FileSize: sa, ImportDate: ma.Add(time.Hour), HasArchiveCopy: true, Verified: true, BackupComplete: true}},
		pb: {{ID: "b", OriginalFullPath: pb, FileSize: sb, ImportDate: mb.Add(time.Hour), HasArchiveCopy: true, Verified: true, BackupComplete: true}},
	}}

	h := &countingHasher{}
	id := NewIdentifier(nil, nil, h, nil)
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true (reason: %s)", rep.Reason)
	}
	if h.quick != 0 || h.full != 0 {
		t.Errorf("fast path hashed: quick=%d full=%d, want 0/0", h.quick, h.full)
	}
	if rep.FastPath != 2 || rep.Hashed != 0 {
		t.Errorf("FastPath=%d Hashed=%d, want 2/0", rep.FastPath, rep.Hashed)
	}
	if len(rep.SafeFiles) != 2 {
		t.Errorf("SafeFiles=%v, want the 2 media paths", rep.SafeFiles)
	}
}

// TestEvaluateSafeToErase_ModifiedFileFallsBackToHash verifies that a file whose
// mtime is after the asset's ImportDate (touched since import) is NOT trusted via
// the fast path: exactly that file is hashed, the unmodified one is not.
func TestEvaluateSafeToErase_ModifiedFileFallsBackToHash(t *testing.T) {
	root := t.TempDir()
	pa := filepath.Join(root, "a.jpg") // unmodified
	pb := filepath.Join(root, "b.jpg") // modified after import
	writeFile(t, pa, "one")
	writeFile(t, pb, "two")

	sa, ma := statOf(t, pa)
	sb, _ := statOf(t, pb)

	importB := time.Now().Add(-time.Hour)
	// Make b's mtime clearly after its ImportDate.
	if err := os.Chtimes(pb, time.Now(), time.Now()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	lookup := fakeLookup{
		byPath: map[string][]ArchivedAsset{
			pa: {{ID: "a", OriginalFullPath: pa, FileSize: sa, ImportDate: ma.Add(time.Hour), HasArchiveCopy: true, Verified: true, BackupComplete: true}},
			pb: {{ID: "b", OriginalFullPath: pb, FileSize: sb, ImportDate: importB, HasArchiveCopy: true, Verified: true, BackupComplete: true}},
		},
		byQuick: map[string][]ArchivedAsset{
			quickOf("two"): {{ID: "b", Verified: true, BackupComplete: true}},
		},
	}

	h := &countingHasher{}
	id := NewIdentifier(nil, nil, h, nil)
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true (reason: %s)", rep.Reason)
	}
	if h.quick != 1 {
		t.Errorf("QuickHash calls = %d, want 1 (only the modified file)", h.quick)
	}
	if rep.FastPath != 1 || rep.Hashed != 1 {
		t.Errorf("FastPath=%d Hashed=%d, want 1/1", rep.FastPath, rep.Hashed)
	}
}

// TestEvaluateSafeToErase_SizeMismatchFallsBackToHash verifies that a file whose
// size differs from the recorded asset is not trusted via the fast path.
func TestEvaluateSafeToErase_SizeMismatchFallsBackToHash(t *testing.T) {
	root := t.TempDir()
	pa := filepath.Join(root, "a.jpg")
	writeFile(t, pa, "one")
	_, ma := statOf(t, pa)

	lookup := fakeLookup{
		byPath: map[string][]ArchivedAsset{
			// Recorded size is wrong (999), so the fast path must decline.
			pa: {{ID: "a", OriginalFullPath: pa, FileSize: 999, ImportDate: ma.Add(time.Hour), HasArchiveCopy: true, Verified: true, BackupComplete: true}},
		},
		byQuick: map[string][]ArchivedAsset{
			quickOf("one"): {{ID: "a", Verified: true, BackupComplete: true}},
		},
	}

	h := &countingHasher{}
	id := NewIdentifier(nil, nil, h, nil)
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.quick != 1 {
		t.Errorf("QuickHash calls = %d, want 1 (size mismatch forces hashing)", h.quick)
	}
	if rep.FastPath != 0 || rep.Hashed != 1 {
		t.Errorf("FastPath=%d Hashed=%d, want 0/1", rep.FastPath, rep.Hashed)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true: %+v", rep)
	}
}

// TestEvaluateSafeToErase_FastPathCollectsOnlySafe verifies the SafeFiles list
// contains exactly the archived (safe) media, excluding unverified media.
func TestEvaluateSafeToErase_FastPathCollectsOnlySafe(t *testing.T) {
	root := t.TempDir()
	pSafe := filepath.Join(root, "safe.jpg")
	pUnver := filepath.Join(root, "unver.jpg")
	writeFile(t, pSafe, "one")
	writeFile(t, pUnver, "two")
	ss, ms := statOf(t, pSafe)
	su, mu := statOf(t, pUnver)

	lookup := fakeLookup{byPath: map[string][]ArchivedAsset{
		pSafe:  {{ID: "s", OriginalFullPath: pSafe, FileSize: ss, ImportDate: ms.Add(time.Hour), HasArchiveCopy: true, Verified: true, BackupComplete: true}},
		pUnver: {{ID: "u", OriginalFullPath: pUnver, FileSize: su, ImportDate: mu.Add(time.Hour), HasArchiveCopy: true, Verified: false, BackupComplete: true}},
	}}

	h := &countingHasher{}
	id := NewIdentifier(nil, nil, h, nil)
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false (one unverified)")
	}
	if len(rep.SafeFiles) != 1 || rep.SafeFiles[0] != pSafe {
		t.Errorf("SafeFiles = %v, want exactly [%s]", rep.SafeFiles, pSafe)
	}
	if h.quick != 0 {
		t.Errorf("QuickHash calls = %d, want 0 (both fast-pathed)", h.quick)
	}
}

// helpers ---------------------------------------------------------------------

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
