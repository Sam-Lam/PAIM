package services

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/library"
)

// fakeSource (an activitySource returning a fixed op set) is defined in
// activity_test.go and reused here.

func TestForegroundYield_Gate(t *testing.T) {
	tracker := NewActivityTracker()
	src := &fakeSource{}
	tracker.Register(src)

	y := NewForegroundYield(tracker, true)

	// No activity: never yields.
	if y.Gate() {
		t.Fatalf("Gate() = true with no activity, want false")
	}

	// A foreground-kind op yields.
	src.ops = []OperationInfo{{Kind: OpKindImport}}
	if !y.Gate() {
		t.Fatalf("Gate() = false during an import, want true")
	}

	// A non-foreground op (backup's own upload) must NOT yield.
	src.ops = []OperationInfo{{Kind: OpKindBackupUpload}}
	if y.Gate() {
		t.Fatalf("Gate() = true during a backup upload, want false (backup would pause itself)")
	}

	// Disabling the preference makes Gate return false even during a foreground op
	// (the setting toggles the gate live).
	src.ops = []OperationInfo{{Kind: OpKindReorganize}}
	if !y.Gate() {
		t.Fatalf("Gate() = false during a reorganize, want true")
	}
	y.SetEnabled(false)
	if y.Gate() {
		t.Fatalf("Gate() = true after SetEnabled(false), want false")
	}
	y.SetEnabled(true)
	if !y.Gate() {
		t.Fatalf("Gate() = false after SetEnabled(true), want true")
	}
}

// TestBackupService_SetPauseBackupsDuringForeground verifies the settings-service
// setter persists the per-machine preference to library.Config and applies it to
// the live yield gate immediately (mirroring thumbnail parallelism live-apply).
func TestBackupService_SetPauseBackupsDuringForeground(t *testing.T) {
	cfgStore, err := library.NewConfigStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	tracker := NewActivityTracker()
	src := &fakeSource{ops: []OperationInfo{{Kind: OpKindImport}}}
	tracker.Register(src)
	yield := NewForegroundYield(tracker, true)

	// jobs is nil: the setter's UI nudge is skipped, but persistence + live apply
	// are exercised. Gate is nil so guard() never blocks (unit-test convention).
	svc := NewBackupService(nil, nil, nil, cfgStore, yield, nil, nil)

	ctx := context.Background()

	// Default reads true (from the live gate seeded true).
	if v, err := svc.PauseBackupsDuringForeground(ctx); err != nil || !v {
		t.Fatalf("PauseBackupsDuringForeground default = %v, err=%v, want true", v, err)
	}
	if !yield.Gate() {
		t.Fatalf("gate should yield during an import with the preference on")
	}

	// Turn it off: persisted to config, live gate stops yielding.
	if v, err := svc.SetPauseBackupsDuringForeground(ctx, false); err != nil || v {
		t.Fatalf("SetPauseBackupsDuringForeground(false) = %v, err=%v, want false", v, err)
	}
	if yield.Enabled() {
		t.Fatalf("yield still enabled after setting off")
	}
	if yield.Gate() {
		t.Fatalf("gate should NOT yield after the preference is turned off")
	}
	if cfg, err := cfgStore.Load(); err != nil || cfg.PauseBackupsDuringForegroundEnabled() {
		t.Fatalf("config not persisted as off: enabled=%v err=%v", cfg.PauseBackupsDuringForegroundEnabled(), err)
	}
	if v, err := svc.PauseBackupsDuringForeground(ctx); err != nil || v {
		t.Fatalf("getter after off = %v, want false", v)
	}

	// Turn it back on: gate yields again during the import.
	if v, err := svc.SetPauseBackupsDuringForeground(ctx, true); err != nil || !v {
		t.Fatalf("SetPauseBackupsDuringForeground(true) = %v, err=%v, want true", v, err)
	}
	if !yield.Gate() {
		t.Fatalf("gate should yield again after the preference is turned back on")
	}
}

func TestForegroundYield_NilSafe(t *testing.T) {
	var y *ForegroundYield
	if y.Gate() {
		t.Fatalf("nil ForegroundYield must never yield")
	}
	if y.Enabled() {
		t.Fatalf("nil ForegroundYield must report disabled")
	}
	y.SetEnabled(true) // must not panic
}

// TestForegroundKindsPartitionAllKinds keeps foregroundKinds exhaustive against
// AllOperationKinds: every defined OperationInfo.Kind must be deliberately
// classified as foreground (yield-triggering) or background here. Adding a new
// kind to AllOperationKinds without categorizing it below fails this test,
// forcing a conscious decision about whether it should pause background backups.
func TestForegroundKindsPartitionAllKinds(t *testing.T) {
	// The expected classification, maintained alongside AllOperationKinds.
	wantForeground := map[string]bool{
		OpKindImport:      true,
		OpKindAnalyze:     true,
		OpKindReorganize:  true,
		OpKindSafeToErase: true,
		OpKindCleanup:     true,
		OpKindClearSource: true,
	}
	wantBackground := map[string]bool{
		OpKindBackupUpload:    true,
		OpKindBackupBackfill:  true,
		OpKindThumbnailWarmup: true,
	}

	for _, kind := range AllOperationKinds {
		fg, bg := wantForeground[kind], wantBackground[kind]
		if fg == bg {
			t.Fatalf("kind %q is not classified exactly once (foreground=%v background=%v) — add it to wantForeground or wantBackground", kind, fg, bg)
		}
		if got := IsForegroundKind(kind); got != fg {
			t.Fatalf("IsForegroundKind(%q) = %v, want %v", kind, got, fg)
		}
	}

	// The reverse direction: every classified kind must be a real, listed kind, so
	// the maps above cannot drift ahead of AllOperationKinds either.
	all := make(map[string]bool, len(AllOperationKinds))
	for _, k := range AllOperationKinds {
		all[k] = true
	}
	for k := range wantForeground {
		if !all[k] {
			t.Fatalf("wantForeground lists %q which is not in AllOperationKinds", k)
		}
	}
	for k := range wantBackground {
		if !all[k] {
			t.Fatalf("wantBackground lists %q which is not in AllOperationKinds", k)
		}
	}

	// Explicit belt-and-suspenders: backup and backfill kinds must never trigger
	// yielding, or backups would pause themselves.
	if IsForegroundKind(OpKindBackupUpload) || IsForegroundKind(OpKindBackupBackfill) {
		t.Fatalf("backup kinds must be excluded from foreground yielding")
	}
}
