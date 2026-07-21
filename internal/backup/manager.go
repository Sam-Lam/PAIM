package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// Default option values.
const (
	defaultWorkers      = 2
	defaultMaxRetries   = 5
	defaultBaseBackoff  = 2 * time.Second
	defaultPollInterval = 50 * time.Millisecond
	maxBackoffExponent  = 20 // caps BaseBackoff<<exp to avoid overflow

	// recordTimeout bounds the detached contexts used to record terminal
	// job/asset state (see recordCtx).
	recordTimeout = 10 * time.Second

	// queueChangeInterval throttles OnQueueChanged notifications so a burst of job
	// transitions cannot flood the UI with refresh signals.
	queueChangeInterval = 500 * time.Millisecond
)

// notifyQueueChanged invokes the OnQueueChanged callback (if configured) at most
// once per queueChangeInterval, without blocking the caller. It is called after
// any job state transition the Manager performs so the UI can refresh queue
// counts without waiting for its poll.
func (m *Manager) notifyQueueChanged() {
	if m.opts.OnQueueChanged == nil {
		return
	}
	m.mu.Lock()
	now := time.Now()
	if !m.lastQueueNotify.IsZero() && now.Sub(m.lastQueueNotify) < queueChangeInterval {
		m.mu.Unlock()
		return
	}
	m.lastQueueNotify = now
	m.mu.Unlock()
	go m.opts.OnQueueChanged()
}

// recordCtx returns a short-lived context detached from the worker context, used
// to record terminal job/asset state (MarkCompleted/MarkFailed and the asset's
// recomputed aggregate BackupStatus).
//
// These writes happen after the point of no return — the upload has already
// succeeded or definitively failed — so they must not be abortable by worker-ctx
// cancellation: GORM opens its per-write transaction with the supplied context,
// and database/sql auto-rolls-back a transaction whose context is cancelled. If
// the worker ctx were used, a graceful Stop racing with the write would abort it
// mid-transaction ("transaction has already been committed or rolled back"),
// leaving a completed upload unrecorded or an aggregate status stale. This is
// the same principle as the fresh background context Stop already uses for
// ResetRunningOnStartup. The worker context is still used for the interruptible
// work itself (claiming, plugin resolution, upload, verify).
func recordCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), recordTimeout)
}

// errNoPlugin marks a permanent (non-retryable) failure caused by a job
// referencing a plugin name that is not registered.
var errNoPlugin = errors.New("no plugin registered for name")

// Options configures a Manager. Zero values fall back to sensible defaults.
type Options struct {
	// Workers is the number of concurrent upload workers (default 2).
	Workers int
	// MaxRetries is the number of times a job is retried before being left
	// failed (default 5). A job that has failed MaxRetries times stays failed
	// with its last ErrorMessage.
	MaxRetries int
	// BaseBackoff is the base delay for the exponential backoff between retries
	// (default 2s). The delay for the k-th retry is BaseBackoff*2^k plus jitter.
	BaseBackoff time.Duration
	// PollInterval is how often idle workers poll the queue for pending jobs and
	// how often the backoff scheduler runs (default 50ms).
	PollInterval time.Duration
	// LibraryRoot, when set, is used to compute a job's remote-relative path as
	// the archive path relative to this root so backups mirror the library tree.
	// When empty (or when the path is not under it), the cleaned absolute path
	// with leading separators stripped is used instead.
	LibraryRoot string
	// ProgressFn, when set, receives per-job upload progress. It is called from a
	// worker goroutine and must be non-blocking / concurrency-safe. The services
	// layer uses it to emit throttled backup:progress events.
	ProgressFn func(jobID string, bytesDone, bytesTotal int64)
	// OnQueueChanged, when set, is invoked (throttled to at most one call per
	// queueChangeInterval, and non-blocking) after any job state transition the
	// Manager performs itself — completion, failure, requeue-after-backoff, and
	// reset-on-start/stop. It lets the UI refresh queue counts promptly instead of
	// waiting for its periodic poll. It is called from Manager goroutines and must
	// not block them.
	OnQueueChanged func()
}

func (o Options) withDefaults() Options {
	if o.Workers <= 0 {
		o.Workers = defaultWorkers
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = defaultMaxRetries
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = defaultBaseBackoff
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	return o
}

// Manager runs the backup worker pool over the SQLite-persisted job queue. It is
// restart-safe: on start it reverts jobs left running (from a crash) to pending,
// and on stop it does the same for in-flight jobs it could not finish.
//
// Retry backoff is held in memory (BackupJob has no next-attempt column): a job
// that fails transiently is marked failed, then requeued to pending only once
// its backoff elapses. A process restart therefore resets outstanding backoff
// timers and requeues those jobs immediately, which is acceptable per the spec.
type Manager struct {
	jobs      JobQueue
	assets    AssetStore
	providers ProviderStore
	registry  *Registry
	log       *slog.Logger
	opts      Options

	// pluginMu guards pluginCache. Plugins are initialized once per provider on
	// first use; initialization (Initialize+Authenticate) is serialized, which is
	// acceptable because it is rare and, for localfs, effectively instantaneous.
	pluginMu    sync.Mutex
	pluginCache map[string]Plugin // providerID -> initialized plugin

	// mu guards backoff and lastQueueNotify.
	mu sync.Mutex
	// backoff maps a failed job ID to the time at which it may be requeued. The
	// scheduler goroutine promotes elapsed entries back to pending.
	backoff map[string]time.Time
	// lastQueueNotify is the time OnQueueChanged was last invoked; it throttles
	// queue-change notifications (see notifyQueueChanged).
	lastQueueNotify time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewManager constructs a Manager. registry must be non-nil; a nil logger falls
// back to slog.Default(). ResetRunningOnStartup is invoked when Start runs.
func NewManager(jobs JobQueue, assets AssetStore, providers ProviderStore, registry *Registry, logger *slog.Logger, opts Options) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		jobs:        jobs,
		assets:      assets,
		providers:   providers,
		registry:    registry,
		log:         logger,
		opts:        opts.withDefaults(),
		pluginCache: make(map[string]Plugin),
		backoff:     make(map[string]time.Time),
	}
}

// Start reverts orphaned running jobs to pending and launches the worker pool
// plus the backoff scheduler. The workers run until the given context is
// cancelled or Stop is called. Start returns after the goroutines are launched.
func (m *Manager) Start(ctx context.Context) error {
	if n, err := m.jobs.ResetRunningOnStartup(ctx); err != nil {
		return fmt.Errorf("backup: reset running jobs on start: %w", err)
	} else if n > 0 {
		m.notifyQueueChanged()
	}

	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	for i := 0; i < m.opts.Workers; i++ {
		m.wg.Add(1)
		go m.worker(runCtx)
	}
	m.wg.Add(1)
	go m.runScheduler(runCtx)

	m.log.Info("backup manager started", "workers", m.opts.Workers, "maxRetries", m.opts.MaxRetries)
	return nil
}

// Stop cancels the workers, waits for in-flight uploads to finish, and reverts
// any job still marked running (an upload interrupted by the cancellation) back
// to pending using the same semantics as startup recovery.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := m.jobs.ResetRunningOnStartup(ctx); err != nil {
		m.log.Error("backup: reset running jobs on stop", "error", err)
	} else if n > 0 {
		m.notifyQueueChanged()
	}
	m.log.Info("backup manager stopped")
}

// worker claims and processes pending jobs until the context is cancelled.
func (m *Manager) worker(ctx context.Context) {
	defer m.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := m.jobs.NextPending(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Error("backup: claim next pending job", "error", err)
			if !m.idle(ctx) {
				return
			}
			continue
		}
		if job == nil {
			if !m.idle(ctx) {
				return
			}
			continue
		}
		m.handle(ctx, job)
	}
}

// idle sleeps for one poll interval, returning false if the context is cancelled
// while waiting.
func (m *Manager) idle(ctx context.Context) bool {
	t := time.NewTimer(m.opts.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// handle processes a claimed job and applies the outcome: completion is recorded
// inside process; a failure is marked, then either scheduled for retry (with
// backoff) or left permanently failed once retries are exhausted or the failure
// is permanent.
func (m *Manager) handle(ctx context.Context, job *domain.BackupJob) {
	terminal, err := m.process(ctx, job)
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		// Shutting down mid-upload: leave the job running so Stop() (and the next
		// startup) reverts it to pending. Do not burn a retry on a cancellation.
		return
	}

	// Record the failure on a detached context (see recordCtx): the outcome is
	// already decided and must be persisted even if a shutdown begins now.
	recCtx, cancel := recordCtx()
	defer cancel()

	msg := err.Error()
	if markErr := m.jobs.MarkFailed(recCtx, job.ID, msg); markErr != nil {
		m.log.Error("backup: mark job failed", "job", job.ID, "error", markErr)
		return
	}
	m.notifyQueueChanged()

	newRetries := job.Retries + 1
	if terminal || newRetries >= m.opts.MaxRetries {
		m.log.Warn("backup: job permanently failed",
			"job", job.ID, "asset", job.AssetID, "retries", newRetries, "terminal", terminal, "error", msg)
		m.recomputeAsset(recCtx, job.AssetID)
		return
	}
	m.log.Warn("backup: job failed, scheduling retry",
		"job", job.ID, "asset", job.AssetID, "retries", newRetries, "error", msg)
	m.scheduleRetry(job.ID, job.Retries)
}

// process performs one full attempt: resolve provider -> plugin -> asset, upload,
// verify (when supported), and mark the job completed while recomputing the
// asset's aggregate backup status. It returns (terminal, err): terminal is true
// when the error is permanent and must not be retried.
func (m *Manager) process(ctx context.Context, job *domain.BackupJob) (terminal bool, err error) {
	provider, err := m.providers.GetByID(ctx, job.Destination)
	if err != nil {
		return true, fmt.Errorf("resolve provider %q: %w", job.Destination, err)
	}
	if !provider.Enabled {
		return true, fmt.Errorf("provider %q is disabled", job.Destination)
	}

	plugin, err := m.pluginFor(ctx, provider)
	if err != nil {
		return errors.Is(err, errNoPlugin), fmt.Errorf("plugin for provider %q: %w", provider.ID, err)
	}

	asset, err := m.assets.GetByID(ctx, job.AssetID)
	if err != nil {
		return true, fmt.Errorf("resolve asset %q: %w", job.AssetID, err)
	}
	if asset.CurrentArchivePath == "" {
		return true, fmt.Errorf("asset %q has no archive path", job.AssetID)
	}

	caps := plugin.Capabilities()
	if caps.MaxFileSize > 0 && asset.FileSize > caps.MaxFileSize {
		return true, fmt.Errorf("asset %q size %d exceeds plugin max %d", job.AssetID, asset.FileSize, caps.MaxFileSize)
	}

	remoteRel := m.remoteRelPath(asset)
	localPath := library.ResolvePath(m.opts.LibraryRoot, asset.CurrentArchivePath)
	progress := func(done, total int64) {
		if m.opts.ProgressFn != nil {
			m.opts.ProgressFn(job.ID, done, total)
		}
	}

	if err := plugin.Upload(ctx, localPath, remoteRel, progress); err != nil {
		return false, fmt.Errorf("upload asset %q to %q: %w", job.AssetID, remoteRel, err)
	}

	if caps.SupportsVerify {
		ok, err := plugin.Verify(ctx, localPath, remoteRel)
		if err != nil {
			return false, fmt.Errorf("verify asset %q at %q: %w", job.AssetID, remoteRel, err)
		}
		if !ok {
			return false, fmt.Errorf("verify asset %q at %q: destination does not match source", job.AssetID, remoteRel)
		}
	}

	// Point of no return: the upload is verified. Record the outcome on a
	// detached context (see recordCtx) so a graceful shutdown cannot abort the
	// bookkeeping and leave a completed upload unrecorded.
	recCtx, cancel := recordCtx()
	defer cancel()

	// Respect a concurrent cancel: if the job left the running state while we were
	// uploading (e.g. Cancel), do not record it as completed. The uploaded file is
	// a complete, verified copy and is left in place harmlessly.
	fresh, err := m.jobs.GetByID(recCtx, job.ID)
	if err != nil {
		return false, fmt.Errorf("reload job %q: %w", job.ID, err)
	}
	if fresh.Status != domain.JobStatusRunning {
		m.log.Info("backup: job left running state during upload; not marking completed",
			"job", job.ID, "status", fresh.Status)
		m.recomputeAsset(recCtx, job.AssetID)
		return false, nil
	}

	if err := m.jobs.MarkCompleted(recCtx, job.ID); err != nil {
		return false, fmt.Errorf("mark job %q completed: %w", job.ID, err)
	}
	m.recomputeAsset(recCtx, job.AssetID)
	m.notifyQueueChanged()
	m.log.Info("backup: job completed", "job", job.ID, "asset", job.AssetID, "plugin", plugin.Name())
	return false, nil
}

// pluginFor returns the initialized plugin for a provider, constructing and
// caching it on first use. A missing registration is a permanent error
// (errNoPlugin); Initialize/Authenticate failures are transient and are not
// cached, so a later retry re-attempts initialization.
func (m *Manager) pluginFor(ctx context.Context, provider *domain.BackupProvider) (Plugin, error) {
	m.pluginMu.Lock()
	defer m.pluginMu.Unlock()

	if p, ok := m.pluginCache[provider.ID]; ok {
		return p, nil
	}

	p, ok := m.registry.New(provider.PluginName)
	if !ok {
		return nil, fmt.Errorf("%w: %q", errNoPlugin, provider.PluginName)
	}
	if err := p.Initialize(ctx, provider.ConfigJSON); err != nil {
		return nil, fmt.Errorf("initialize %q: %w", provider.PluginName, err)
	}
	if err := p.Authenticate(ctx); err != nil {
		return nil, fmt.Errorf("authenticate %q: %w", provider.PluginName, err)
	}
	m.pluginCache[provider.ID] = p
	return p, nil
}

// remoteRelPath computes the destination-relative path for an asset so backups
// mirror the library tree. Portable-library archive paths are stored relative to
// the root already, so they ARE the remote-relative path (normalized to forward
// slashes). A legacy absolute path is made relative to LibraryRoot when possible,
// otherwise reduced to a leading-separator-stripped clean path.
func (m *Manager) remoteRelPath(asset *domain.Asset) string {
	p := asset.CurrentArchivePath
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p))
	}
	if m.opts.LibraryRoot != "" {
		if rel, err := filepath.Rel(m.opts.LibraryRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	return strings.TrimLeft(clean, "/")
}

// scheduleRetry records the time at which a failed job may be requeued.
// priorRetries is the job's retry count before this failure, so the first retry
// waits BaseBackoff and each subsequent one doubles, with additive jitter.
func (m *Manager) scheduleRetry(jobID string, priorRetries int) {
	exp := priorRetries
	if exp > maxBackoffExponent {
		exp = maxBackoffExponent
	}
	delay := m.opts.BaseBackoff * time.Duration(int64(1)<<uint(exp))
	jitter := time.Duration(rand.Int63n(int64(delay/2) + 1))
	readyAt := time.Now().Add(delay + jitter)

	m.mu.Lock()
	m.backoff[jobID] = readyAt
	m.mu.Unlock()
}

// runScheduler periodically requeues failed jobs whose backoff has elapsed.
func (m *Manager) runScheduler(ctx context.Context) {
	defer m.wg.Done()

	t := time.NewTicker(m.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.promoteReady(ctx)
		}
	}
}

// promoteReady requeues every failed job whose backoff has elapsed.
func (m *Manager) promoteReady(ctx context.Context) {
	now := time.Now()
	var ready []string
	m.mu.Lock()
	for id, at := range m.backoff {
		if !at.After(now) {
			ready = append(ready, id)
			delete(m.backoff, id)
		}
	}
	m.mu.Unlock()

	requeued := 0
	for _, id := range ready {
		if err := m.jobs.Requeue(ctx, id); err != nil {
			// The job may have been cancelled or already retried; that is fine.
			if !errors.Is(err, repo.ErrNotFound) {
				m.log.Error("backup: requeue after backoff", "job", id, "error", err)
			}
			continue
		}
		requeued++
	}
	if requeued > 0 {
		m.notifyQueueChanged()
	}
}

// recomputeAsset recomputes and persists the asset's aggregate BackupStatus from
// the current state of all its jobs. Failures are logged, not fatal. Callers
// recording terminal state must pass a detached context (see recordCtx), never
// the worker context, so shutdown cannot leave the aggregate stale.
func (m *Manager) recomputeAsset(ctx context.Context, assetID string) {
	jobs, err := m.jobs.JobsForAsset(ctx, assetID)
	if err != nil {
		m.log.Error("backup: load jobs for asset status", "asset", assetID, "error", err)
		return
	}
	status := AggregateBackupStatus(jobs)
	if err := m.assets.UpdateBackupStatus(ctx, assetID, status); err != nil {
		m.log.Error("backup: update asset backup status", "asset", assetID, "error", err)
	}
}

// AggregateBackupStatus reduces an asset's jobs to a single aggregate
// BackupStatus. Cancelled jobs are excluded (a cancelled destination is no
// longer a required backup). The rules:
//
//   - no active jobs            -> none
//   - every active job complete -> complete
//   - some complete, some not   -> partial
//   - none complete, any failed -> failed
//   - otherwise (pending/paused/running) -> pending
func AggregateBackupStatus(jobs []domain.BackupJob) domain.BackupStatus {
	var total, completed, failed int
	for _, j := range jobs {
		if j.Status == domain.JobStatusCancelled {
			continue
		}
		total++
		switch j.Status {
		case domain.JobStatusCompleted:
			completed++
		case domain.JobStatusFailed:
			failed++
		}
	}
	switch {
	case total == 0:
		return domain.BackupStatusNone
	case completed == total:
		return domain.BackupStatusComplete
	case completed > 0:
		return domain.BackupStatusPartial
	case failed > 0:
		return domain.BackupStatusFailed
	default:
		return domain.BackupStatusPending
	}
}

// EnqueueForAsset enqueues one backup job per enabled provider for the given
// asset, inside the caller's transaction. It is idempotent (repo.Enqueue skips
// duplicates), so it is safe to call during import even after a crash-and-resume.
// It returns the number of jobs newly created so the importer can set the
// asset's BackupStatus to pending only when there is real backup work (with no
// enabled providers it creates zero jobs and the caller records none). This
// signature is injected into the importer and must remain stable.
func (m *Manager) EnqueueForAsset(ctx context.Context, tx *gorm.DB, assetID string) (int, error) {
	providers, err := m.providers.WithTx(tx).ListEnabled(ctx)
	if err != nil {
		return 0, fmt.Errorf("backup: list providers for asset %q: %w", assetID, err)
	}
	q := m.jobs.WithTx(tx)
	created := 0
	for _, p := range providers {
		_, wasCreated, err := q.Enqueue(ctx, assetID, p.PluginName, p.ID)
		if err != nil {
			return created, fmt.Errorf("backup: enqueue job for asset %q provider %q: %w", assetID, p.ID, err)
		}
		if wasCreated {
			created++
		}
	}
	return created, nil
}

// Pause moves a pending job to paused so workers stop claiming it. A job that is
// already running finishes its current upload (uploads are never aborted
// mid-flight); pause it once it returns to a pending state to prevent further
// retries.
func (m *Manager) Pause(ctx context.Context, jobID string) error {
	return m.jobs.Pause(ctx, jobID)
}

// Resume moves a paused job back to pending.
func (m *Manager) Resume(ctx context.Context, jobID string) error {
	return m.jobs.Resume(ctx, jobID)
}

// PauseAll pauses every currently pending job.
func (m *Manager) PauseAll(ctx context.Context) error {
	pending := domain.JobStatusPending
	jobs, _, err := m.jobs.ListJobs(ctx, &pending, repo.Page{})
	if err != nil {
		return fmt.Errorf("backup: list pending jobs: %w", err)
	}
	for _, j := range jobs {
		if err := m.jobs.Pause(ctx, j.ID); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return fmt.Errorf("backup: pause job %q: %w", j.ID, err)
		}
	}
	return nil
}

// ResumeAll resumes every currently paused job.
func (m *Manager) ResumeAll(ctx context.Context) error {
	paused := domain.JobStatusPaused
	jobs, _, err := m.jobs.ListJobs(ctx, &paused, repo.Page{})
	if err != nil {
		return fmt.Errorf("backup: list paused jobs: %w", err)
	}
	for _, j := range jobs {
		if err := m.jobs.Resume(ctx, j.ID); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return fmt.Errorf("backup: resume job %q: %w", j.ID, err)
		}
	}
	return nil
}

// Retry requeues a failed job immediately, clearing any pending backoff timer.
func (m *Manager) Retry(ctx context.Context, jobID string) error {
	m.mu.Lock()
	delete(m.backoff, jobID)
	m.mu.Unlock()
	return m.jobs.Requeue(ctx, jobID)
}

// Cancel cancels a job (from any non-terminal state), clearing any pending
// backoff timer. A job currently running finishes its upload but will not be
// recorded as completed (see process).
func (m *Manager) Cancel(ctx context.Context, jobID string) error {
	m.mu.Lock()
	delete(m.backoff, jobID)
	m.mu.Unlock()
	return m.jobs.Cancel(ctx, jobID)
}
