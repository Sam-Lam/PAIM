package library

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStableIDDeterministic(t *testing.T) {
	a := StableID("/Volumes/Photos")
	b := StableID("/Volumes/Photos")
	if a != b {
		t.Errorf("StableID not deterministic: %s != %s", a, b)
	}
	if a == StableID("/Volumes/Other") {
		t.Error("StableID collided for different roots")
	}
	if len(a) != 16 {
		t.Errorf("StableID length = %d, want 16", len(a))
	}
}

func TestResolveThumbCacheDir(t *testing.T) {
	root := "/Volumes/Photos"
	lib, err := ResolveThumbCacheDir(root, ThumbLocationLibrary)
	if err != nil {
		t.Fatalf("resolve library: %v", err)
	}
	if lib != filepath.Join(root, ".paim", "thumbs") {
		t.Errorf("library dir = %s", lib)
	}
	// Empty preference falls back to the library location.
	if def, _ := ResolveThumbCacheDir(root, ""); def != lib {
		t.Errorf("empty preference = %s, want %s", def, lib)
	}

	local, err := ResolveThumbCacheDir(root, ThumbLocationLocal)
	if err != nil {
		t.Fatalf("resolve local: %v", err)
	}
	if !strings.Contains(local, filepath.Join("Application Support", "PAIM", "thumbs", StableID(root))) {
		t.Errorf("local dir = %s, missing app-support/thumbs/<id>", local)
	}
}

func TestKnownThumbCacheDirsContainsBoth(t *testing.T) {
	root := "/Volumes/Photos"
	dirs, err := KnownThumbCacheDirs(root)
	if err != nil {
		t.Fatalf("known dirs: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("known dirs = %d, want 2", len(dirs))
	}
	lib := LibraryThumbsDir(root)
	local, _ := LocalThumbsDir(root)
	if dirs[0] != lib || dirs[1] != local {
		t.Errorf("known dirs = %v, want [%s %s]", dirs, lib, local)
	}
}
