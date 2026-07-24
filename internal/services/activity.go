package services

import (
	"sync"
	"sync/atomic"
)

// Operation kinds: the stable machine tokens used in OperationInfo.Kind. Every
// long operation reported to the quit guard uses exactly one of these. They are
// defined here, in one place, so the foreground-yield set (foregroundKinds) can
// be kept exhaustive against them: a test enumerates AllOperationKinds and fails
// if a new kind is added without deciding whether it should pause background
// backups. When adding a new kind, add its constant here, list it in
// AllOperationKinds, and categorize it in foregroundKinds (or deliberately leave
// it out).
const (
	OpKindImport           = "import"
	OpKindAnalyze          = "analyze"
	OpKindReorganize       = "reorganize"
	OpKindSafeToErase      = "safe_to_erase"
	OpKindCleanup          = "cleanup"
	OpKindClearSource      = "clear_source"
	OpKindDuplicateResolve = "duplicate_resolve"
	OpKindBackupUpload     = "backup_upload"
	OpKindBackupBackfill   = "backup_backfill"
	OpKindThumbnailWarmup  = "thumbnail_warmup"
)

// AllOperationKinds enumerates every defined OperationInfo.Kind. It is held
// exhaustive by TestForegroundKindsPartitionAllKinds, which asserts each entry
// is classified as either foreground (yield-triggering) or background.
var AllOperationKinds = []string{
	OpKindImport, OpKindAnalyze, OpKindReorganize, OpKindSafeToErase,
	OpKindCleanup, OpKindClearSource, OpKindDuplicateResolve,
	OpKindBackupUpload, OpKindBackupBackfill, OpKindThumbnailWarmup,
}

// foregroundKinds are the operation kinds whose activity makes the backup
// manager yield — stop claiming NEW upload jobs — until they finish. On spinning
// media a backup upload's reads seek-compete with these operations' reads/writes
// on the same drive, degrading both; backups are patient background work.
//
// Backup's own kinds (backup_upload, backup_backfill) are deliberately excluded:
// yielding on them would make backups pause themselves. The disposable thumbnail
// warm-up is excluded too — it is a trivial cache job, not a data movement worth
// pausing custody work for.
var foregroundKinds = map[string]bool{
	OpKindImport:      true,
	OpKindAnalyze:     true,
	OpKindReorganize:  true,
	OpKindSafeToErase: true,
	OpKindCleanup:     true,
	OpKindClearSource: true,
	// A bulk duplicate resolve moves/trashes archive files, so it competes for the
	// same spindle as a backup upload — yield backups while it runs.
	OpKindDuplicateResolve: true,
}

// IsForegroundKind reports whether an operation kind should trigger backup
// yielding (see foregroundKinds).
func IsForegroundKind(kind string) bool { return foregroundKinds[kind] }

// OperationInfo is one running long operation reported to the quit guard. Kind is
// a stable machine token (one of the OpKind* constants); Label is a human phrase.
// FilesDone/FilesTotal and BytesDone/BytesTotal are the latest progress snapshot
// (a zero total means the count is indeterminate). It is JSON-serializable so it
// can ride the app:quit-requested event payload and the
// AppService.ActiveOperations binding.
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

// activePather is optionally implemented by an activity source to report the
// filesystem paths its running operations are currently touching. The eject
// guard uses it to refuse ejecting a volume ONLY when a live operation is on
// that volume, rather than blocking every eject while any work runs. Sources
// that do not implement it contribute no paths (the eject still relies on the
// library-volume guard and the OS's own busy-volume refusal as backstops).
type activePather interface {
	activePaths() []string
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

// ActivePaths collects the filesystem paths every activity source that
// implements activePather reports as currently in use. It is freshly built per
// call. The eject guard cross-references these against a target volume.
func (t *ActivityTracker) ActivePaths() []string {
	t.mu.Lock()
	srcs := append([]activitySource(nil), t.sources...)
	t.mu.Unlock()
	var out []string
	for _, s := range srcs {
		if ap, ok := s.(activePather); ok {
			out = append(out, ap.activePaths()...)
		}
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

// ForegroundYield decides whether the backup manager should yield — stop
// claiming NEW upload jobs — because a foreground operation is running. It is the
// backup.Options.ForegroundGate: main.go passes its Gate method to the Manager,
// and the Backup service flips its enabled flag live when the user toggles the
// per-machine "Pause backups while imports run" preference.
//
// Gate composes two conditions:
//   - the PauseBackupsDuringForeground preference is on (enabled), AND
//   - the activity tracker currently reports at least one foreground-kind op.
//
// When the preference is off, Gate always returns false (never yield). The
// enabled flag is an atomic so the settings toggle applies live without
// restarting the manager, mirroring how thumbnail parallelism is live-applied.
type ForegroundYield struct {
	tracker *ActivityTracker
	enabled atomic.Bool
}

// NewForegroundYield constructs a yield gate over the tracker with the initial
// preference value (from library.Config at startup).
func NewForegroundYield(tracker *ActivityTracker, enabled bool) *ForegroundYield {
	y := &ForegroundYield{tracker: tracker}
	y.enabled.Store(enabled)
	return y
}

// SetEnabled updates the live preference. The caller persists it to
// library.Config separately; this only flips the in-memory gate.
func (y *ForegroundYield) SetEnabled(on bool) {
	if y == nil {
		return
	}
	y.enabled.Store(on)
}

// Enabled reports the current preference.
func (y *ForegroundYield) Enabled() bool { return y != nil && y.enabled.Load() }

// Gate reports whether the backup manager should yield right now: the preference
// is on and a foreground-kind operation is in flight. A nil gate never yields.
func (y *ForegroundYield) Gate() bool {
	if y == nil || y.tracker == nil || !y.enabled.Load() {
		return false
	}
	for _, op := range y.tracker.Snapshot() {
		if IsForegroundKind(op.Kind) {
			return true
		}
	}
	return false
}
