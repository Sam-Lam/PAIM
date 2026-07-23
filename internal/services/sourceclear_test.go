package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/hashing"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// archiveFile writes content to path and inserts a matching archived asset
// (quick hash computed the same way the real evaluation will), so the file
// classifies as safe (verified + backed up) unless flags say otherwise.
func archiveFile(t *testing.T, assets *repo.AssetRepo, sessionID, path, content string, verified, backedUp bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	quick, err := hashing.QuickHash(path)
	if err != nil {
		t.Fatalf("quick hash: %v", err)
	}
	info, _ := os.Stat(path)
	vs := domain.VerificationStatusVerified
	if !verified {
		vs = domain.VerificationStatusPending
	}
	bs := domain.BackupStatusComplete
	if !backedUp {
		bs = domain.BackupStatusPending
	}
	a := &domain.Asset{
		OriginalFilename:   filepath.Base(path),
		OriginalFullPath:   path,
		SessionID:          sessionID,
		QuickHash:          quick,
		FileSize:           info.Size(),
		ImportDate:         time.Now(),
		CurrentArchivePath: "archive/" + filepath.Base(path),
		VerificationStatus: vs,
		BackupStatus:       bs,
	}
	if err := assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
}

// evaluateGreen runs a safe-to-erase evaluation over root and waits for it to
// complete, returning the completed snapshot.
func evaluateGreen(t *testing.T, svc *SourcesService, root string) ActiveSafeToEraseDTO {
	t.Helper()
	if _, err := svc.StartSafeToErase(context.Background(), "", root); err != nil {
		t.Fatalf("StartSafeToErase: %v", err)
	}
	return waitSafeErase(t, svc, "completed")
}

func waitClear(t *testing.T, svc *SourcesService, state string) ActiveClearSourceDTO {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dto, err := svc.ActiveClearSource(context.Background())
		if err != nil {
			t.Fatalf("ActiveClearSource: %v", err)
		}
		if dto.State == state {
			return dto
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for clear state %q", state)
	return ActiveClearSourceDTO{}
}

// TestClearSourceGateRequiresFreshGreenEval covers the gate: no eval, red eval,
// and stale eval are all refused; a fresh green eval is required to run.
func TestClearSourceGateRequiresFreshGreenEval(t *testing.T) {
	svc, _ := newSourcesService(t)
	assets := svc.assets

	// No evaluation yet → refused.
	root := t.TempDir()
	archiveFile(t, assets, "s1", filepath.Join(root, "a.jpg"), "one", true, true)
	if _, err := svc.StartClearSource(context.Background(), root); err != ErrClearGateNotMet {
		t.Fatalf("no-eval start err = %v, want ErrClearGateNotMet", err)
	}

	// Red evaluation (a file with no matching asset) → refused.
	redRoot := t.TempDir()
	archiveFile(t, assets, "s1", filepath.Join(redRoot, "a.jpg"), "red-one", true, true)
	if err := os.WriteFile(filepath.Join(redRoot, "orphan.jpg"), []byte("no-asset"), 0o644); err != nil {
		t.Fatal(err)
	}
	red := evaluateGreen(t, svc, redRoot)
	if red.Report.Safe {
		t.Fatal("expected red report")
	}
	if _, err := svc.StartClearSource(context.Background(), redRoot); err != ErrClearGateNotMet {
		t.Fatalf("red-eval start err = %v, want ErrClearGateNotMet", err)
	}

	// Green evaluation, but for a DIFFERENT root than we try to clear → refused.
	greenRoot := t.TempDir()
	archiveFile(t, assets, "s1", filepath.Join(greenRoot, "a.jpg"), "green-one", true, true)
	evaluateGreen(t, svc, greenRoot)
	if _, err := svc.StartClearSource(context.Background(), root); err != ErrClearGateNotMet {
		t.Fatalf("wrong-root start err = %v, want ErrClearGateNotMet", err)
	}

	// Same green root, but stale (age the cached report past the TTL) → refused.
	svc.mu.Lock()
	svc.run.at = time.Now().Add(-safeEraseReportTTL - time.Minute)
	svc.mu.Unlock()
	if _, err := svc.StartClearSource(context.Background(), greenRoot); err != ErrClearGateNotMet {
		t.Fatalf("stale-eval start err = %v, want ErrClearGateNotMet", err)
	}

	// Fresh green for the right root → runs.
	fresh := evaluateGreen(t, svc, greenRoot)
	if !fresh.Report.Safe {
		t.Fatal("expected green report")
	}
	res, err := svc.StartClearSource(context.Background(), greenRoot)
	if err != nil {
		t.Fatalf("green start err = %v, want nil", err)
	}
	if res.Files != 1 {
		t.Fatalf("Files = %d, want 1", res.Files)
	}
	waitClear(t, svc, "completed")
}

// TestClearSourceMovesExactlySafeSet builds a mixed nested tree of archived
// media plus non-media, and verifies a clear moves exactly the media into the
// timestamped trash preserving relative paths, leaving non-media untouched.
func TestClearSourceMovesExactlySafeSet(t *testing.T) {
	svc, _ := newSourcesService(t)
	assets := svc.assets
	root := t.TempDir()

	// Two media with the SAME basename in different subfolders (would collide in a
	// flat trash — the relative-path layout must keep them apart).
	m1 := filepath.Join(root, "DCIM", "100", "IMG_0001.JPG")
	m2 := filepath.Join(root, "DCIM", "101", "IMG_0001.JPG")
	m3 := filepath.Join(root, "clip.mov")
	archiveFile(t, assets, "s1", m1, "one", true, true)
	archiveFile(t, assets, "s1", m2, "two", true, true)
	archiveFile(t, assets, "s1", m3, "three", true, true)

	// Non-media: never touched.
	notes := filepath.Join(root, "DCIM", "notes.txt")
	if err := os.WriteFile(notes, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	green := evaluateGreen(t, svc, root)
	if !green.Report.Safe {
		t.Fatalf("expected green, got: %s", green.Report.Reason)
	}

	res, err := svc.StartClearSource(context.Background(), root)
	if err != nil {
		t.Fatalf("StartClearSource: %v", err)
	}
	done := waitClear(t, svc, "completed")
	if done.Result.Moved != 3 || done.Result.Errors != 0 {
		t.Fatalf("result = %+v, want moved 3 / errors 0", done.Result)
	}
	trashDir := res.TrashDir

	// Media moved out of their original locations...
	for _, p := range []string{m1, m2, m3} {
		if pathExists(p) {
			t.Errorf("media file %q should have been moved", p)
		}
	}
	// ...and reappear under the timestamped trash at their relative paths.
	for _, rel := range []string{
		filepath.Join("DCIM", "100", "IMG_0001.JPG"),
		filepath.Join("DCIM", "101", "IMG_0001.JPG"),
		"clip.mov",
	} {
		if !pathExists(filepath.Join(trashDir, rel)) {
			t.Errorf("expected %q under trash dir", rel)
		}
	}
	// Non-media untouched.
	if !pathExists(notes) {
		t.Error("non-media notes.txt should be untouched")
	}
}

// TestClearSourceRefusesLibraryRoot verifies a root overlapping the open library
// root is refused outright.
func TestClearSourceRefusesLibraryRoot(t *testing.T) {
	svc, _ := newSourcesService(t)
	lib := t.TempDir()
	svc.root = lib // simulate an open library at this root

	// The library root itself.
	if _, err := svc.StartClearSource(context.Background(), lib); err != ErrClearInsideLibrary {
		t.Fatalf("clear library root err = %v, want ErrClearInsideLibrary", err)
	}
	// A subdirectory inside the library.
	sub := filepath.Join(lib, "2026")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartClearSource(context.Background(), sub); err != ErrClearInsideLibrary {
		t.Fatalf("clear library subdir err = %v, want ErrClearInsideLibrary", err)
	}
}

// TestClearSourceCancellationIsSafe verifies that cancelling a clear never loses
// data: every safe file ends up in exactly one place (source or trash), and the
// reported Moved count matches what actually landed in trash.
func TestClearSourceCancellationIsSafe(t *testing.T) {
	svc, _ := newSourcesService(t)
	assets := svc.assets
	root := t.TempDir()

	var files []string
	for i := 0; i < 300; i++ {
		p := filepath.Join(root, "sub"+strconvI(i%10), "f"+strconvI(i)+".jpg")
		archiveFile(t, assets, "s1", p, "content-"+strconvI(i), true, true)
		files = append(files, p)
	}

	green := evaluateGreen(t, svc, root)
	if !green.Report.Safe {
		t.Fatalf("expected green: %s", green.Report.Reason)
	}

	res, err := svc.StartClearSource(context.Background(), root)
	if err != nil {
		t.Fatalf("StartClearSource: %v", err)
	}
	// Cancel as soon as the job reports any progress.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dto, _ := svc.ActiveClearSource(context.Background())
		if dto.State == "completed" {
			break
		}
		if dto.State == "running" && dto.Progress != nil && dto.Progress.FilesDone >= 1 {
			_ = svc.CancelClearSource(context.Background())
			break
		}
		time.Sleep(time.Millisecond)
	}
	done := waitClear(t, svc, "completed")

	// Safety invariant: every file exists at exactly one of {source, trash}.
	inTrash := 0
	for i, src := range files {
		rel := filepath.Join("sub"+strconvI(i%10), filepath.Base(src))
		trashPath := filepath.Join(res.TrashDir, rel)
		atSource := pathExists(src)
		atTrash := pathExists(trashPath)
		if atSource == atTrash {
			t.Fatalf("file %q: atSource=%v atTrash=%v (must be exactly one)", src, atSource, atTrash)
		}
		if atTrash {
			inTrash++
		}
	}
	if done.Result.Moved != inTrash {
		t.Fatalf("result.Moved = %d, but %d files are in trash", done.Result.Moved, inTrash)
	}
}

// TestSessionBackupStatus covers the per-session backup-status query.
func TestSessionBackupStatus(t *testing.T) {
	svc, _ := newSourcesService(t)
	assets := svc.assets
	root := t.TempDir()

	// Session s1: 2 backed up, 1 pending backup (all with an archive copy).
	archiveFile(t, assets, "s1", filepath.Join(root, "a.jpg"), "a", true, true)
	archiveFile(t, assets, "s1", filepath.Join(root, "b.jpg"), "b", true, true)
	archiveFile(t, assets, "s1", filepath.Join(root, "c.jpg"), "c", true, false)

	bs := NewBackupService(nil, nil, assets, nil, nil, nil, nil)
	status, err := bs.SessionBackupStatus(context.Background(), "s1")
	if err != nil {
		t.Fatalf("SessionBackupStatus: %v", err)
	}
	if status.TotalAssets != 3 || status.BackedUp != 2 {
		t.Fatalf("status = %+v, want total 3 / backedUp 2", status)
	}
	if status.Complete {
		t.Fatal("Complete should be false while a backup is pending")
	}

	// Session s2: all backed up → Complete true.
	archiveFile(t, assets, "s2", filepath.Join(root, "d.jpg"), "d", true, true)
	archiveFile(t, assets, "s2", filepath.Join(root, "e.jpg"), "e", true, true)
	status2, err := bs.SessionBackupStatus(context.Background(), "s2")
	if err != nil {
		t.Fatalf("SessionBackupStatus s2: %v", err)
	}
	if status2.TotalAssets != 2 || status2.BackedUp != 2 || !status2.Complete {
		t.Fatalf("s2 status = %+v, want total 2 / backedUp 2 / complete true", status2)
	}
}
