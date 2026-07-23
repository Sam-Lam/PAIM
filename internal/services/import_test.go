package services

import (
	"context"
	"errors"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/source"
	"github.com/Sam-Lam/PAIM/internal/volumes"
)

// fakeLister is a volumeLister returning a fixed set of mounted volumes.
type fakeLister struct {
	infos []volumes.Info
	err   error
}

func (f fakeLister) List(context.Context) ([]volumes.Info, error) { return f.infos, f.err }

// fakeIdentifier is a sourceIdentifier returning a fixed match (or error).
type fakeIdentifier struct {
	match *source.Match
	err   error
}

func (f fakeIdentifier) Identify(context.Context, string, func(int)) (*source.Match, error) {
	return f.match, f.err
}

// TestStartImportAutoLinksSource verifies the copy-mode source auto-link: with no
// SourceID supplied, StartImport resolves the mount the source root sits on,
// identifies it, persists the ImportSource, and stamps its ID on the session.
func TestStartImportAutoLinksSource(t *testing.T) {
	h := newAnalyzeHarness(t)
	sources := repo.NewSourceRepo(h.gdb)
	h.svc.sources = sources
	h.svc.collector = fakeLister{infos: []volumes.Info{{MountPoint: h.src, VolumeName: "CARD"}}}
	rec := &domain.ImportSource{SourceType: domain.SourceTypeSDCard, VolumeLabel: "CARD"}
	h.svc.identifier = fakeIdentifier{match: &source.Match{SourceRecord: rec, Confidence: 100}}

	h.write("a.jpg", "alpha-content")

	res, err := h.svc.StartImport(context.Background(), h.copyOpts())
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	h.waitIdle()

	session, gerr := repo.NewSessionRepo(h.gdb).GetByID(context.Background(), res.SessionID)
	if gerr != nil {
		t.Fatalf("get session: %v", gerr)
	}
	if session.SourceID == "" {
		t.Fatal("session SourceID empty — auto-link did not stamp it")
	}
	if _, gerr := sources.GetByID(context.Background(), session.SourceID); gerr != nil {
		t.Fatalf("auto-linked source was not persisted: %v", gerr)
	}
}

// TestStartImportProceedsWhenIdentifyFails verifies identification is strictly
// best-effort: an Identify error leaves the session's SourceID empty and never
// fails or blocks the import.
func TestStartImportProceedsWhenIdentifyFails(t *testing.T) {
	h := newAnalyzeHarness(t)
	sources := repo.NewSourceRepo(h.gdb)
	h.svc.sources = sources
	h.svc.collector = fakeLister{infos: []volumes.Info{{MountPoint: h.src}}}
	h.svc.identifier = fakeIdentifier{err: errors.New("describe failed")}

	h.write("a.jpg", "alpha-content")

	res, err := h.svc.StartImport(context.Background(), h.copyOpts())
	if err != nil {
		t.Fatalf("StartImport must not fail on identify error: %v", err)
	}
	h.waitIdle()

	session, gerr := repo.NewSessionRepo(h.gdb).GetByID(context.Background(), res.SessionID)
	if gerr != nil {
		t.Fatalf("get session: %v", gerr)
	}
	if session.SourceID != "" {
		t.Errorf("SourceID = %q, want empty on identify failure", session.SourceID)
	}
	if session.Status != domain.SessionStatusCompleted {
		t.Errorf("session status = %q, want completed (import should proceed)", session.Status)
	}
}

// TestLaunchRejectsConcurrentImport verifies the one-active-import guard: when an
// import is already active, launch refuses a second start without touching any
// repository or the pipeline.
func TestLaunchRejectsConcurrentImport(t *testing.T) {
	svc := &ImportService{active: true}
	_, err := svc.launch(context.Background(), resumeState{SourceRoot: "/x"}, nil)
	if !errors.Is(err, ErrImportInProgress) {
		t.Fatalf("expected ErrImportInProgress, got %v", err)
	}
}

// TestActiveImportNilWhenIdle verifies ActiveImport returns nil when nothing is
// running.
func TestActiveImportNilWhenIdle(t *testing.T) {
	svc := &ImportService{}
	got, err := svc.ActiveImport(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil progress when idle, got %+v", got)
	}
}

// TestCancelImportNoActiveIsNoop verifies CancelImport is a no-op when idle.
func TestCancelImportNoActiveIsNoop(t *testing.T) {
	svc := &ImportService{}
	if err := svc.CancelImport(context.Background()); err != nil {
		t.Fatalf("CancelImport with no active import should be nil, got %v", err)
	}
}
