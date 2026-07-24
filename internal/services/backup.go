package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// BackupService inspects and controls the SQLite-persisted backup queue via the
// backup Manager. State-changing methods emit backup:queue-changed so the UI can
// refresh; per-upload progress is emitted as backup:progress by the Manager's
// ProgressFn (wired in main.go, see NewBackupProgressEmitter).
type BackupService struct {
	gated
	sleepAware
	manager *backup.Manager
	jobs    *repo.BackupRepo
	assets  *repo.AssetRepo
	db      *gorm.DB
	emitter Emitter
	log     *slog.Logger
	root    string

	// config and yield back the per-machine "Pause backups while imports run"
	// preference: config persists it (library.Config), yield is the live gate the
	// backup Manager consults. Both are library-independent, injected once at
	// construction (not per-library Bind).
	config *library.ConfigStore
	yield  *ForegroundYield

	// Backfill single-instance guard and live state. Only one provider backfill
	// runs at a time (bfRunning); its progress is mirrored here so a re-attaching
	// UI can read it via BackfillStatus and the quit guard can report it.
	bfMu       sync.Mutex
	bfRunning  bool
	bfCancel   context.CancelFunc
	bfProvider string
	bfDone     int
	bfTotal    int
	// bfPageSize overrides the keyset page size; 0 means backfillPageSize. Set only
	// by tests to exercise multi-page paging and mid-run cancellation.
	bfPageSize int
}

// Bind wires the BackupService to an open library's catalog in place.
func (s *BackupService) Bind(core *AppCore) {
	s.manager = core.Manager
	s.jobs = core.Backups
	s.assets = core.Assets
	s.db = core.DB
	s.root = core.Root
}

// NewBackupService constructs a BackupService. config and yield back the
// per-machine "Pause backups while imports run" preference (both may be nil in
// tests that do not exercise it).
func NewBackupService(manager *backup.Manager, jobs *repo.BackupRepo, assets *repo.AssetRepo, config *library.ConfigStore, yield *ForegroundYield, emitter Emitter, logger *slog.Logger) *BackupService {
	if logger == nil {
		logger = slog.Default()
	}
	return &BackupService{manager: manager, jobs: jobs, assets: assets, config: config, yield: yield, emitter: emitter, log: logger.With(slog.String("subsystem", "backup"))}
}

// QueueSummary returns the count of jobs in each status.
func (s *BackupService) QueueSummary(ctx context.Context) (QueueSummaryDTO, error) {
	if err := s.guard(); err != nil {
		return QueueSummaryDTO{}, err
	}
	return s.queueSummary(ctx)
}

func (s *BackupService) queueSummary(ctx context.Context) (QueueSummaryDTO, error) {
	counts, err := s.jobs.QueueSummary(ctx)
	if err != nil {
		return QueueSummaryDTO{}, err
	}
	statuses := make([]domain.JobStatus, len(counts))
	values := make([]int64, len(counts))
	for i, c := range counts {
		statuses[i] = c.Status
		values[i] = c.Count
	}
	summary := summaryFromCounts(statuses, values)
	var jpm float64
	var last time.Time
	if s.manager != nil {
		summary.Cooldowns = cooldownDTOs(s.manager.Cooldowns())
		summary.Yielding = s.manager.Yielding()
		jpm, last = s.manager.CompletionStats()
	}
	summary = enrichQueueSummary(ctx, s.jobs, summary, time.Now(), jpm, last)
	return summary, nil
}

// enrichQueueSummary fills the live rate/ETA fields on a base QueueSummaryDTO
// (already carrying status counts, cooldowns, and the yielding flag): the
// outstanding byte workload (SQL aggregate), the rolling completion rate and
// last-completed time, and the derived ETA. It is shared by
// BackupService.queueSummary and the OnQueueChanged emitter so the polled and
// event-pushed summaries carry identical numbers. jobsPerMinute/lastCompleted come
// from the Manager's CompletionStats; a nil jobs repo (tests) skips the byte query.
func enrichQueueSummary(ctx context.Context, jobs *repo.BackupRepo, base QueueSummaryDTO, now time.Time, jobsPerMinute float64, lastCompleted time.Time) QueueSummaryDTO {
	base.JobsPerMinute = jobsPerMinute
	if !lastCompleted.IsZero() {
		lc := lastCompleted
		base.LastCompletedAt = &lc
	}
	if jobs != nil {
		if br, err := jobs.BytesRemaining(ctx); err == nil {
			base.BytesRemaining = br
		}
	}
	// A paused, yielding, or cooling queue has no honest ETA: suppress it so the UI
	// shows the paused state instead of a stale "done ~…".
	paused := base.Yielding || len(base.Cooldowns) > 0
	if secs, at, ok := backupETA(now, base.Pending+base.Running, jobsPerMinute, paused); ok {
		base.EtaSeconds = secs
		ea := at
		base.EtaAt = &ea
	}
	return base
}

// backupETA estimates the seconds until remainingJobs drain at jobsPerMinute and
// the corresponding wall-clock completion instant (now + eta). It returns
// ok=false (and zero values) when backups are paused, there is no remaining work,
// or the rate is non-positive/degenerate — so callers render "—" and never
// "done ~Infinity".
func backupETA(now time.Time, remainingJobs int64, jobsPerMinute float64, paused bool) (etaSeconds int64, etaAt time.Time, ok bool) {
	if paused || remainingJobs <= 0 || jobsPerMinute <= 0 || math.IsInf(jobsPerMinute, 0) || math.IsNaN(jobsPerMinute) {
		return 0, time.Time{}, false
	}
	secs := float64(remainingJobs) / jobsPerMinute * 60.0
	if secs <= 0 || math.IsInf(secs, 0) || math.IsNaN(secs) {
		return 0, time.Time{}, false
	}
	etaSeconds = int64(math.Ceil(secs))
	return etaSeconds, now.Add(time.Duration(etaSeconds) * time.Second), true
}

// PauseBackupsDuringForeground reports the per-machine "Pause backups while
// imports run" preference (default true). Read from the live gate so it reflects
// any in-session toggle immediately.
func (s *BackupService) PauseBackupsDuringForeground(ctx context.Context) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	if s.yield != nil {
		return s.yield.Enabled(), nil
	}
	if s.config == nil {
		return true, nil
	}
	cfg, err := s.config.Load()
	if err != nil {
		return false, err
	}
	return cfg.PauseBackupsDuringForegroundEnabled(), nil
}

// SetPauseBackupsDuringForeground persists the per-machine preference to
// library.Config and applies it to the live backup yield gate immediately
// (mirroring how thumbnail parallelism is live-applied). It returns the stored
// value.
func (s *BackupService) SetPauseBackupsDuringForeground(ctx context.Context, on bool) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	if s.config != nil {
		cfg, err := s.config.Load()
		if err != nil {
			return false, err
		}
		v := on
		cfg.PauseBackupsDuringForeground = &v
		if err := s.config.Save(cfg); err != nil {
			return false, err
		}
	}
	s.yield.SetEnabled(on) // nil-safe
	s.log.Info("pause backups during foreground changed", "enabled", on)
	// Nudge the Backup Queue UI so its yield banner reflects the new setting
	// without waiting for the next poll.
	if s.jobs != nil {
		if summary, sErr := s.queueSummary(ctx); sErr == nil {
			emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
		}
	}
	return on, nil
}

// SessionBackupStatusDTO is the live per-session backup progress the import
// completion panel shows ("N of M backed up"). TotalAssets counts the session's
// backup-eligible assets (those with an archive copy); BackedUp counts those
// whose aggregate BackupStatus is complete — the same signal safe-to-erase uses
// per asset. Complete is true when every eligible asset is backed up (or there
// are none to back up), i.e. the usual clear-after-import blocker has cleared.
type SessionBackupStatusDTO struct {
	SessionID   string `json:"sessionId"`
	TotalAssets int    `json:"totalAssets"`
	BackedUp    int    `json:"backedUp"`
	Complete    bool   `json:"complete"`
}

// SessionBackupStatus reports how many of a session's backup-eligible assets are
// fully backed up, so the import completion panel can show live progress and
// enable evaluation once backups drain.
func (s *BackupService) SessionBackupStatus(ctx context.Context, sessionID string) (SessionBackupStatusDTO, error) {
	if err := s.guard(); err != nil {
		return SessionBackupStatusDTO{}, err
	}
	if sessionID == "" {
		return SessionBackupStatusDTO{}, fmt.Errorf("services: session backup status: empty session id")
	}
	total, complete, err := s.assets.SessionBackupCounts(ctx, sessionID)
	if err != nil {
		return SessionBackupStatusDTO{}, err
	}
	return SessionBackupStatusDTO{
		SessionID:   sessionID,
		TotalAssets: int(total),
		BackedUp:    int(complete),
		Complete:    complete >= total,
	}, nil
}

// ListJobs returns a page of backup jobs (optionally filtered by status), joined
// with each job's asset filename and archive path. An empty statusFilter lists
// all statuses.
func (s *BackupService) ListJobs(ctx context.Context, statusFilter string, page, pageSize int) (PageResult[BackupJobDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[BackupJobDTO]{}, err
	}
	limit, offset := normalizePage(page, pageSize)

	var status *domain.JobStatus
	if statusFilter != "" {
		st := domain.JobStatus(statusFilter)
		status = &st
	}

	rows, total, err := s.jobs.ListJobs(ctx, status, repo.Page{Limit: limit, Offset: offset})
	if err != nil {
		return PageResult[BackupJobDTO]{}, err
	}

	items := make([]BackupJobDTO, 0, len(rows))
	for _, j := range rows {
		dto := BackupJobDTO{
			ID:           j.ID,
			AssetID:      j.AssetID,
			Plugin:       j.Plugin,
			Destination:  j.Destination,
			Status:       string(j.Status),
			Retries:      j.Retries,
			StartedAt:    j.StartedAt,
			CompletedAt:  j.CompletedAt,
			ErrorMessage: j.ErrorMessage,
		}
		if asset, err := s.assets.GetByID(ctx, j.AssetID); err == nil {
			dto.Filename = asset.OriginalFilename
			dto.ArchivePath = library.ResolvePath(s.root, asset.CurrentArchivePath)
		}
		items = append(items, dto)
	}
	return PageResult[BackupJobDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// Retry requeues a failed job immediately.
func (s *BackupService) Retry(ctx context.Context, jobID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	return s.mutate(ctx, s.manager.Retry(ctx, jobID))
}

// RetryAllFailed requeues every failed backup job to pending in one transition
// and returns the number requeued. It emits backup:queue-changed on success. It
// composes with provider cooldowns and the foreground yield gate naturally (those
// gate claiming, not status): requeued jobs wait pending until claiming resumes.
func (s *BackupService) RetryAllFailed(ctx context.Context) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	n, err := s.manager.RetryAllFailed(ctx)
	if err != nil {
		return 0, err
	}
	if summary, sErr := s.queueSummary(ctx); sErr == nil {
		emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
	}
	s.log.Info("retried all failed backup jobs", "count", n)
	return n, nil
}

// CancelAllPending cancels every pending or paused backup job in one transition
// and returns the number cancelled, mirroring RetryAllFailed's shape. Jobs
// currently running (uploading) are NOT touched — an in-flight upload finishes
// normally. It emits backup:queue-changed on success. Cancellation is a soft status
// flip (rows are never hard-deleted); the archived originals are untouched and
// backups can be re-queued later.
func (s *BackupService) CancelAllPending(ctx context.Context) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	n, err := s.manager.CancelAllPending(ctx)
	if err != nil {
		return 0, err
	}
	if summary, sErr := s.queueSummary(ctx); sErr == nil {
		emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
	}
	s.log.Info("cancelled all pending/paused backup jobs", "count", n)
	return n, nil
}

// RequeueOptedOut reverses a per-import provider opt-out: it flips opted-out
// backup jobs for the given provider back to pending so they upload ("Queue
// anyway"). When sessionID is non-empty the reversal is scoped to the assets
// imported under that session; an empty sessionID requeues every opted-out job
// for the provider. It returns the number of jobs requeued and emits
// backup:queue-changed so the queue and provider cards refresh. Per-session
// scoping from the UI is deferred to the coming Coverage view; the plumbing is in
// place here.
func (s *BackupService) RequeueOptedOut(ctx context.Context, providerID, sessionID string) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	if providerID == "" {
		return 0, fmt.Errorf("services: requeue opted-out: empty provider id")
	}
	n, err := s.jobs.RequeueOptedOut(ctx, providerID, sessionID)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if summary, sErr := s.queueSummary(ctx); sErr == nil {
			emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
		}
	}
	s.log.Info("requeued opted-out backups", "provider", providerID, "session", sessionID, "count", n)
	return int(n), nil
}

// Pause pauses a pending job.
func (s *BackupService) Pause(ctx context.Context, jobID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	return s.mutate(ctx, s.manager.Pause(ctx, jobID))
}

// Resume resumes a paused job.
func (s *BackupService) Resume(ctx context.Context, jobID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	return s.mutate(ctx, s.manager.Resume(ctx, jobID))
}

// Cancel cancels a job from any non-terminal state.
func (s *BackupService) Cancel(ctx context.Context, jobID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	return s.mutate(ctx, s.manager.Cancel(ctx, jobID))
}

// PauseAll pauses every pending job.
func (s *BackupService) PauseAll(ctx context.Context) error {
	if err := s.guard(); err != nil {
		return err
	}
	return s.mutate(ctx, s.manager.PauseAll(ctx))
}

// ResumeAll resumes every paused job.
func (s *BackupService) ResumeAll(ctx context.Context) error {
	if err := s.guard(); err != nil {
		return err
	}
	return s.mutate(ctx, s.manager.ResumeAll(ctx))
}

// mutate emits backup:queue-changed after a successful state transition and
// returns the original error otherwise.
func (s *BackupService) mutate(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	if summary, sErr := s.queueSummary(ctx); sErr == nil {
		emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
	}
	return nil
}

// activeOps reports the backup jobs currently uploading (transferring bytes) plus
// a running backfill (enqueue of a provider's missing jobs) for the quit guard.
// The pending queue is deliberately not reported: pending jobs resume next launch
// and must not nag the user at quit. One OperationInfo is emitted per in-flight
// upload, and one for an active backfill.
func (s *BackupService) activeOps() []OperationInfo {
	var out []OperationInfo
	if s.manager != nil {
		for _, j := range s.manager.RunningJobs() {
			out = append(out, OperationInfo{
				Kind:       OpKindBackupUpload,
				Label:      "Uploading a backup",
				BytesDone:  j.BytesDone,
				BytesTotal: j.BytesTotal,
			})
		}
	}
	s.bfMu.Lock()
	if s.bfRunning {
		out = append(out, OperationInfo{
			Kind:       OpKindBackupBackfill,
			Label:      "Queueing missing backups",
			FilesDone:  s.bfDone,
			FilesTotal: s.bfTotal,
		})
	}
	s.bfMu.Unlock()
	return out
}

// cancelActive cancels a running backfill (resumable — a later run enqueues the
// remainder) but is a no-op for uploads: uploads are never aborted mid-flight, and
// a job still running at shutdown is reverted to pending by the manager's Stop
// (and startup recovery), then resumes next launch. The quit guard's bounded grace
// wait simply proceeds after the deadline.
func (s *BackupService) cancelActive() {
	s.bfMu.Lock()
	cancel := s.bfCancel
	s.bfMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// NewBackupQueueChangedEmitter returns a backup.Options.OnQueueChanged callback
// that emits a backup:queue-changed event carrying the current queue summary.
// main.go wires it into the Manager so background job transitions (completion,
// failure, requeue, reset) refresh the UI without waiting for its poll. It is
// invoked (throttled) from Manager goroutines, so it must not block; a single
// summary query plus emit is acceptable. The payload matches BackupService.mutate
// so the frontend handles both identically.
// cooldownsFn, when non-nil, supplies the current provider cooldowns so the
// emitted summary carries them; yieldingFn, when non-nil, supplies the current
// foreground-yield state; statsFn, when non-nil, supplies the Manager's rolling
// completion rate and last-completed time so the pushed summary carries the same
// live rate/ETA the poll does. All are wired in main.go/appcore after the Manager
// exists (the Manager owns that state and invokes this emitter).
func NewBackupQueueChangedEmitter(emitter Emitter, jobs *repo.BackupRepo, cooldownsFn func() []ProviderCooldownDTO, yieldingFn func() bool, statsFn func() (float64, time.Time)) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		counts, err := jobs.QueueSummary(ctx)
		if err != nil {
			return
		}
		statuses := make([]domain.JobStatus, len(counts))
		values := make([]int64, len(counts))
		for i, c := range counts {
			statuses[i] = c.Status
			values[i] = c.Count
		}
		summary := summaryFromCounts(statuses, values)
		if cooldownsFn != nil {
			summary.Cooldowns = cooldownsFn()
		}
		if yieldingFn != nil {
			summary.Yielding = yieldingFn()
		}
		var jpm float64
		var last time.Time
		if statsFn != nil {
			jpm, last = statsFn()
		}
		summary = enrichQueueSummary(ctx, jobs, summary, time.Now(), jpm, last)
		emitSafe(emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
	}
}

// NewBackupProgressEmitter returns a backup.Options.ProgressFn that emits
// throttled backup:progress events. main.go passes it into the Manager so upload
// progress reaches the frontend without the Manager depending on the emitter.
func NewBackupProgressEmitter(emitter Emitter) func(jobID string, bytesDone, bytesTotal int64) {
	tr := newThrottle()
	return func(jobID string, bytesDone, bytesTotal int64) {
		if bytesTotal > 0 && bytesDone >= bytesTotal {
			// Always emit the final byte of a job so the bar reaches 100%.
			emitSafe(emitter, EventBackupProgress, BackupProgress{JobID: jobID, BytesDone: bytesDone, BytesTotal: bytesTotal})
			return
		}
		if tr.allow() {
			emitSafe(emitter, EventBackupProgress, BackupProgress{JobID: jobID, BytesDone: bytesDone, BytesTotal: bytesTotal})
		}
	}
}

// NewProviderFailingEmitter returns a backup.Options.OnProviderFailing callback
// that emits a backup:provider-failing event when the Manager reports a provider
// crossing the failing edge. The Manager already throttles the edge (at most once
// per provider per failingNotifyInterval) and calls this on a detached goroutine,
// so it may do a bounded catalog read to resolve a human label for the toast.
// main.go/appcore wires it after the catalog is open.
func NewProviderFailingEmitter(emitter Emitter, db *gorm.DB) func(providerID string) {
	return func(providerID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		emitSafe(emitter, EventBackupProviderFailing, BackupProviderFailing{
			ProviderID:   providerID,
			ProviderName: providerDisplayName(ctx, db, providerID),
		})
	}
}

// providerDisplayName resolves a short human label for a provider (for the
// failing toast): the rclone remote or localfs root from its config when present,
// else the plugin name, else the raw ID. Best-effort — any failure degrades to
// the next fallback.
func providerDisplayName(ctx context.Context, db *gorm.DB, providerID string) string {
	if db == nil {
		return providerID
	}
	var p domain.BackupProvider
	if err := db.WithContext(ctx).First(&p, "id = ?", providerID).Error; err != nil {
		return providerID
	}
	var cfg struct {
		Remote string `json:"remote"`
		Root   string `json:"root"`
	}
	if json.Unmarshal([]byte(p.ConfigJSON), &cfg) == nil {
		if cfg.Remote != "" {
			return cfg.Remote
		}
		if cfg.Root != "" {
			return cfg.Root
		}
	}
	if p.PluginName != "" {
		return p.PluginName
	}
	return providerID
}
