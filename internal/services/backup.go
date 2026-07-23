package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// BackupService inspects and controls the SQLite-persisted backup queue via the
// backup Manager. State-changing methods emit backup:queue-changed so the UI can
// refresh; per-upload progress is emitted as backup:progress by the Manager's
// ProgressFn (wired in main.go, see NewBackupProgressEmitter).
type BackupService struct {
	gated
	manager *backup.Manager
	jobs    *repo.BackupRepo
	assets  *repo.AssetRepo
	emitter Emitter
	log     *slog.Logger
	root    string
}

// Bind wires the BackupService to an open library's catalog in place.
func (s *BackupService) Bind(core *AppCore) {
	s.manager = core.Manager
	s.jobs = core.Backups
	s.assets = core.Assets
	s.root = core.Root
}

// NewBackupService constructs a BackupService.
func NewBackupService(manager *backup.Manager, jobs *repo.BackupRepo, assets *repo.AssetRepo, emitter Emitter, logger *slog.Logger) *BackupService {
	if logger == nil {
		logger = slog.Default()
	}
	return &BackupService{manager: manager, jobs: jobs, assets: assets, emitter: emitter, log: logger.With(slog.String("subsystem", "backup"))}
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
	return summaryFromCounts(statuses, values), nil
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

// activeOps reports the backup jobs currently uploading (transferring bytes) for
// the quit guard. The pending queue is deliberately not reported: pending jobs
// resume next launch and must not nag the user at quit. One OperationInfo is
// emitted per in-flight upload.
func (s *BackupService) activeOps() []OperationInfo {
	if s.manager == nil {
		return nil
	}
	jobs := s.manager.RunningJobs()
	out := make([]OperationInfo, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, OperationInfo{
			Kind:       "backup_upload",
			Label:      "Uploading a backup",
			BytesDone:  j.BytesDone,
			BytesTotal: j.BytesTotal,
		})
	}
	return out
}

// cancelActive is a no-op for backups: uploads are never aborted mid-flight, and
// a job still running at shutdown is reverted to pending by the manager's Stop
// (and startup recovery), then resumes next launch. The quit guard's bounded
// grace wait simply proceeds after the deadline.
func (s *BackupService) cancelActive() {}

// NewBackupQueueChangedEmitter returns a backup.Options.OnQueueChanged callback
// that emits a backup:queue-changed event carrying the current queue summary.
// main.go wires it into the Manager so background job transitions (completion,
// failure, requeue, reset) refresh the UI without waiting for its poll. It is
// invoked (throttled) from Manager goroutines, so it must not block; a single
// summary query plus emit is acceptable. The payload matches BackupService.mutate
// so the frontend handles both identically.
func NewBackupQueueChangedEmitter(emitter Emitter, jobs *repo.BackupRepo) func() {
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
		emitSafe(emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summaryFromCounts(statuses, values)})
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
