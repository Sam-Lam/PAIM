package services

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/repo"
)

func newTestSettingsRepo(t *testing.T) *repo.SettingsRepo {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return repo.NewSettingsRepo(gdb)
}

func TestSettingsDefaults(t *testing.T) {
	sr := newTestSettingsRepo(t)
	svc := NewSettingsService(sr, true)

	got, err := svc.GetAll(context.Background())
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if got.MasterLibraryRoot != "" {
		t.Errorf("default master root should be empty, got %q", got.MasterLibraryRoot)
	}
	if got.BackupWorkers != DefaultBackupWorkers {
		t.Errorf("default backup workers = %d want %d", got.BackupWorkers, DefaultBackupWorkers)
	}
	if got.MaxRetries != DefaultMaxRetries {
		t.Errorf("default max retries = %d want %d", got.MaxRetries, DefaultMaxRetries)
	}
	if !got.MetadataAvailable {
		t.Error("MetadataAvailable should reflect the constructor argument (true)")
	}
}

func TestSettingsUpdateRoundtrip(t *testing.T) {
	sr := newTestSettingsRepo(t)
	svc := NewSettingsService(sr, false)
	dir := t.TempDir()

	updated, err := svc.Update(context.Background(), Settings{
		MasterLibraryRoot: dir,
		BackupWorkers:     4,
		MaxRetries:        7,
		ImportConcurrency: 3,
		DefaultEventName:  "Vacation",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.MasterLibraryRoot != dir || updated.BackupWorkers != 4 || updated.MaxRetries != 7 {
		t.Fatalf("update did not persist: %+v", updated)
	}

	reloaded, err := svc.GetAll(context.Background())
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if reloaded.DefaultEventName != "Vacation" || reloaded.ImportConcurrency != 3 {
		t.Fatalf("reloaded mismatch: %+v", reloaded)
	}
}

func TestSettingsUpdateRejectsMissingRoot(t *testing.T) {
	sr := newTestSettingsRepo(t)
	svc := NewSettingsService(sr, false)

	_, err := svc.Update(context.Background(), Settings{
		MasterLibraryRoot: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err == nil {
		t.Fatal("expected error updating with a nonexistent master root")
	}
}
