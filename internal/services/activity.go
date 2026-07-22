package services

import "sync"

// OperationInfo is one running long operation reported to the quit guard. Kind is
// a stable machine token (import | analyze | reorganize | safe_to_erase | cleanup
// | backup_upload); Label is a human phrase. FilesDone/FilesTotal and
// BytesDone/BytesTotal are the latest progress snapshot (a zero total means the
// count is indeterminate). It is JSON-serializable so it can ride the
// app:quit-requested event payload and the AppService.ActiveOperations binding.
type OperationInfo struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	FilesDone  int    `json:"filesDone"`
	FilesTotal int    `json:"filesTotal"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
}

// activitySource is implemented by every service that runs a cancellable long
// operation. activeOps returns the operations it currently has in flight (empty
// when idle); cancelActive triggers the operation's existing cancel path (a
// no-op when nothing is running). The methods are unexported so registering a
// service as a source does not add them to the Wails binding surface.
type activitySource interface {
	activeOps() []OperationInfo
	cancelActive()
}

// ActivityTracker aggregates the running long operations across all registered
// services for the quit guard. Services register themselves once at startup (the
// same shape as SleepGuard wiring); Snapshot pulls a fresh view on demand and
// CancelAll fans the cancel out to every source. A nil-safe zero value is fine —
// an unregistered tracker simply reports no activity.
type ActivityTracker struct {
	mu      sync.Mutex
	sources []activitySource
}

// NewActivityTracker returns an empty tracker.
func NewActivityTracker() *ActivityTracker { return &ActivityTracker{} }

// Register adds a source. It is called once per service at startup, before the
// app runs, so no synchronization with Snapshot/CancelAll callers is required
// beyond the mutex.
func (t *ActivityTracker) Register(src activitySource) {
	if src == nil {
		return
	}
	t.mu.Lock()
	t.sources = append(t.sources, src)
	t.mu.Unlock()
}

// Snapshot collects the currently-running operations from every source. The
// returned slice is freshly built on each call, so callers may retain it.
func (t *ActivityTracker) Snapshot() []OperationInfo {
	t.mu.Lock()
	srcs := append([]activitySource(nil), t.sources...)
	t.mu.Unlock()
	var out []OperationInfo
	for _, s := range srcs {
		out = append(out, s.activeOps()...)
	}
	return out
}

// CancelAll asks every source to cancel whatever it is running, reusing each
// operation's existing cancel path. It does not wait; callers poll Snapshot for
// the operations to drain.
func (t *ActivityTracker) CancelAll() {
	t.mu.Lock()
	srcs := append([]activitySource(nil), t.sources...)
	t.mu.Unlock()
	for _, s := range srcs {
		s.cancelActive()
	}
}
