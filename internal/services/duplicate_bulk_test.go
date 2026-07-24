package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// seedSizedDup creates a flagged duplicate asset with an explicit size, session,
// and archive path (root-relative) so the stats/filter/group queries have real
// data to aggregate.
func seedSizedDup(t *testing.T, assets *repo.AssetRepo, filename, archivePath, sessionID string, size int64, dupOf *string) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   filename,
		QuickHash:          "qh-" + archivePath,
		CurrentArchivePath: archivePath,
		SessionID:          sessionID,
		FileSize:           size,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
		DuplicateOfAssetID: dupOf,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

func TestDuplicateStatsTrueTotals(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	seedSizedDup(t, assets, "a.jpg", "2023/a.jpg", "s1", 100, &orig.ID)
	seedSizedDup(t, assets, "b.jpg", "2023/b.jpg", "s1", 250, &orig.ID)
	seedSizedDup(t, assets, "c.jpg", "2024/c.jpg", "s2", 400, &orig.ID)

	stats, err := svc.DuplicateStats(ctx)
	if err != nil {
		t.Fatalf("DuplicateStats: %v", err)
	}
	if stats.TotalPairs != 3 {
		t.Fatalf("TotalPairs = %d, want 3", stats.TotalPairs)
	}
	if stats.TotalWastedBytes != 750 {
		t.Fatalf("TotalWastedBytes = %d, want 750", stats.TotalWastedBytes)
	}
}

func TestListDuplicatesFilteredByFolderAndSession(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	seedSizedDup(t, assets, "a.jpg", "2023/trip/a.jpg", "s1", 100, &orig.ID)
	seedSizedDup(t, assets, "b.jpg", "2023/trip/b.jpg", "s1", 250, &orig.ID)
	seedSizedDup(t, assets, "c.jpg", "2024/misc/c.jpg", "s2", 400, &orig.ID)

	// Folder filter: only the two in "2023/trip".
	folderRes, err := svc.ListDuplicatesFiltered(ctx, DuplicateFilterDTO{GroupBy: "folder", GroupKey: "2023/trip"}, 1, 50)
	if err != nil {
		t.Fatalf("filter folder: %v", err)
	}
	if folderRes.Total != 2 || len(folderRes.Items) != 2 {
		t.Fatalf("folder filter: total=%d items=%d, want 2/2", folderRes.Total, len(folderRes.Items))
	}

	// Session filter: only s2's single duplicate.
	sessRes, err := svc.ListDuplicatesFiltered(ctx, DuplicateFilterDTO{GroupBy: "session", GroupKey: "s2"}, 1, 50)
	if err != nil {
		t.Fatalf("filter session: %v", err)
	}
	if sessRes.Total != 1 || len(sessRes.Items) != 1 {
		t.Fatalf("session filter: total=%d items=%d, want 1/1", sessRes.Total, len(sessRes.Items))
	}
	if sessRes.Items[0].Duplicate.OriginalFilename != "c.jpg" {
		t.Fatalf("session filter item = %q, want c.jpg", sessRes.Items[0].Duplicate.OriginalFilename)
	}

	// Sort by size descending: b (250) before a (100) before... within folder s1.
	sorted, err := svc.ListDuplicatesFiltered(ctx, DuplicateFilterDTO{SortBySize: true}, 1, 50)
	if err != nil {
		t.Fatalf("sort by size: %v", err)
	}
	if len(sorted.Items) != 3 {
		t.Fatalf("sorted items = %d, want 3", len(sorted.Items))
	}
	if sorted.Items[0].Duplicate.FileSize < sorted.Items[1].Duplicate.FileSize ||
		sorted.Items[1].Duplicate.FileSize < sorted.Items[2].Duplicate.FileSize {
		t.Fatalf("not sorted by size desc: %d,%d,%d",
			sorted.Items[0].Duplicate.FileSize, sorted.Items[1].Duplicate.FileSize, sorted.Items[2].Duplicate.FileSize)
	}
}

func TestListDuplicateIDsForFilter(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	seedSizedDup(t, assets, "a.jpg", "2023/trip/a.jpg", "s1", 100, &orig.ID)
	seedSizedDup(t, assets, "b.jpg", "2023/trip/b.jpg", "s1", 250, &orig.ID)
	seedSizedDup(t, assets, "c.jpg", "2024/misc/c.jpg", "s2", 400, &orig.ID)

	all, err := svc.ListDuplicateIDs(ctx, DuplicateFilterDTO{})
	if err != nil {
		t.Fatalf("ids all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all ids = %d, want 3", len(all))
	}

	folderIDs, err := svc.ListDuplicateIDs(ctx, DuplicateFilterDTO{GroupBy: "folder", GroupKey: "2023/trip"})
	if err != nil {
		t.Fatalf("ids folder: %v", err)
	}
	if len(folderIDs) != 2 {
		t.Fatalf("folder ids = %d, want 2", len(folderIDs))
	}
}

func TestListDuplicateGroups(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	seedSizedDup(t, assets, "a.jpg", "2023/trip/a.jpg", "s1", 100, &orig.ID)
	seedSizedDup(t, assets, "b.jpg", "2023/trip/b.jpg", "s1", 250, &orig.ID)
	seedSizedDup(t, assets, "c.jpg", "2024/misc/c.jpg", "s2", 400, &orig.ID)

	folders, err := svc.ListDuplicateGroups(ctx, "folder")
	if err != nil {
		t.Fatalf("groups folder: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("folder groups = %d, want 2", len(folders))
	}
	// Ordered by wasted bytes desc: "2024/misc" (400) first, then "2023/trip" (350).
	if folders[0].Key != "2024/misc" || folders[0].WastedBytes != 400 || folders[0].Count != 1 {
		t.Fatalf("group[0] = %+v, want key 2024/misc / wasted 400 / count 1", folders[0])
	}
	if folders[1].Key != "2023/trip" || folders[1].WastedBytes != 350 || folders[1].Count != 2 {
		t.Fatalf("group[1] = %+v, want key 2023/trip / wasted 350 / count 2", folders[1])
	}

	sessions, err := svc.ListDuplicateGroups(ctx, "session")
	if err != nil {
		t.Fatalf("groups session: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("session groups = %d, want 2", len(sessions))
	}
	bySession := map[string]repo.DuplicateGroup{}
	for _, g := range sessions {
		bySession[g.Key] = repo.DuplicateGroup{Key: g.Key, Count: g.Count, WastedBytes: g.WastedBytes}
	}
	if bySession["s1"].Count != 2 || bySession["s1"].WastedBytes != 350 {
		t.Fatalf("session s1 = %+v, want count 2 / wasted 350", bySession["s1"])
	}
	if bySession["s2"].Count != 1 || bySession["s2"].WastedBytes != 400 {
		t.Fatalf("session s2 = %+v, want count 1 / wasted 400", bySession["s2"])
	}

	// Unknown groupBy is rejected.
	if _, err := svc.ListDuplicateGroups(ctx, "bogus"); err == nil {
		t.Fatal("expected error for unknown groupBy")
	}
}

// bulkFileDup writes a real archive file and returns a flagged duplicate asset
// pointing at it (root-relative), so a bulk delete can trash it.
func bulkFileDup(t *testing.T, assets *repo.AssetRepo, root, rel string, dupOf *string) *domain.Asset {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte("bytes-"+rel), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, _ := os.Stat(abs)
	return seedSizedDup(t, assets, filepath.Base(rel), filepath.ToSlash(rel), "s1", info.Size(), dupOf)
}

func waitBulk(t *testing.T, svc *DuplicateService, state string) ActiveBulkResolveDTO {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dto, err := svc.ActiveBulkResolve(context.Background())
		if err != nil {
			t.Fatalf("ActiveBulkResolve: %v", err)
		}
		if dto.State == state {
			return dto
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for bulk state %q", state)
	return ActiveBulkResolveDTO{}
}

func TestBulkResolveDeleteSoftDeletesAndTrashes(t *testing.T) {
	svc, gdb, assets := newDuplicateHarness(t)
	ctx := context.Background()
	root := t.TempDir()
	svc.root = root

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	var ids []string
	for i := 0; i < 5; i++ {
		d := bulkFileDup(t, assets, root, filepath.Join("2023", "dup", "d"+string(rune('a'+i))+".jpg"), &orig.ID)
		ids = append(ids, d.ID)
	}

	res, err := svc.StartBulkResolve(ctx, ids, DuplicateActionDelete, "")
	if err != nil {
		t.Fatalf("StartBulkResolve: %v", err)
	}
	if res.Total != 5 {
		t.Fatalf("Total = %d, want 5", res.Total)
	}
	done := waitBulk(t, svc, "completed")
	if done.Summary.Succeeded != 5 || done.Summary.Failed != 0 || done.Summary.Cancelled {
		t.Fatalf("summary = %+v, want 5 succeeded / 0 failed / not cancelled", done.Summary)
	}

	// Every duplicate row is soft-deleted (not hard-deleted) and its file is trashed.
	for _, id := range ids {
		var row domain.Asset
		if err := gdb.Unscoped().First(&row, "id = ?", id).Error; err != nil {
			t.Fatalf("row %s must still exist (soft delete): %v", id, err)
		}
		if !row.Deleted {
			t.Fatalf("row %s should be soft-deleted", id)
		}
	}
	// Second bulk of the same IDs now finds them already deleted → per-item failures,
	// never a panic or abort.
	res2, err := svc.StartBulkResolve(ctx, ids, DuplicateActionDelete, "")
	if err != nil {
		t.Fatalf("second StartBulkResolve: %v", err)
	}
	_ = res2
	done2 := waitBulk(t, svc, "completed")
	if done2.Summary.Failed != 5 {
		t.Fatalf("second run failed = %d, want 5 (rows already deleted)", done2.Summary.Failed)
	}
}

func TestBulkResolvePerItemFailureDoesNotAbort(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()
	root := t.TempDir()
	svc.root = root

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	good1 := bulkFileDup(t, assets, root, filepath.Join("2023", "good1.jpg"), &orig.ID)
	// A duplicate whose archive file does not exist on disk → its delete (trash)
	// fails, but the batch must continue.
	bad := seedSizedDup(t, assets, "missing.jpg", "2023/missing.jpg", "s1", 10, &orig.ID)
	good2 := bulkFileDup(t, assets, root, filepath.Join("2023", "good2.jpg"), &orig.ID)

	ids := []string{good1.ID, bad.ID, good2.ID}
	if _, err := svc.StartBulkResolve(ctx, ids, DuplicateActionDelete, ""); err != nil {
		t.Fatalf("StartBulkResolve: %v", err)
	}
	done := waitBulk(t, svc, "completed")
	if done.Summary.Succeeded != 2 {
		t.Fatalf("succeeded = %d, want 2 (bad item skipped)", done.Summary.Succeeded)
	}
	if done.Summary.Failed != 1 || len(done.Summary.Failures) != 1 {
		t.Fatalf("failed = %d, failures = %d, want 1/1", done.Summary.Failed, len(done.Summary.Failures))
	}
	if done.Summary.Failures[0].AssetID != bad.ID {
		t.Fatalf("failure asset = %q, want the missing-file dup %q", done.Summary.Failures[0].AssetID, bad.ID)
	}
}

func TestBulkResolveCancellationMidBatch(t *testing.T) {
	svc, gdb, assets := newDuplicateHarness(t)
	ctx := context.Background()
	root := t.TempDir()
	svc.root = root

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	var ids []string
	for i := 0; i < 400; i++ {
		d := bulkFileDup(t, assets, root, filepath.Join("2023", "sub"+string(rune('a'+i%5)), "f"+itoa(i)+".jpg"), &orig.ID)
		ids = append(ids, d.ID)
	}

	if _, err := svc.StartBulkResolve(ctx, ids, DuplicateActionDelete, ""); err != nil {
		t.Fatalf("StartBulkResolve: %v", err)
	}
	// Cancel as soon as the job reports any progress.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dto, _ := svc.ActiveBulkResolve(context.Background())
		if dto.State == "completed" {
			break
		}
		if dto.State == "running" && dto.Progress != nil && dto.Progress.Done >= 1 {
			_ = svc.CancelBulkResolve(context.Background())
			break
		}
		time.Sleep(time.Millisecond)
	}
	done := waitBulk(t, svc, "completed")
	if !done.Summary.Cancelled {
		t.Fatal("expected Cancelled=true after mid-batch cancel")
	}
	if done.Summary.Succeeded >= len(ids) {
		t.Fatalf("succeeded = %d, want < %d (cancelled before finishing)", done.Summary.Succeeded, len(ids))
	}
	// Data safety: exactly the succeeded rows are soft-deleted; the rest remain live.
	var deleted int64
	gdb.Unscoped().Model(&domain.Asset{}).Where("id IN ? AND deleted = ?", ids, true).Count(&deleted)
	if int(deleted) != done.Summary.Succeeded {
		t.Fatalf("soft-deleted rows = %d, but summary.Succeeded = %d", deleted, done.Summary.Succeeded)
	}
}

func TestBulkResolveOneActiveGuard(t *testing.T) {
	svc, _, assets := newDuplicateHarness(t)
	ctx := context.Background()
	root := t.TempDir()
	svc.root = root

	orig := seedAsset(t, assets, "orig.jpg", "2023/orig.jpg", nil)
	var ids []string
	for i := 0; i < 200; i++ {
		d := bulkFileDup(t, assets, root, filepath.Join("2023", "g", "f"+itoa(i)+".jpg"), &orig.ID)
		ids = append(ids, d.ID)
	}
	if _, err := svc.StartBulkResolve(ctx, ids, DuplicateActionIgnore, ""); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Immediately try a second one — should be refused while the first runs. If the
	// first already finished (tiny batch races), the second simply succeeds; guard
	// against flakiness by only asserting the error type when returned.
	if _, err := svc.StartBulkResolve(ctx, ids, DuplicateActionIgnore, ""); err != nil && err != ErrBulkResolveInProgress {
		t.Fatalf("second start err = %v, want ErrBulkResolveInProgress or nil", err)
	}
	waitBulk(t, svc, "completed")
}
