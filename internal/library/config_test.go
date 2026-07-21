package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.json")
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}

	// A missing file loads as a zero config (first run), not an error.
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg.LastLibrary != "" || len(cfg.RecentLibraries) != 0 {
		t.Fatalf("expected zero config, got %+v", cfg)
	}

	if err := store.RecordOpened("/Volumes/A", "A"); err != nil {
		t.Fatalf("RecordOpened: %v", err)
	}
	if err := store.RecordOpened("/Volumes/B", ""); err != nil {
		t.Fatalf("RecordOpened B: %v", err)
	}
	// Re-open A: it should move to the front and not duplicate.
	if err := store.RecordOpened("/Volumes/A", "A"); err != nil {
		t.Fatalf("RecordOpened A again: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.LastLibrary != "/Volumes/A" {
		t.Fatalf("LastLibrary = %q, want /Volumes/A", got.LastLibrary)
	}
	if len(got.RecentLibraries) != 2 {
		t.Fatalf("recent count = %d, want 2 (deduped)", len(got.RecentLibraries))
	}
	if got.RecentLibraries[0].Path != "/Volumes/A" {
		t.Fatalf("most-recent should be A, got %q", got.RecentLibraries[0].Path)
	}
}

func TestConfigSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	store, _ := NewConfigStore(path)

	if err := store.Save(Config{LastLibrary: "/x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// No temp files should linger after an atomic write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Fatalf("unexpected leftover file %q after atomic save", e.Name())
		}
	}
}
