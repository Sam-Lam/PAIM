package thumbs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRepointUsesNewDir(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	dirB := filepath.Join(base, "b")
	src := filepath.Join(base, "img.jpg")
	writeJPEG(t, src)

	gen := &fakeGen{}
	c := newCacheAt(dirA, gen, 4, nil)
	if c.Dir() != dirA {
		t.Fatalf("Dir() = %s, want %s", c.Dir(), dirA)
	}

	p1, err := c.Ensure(context.Background(), src, "aabbcc", SizeGrid)
	if err != nil {
		t.Fatalf("ensure in A: %v", err)
	}
	if filepath.Dir(filepath.Dir(filepath.Dir(p1))) != dirA {
		t.Errorf("thumb %s not under dir A %s", p1, dirA)
	}

	// Re-point to B: subsequent thumbnails must land under B, and the same key
	// regenerates (a fresh location has no cache) rather than reusing A.
	c.Repoint(dirB)
	if c.Dir() != dirB {
		t.Fatalf("Dir() after repoint = %s, want %s", c.Dir(), dirB)
	}
	p2, err := c.Ensure(context.Background(), src, "aabbcc", SizeGrid)
	if err != nil {
		t.Fatalf("ensure in B: %v", err)
	}
	if filepath.Dir(filepath.Dir(filepath.Dir(p2))) != dirB {
		t.Errorf("thumb %s not under dir B %s", p2, dirB)
	}
	if got := gen.calls.Load(); got != 2 {
		t.Errorf("generator ran %d times, want 2 (one per location)", got)
	}
}

func TestClearEmptiesActiveDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "thumbs")
	src := filepath.Join(t.TempDir(), "img.jpg")
	writeJPEG(t, src)

	c := newCacheAt(dir, &fakeGen{}, 4, nil)
	p, err := c.Ensure(context.Background(), src, "deadbeef", SizeGrid)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !fileExists(p) {
		t.Fatalf("thumb not written at %s", p)
	}

	if err := c.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if fileExists(p) {
		t.Error("thumbnail still present after Clear")
	}
	// The (empty) cache dir is recreated so subsequent generation works.
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("cache dir not recreated after Clear (err=%v)", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("cache dir not empty after Clear: %d entries", len(entries))
	}
}
