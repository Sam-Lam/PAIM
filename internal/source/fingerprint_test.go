package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// fakeHasher quick-hashes files by their content via SHA-256 (a stand-in for
// BLAKE3; the fingerprint only requires a deterministic per-content hash).
type fakeHasher struct {
	failOn map[string]bool
}

func (h fakeHasher) QuickHash(path string) (string, error) {
	if h.failOn[filepath.Base(path)] {
		return "", os.ErrPermission
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "q:" + hex.EncodeToString(sum[:8]), nil
}

func (h fakeHasher) FullHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "f:" + hex.EncodeToString(sum[:]), nil
}

// mediaExts is a small isMedia policy for tests.
func isMediaTest(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "cr3", "arw", "mov", "mp4", "heic":
		return true
	default:
		return false
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildTree writes a small media tree (plus a non-media and a hidden file) and
// returns the root.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "DCIM", "100CANON", "IMG_0001.CR3"), "raw-one")
	writeFile(t, filepath.Join(root, "DCIM", "100CANON", "IMG_0002.CR3"), "raw-two")
	writeFile(t, filepath.Join(root, "DCIM", "100CANON", "IMG_0001.JPG"), "jpg-one")
	writeFile(t, filepath.Join(root, "DCIM", "100CANON", "CLIP_0001.MOV"), "clip-one")
	writeFile(t, filepath.Join(root, "notes.txt"), "not media")                 // skipped: non-media
	writeFile(t, filepath.Join(root, ".Spotlight-V100", "store.db"), "hidden")  // skipped: hidden dir
	writeFile(t, filepath.Join(root, "DCIM", ".hidden.jpg"), "hidden file")     // skipped: hidden file
	return root
}

func TestComputeFingerprint_Basic(t *testing.T) {
	root := buildTree(t)
	fp, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	if fp.TotalFileCount != 4 {
		t.Errorf("TotalFileCount = %d, want 4 (media only, no hidden/non-media)", fp.TotalFileCount)
	}
	wantBytes := int64(len("raw-one") + len("raw-two") + len("jpg-one") + len("clip-one"))
	if fp.TotalBytes != wantBytes {
		t.Errorf("TotalBytes = %d, want %d", fp.TotalBytes, wantBytes)
	}
	if fp.PathHash == "" || fp.ContentHash == "" {
		t.Errorf("expected non-empty hashes, got path=%q content=%q", fp.PathHash, fp.ContentHash)
	}
}

func TestComputeFingerprint_Deterministic(t *testing.T) {
	root := buildTree(t)
	a, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.PathHash != b.PathHash || a.ContentHash != b.ContentHash {
		t.Errorf("fingerprint not deterministic: %+v vs %+v", a, b)
	}
}

func TestComputeFingerprint_ChangesWhenFileAdded(t *testing.T) {
	root := buildTree(t)
	before, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "DCIM", "100CANON", "IMG_0003.CR3"), "raw-three")
	after, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalFileCount != before.TotalFileCount+1 {
		t.Errorf("count did not increase: %d -> %d", before.TotalFileCount, after.TotalFileCount)
	}
	if after.PathHash == before.PathHash {
		t.Errorf("PathHash unchanged after adding a file")
	}
}

func TestComputeFingerprint_Progress(t *testing.T) {
	root := buildTree(t)
	var last int
	_, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, func(scanned int) {
		last = scanned
	})
	if err != nil {
		t.Fatal(err)
	}
	if last != 4 {
		t.Errorf("final progress = %d, want 4", last)
	}
}

func TestComputeFingerprint_HasherFailureDegrades(t *testing.T) {
	root := buildTree(t)
	// Fail one file's quick hash; fingerprint should still succeed.
	fp, err := ComputeFingerprint(context.Background(), root, fakeHasher{failOn: map[string]bool{"IMG_0001.CR3": true}}, isMediaTest, nil)
	if err != nil {
		t.Fatalf("expected degradation, got error: %v", err)
	}
	if fp.ContentHash == "" {
		t.Error("expected a content hash from the remaining files")
	}
}

func TestComputeFingerprint_ContextCancelled(t *testing.T) {
	root := buildTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ComputeFingerprint(ctx, root, fakeHasher{}, isMediaTest, nil)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

func TestEvenSampleIndexes(t *testing.T) {
	// n <= k returns all.
	if got := evenSampleIndexes(3, 20); len(got) != 3 {
		t.Errorf("n<=k len = %d, want 3", len(got))
	}
	// n > k returns exactly k, spread across the range.
	got := evenSampleIndexes(1000, 20)
	if len(got) != 20 {
		t.Fatalf("len = %d, want 20", len(got))
	}
	if got[0] != 0 {
		t.Errorf("first index = %d, want 0", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("indexes not strictly increasing at %d: %v", i, got)
		}
	}
}

func TestFingerprintJSONRoundTrip(t *testing.T) {
	fp := Fingerprint{TotalFileCount: 12, TotalBytes: 3456, PathHash: "abc", ContentHash: "def"}
	parsed, err := ParseFingerprint(fp.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != fp {
		t.Errorf("round trip mismatch: %+v vs %+v", parsed, fp)
	}
	// Empty string parses to zero value without error.
	if z, err := ParseFingerprint(""); err != nil || z != (Fingerprint{}) {
		t.Errorf("empty parse: %+v, %v", z, err)
	}
}
