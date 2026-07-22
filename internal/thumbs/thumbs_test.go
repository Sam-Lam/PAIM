package thumbs

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGen is an injectable generator for deterministic tests. It records how many
// times it ran, can block until released, and can be forced to fail.
type fakeGen struct {
	calls   atomic.Int32
	fail    bool
	release chan struct{} // if non-nil, generate blocks until closed/received
}

func (g *fakeGen) generate(ctx context.Context, srcPath, dstPath string, sizePx, quality int) error {
	g.calls.Add(1)
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if g.fail {
		return errors.New("forced failure")
	}
	// Write a tiny valid JPEG so downstream size/stat checks pass.
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		return err
	}
	return os.WriteFile(dstPath, buf.Bytes(), 0o644)
}

// writeJPEG creates a minimal valid JPEG at path (also used as a QuickLook source).
func writeJPEG(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jpeg: %v", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
}

func TestEnsureCacheHitGeneratesOnce(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "img.jpg")
	writeJPEG(t, src)

	gen := &fakeGen{}
	c := newCache(dir, gen, 4, nil)

	p1, err := c.Ensure(context.Background(), src, "abcdef", SizeGrid)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !fileExists(p1) {
		t.Fatalf("thumb not written at %s", p1)
	}
	// Content-addressed layout.
	want := filepath.Join(dir, ".paim", "thumbs", "512", "ab", "abcdef.jpg")
	if p1 != want {
		t.Errorf("thumb path = %s, want %s", p1, want)
	}

	// Second call is a pure cache hit — the generator must not run again.
	p2, err := c.Ensure(context.Background(), src, "abcdef", SizeGrid)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if p2 != p1 {
		t.Errorf("second path = %s, want %s", p2, p1)
	}
	if got := gen.calls.Load(); got != 1 {
		t.Errorf("generator ran %d times, want 1", got)
	}
}

func TestEnsureSingleflightCollapsesConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "img.jpg")
	writeJPEG(t, src)

	gen := &fakeGen{release: make(chan struct{})}
	c := newCache(dir, gen, 4, nil)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = c.Ensure(context.Background(), src, "deadbeef", SizePreview)
		}(i)
	}
	// Give the goroutines time to converge on the single in-flight generation, then
	// release it.
	time.Sleep(50 * time.Millisecond)
	close(gen.release)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("ensure[%d]: %v", i, errs[i])
		}
	}
	if got := gen.calls.Load(); got != 1 {
		t.Errorf("generator ran %d times under singleflight, want 1", got)
	}
}

func TestEnsureNegativeCaching(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "img.jpg")
	writeJPEG(t, src)

	gen := &fakeGen{fail: true}
	c := newCache(dir, gen, 4, nil)

	_, err := c.Ensure(context.Background(), src, "f00d", SizeGrid)
	if !errors.Is(err, ErrGenerationFailed) {
		t.Fatalf("first ensure err = %v, want ErrGenerationFailed", err)
	}
	// A negative marker must exist.
	if !fileExists(c.failPath(SizeGrid, "f00d")) {
		t.Fatal("negative marker not written")
	}
	// A second request must short-circuit on the marker without regenerating.
	_, err = c.Ensure(context.Background(), src, "f00d", SizeGrid)
	if !errors.Is(err, ErrGenerationFailed) {
		t.Fatalf("second ensure err = %v, want ErrGenerationFailed", err)
	}
	if got := gen.calls.Load(); got != 1 {
		t.Errorf("generator ran %d times, want 1 (negative cached)", got)
	}

	// A fresh Cache over the same dir sweeps the stale marker and retries once.
	gen2 := &fakeGen{}
	c2 := newCache(dir, gen2, 4, nil)
	if fileExists(c2.failPath(SizeGrid, "f00d")) {
		t.Fatal("stale marker not swept on construction")
	}
	if _, err := c2.Ensure(context.Background(), src, "f00d", SizeGrid); err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if got := gen2.calls.Load(); got != 1 {
		t.Errorf("post-restart generator ran %d times, want 1", got)
	}
}

func TestEnsureMissingSourceIsNotCached(t *testing.T) {
	dir := t.TempDir()
	gen := &fakeGen{}
	c := newCache(dir, gen, 4, nil)

	_, err := c.Ensure(context.Background(), filepath.Join(dir, "gone.jpg"), "ab12", SizeGrid)
	if !errors.Is(err, ErrSourceMissing) {
		t.Fatalf("err = %v, want ErrSourceMissing", err)
	}
	if fileExists(c.failPath(SizeGrid, "ab12")) {
		t.Fatal("missing source must NOT write a negative marker")
	}
	if got := gen.calls.Load(); got != 0 {
		t.Errorf("generator ran %d times for missing source, want 0", got)
	}
}

func TestEnsureAtomicNoPartialFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "img.jpg")
	writeJPEG(t, src)

	// A generator that fails after doing nothing must leave no thumb and no temp
	// files behind.
	gen := &fakeGen{fail: true}
	c := newCache(dir, gen, 4, nil)
	_, _ = c.Ensure(context.Background(), src, "cccc", SizeGrid)

	shardDir := filepath.Join(dir, ".paim", "thumbs", "512", "cc")
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		// Dir may exist (marker written) — that's fine; only stray temps matter.
		return
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("stray temp file left behind: %s", e.Name())
		}
		if strings.HasSuffix(e.Name(), ".jpg") {
			t.Errorf("partial thumbnail left behind: %s", e.Name())
		}
	}
}

func TestEnsureRejectsUnsupportedSize(t *testing.T) {
	c := newCache(t.TempDir(), &fakeGen{}, 4, nil)
	if _, err := c.Ensure(context.Background(), "/x.jpg", "aa", 999); !errors.Is(err, ErrUnsupportedSize) {
		t.Errorf("err = %v, want ErrUnsupportedSize", err)
	}
}

// TestQuickLookPipeline exercises the REAL qlmanage+sips pipeline on a tiny JPEG.
// It skips cleanly when QuickLook is unavailable or cannot render headless (CI).
func TestQuickLookPipeline(t *testing.T) {
	if _, err := exec.LookPath("qlmanage"); err != nil {
		t.Skip("qlmanage not available")
	}
	if _, err := exec.LookPath("sips"); err != nil {
		t.Skip("sips not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "real.jpg")
	writeJPEG(t, src)

	c := New(dir, nil)
	p, err := c.Ensure(context.Background(), src, "0123456789abcdef", SizeGrid)
	if err != nil {
		// QuickLook can fail in headless/sandboxed environments; that is not a test
		// failure of PAIM's code.
		t.Skipf("qlmanage could not render headless: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected a non-empty JPEG at %s (stat err=%v)", p, err)
	}
	// Sanity: the output starts with the JPEG SOI marker.
	b, _ := os.ReadFile(p)
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Errorf("output is not a JPEG (magic = % x)", b[:min(2, len(b))])
	}
}
