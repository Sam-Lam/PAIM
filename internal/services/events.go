// Package services holds PAIM's Wails-bound service layer. Each service is a thin
// struct that validates input, calls the engine packages (importer, backup,
// source, volumes, cleanup, metadata, archive) through their exported APIs, maps
// results to JSON-friendly DTOs, and emits typed progress events. No GORM model
// leaks across the binding boundary: every method returns a DTO defined in this
// package.
//
// Services never import the Wails application package directly. Two small
// interfaces — Emitter (event emission) and Dialoger (native file dialogs) — are
// consumed here and adapted over the Wails App in main.go, which keeps the
// services testable without a running application.
package services

import (
	"sync"
	"time"
)

// Event names emitted by the services layer. main.go registers a typed payload
// for each via application.RegisterEvent so the binding generator produces a
// strongly-typed TS API. The frontend subscribes with Events.On(<name>, cb).
const (
	EventImportProgress     = "import:progress"
	EventImportCompleted    = "import:completed"
	EventAnalyzeCompleted   = "analyze:completed"
	EventBackupProgress     = "backup:progress"
	EventBackupQueueChanged = "backup:queue-changed"
	EventVolumeMounted      = "volume:mounted"
	EventVolumeUnmounted    = "volume:unmounted"
	EventSourceIdentified   = "source:identified"
	EventSourceProgress     = "source:progress"
	EventSourceEvaluated    = "source:evaluated"
	EventCleanupProgress    = "cleanup:progress"
	EventCleanupCompleted   = "cleanup:completed"
	EventReorganizePlan     = "reorganize:plan-progress"
	EventLogExportProgress  = "log:export-progress"
	EventDuplicateProgress  = "duplicate:progress"
	EventLibraryProgress    = "library:progress"
)

// Emitter delivers a typed event payload to the frontend. It is implemented in
// main.go over the Wails App's event manager and injected into every service
// that reports progress. A nil Emitter is tolerated by emitSafe.
type Emitter interface {
	Emit(name string, data any)
}

// emitSafe emits only when the emitter is non-nil, so services constructed
// without one (e.g. in unit tests) never panic.
func emitSafe(e Emitter, name string, data any) {
	if e != nil {
		e.Emit(name, data)
	}
}

// ImportProgress is the payload for import:progress. It mirrors importer.Progress
// plus the owning session ID (so the frontend can correlate updates) and a
// derived completion percentage.
type ImportProgress struct {
	SessionID   string  `json:"sessionId"`
	Phase       string  `json:"phase"`
	FilesDone   int     `json:"filesDone"`
	FilesTotal  int     `json:"filesTotal"`
	BytesDone   int64   `json:"bytesDone"`
	BytesTotal  int64   `json:"bytesTotal"`
	CurrentFile string  `json:"currentFile"`
	Errors      int     `json:"errors"`
	Percent     float64 `json:"percent"`
	Done        bool    `json:"done"`
}

// ImportCompleted is the payload for import:completed, emitted once when a
// background import goroutine finishes (successfully, cancelled, or interrupted).
type ImportCompleted struct {
	SessionID     string `json:"sessionId"`
	Status        string `json:"status"`
	FilesScanned  int    `json:"filesScanned"`
	FilesImported int    `json:"filesImported"`
	Duplicates    int    `json:"duplicates"`
	Failures      int    `json:"failures"`
	Skipped       int    `json:"skipped"`
}

// AnalyzeCompleted is the payload for analyze:completed, emitted once when a
// background analyze (scan + dry run) goroutine finishes. Exactly one of the
// terminal states holds:
//   - success: Report is non-nil, Cancelled=false, Error=="".
//   - cancelled (via CancelImport): Report=nil, Cancelled=true.
//   - failed: Report=nil, Error carries the message.
//
// Root and Opts echo the (normalized) request so the frontend can restore the
// whole step-2 context on re-attach without re-deriving it.
type AnalyzeCompleted struct {
	Root      string           `json:"root"`
	Opts      ImportOptions    `json:"opts"`
	Report    *DryRunReportDTO `json:"report"`
	Cancelled bool             `json:"cancelled"`
	Error     string           `json:"error"`
}

// BackupProgress is the payload for backup:progress, carrying one worker's
// per-job upload progress.
type BackupProgress struct {
	JobID      string `json:"jobId"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
}

// BackupQueueChanged is the payload for backup:queue-changed, emitted after any
// queue state transition so the frontend can refresh counts.
type BackupQueueChanged struct {
	Summary QueueSummaryDTO `json:"summary"`
}

// VolumeEvent is the payload for volume:mounted and volume:unmounted.
type VolumeEvent struct {
	MountPoint string `json:"mountPoint"`
}

// SourceIdentified is the payload for source:identified, emitted after a volume
// is identified and persisted.
type SourceIdentified struct {
	MountPoint string `json:"mountPoint"`
	SourceID   string `json:"sourceId"`
	Confidence int    `json:"confidence"`
	IsKnown    bool   `json:"isKnown"`
}

// SourceProgress is the payload for source:progress, carrying the progress of a
// long-running source operation. Kind is "safe-to-erase" (determinate:
// FilesTotal is known) or "identify" (indeterminate: only Scanned advances,
// FilesTotal is 0). MountPoint correlates the update with a volume card.
type SourceProgress struct {
	Kind        string `json:"kind"`
	MountPoint  string `json:"mountPoint"`
	FilesDone   int    `json:"filesDone"`
	FilesTotal  int    `json:"filesTotal"`
	Scanned     int    `json:"scanned"`
	CurrentFile string `json:"currentFile"`
}

// SourceEvaluated is the payload for source:evaluated, emitted once when a
// background safe-to-erase evaluation finishes. Exactly one terminal state
// holds: success (Report set), cancelled (Cancelled true), or failed (Error set).
type SourceEvaluated struct {
	MountPoint string          `json:"mountPoint"`
	SourceID   string          `json:"sourceId"`
	Report     *SafeToEraseDTO `json:"report"`
	Cancelled  bool            `json:"cancelled"`
	Error      string          `json:"error"`
}

// CleanupProgress is the payload for cleanup:progress. FilesDone advances per
// classified file; FilesTotal is 0 while the analyzer walks-and-classifies in a
// single pass (the total is not known until the walk completes), so the UI
// renders an indeterminate count. CurrentFile is the file being classified.
type CleanupProgress struct {
	FilesDone   int    `json:"filesDone"`
	FilesTotal  int    `json:"filesTotal"`
	CurrentFile string `json:"currentFile"`
}

// CleanupCompleted is the payload for cleanup:completed, emitted once when a
// background cleanup analyze finishes. Exactly one terminal state holds: success
// (Report set), cancelled (Cancelled true), or failed (Error set). Root echoes
// the analyzed folder so the frontend can restore context on re-attach.
type CleanupCompleted struct {
	Root      string            `json:"root"`
	Report    *CleanupReportDTO `json:"report"`
	Cancelled bool              `json:"cancelled"`
	Error     string            `json:"error"`
}

// ReorganizePlanProgress is the payload for reorganize:plan-progress, emitted
// (throttled) while PlanReorganize walks the catalog. Total is known up front
// (after listing archived assets) so the UI can render a determinate bar.
type ReorganizePlanProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// LogExportProgress is the payload for log:export-progress, emitted (throttled)
// while a log export streams rows to disk. RowsWritten is the running count.
type LogExportProgress struct {
	RowsWritten int `json:"rowsWritten"`
}

// DuplicateProgress is the payload for duplicate:progress, carrying the byte
// progress of a cross-volume duplicate move (copy+verify).
type DuplicateProgress struct {
	AssetID    string `json:"assetId"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
}

// LibraryProgress is the payload for library:progress, emitted during a library
// open that runs migrations / legacy installation. Phase names the current step
// ("backing-up", "installing-legacy", "converting-paths", "verifying",
// "migrating"); Done/Total are populated for determinate phases (0 otherwise).
// Not cancellable: migrations must run to completion or roll back atomically.
type LibraryProgress struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
}

// throttleInterval bounds progress emission to at most one event per 100ms
// (<=10/sec) as required by the architecture, so a fast import cannot flood the
// event bus.
const throttleInterval = 100 * time.Millisecond

// throttle rate-limits progress emission. allow() reports whether enough time
// has elapsed since the last permitted emission; terminal events should bypass
// it and always emit. It is safe for concurrent use (backup workers share one).
type throttle struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// newThrottle constructs a throttle at the default <=10/sec rate.
func newThrottle() *throttle {
	return &throttle{interval: throttleInterval}
}

// allow reports whether an emission is permitted now, updating the last-emit
// timestamp when it returns true.
func (t *throttle) allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.last.IsZero() || now.Sub(t.last) >= t.interval {
		t.last = now
		return true
	}
	return false
}

// percent computes a 0..100 completion percentage from a done/total pair,
// returning 0 when total is non-positive.
func percent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(done) / float64(total) * 100
	if p > 100 {
		return 100
	}
	return p
}
