package services

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newFailureHistory builds a HistoryService wired to a real SQLite catalog with
// the import-failure repo bound (as Bind does in production).
func newFailureHistory(t *testing.T) (*HistoryService, *repo.SessionRepo, *repo.ImportFailureRepo) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "hist.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessions := repo.NewSessionRepo(gdb)
	logs := repo.NewLogRepo(gdb)
	failures := repo.NewImportFailureRepo(gdb)
	svc := NewHistoryService(sessions, logs, quietLogger())
	svc.failures = failures
	return svc, sessions, failures
}

func mustSession(t *testing.T, sessions *repo.SessionRepo, notes string) *domain.ImportSession {
	t.Helper()
	s := &domain.ImportSession{Status: domain.SessionStatusCompleted, Notes: notes}
	if err := sessions.Create(context.Background(), s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func TestListSessionFailuresPaging(t *testing.T) {
	svc, sessions, failures := newFailureHistory(t)
	ctx := context.Background()
	s := mustSession(t, sessions, "")

	for i := 0; i < 5; i++ {
		f := &domain.ImportFailure{
			SessionID: s.ID, Path: "/src/f" + string(rune('0'+i)) + ".jpg",
			Op: domain.ImportFailureOpCopy, Status: domain.ImportFailureStatusOpen,
		}
		if err := failures.Create(ctx, f); err != nil {
			t.Fatalf("create failure: %v", err)
		}
	}

	page1, err := svc.ListSessionFailures(ctx, s.ID, 1, 2)
	if err != nil {
		t.Fatalf("ListSessionFailures: %v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page 1 items = %d, want 2", len(page1.Items))
	}
	if page1.Total != 5 {
		t.Fatalf("total = %d, want 5", page1.Total)
	}
	page3, err := svc.ListSessionFailures(ctx, s.ID, 3, 2)
	if err != nil {
		t.Fatalf("ListSessionFailures page 3: %v", err)
	}
	if len(page3.Items) != 1 {
		t.Fatalf("page 3 items = %d, want 1 (remainder)", len(page3.Items))
	}

	// A session with no structured rows reports Total 0 (legacy path signal).
	empty := mustSession(t, sessions, "")
	pe, err := svc.ListSessionFailures(ctx, empty.ID, 1, 50)
	if err != nil {
		t.Fatalf("ListSessionFailures empty: %v", err)
	}
	if pe.Total != 0 || len(pe.Items) != 0 {
		t.Fatalf("empty session: total=%d items=%d, want 0/0", pe.Total, len(pe.Items))
	}
}

func TestDismissFailureStateChange(t *testing.T) {
	svc, sessions, failures := newFailureHistory(t)
	ctx := context.Background()
	s := mustSession(t, sessions, "")
	f := &domain.ImportFailure{
		SessionID: s.ID, Path: "/src/gone.jpg", Op: domain.ImportFailureOpStat,
		Status: domain.ImportFailureStatusOpen,
	}
	if err := failures.Create(ctx, f); err != nil {
		t.Fatalf("create failure: %v", err)
	}

	dto, err := svc.DismissFailure(ctx, f.ID, "file deleted off the card")
	if err != nil {
		t.Fatalf("DismissFailure: %v", err)
	}
	if dto.Status != string(domain.ImportFailureStatusDismissed) {
		t.Fatalf("status = %q, want dismissed", dto.Status)
	}
	if dto.ResolvedAt == nil {
		t.Fatal("dismissed failure should have ResolvedAt set")
	}
	if dto.DismissReason != "file deleted off the card" {
		t.Fatalf("reason = %q", dto.DismissReason)
	}

	// Dismissing an already-resolved record is refused.
	if _, err := svc.DismissFailure(ctx, f.ID, ""); !errors.Is(err, ErrFailureAlreadyResolved) {
		t.Fatalf("second dismiss err = %v, want ErrFailureAlreadyResolved", err)
	}
}

// retryHarness wires a real ImportService + pipeline with the failure repo for
// the RetryFailedFile tests.
type retryHarness struct {
	svc      *ImportService
	sessions *repo.SessionRepo
	failures *repo.ImportFailureRepo
	src      string
	master   string
}

func newRetryHarness(t *testing.T) *retryHarness {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	master := filepath.Join(root, "master")
	for _, d := range []string{src, master} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	gdb, err := db.Open(filepath.Join(root, "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	failures := repo.NewImportFailureRepo(gdb)
	settings := repo.NewSettingsRepo(gdb)
	pipe := importer.New(importer.Config{
		DB: gdb, Assets: assets, Sessions: sessions, Failures: failures,
		Layout: archive.New(master),
	})
	svc := NewImportService(pipe, sessions, settings, nil, &captureEmitter{}, quietLogger())
	svc.failures = failures
	return &retryHarness{svc: svc, sessions: sessions, failures: failures, src: src, master: master}
}

func (h *retryHarness) session(t *testing.T) *domain.ImportSession {
	t.Helper()
	notes := resumeState{Mode: "copy", SourceRoot: h.src, DestinationRoot: h.master, EventName: "Trip"}.encode()
	s := &domain.ImportSession{Status: domain.SessionStatusCompleted, Notes: notes}
	if err := h.sessions.Create(context.Background(), s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func TestRetryFailedFileSuccess(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	s := h.session(t)
	// The original run counted one failure for this file.
	if err := h.sessions.IncFailures(ctx, s.ID, 1); err != nil {
		t.Fatalf("inc failures: %v", err)
	}
	path := filepath.Join(h.src, "a.jpg")
	if err := os.WriteFile(path, []byte("good jpeg content"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	f := &domain.ImportFailure{SessionID: s.ID, Path: path, Op: domain.ImportFailureOpCopy, Status: domain.ImportFailureStatusOpen}
	if err := h.failures.Create(ctx, f); err != nil {
		t.Fatalf("create failure: %v", err)
	}

	res, err := h.svc.RetryFailedFile(ctx, f.ID)
	if err != nil {
		t.Fatalf("RetryFailedFile: %v", err)
	}
	if !res.Success || res.AssetID == "" {
		t.Fatalf("result = %+v, want success with asset", res)
	}

	// Failure record is now retried and resolved.
	updated, err := h.failures.GetByID(ctx, f.ID)
	if err != nil {
		t.Fatalf("get failure: %v", err)
	}
	if updated.Status != domain.ImportFailureStatusRetried || updated.ResolvedAt == nil {
		t.Fatalf("failure = %+v, want retried+resolved", updated)
	}
	// Counter reconciled back to zero.
	session, err := h.sessions.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.Failures != 0 {
		t.Fatalf("Failures = %d, want 0 after successful retry", session.Failures)
	}
}

func TestRetryFailedFileMissingSource(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	s := h.session(t)
	f := &domain.ImportFailure{
		SessionID: s.ID, Path: filepath.Join(h.src, "ghost.jpg"),
		Op: domain.ImportFailureOpStat, Status: domain.ImportFailureStatusOpen,
	}
	if err := h.failures.Create(ctx, f); err != nil {
		t.Fatalf("create failure: %v", err)
	}
	_, err := h.svc.RetryFailedFile(ctx, f.ID)
	if !errors.Is(err, ErrRetrySourceMissing) {
		t.Fatalf("err = %v, want ErrRetrySourceMissing", err)
	}
	// The record stays open so the user can dismiss it.
	got, _ := h.failures.GetByID(ctx, f.ID)
	if got.Status != domain.ImportFailureStatusOpen {
		t.Fatalf("status = %q, want still open", got.Status)
	}
}

func TestRetryFailedFileRefusesWhileActive(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	s := h.session(t)
	path := filepath.Join(h.src, "b.jpg")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	f := &domain.ImportFailure{SessionID: s.ID, Path: path, Op: domain.ImportFailureOpCopy, Status: domain.ImportFailureStatusOpen}
	if err := h.failures.Create(ctx, f); err != nil {
		t.Fatalf("create failure: %v", err)
	}

	// Simulate an import already running.
	h.svc.mu.Lock()
	h.svc.active = true
	h.svc.mu.Unlock()

	_, err := h.svc.RetryFailedFile(ctx, f.ID)
	if !errors.Is(err, ErrImportInProgress) {
		t.Fatalf("err = %v, want ErrImportInProgress", err)
	}
}

// fakeEjectActivity is an ejectActivity returning fixed active paths.
type fakeEjectActivity struct{ paths []string }

func (f fakeEjectActivity) ActivePaths() []string { return f.paths }

func TestEjectVolumeGuards(t *testing.T) {
	svc := NewSourcesService(nil, nil, nil, nil, nil, nil, quietLogger())
	ctx := context.Background()

	// Empty mount point.
	if err := svc.EjectVolume(ctx, ""); !errors.Is(err, ErrEjectEmptyMount) {
		t.Fatalf("empty mount err = %v, want ErrEjectEmptyMount", err)
	}

	// Library volume refusal.
	svc.root = "/Volumes/Master/PhotoLibrary"
	if err := svc.EjectVolume(ctx, "/Volumes/Master"); !errors.Is(err, ErrEjectLibraryVolume) {
		t.Fatalf("library-volume err = %v, want ErrEjectLibraryVolume", err)
	}

	// Busy volume: an operation is touching a path on the target volume.
	svc.root = "/Volumes/Master"
	svc.activity = fakeEjectActivity{paths: []string{"/Volumes/Card/DCIM/IMG_0001.JPG"}}
	err := svc.EjectVolume(ctx, "/Volumes/Card")
	if err == nil || errors.Is(err, ErrEjectLibraryVolume) || !strings.Contains(err.Error(), "still using") {
		t.Fatalf("busy-volume err = %v, want a 'still using' refusal", err)
	}

	// Clean eject: stub the runner so nothing real is ejected.
	oldRunner := ejectRunner
	defer func() { ejectRunner = oldRunner }()
	var ejected string
	ejectRunner = func(_ context.Context, mp string) ([]byte, error) {
		ejected = mp
		return []byte("Disk /dev/disk4 ejected"), nil
	}
	svc.activity = fakeEjectActivity{paths: nil}
	if err := svc.EjectVolume(ctx, "/Volumes/Card"); err != nil {
		t.Fatalf("clean eject err = %v, want nil", err)
	}
	if ejected != "/Volumes/Card" {
		t.Fatalf("ejected = %q, want /Volumes/Card", ejected)
	}
}
