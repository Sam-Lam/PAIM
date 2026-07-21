package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLock writes a lock file with the given identity for tests that exercise
// reclaim/refusal decisions.
func writeLock(t *testing.T, path string, info LockInfo) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, _ := json.Marshal(info)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

func TestLockAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".paim", "lock")
	lock, err := AcquireLock(path, "0.1.0")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock file should be gone after Release, err = %v", err)
	}
	// Release is idempotent.
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestLockReclaimsSameHostDeadPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".paim", "lock")
	host, _ := os.Hostname()
	// A PID above the platform maximum is guaranteed not to be running.
	writeLock(t, path, LockInfo{Hostname: host, PID: 4000000, AppVersion: "0.1.0", AcquiredAt: time.Now()})

	lock, err := AcquireLock(path, "0.1.0")
	if err != nil {
		t.Fatalf("expected reclaim of dead same-host lock, got: %v", err)
	}
	if lock.Info().PID != os.Getpid() {
		t.Fatalf("reclaimed lock should carry our PID, got %d", lock.Info().PID)
	}
	_ = lock.Release()
}

func TestLockRefusesSameHostLivePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".paim", "lock")
	host, _ := os.Hostname()
	writeLock(t, path, LockInfo{Hostname: host, PID: os.Getpid(), AppVersion: "0.1.0", AcquiredAt: time.Now()})

	_, err := AcquireLock(path, "0.1.0")
	held, ok := AsLockHeld(err)
	if !ok {
		t.Fatalf("expected LockHeldError, got %v", err)
	}
	if !held.SameHost || !held.LivePID {
		t.Fatalf("expected same-host live refusal, got %+v", held)
	}

	// Force Open breaks it.
	lock, ferr := ForceAcquireLock(path, "0.1.0")
	if ferr != nil {
		t.Fatalf("ForceAcquireLock: %v", ferr)
	}
	_ = lock.Release()
}

func TestLockRefusesOtherHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".paim", "lock")
	writeLock(t, path, LockInfo{Hostname: "some-other-mac", PID: 1, AppVersion: "0.1.0", AcquiredAt: time.Now().Add(-time.Minute)})

	_, err := AcquireLock(path, "0.1.0")
	held, ok := AsLockHeld(err)
	if !ok {
		t.Fatalf("expected LockHeldError, got %v", err)
	}
	if held.SameHost {
		t.Fatalf("expected other-host refusal, got SameHost=true")
	}
	if held.Info.Hostname != "some-other-mac" {
		t.Fatalf("held hostname = %q", held.Info.Hostname)
	}
}
