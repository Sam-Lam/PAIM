package services

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// TestDashboardBackupSummaryExcludesMirror confirms that mirror-provider jobs are
// kept out of the headline pending/failed backup numbers and surfaced as a
// separate soft count.
func TestDashboardBackupSummaryExcludesMirror(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "dash.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()

	real := &domain.BackupProvider{PluginName: "localfs", ConfigJSON: "{}", Enabled: true}
	mirror := &domain.BackupProvider{PluginName: "rclone", ConfigJSON: "{}", Enabled: true, Mirror: true}
	if err := gdb.Create(real).Error; err != nil {
		t.Fatalf("create real provider: %v", err)
	}
	if err := gdb.Create(mirror).Error; err != nil {
		t.Fatalf("create mirror provider: %v", err)
	}

	mk := func(dest string, st domain.JobStatus) {
		j := &domain.BackupJob{AssetID: "a", Plugin: "p", Destination: dest, Status: st}
		if err := gdb.Create(j).Error; err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	// Headline: 2 pending, 1 failed on the required provider.
	mk(real.ID, domain.JobStatusPending)
	mk(real.ID, domain.JobStatusPending)
	mk(real.ID, domain.JobStatusFailed)
	// Mirror: 3 pending, 2 failed — must NOT touch the headline.
	mk(mirror.ID, domain.JobStatusPending)
	mk(mirror.ID, domain.JobStatusPending)
	mk(mirror.ID, domain.JobStatusPending)
	mk(mirror.ID, domain.JobStatusFailed)
	mk(mirror.ID, domain.JobStatusFailed)

	svc := NewDashboardService(gdb, repo.NewAssetRepo(gdb), repo.NewBackupRepo(gdb), repo.NewSourceRepo(gdb), nil)
	got, err := svc.backupSummary(ctx)
	if err != nil {
		t.Fatalf("backupSummary: %v", err)
	}
	if got.Pending != 2 || got.Failed != 1 {
		t.Fatalf("headline pending/failed = %d/%d, want 2/1", got.Pending, got.Failed)
	}
	if got.MirrorPending != 3 || got.MirrorFailed != 2 {
		t.Fatalf("mirror pending/failed = %d/%d, want 3/2", got.MirrorPending, got.MirrorFailed)
	}
}
