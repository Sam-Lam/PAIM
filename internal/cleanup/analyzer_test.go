package cleanup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
)

// fakeLookup returns pre-seeded candidates by quick hash.
type fakeLookup struct {
	byQuick map[string][]domain.Asset
}

func (f *fakeLookup) FindByQuickHash(_ context.Context, qh string) ([]domain.Asset, error) {
	return f.byQuick[qh], nil
}
func (f *fakeLookup) FindByFullHash(_ context.Context, fh string) ([]domain.Asset, error) {
	return nil, nil
}

// hashModel drives the injected hashing functions by file path.
type hashModel struct {
	quick map[string]string
	full  map[string]string
	qErr  map[string]bool
	fErr  map[string]bool
}

func (m *hashModel) quickFn(path string) (string, error) {
	if m.qErr[path] {
		return "", fmt.Errorf("simulated unreadable: %s", path)
	}
	return m.quick[path], nil
}
func (m *hashModel) fullFn(_ context.Context, path string) (string, error) {
	if m.fErr[path] {
		return "", fmt.Errorf("simulated read error mid-hash: %s", path)
	}
	return m.full[path], nil
}

func newHashModel() *hashModel {
	return &hashModel{
		quick: map[string]string{},
		full:  map[string]string{},
		qErr:  map[string]bool{},
		fErr:  map[string]bool{},
	}
}

// mkFile creates a real file (content is irrelevant; hashing is injected).
func mkFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func archivedAsset(id, quick, full string, verified bool, backup domain.BackupStatus, archivePath string) domain.Asset {
	vs := domain.VerificationStatusVerified
	if !verified {
		vs = domain.VerificationStatusPending
	}
	return domain.Asset{
		UUIDModel:          domain.UUIDModel{ID: id},
		QuickHash:          quick,
		FullHash:           full,
		VerificationStatus: vs,
		BackupStatus:       backup,
		CurrentArchivePath: archivePath,
	}
}

func TestAnalyze_AllArchivedAndBacked_Safe(t *testing.T) {
	scan := t.TempDir()
	arch := t.TempDir()
	archFile := mkFile(t, arch, "a.jpg")
	f1 := mkFile(t, scan, "a.jpg")

	hm := newHashModel()
	hm.quick[f1] = "Q1"
	hm.full[f1] = "F1"

	asset := archivedAsset("asset-1", "Q1", "F1", true, domain.BackupStatusComplete, archFile)
	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{"Q1": {asset}}}

	an := NewAnalyzer(lookup, hm.quickFn, hm.fullFn, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.Count(ClassAlreadyArchived) != 1 || report.MediaFiles != 1 {
		t.Fatalf("expected 1 already_archived media file, got %+v", report.Classes)
	}
	rec := report.Recommendation()
	if !rec.SafeToDelete {
		t.Fatalf("expected safe to delete, reasons=%v", rec.Reasons)
	}
	if rec.Title != "Already Archived" {
		t.Fatalf("title = %q", rec.Title)
	}
	t.Logf("summary: %s", rec.Summary)
}

func TestAnalyze_OneNewFile_Unsafe(t *testing.T) {
	scan := t.TempDir()
	f1 := mkFile(t, scan, "new.jpg")

	hm := newHashModel()
	hm.quick[f1] = "QNEW"
	// No archive candidate for QNEW.

	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{}}
	an := NewAnalyzer(lookup, hm.quickFn, hm.fullFn, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.Count(ClassNew) != 1 {
		t.Fatalf("expected 1 new, got %d", report.Count(ClassNew))
	}
	rec := report.Recommendation()
	if rec.SafeToDelete {
		t.Fatalf("expected unsafe")
	}
	if !containsSubstr(rec.Reasons, "not yet archived") {
		t.Fatalf("expected 'not yet archived' reason, got %v", rec.Reasons)
	}
}

func TestAnalyze_ArchivedButBackupPending_Unsafe(t *testing.T) {
	scan := t.TempDir()
	arch := t.TempDir()
	archFile := mkFile(t, arch, "b.jpg")
	f1 := mkFile(t, scan, "b.jpg")

	hm := newHashModel()
	hm.quick[f1] = "Q1"
	hm.full[f1] = "F1"

	asset := archivedAsset("asset-1", "Q1", "F1", true, domain.BackupStatusPending, archFile)
	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{"Q1": {asset}}}

	an := NewAnalyzer(lookup, hm.quickFn, hm.fullFn, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.Count(ClassAlreadyArchived) != 1 {
		t.Fatalf("expected already_archived, got %+v", report.Classes)
	}
	if report.BackupIncomplete != 1 {
		t.Fatalf("expected BackupIncomplete=1, got %d", report.BackupIncomplete)
	}
	rec := report.Recommendation()
	if rec.SafeToDelete {
		t.Fatalf("expected unsafe due to incomplete backup")
	}
	if !containsSubstr(rec.Reasons, "incomplete backups") {
		t.Fatalf("expected 'incomplete backups' reason, got %v", rec.Reasons)
	}
}

func TestAnalyze_UnreadableFile_UnknownAndUnsafe(t *testing.T) {
	scan := t.TempDir()
	f1 := mkFile(t, scan, "broken.jpg")

	hm := newHashModel()
	hm.qErr[f1] = true // cannot be read at all

	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{}}
	an := NewAnalyzer(lookup, hm.quickFn, hm.fullFn, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.Count(ClassUnknown) != 1 || report.UnreadableMedia != 1 {
		t.Fatalf("expected unknown+unreadableMedia, got unknown=%d unreadable=%d",
			report.Count(ClassUnknown), report.UnreadableMedia)
	}
	rec := report.Recommendation()
	if rec.SafeToDelete {
		t.Fatalf("expected unsafe for unreadable media file")
	}
	if !containsSubstr(rec.Reasons, "could not be read") {
		t.Fatalf("expected 'could not be read' reason, got %v", rec.Reasons)
	}
}

func TestAnalyze_VerificationFailedMatch(t *testing.T) {
	scan := t.TempDir()
	arch := t.TempDir()
	archFile := mkFile(t, arch, "v.jpg")
	f1 := mkFile(t, scan, "v.jpg")

	hm := newHashModel()
	hm.quick[f1] = "Q1"
	hm.full[f1] = "F1"

	asset := archivedAsset("asset-1", "Q1", "F1", false, domain.BackupStatusComplete, archFile)
	asset.VerificationStatus = domain.VerificationStatusFailed
	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{"Q1": {asset}}}

	an := NewAnalyzer(lookup, hm.quickFn, hm.fullFn, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.Count(ClassVerificationFailed) != 1 {
		t.Fatalf("expected verification_failed, got %+v", report.Classes)
	}
	if report.Recommendation().SafeToDelete {
		t.Fatalf("expected unsafe")
	}
}

func TestAnalyze_InFolderDuplicate(t *testing.T) {
	scan := t.TempDir()
	f1 := mkFile(t, scan, "one.jpg")
	f2 := mkFile(t, scan, "two.jpg") // identical content to f1

	hm := newHashModel()
	// Same content -> same quick and full hash.
	for _, f := range []string{f1, f2} {
		hm.quick[f] = "QD"
		hm.full[f] = "FD"
	}
	// No archive match; second occurrence is the duplicate.
	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{}}
	an := NewAnalyzer(lookup, hm.quickFn, hm.fullFn, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// One is new (first occurrence), one is duplicate.
	if report.Count(ClassNew) != 1 || report.Count(ClassDuplicate) != 1 {
		t.Fatalf("expected 1 new + 1 duplicate, got new=%d dup=%d",
			report.Count(ClassNew), report.Count(ClassDuplicate))
	}
}

func TestAnalyze_EmptyFolder(t *testing.T) {
	scan := t.TempDir()
	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{}}
	an := NewAnalyzer(lookup, nil, nil, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.MediaFiles != 0 || report.TotalFiles != 0 {
		t.Fatalf("expected empty report, got %+v", report)
	}
	rec := report.Recommendation()
	if rec.SafeToDelete {
		t.Fatalf("empty folder should not be safe to delete")
	}
	if !containsSubstr(rec.Reasons, "no media files") {
		t.Fatalf("expected 'no media files' reason, got %v", rec.Reasons)
	}
}

func TestAnalyze_NonMediaOnlyFolder(t *testing.T) {
	scan := t.TempDir()
	mkFile(t, scan, "notes.txt")
	mkFile(t, scan, "readme.md")

	lookup := &fakeLookup{byQuick: map[string][]domain.Asset{}}
	an := NewAnalyzer(lookup, nil, nil, nil)
	report, err := an.Analyze(context.Background(), scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.MediaFiles != 0 || report.NonMedia != 2 || report.Count(ClassUnknown) != 2 {
		t.Fatalf("expected 2 non-media unknown files, got %+v", report)
	}
	if report.Recommendation().SafeToDelete {
		t.Fatalf("non-media-only folder should not be safe to delete")
	}
}

func TestAnalyze_ReadOnly_NoDBMutations(t *testing.T) {
	gdb, err := db.Open(filepath.Join(t.TempDir(), "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	ctx := context.Background()

	// Seed an archived asset whose FullHash is intentionally empty (lazy) with a
	// real archive file; the scanned copy has identical content so real hashing
	// confirms the match by reading the archive file (never backfilling the DB).
	arch := t.TempDir()
	archFile := filepath.Join(arch, "photo.jpg")
	content := []byte("some real photo bytes for hashing")
	if err := os.WriteFile(archFile, content, 0o644); err != nil {
		t.Fatalf("write archive file: %v", err)
	}
	// Compute the real quick hash so the scanned copy matches.
	quick, err := realQuick(archFile)
	if err != nil {
		t.Fatalf("quick hash: %v", err)
	}
	asset := &domain.Asset{
		OriginalFilename:   "photo.jpg",
		OriginalExtension:  "jpg",
		QuickHash:          quick,
		FullHash:           "", // lazy, must remain empty (read-only)
		FileSize:           int64(len(content)),
		MediaType:          domain.MediaTypePhoto,
		CurrentArchivePath: archFile,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
	}
	if err := assets.Create(ctx, asset); err != nil {
		t.Fatalf("create asset: %v", err)
	}

	scan := t.TempDir()
	if err := os.WriteFile(filepath.Join(scan, "copy.jpg"), content, 0o644); err != nil {
		t.Fatalf("write scan copy: %v", err)
	}
	mkFile(t, scan, "unrelated.txt")

	before := countRows(t, gdb)

	// Use REAL hashing (nil funcs) so the match is genuine.
	an := NewAnalyzer(assets, nil, nil, nil)
	report, err := an.Analyze(ctx, scan, nil)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if report.Count(ClassAlreadyArchived) != 1 {
		t.Fatalf("expected the copy to be already_archived, got %+v", report.Classes)
	}

	after := countRows(t, gdb)
	if before != after {
		t.Fatalf("row counts changed: before=%d after=%d (analysis must be read-only)", before, after)
	}
	// The asset's FullHash must NOT have been backfilled.
	reloaded, err := assets.GetByID(ctx, asset.ID)
	if err != nil {
		t.Fatalf("reload asset: %v", err)
	}
	if reloaded.FullHash != "" {
		t.Fatalf("analysis backfilled FullHash %q; it must stay read-only", reloaded.FullHash)
	}
}

func TestClassStat_TruncationCap(t *testing.T) {
	s := &ClassStat{}
	for i := 0; i < maxFilesPerClass+5; i++ {
		s.add(fmt.Sprintf("/f/%d.jpg", i), 10, maxFilesPerClass, discardLogger(), ClassNew)
	}
	if s.Count != maxFilesPerClass+5 {
		t.Fatalf("count = %d, want %d", s.Count, maxFilesPerClass+5)
	}
	if len(s.Files) != maxFilesPerClass {
		t.Fatalf("files len = %d, want %d (capped)", len(s.Files), maxFilesPerClass)
	}
	if !s.Truncated {
		t.Fatalf("expected Truncated=true")
	}
	if s.Bytes != int64(10*(maxFilesPerClass+5)) {
		t.Fatalf("bytes should keep accumulating past the cap, got %d", s.Bytes)
	}
}
