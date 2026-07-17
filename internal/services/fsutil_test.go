package services

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyVerifyChunkedCopiesAndVerifies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	// Larger than one copy chunk so the chunked loop iterates.
	content := bytes.Repeat([]byte("photo-archive-integrity-"), 200000)
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	dst := filepath.Join(dir, "nested", "dst.bin")
	if err := copyVerify(context.Background(), src, dst); err != nil {
		t.Fatalf("copyVerify: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("destination content mismatch")
	}
	// No partial temp file left behind.
	entries, _ := os.ReadDir(filepath.Dir(dst))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".paim-partial-") {
			t.Fatalf("stray partial remains: %s", e.Name())
		}
	}
}

func TestCopyVerifyHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 4<<20), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	dst := filepath.Join(dir, "dst.bin")
	if err := copyVerify(ctx, src, dst); err == nil {
		t.Fatalf("expected cancellation error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("cancelled copy should not publish a destination file")
	}
}
