package importer

import (
	"path/filepath"
	"testing"
	"time"
)

// TestBuildAssetStoresRelativePath verifies that with a LibraryRoot configured,
// buildAsset stores CurrentArchivePath relative to the root (forward slashes),
// and that a path outside the root is kept absolute.
func TestBuildAssetStoresRelativePath(t *testing.T) {
	root := filepath.Join("/", "Volumes", "Library")
	p := &Pipeline{libraryRoot: root}

	fi := FileInfo{Path: filepath.Join("/", "src", "IMG.JPG"), Ext: "jpg", Size: 10}
	cls := classification{QuickHash: "qh", FullHash: "fh"}

	inside := filepath.Join(root, "2024", "2024-01-01", "IMG.JPG")
	a := p.buildAsset("sess", fi, cls, nil, inside, time.Now(), nil)
	if a.CurrentArchivePath != "2024/2024-01-01/IMG.JPG" {
		t.Fatalf("inside-root path not relativized: %q", a.CurrentArchivePath)
	}

	outside := filepath.Join("/", "elsewhere", "IMG.JPG")
	b := p.buildAsset("sess", fi, cls, nil, outside, time.Now(), nil)
	if b.CurrentArchivePath != outside {
		t.Fatalf("outside-root path should stay absolute: %q", b.CurrentArchivePath)
	}

	// A not-copied duplicate has no path and stays empty.
	c := p.buildAsset("sess", fi, cls, nil, "", time.Now(), nil)
	if c.CurrentArchivePath != "" {
		t.Fatalf("empty archive path should stay empty: %q", c.CurrentArchivePath)
	}

	// With no library root (dev/legacy), absolute paths are preserved.
	dev := &Pipeline{}
	d := dev.buildAsset("sess", fi, cls, nil, inside, time.Now(), nil)
	if d.CurrentArchivePath != inside {
		t.Fatalf("dev mode should keep absolute path: %q", d.CurrentArchivePath)
	}
}
