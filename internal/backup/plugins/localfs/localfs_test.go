package localfs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/backup/plugins/localfs"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func initPlugin(t *testing.T, root string) *localfs.Plugin {
	t.Helper()
	p := localfs.New().(*localfs.Plugin)
	cfg := `{"root":"` + root + `"}`
	if err := p.Initialize(context.Background(), cfg); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return p
}

func TestUploadVerifyRoundTrip(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "IMG_0001.JPG")
	content := bytes.Repeat([]byte("photo-bytes-"), 500_000) // ~6 MB, spans chunks
	writeFile(t, src, content)

	p := initPlugin(t, root)
	ctx := context.Background()

	var lastDone, lastTotal int64
	progress := func(done, total int64) { lastDone, lastTotal = done, total }

	remote := "2024/2024-01-01/IMG_0001.JPG"
	if err := p.Upload(ctx, src, remote, progress); err != nil {
		t.Fatalf("upload: %v", err)
	}

	dst := filepath.Join(root, filepath.FromSlash(remote))
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("destination content mismatch")
	}
	if lastDone != int64(len(content)) || lastTotal != int64(len(content)) {
		t.Fatalf("progress final = %d/%d, want %d", lastDone, lastTotal, len(content))
	}

	ok, err := p.Verify(ctx, src, remote)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("verify should succeed for identical copy")
	}

	// No stray partial files should remain.
	assertNoPartials(t, filepath.Dir(dst))
}

func TestVerifyDetectsCorruptedDestination(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a.mov")
	writeFile(t, src, []byte("original content"))

	p := initPlugin(t, root)
	ctx := context.Background()
	remote := "vids/a.mov"
	if err := p.Upload(ctx, src, remote, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Corrupt the destination.
	dst := filepath.Join(root, filepath.FromSlash(remote))
	writeFile(t, dst, []byte("tampered content!"))

	ok, err := p.Verify(ctx, src, remote)
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if ok {
		t.Fatalf("verify should fail for corrupted destination")
	}
}

func TestVerifyMissingDestination(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a.jpg")
	writeFile(t, src, []byte("x"))
	p := initPlugin(t, root)

	ok, err := p.Verify(context.Background(), src, "not/there.jpg")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if ok {
		t.Fatalf("verify of missing destination should be false")
	}
}

func TestDeleteMovesToTrash(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "old.jpg")
	writeFile(t, src, []byte("delete me"))

	p := initPlugin(t, root)
	ctx := context.Background()
	remote := "2023/old.jpg"
	if err := p.Upload(ctx, src, remote, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	dst := filepath.Join(root, filepath.FromSlash(remote))

	if err := p.Delete(ctx, remote); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination should be gone after delete, err=%v", err)
	}

	trash := filepath.Join(root, ".paim-trash")
	entries, err := os.ReadDir(trash)
	if err != nil {
		t.Fatalf("read trash: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 trashed file, got %d", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), "-old.jpg") {
		t.Fatalf("trashed name %q should end with -old.jpg", entries[0].Name())
	}

	// Deleting a missing object is a no-op (no error).
	if err := p.Delete(ctx, "does/not/exist.jpg"); err != nil {
		t.Fatalf("delete missing should be nil, got %v", err)
	}
}

func TestUploadCleansUpPartialOnCancel(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "big.mov")
	writeFile(t, src, bytes.Repeat([]byte("z"), 4<<20)) // 4 MB

	p := initPlugin(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the copy loop can complete

	remote := "2024/big.mov"
	err := p.Upload(ctx, src, remote, nil)
	if err == nil {
		t.Fatalf("expected upload to fail on cancelled context")
	}

	// No destination file and no stray partial in the destination directory.
	dst := filepath.Join(root, filepath.FromSlash(remote))
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("destination should not exist after cancelled upload")
	}
	assertNoPartials(t, filepath.Dir(dst))
}

func TestInitializeValidatesRoot(t *testing.T) {
	// Non-existent root.
	p := localfs.New()
	if err := p.Initialize(context.Background(), `{"root":"/no/such/dir/xyz123"}`); err == nil {
		t.Fatalf("expected error for non-existent root")
	}

	// Root that is a file, not a directory.
	f := filepath.Join(t.TempDir(), "afile")
	writeFile(t, f, []byte("x"))
	if err := localfs.New().Initialize(context.Background(), `{"root":"`+f+`"}`); err == nil {
		t.Fatalf("expected error when root is a file")
	}

	// Empty config.
	if err := localfs.New().Initialize(context.Background(), `{}`); err == nil {
		t.Fatalf("expected error for empty root")
	}

	// Valid root, plus capability sanity.
	root := t.TempDir()
	good := localfs.New()
	if err := good.Initialize(context.Background(), `{"root":"`+root+`"}`); err != nil {
		t.Fatalf("valid root should initialize: %v", err)
	}
	caps := good.Capabilities()
	if !caps.SupportsVerify || !caps.SupportsDelete || caps.SupportsResume {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}

func TestUploadContainsPathTraversal(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a.jpg")
	writeFile(t, src, []byte("x"))
	p := initPlugin(t, root)

	// A "../" prefix must be contained within root, never escape it.
	if err := p.Upload(context.Background(), src, "../escape.jpg", nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape.jpg")); err != nil {
		t.Fatalf("traversal should be contained inside root: %v", err)
	}
	parent := filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(parent, "escape.jpg")); !os.IsNotExist(err) {
		t.Fatalf("file escaped root into parent directory")
	}
}

// assertNoPartials fails if any .paim-partial-* file exists in dir.
func assertNoPartials(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".paim-partial-") {
			t.Fatalf("stray partial file left behind: %s", e.Name())
		}
	}
}
