package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Sam-Lam/PAIM/internal/version"
)

// heartbeatInterval is how often a held lock refreshes its mtime so other
// machines can tell a live holder from a crashed one.
const defaultHeartbeatInterval = 30 * time.Second

// LockInfo is the JSON payload written into a library lock file.
type LockInfo struct {
	Hostname   string    `json:"hostname"`
	PID        int       `json:"pid"`
	AppVersion string    `json:"appVersion"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

// LockHeldError reports that a library's lock is already held. It distinguishes
// the same-host-live case (the library is open in another PAIM on this Mac) from
// the other-host case (open on a different Mac, possibly crashed), and carries
// the holder's identity plus how long ago its heartbeat last touched the lock so
// the UI can present an informed Force Open choice.
type LockHeldError struct {
	Info LockInfo
	// SameHost is true when the lock's hostname matches this machine.
	SameHost bool
	// LivePID is true when SameHost and the recorded PID is still running.
	LivePID bool
	// HeartbeatAge is time since the lock file was last touched (its mtime).
	HeartbeatAge time.Duration
}

func (e *LockHeldError) Error() string {
	if e.SameHost && e.LivePID {
		return fmt.Sprintf("library is already open in another PAIM on this machine (pid %d)", e.Info.PID)
	}
	host := e.Info.Hostname
	if host == "" {
		host = "another machine"
	}
	return fmt.Sprintf(
		"library is locked by %q (pid %d, last active %s ago) — it may still be open there or have crashed; Force Open to take it over",
		host, e.Info.PID, e.HeartbeatAge.Round(time.Second),
	)
}

// AsLockHeld returns the *LockHeldError in err's chain, if any.
func AsLockHeld(err error) (*LockHeldError, bool) {
	var held *LockHeldError
	if errors.As(err, &held) {
		return held, true
	}
	return nil, false
}

// Lock is a held single-writer library lock. Its heartbeat goroutine refreshes
// the lock file's mtime until Release is called.
type Lock struct {
	path string
	info LockInfo

	mu       sync.Mutex
	released bool
	stop     chan struct{}
	done     chan struct{}
}

// Info returns the identity written into the lock.
func (l *Lock) Info() LockInfo { return l.info }

// AcquireLock creates the lock file at path (O_CREATE|O_EXCL), writes the caller's
// identity, fsyncs it, and starts the heartbeat. An existing lock from a dead
// same-host PID is reclaimed (logged via the returned reclaimed flag semantics in
// the caller). A live same-host lock or an other-host lock is refused with a
// *LockHeldError. Use ForceAcquireLock to break a refused lock.
func AcquireLock(path, appVersion string) (*Lock, error) {
	return acquireLock(path, appVersion, defaultHeartbeatInterval)
}

// ForceAcquireLock removes any existing lock at path and then acquires it. It is
// the explicit, user-confirmed override for a refused lock (the crashed-at-the-
// office case).
func ForceAcquireLock(path, appVersion string) (*Lock, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("library: break existing lock %q: %w", path, err)
	}
	return acquireLock(path, appVersion, defaultHeartbeatInterval)
}

func acquireLock(path, appVersion string, hb time.Duration) (*Lock, error) {
	if appVersion == "" {
		appVersion = version.Version
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("library: create lock dir: %w", err)
	}

	// Retry to cover the race where a stale same-host lock is reclaimed between
	// the failed create and the next attempt.
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return finishAcquire(f, path, appVersion, hb)
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("library: create lock %q: %w", path, err)
		}

		// Lock exists — decide whether to reclaim or refuse.
		reclaimed, held := inspectLock(path)
		if held != nil {
			return nil, held
		}
		if !reclaimed {
			// Could neither reclaim nor identify a live holder; refuse conservatively.
			return nil, &LockHeldError{Info: readLockInfo(path)}
		}
		// Reclaimed a dead same-host lock; loop to re-create it.
	}
	return nil, fmt.Errorf("library: could not acquire lock %q after reclaim retries", path)
}

// finishAcquire writes the identity to the freshly created lock file, fsyncs it,
// and starts the heartbeat.
func finishAcquire(f *os.File, path, appVersion string, hb time.Duration) (*Lock, error) {
	host, _ := os.Hostname()
	info := LockInfo{
		Hostname:   host,
		PID:        os.Getpid(),
		AppVersion: appVersion,
		AcquiredAt: time.Now(),
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("library: write lock %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("library: fsync lock %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("library: close lock %q: %w", path, err)
	}

	l := &Lock{path: path, info: info, stop: make(chan struct{}), done: make(chan struct{})}
	go l.heartbeat(hb)
	return l, nil
}

// inspectLock examines an existing lock file. It returns (reclaimed=true, nil)
// after removing a lock whose same-host PID is dead, or (false, *LockHeldError)
// when the lock is held (same-host live, or another host). A completely
// unreadable lock returns (false, nil) so the caller can refuse conservatively.
func inspectLock(path string) (bool, *LockHeldError) {
	info := readLockInfo(path)
	age := lockAge(path)
	host, _ := os.Hostname()
	sameHost := info.Hostname != "" && info.Hostname == host

	if sameHost {
		if pidAlive(info.PID) {
			return false, &LockHeldError{Info: info, SameHost: true, LivePID: true, HeartbeatAge: age}
		}
		// Same host, dead PID: safe to reclaim.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return false, &LockHeldError{Info: info, SameHost: true, HeartbeatAge: age}
		}
		return true, nil
	}

	// Another host (or unknown): never silently reclaim — refuse, forceable.
	return false, &LockHeldError{Info: info, SameHost: false, HeartbeatAge: age}
}

// readLockInfo best-effort reads and parses the lock file. A missing or
// malformed file yields a zero LockInfo.
func readLockInfo(path string) LockInfo {
	var info LockInfo
	data, err := os.ReadFile(path)
	if err != nil {
		return info
	}
	_ = json.Unmarshal(data, &info)
	return info
}

// lockAge returns time since the lock file's mtime (the heartbeat), or 0 when it
// cannot be stat'd.
func lockAge(path string) time.Duration {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return time.Since(info.ModTime())
}

// pidAlive reports whether a process with the given pid exists. Signal 0 probes
// existence without delivering a signal; EPERM means the process exists but is
// owned by another user (still alive), ESRCH means it is gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// heartbeat refreshes the lock file's mtime every interval until Release stops
// it, so other machines can distinguish a live holder from a crashed one.
func (l *Lock) heartbeat(interval time.Duration) {
	defer close(l.done)
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

// Release stops the heartbeat and removes the lock file. It is idempotent and
// safe to call from a shutdown handler.
func (l *Lock) Release() error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	l.mu.Unlock()

	close(l.stop)
	<-l.done
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("library: release lock %q: %w", l.path, err)
	}
	return nil
}
