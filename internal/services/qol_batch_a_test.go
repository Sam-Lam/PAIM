package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/volumes"
)

// TestDashboardEnabledRequiredProviders confirms the dashboard counts only enabled
// NON-mirror (required) providers, so a zero here (with assets present) is the "no
// backup destination" signal the UI warns on.
func TestDashboardEnabledRequiredProviders(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "dash.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()

	// Enabled required, enabled mirror, and disabled required — only the first counts.
	for _, p := range []*domain.BackupProvider{
		{PluginName: "localfs", ConfigJSON: "{}", Enabled: true, Mirror: false},
		{PluginName: "rclone", ConfigJSON: "{}", Enabled: true, Mirror: true},
		{PluginName: "localfs", ConfigJSON: "{}", Enabled: false, Mirror: false},
	} {
		if err := gdb.Create(p).Error; err != nil {
			t.Fatalf("create provider: %v", err)
		}
	}

	svc := NewDashboardService(gdb, repo.NewAssetRepo(gdb), repo.NewBackupRepo(gdb), repo.NewSourceRepo(gdb), nil)
	stats, err := svc.GetStats(ctx)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.EnabledRequiredProviders != 1 {
		t.Fatalf("EnabledRequiredProviders = %d, want 1", stats.EnabledRequiredProviders)
	}

	// With none configured, the count is zero (the warning condition).
	gdb2, _ := db.Open(filepath.Join(t.TempDir(), "dash2.db"))
	svc2 := NewDashboardService(gdb2, repo.NewAssetRepo(gdb2), repo.NewBackupRepo(gdb2), repo.NewSourceRepo(gdb2), nil)
	stats2, err := svc2.GetStats(ctx)
	if err != nil {
		t.Fatalf("GetStats(2): %v", err)
	}
	if stats2.EnabledRequiredProviders != 0 {
		t.Fatalf("EnabledRequiredProviders (none) = %d, want 0", stats2.EnabledRequiredProviders)
	}
}

// TestBackupServiceRetryAllFailed covers count + transition + emission: every
// failed job moves to pending, the returned count is right, and a
// backup:queue-changed event is emitted.
func TestBackupServiceRetryAllFailed(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	jobs := repo.NewBackupRepo(gdb)
	assets := repo.NewAssetRepo(gdb)
	manager := backup.NewManager(
		backup.NewRepoJobQueue(gdb), assets, backup.NewRepoProviderStore(gdb),
		backup.NewRegistry(), nil, backup.Options{},
	)
	emitter := &captureEmitter{}
	svc := NewBackupService(manager, jobs, assets, nil, nil, emitter, nil)

	// Two failed jobs + one pending (pending must stay pending, count == failed only).
	mk := func(st domain.JobStatus) {
		j := &domain.BackupJob{AssetID: "a", Plugin: "localfs", Destination: "dest", Status: st}
		if err := gdb.Create(j).Error; err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	mk(domain.JobStatusFailed)
	mk(domain.JobStatusFailed)
	mk(domain.JobStatusPending)

	n, err := svc.RetryAllFailed(ctx)
	if err != nil {
		t.Fatalf("RetryAllFailed: %v", err)
	}
	if n != 2 {
		t.Fatalf("RetryAllFailed count = %d, want 2", n)
	}

	var failedLeft int64
	if err := gdb.Model(&domain.BackupJob{}).Where("status = ?", domain.JobStatusFailed).Count(&failedLeft).Error; err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if failedLeft != 0 {
		t.Fatalf("failed jobs remaining = %d, want 0", failedLeft)
	}
	var pending int64
	if err := gdb.Model(&domain.BackupJob{}).Where("status = ?", domain.JobStatusPending).Count(&pending).Error; err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 3 {
		t.Fatalf("pending jobs = %d, want 3 (2 retried + 1 original)", pending)
	}
	if got := emitter.byName(EventBackupQueueChanged); len(got) == 0 {
		t.Fatal("RetryAllFailed did not emit backup:queue-changed")
	}
}

// TestProviderHealthDerivation covers the three health states: failed-recent
// (LastError set), success-recent (LastSuccessAt set), and no-jobs (both nil).
func TestProviderHealthDerivation(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()

	failing := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true}
	healthy := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true}
	quiet := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true}
	for _, p := range []*domain.BackupProvider{failing, healthy, quiet} {
		if err := gdb.Create(p).Error; err != nil {
			t.Fatalf("create provider: %v", err)
		}
	}

	// failing: a currently-failed job.
	if err := gdb.Create(&domain.BackupJob{AssetID: "a", Plugin: "localfs", Destination: failing.ID, Status: domain.JobStatusFailed, ErrorMessage: "quota exceeded"}).Error; err != nil {
		t.Fatalf("create failed job: %v", err)
	}
	// healthy: a completed job.
	done := time.Now().Add(-time.Minute)
	if err := gdb.Create(&domain.BackupJob{AssetID: "b", Plugin: "localfs", Destination: healthy.ID, Status: domain.JobStatusCompleted, CompletedAt: &done}).Error; err != nil {
		t.Fatalf("create completed job: %v", err)
	}
	// quiet: no jobs.

	svc := NewProviderService(gdb, backup.NewRegistry(), nil)
	dtos, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]ProviderDTO{}
	for _, d := range dtos {
		byID[d.ID] = d
	}

	if fd := byID[failing.ID]; fd.LastError == nil || fd.LastError.Message != "quota exceeded" {
		t.Fatalf("failing provider LastError = %+v, want message 'quota exceeded'", fd.LastError)
	} else if fd.LastError.At.IsZero() {
		t.Fatal("failing provider LastError.At is zero")
	} else if fd.LastSuccessAt != nil {
		t.Fatalf("failing provider LastSuccessAt = %v, want nil", fd.LastSuccessAt)
	}

	if hd := byID[healthy.ID]; hd.LastSuccessAt == nil {
		t.Fatal("healthy provider LastSuccessAt = nil, want set")
	} else if hd.LastError != nil {
		t.Fatalf("healthy provider LastError = %+v, want nil", hd.LastError)
	}

	if qd := byID[quiet.ID]; qd.LastError != nil || qd.LastSuccessAt != nil {
		t.Fatalf("quiet provider health = (%+v, %v), want nil/nil", qd.LastError, qd.LastSuccessAt)
	}
}

// TestEnrichMountedEvent covers the pure enrichment mapping used to build the
// volume:mounted payload: removable/ejectable/name pass through, a nil describe
// yields a bare payload, and the library's own volume is flagged.
func TestEnrichMountedEvent(t *testing.T) {
	// Removable ejectable card, not the library volume.
	info := &volumes.Info{VolumeName: "EOS_DIGITAL", Removable: true, Ejectable: true}
	ev := enrichMountedEvent("/Volumes/EOS_DIGITAL", info, "/Volumes/Master")
	if ev.MountPoint != "/Volumes/EOS_DIGITAL" || ev.VolumeName != "EOS_DIGITAL" {
		t.Fatalf("unexpected mount/name: %+v", ev)
	}
	if !ev.Removable || !ev.Ejectable {
		t.Fatalf("removable/ejectable = %v/%v, want true/true", ev.Removable, ev.Ejectable)
	}
	if ev.IsLibraryVolume {
		t.Fatal("IsLibraryVolume = true, want false (different volume)")
	}

	// nil info -> bare payload (no toast will fire on the frontend).
	bare := enrichMountedEvent("/Volumes/Unknown", nil, "")
	if bare.Removable || bare.Ejectable || bare.VolumeName != "" || bare.IsLibraryVolume {
		t.Fatalf("bare payload not bare: %+v", bare)
	}

	// Library lives on this volume (root under the mount) -> flagged.
	lib := enrichMountedEvent("/Volumes/Master", &volumes.Info{VolumeName: "Master", Removable: true}, "/Volumes/Master/Photos")
	if !lib.IsLibraryVolume {
		t.Fatal("IsLibraryVolume = false, want true (library root under mount)")
	}

	// Root equals the mount point -> flagged.
	eq := enrichMountedEvent("/Volumes/Master", nil, "/Volumes/Master")
	if !eq.IsLibraryVolume {
		t.Fatal("IsLibraryVolume = false, want true (root == mount)")
	}
}
