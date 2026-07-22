package services

import (
	"context"
	"errors"
	"testing"
)

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
