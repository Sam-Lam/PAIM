package services

import (
	"context"
	"log/slog"

	"github.com/autolinepro/paim/internal/backup"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
)

// BackupService inspects and controls the SQLite-persisted backup queue via the
// backup Manager. State-changing methods emit backup:queue-changed so the UI can
// refresh; per-upload progress is emitted as backup:progress by the Manager's
// ProgressFn (wired in main.go, see NewBackupProgressEmitter).
type BackupService struct {
	manager *backup.Manager
	jobs    *repo.BackupRepo
	assets  *repo.AssetRepo
	emitter Emitter
	log     *slog.Logger
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

// ListJobs returns a page of backup jobs (optionally filtered by status), joined
// with each job's asset filename and archive path. An empty statusFilter lists
// all statuses.
func (s *BackupService) ListJobs(ctx context.Context, statusFilter string, page, pageSize int) (PageResult[BackupJobDTO], error) {
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
			dto.ArchivePath = asset.CurrentArchivePath
		}
		items = append(items, dto)
	}
	return PageResult[BackupJobDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// Retry requeues a failed job immediately.
func (s *BackupService) Retry(ctx context.Context, jobID string) error {
	return s.mutate(ctx, s.manager.Retry(ctx, jobID))
}

// Pause pauses a pending job.
func (s *BackupService) Pause(ctx context.Context, jobID string) error {
	return s.mutate(ctx, s.manager.Pause(ctx, jobID))
}

// Resume resumes a paused job.
func (s *BackupService) Resume(ctx context.Context, jobID string) error {
	return s.mutate(ctx, s.manager.Resume(ctx, jobID))
}

// Cancel cancels a job from any non-terminal state.
func (s *BackupService) Cancel(ctx context.Context, jobID string) error {
	return s.mutate(ctx, s.manager.Cancel(ctx, jobID))
}

// PauseAll pauses every pending job.
func (s *BackupService) PauseAll(ctx context.Context) error {
	return s.mutate(ctx, s.manager.PauseAll(ctx))
}

// ResumeAll resumes every paused job.
func (s *BackupService) ResumeAll(ctx context.Context) error {
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
