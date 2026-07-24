package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
)

// ErrBackfillInProgress is returned when a provider backfill is requested while
// one is already running. Only one runs at a time (a second request is refused
// rather than queued); the running one can be cancelled or awaited.
var ErrBackfillInProgress = errors.New("services: a backup backfill is already running")

// backfillPageSize is how many eligible assets are read (and enqueued) per keyset
// page. Each page is enqueued inside one transaction for insert throughput, and a
// page boundary is a cancellation/resume checkpoint.
const backfillPageSize = 1000

// BackfillStatusDTO is the re-attachment snapshot of the current provider backfill
// (or the initial status returned by StartBackfill). Done/Total count eligible
// assets scanned; ProviderID names the destination being filled.
type BackfillStatusDTO struct {
	Running    bool   `json:"running"`
	ProviderID string `json:"providerId"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
}

// BackfillStatus returns the current backfill state so a re-attaching UI can
// resume showing inline progress.
func (s *BackupService) BackfillStatus(ctx context.Context) (BackfillStatusDTO, error) {
	if err := s.guard(); err != nil {
		return BackfillStatusDTO{}, err
	}
	s.bfMu.Lock()
	defer s.bfMu.Unlock()
	return BackfillStatusDTO{Running: s.bfRunning, ProviderID: s.bfProvider, Done: s.bfDone, Total: s.bfTotal}, nil
}

// StartBackfill enqueues a backup job for every eligible asset that does not yet
// have one for the given (enabled) provider — closing the gap left when a provider
// is added AFTER a library is already populated (import-time enqueue only covers
// assets imported while the provider existed). It runs in the background: activity-
// tracked (quit-guard aware), sleep-guarded, cancellable, and resumable — a
// cancelled run re-scans from the start next time and skips what it already
// enqueued (Enqueue is idempotent). Only one backfill runs at a time
// (ErrBackfillInProgress otherwise).
//
// Eligibility mirrors the importer exactly: non-deleted, verified assets with an
// archive copy (CurrentArchivePath <> ”). Copy-mode duplicate placeholders (empty
// path) are excluded; adopt-flagged duplicates (which carry a path) are included.
// Each backfilled job is stamped with the same capture/import-date SortKey the
// importer uses (backup.SortKeyForAsset) so it honors the provider's upload order.
func (s *BackupService) StartBackfill(ctx context.Context, providerID string) (BackfillStatusDTO, error) {
	if err := s.guard(); err != nil {
		return BackfillStatusDTO{}, err
	}
	if providerID == "" {
		return BackfillStatusDTO{}, fmt.Errorf("services: start backfill: empty provider id")
	}

	// The provider must exist and be enabled: a disabled destination's jobs would
	// never run, and EnqueueForAsset only targets enabled providers, so backfilling
	// a disabled one would silently pile up unrunnable work.
	var provider domain.BackupProvider
	if err := s.db.WithContext(ctx).First(&provider, "id = ?", providerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return BackfillStatusDTO{}, fmt.Errorf("services: start backfill: provider %q not found", providerID)
		}
		return BackfillStatusDTO{}, fmt.Errorf("services: start backfill: load provider %q: %w", providerID, err)
	}
	if !provider.Enabled {
		return BackfillStatusDTO{}, fmt.Errorf("services: start backfill: provider %q is disabled", providerID)
	}

	// The eligible total (progress denominator) is scope-aware: a scoped provider
	// backfills only its in-scope kinds, so out-of-scope assets never enter the scan.
	total, err := s.assets.CountEligibleForBackup(ctx, provider.MediaScope)
	if err != nil {
		return BackfillStatusDTO{}, err
	}

	s.bfMu.Lock()
	if s.bfRunning {
		s.bfMu.Unlock()
		return BackfillStatusDTO{}, ErrBackfillInProgress
	}
	// Detach from the request context so the job outlives the binding call; it is
	// cancelled via CancelBackfill / the quit guard.
	runCtx, cancel := context.WithCancel(context.Background())
	s.bfRunning = true
	s.bfCancel = cancel
	s.bfProvider = providerID
	s.bfDone = 0
	s.bfTotal = int(total)
	s.bfMu.Unlock()

	s.sleep.Acquire()
	go s.runBackfill(runCtx, provider.ID, provider.PluginName, provider.MediaScope, int(total))

	return BackfillStatusDTO{Running: true, ProviderID: providerID, Done: 0, Total: int(total)}, nil
}

// CancelBackfill cancels the active backfill (if any). It is a no-op when nothing
// is running. The partially-enqueued jobs are kept (they are valid work); a later
// StartBackfill enqueues the remainder.
func (s *BackupService) CancelBackfill(ctx context.Context) error {
	s.bfMu.Lock()
	cancel := s.bfCancel
	s.bfMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// runBackfill is the background driver: it pages eligible assets by ID and
// enqueues an idempotent job per asset for the provider (scope-aware — the page
// query filters to the provider's in-scope kinds), then emits a terminal
// backup:backfill-progress tick and a backup:backfill-completed event with the
// final {enqueued, skipped} counts.
func (s *BackupService) runBackfill(ctx context.Context, providerID, pluginName, scope string, total int) {
	var enqueued, skipped int
	cancelled := false

	defer func() {
		s.bfMu.Lock()
		done := s.bfDone
		s.bfRunning = false
		s.bfCancel = nil
		s.bfMu.Unlock()
		s.sleep.Release()

		// Terminal progress tick so the UI clears its running indicator, then the
		// completion event with the outcome counts for the toast.
		emitSafe(s.emitter, EventBackupBackfillProgress, BackupBackfillProgress{
			ProviderID: providerID, Done: done, Total: total, Running: false,
		})
		emitSafe(s.emitter, EventBackupBackfillCompleted, BackupBackfillCompleted{
			ProviderID: providerID, Enqueued: enqueued, Skipped: skipped, Cancelled: cancelled,
		})
		// A backfill that created work changes the queue: refresh the summary so the
		// Backup Queue reflects the new pending jobs immediately.
		if enqueued > 0 && s.manager != nil {
			if summary, err := s.queueSummary(context.Background()); err == nil {
				emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
			}
		}
		s.log.Info("backup backfill finished",
			"provider", providerID, "enqueued", enqueued, "skipped", skipped, "cancelled", cancelled)
	}()

	enqueued, skipped, cancelled = s.scanAndEnqueue(ctx, providerID, pluginName, scope, total)
}

// scanAndEnqueue pages the provider's in-scope eligible assets by ID and enqueues
// an idempotent job per asset, one transaction per page. It updates s.bfDone and
// emits throttled backup:backfill-progress ticks as it scans. It returns the
// newly-created and already-present (skipped) counts and whether the run was
// cancelled early. It is shared by runBackfill and runReconcile so the "enqueue
// every missing in-scope job" path is identical for both (and so a backfill and a
// reconcile can never run concurrently — both hold the bf single-instance guard).
func (s *BackupService) scanAndEnqueue(ctx context.Context, providerID, pluginName, scope string, total int) (enqueued, skipped int, cancelled bool) {
	pageSize := s.bfPageSize
	if pageSize <= 0 {
		pageSize = backfillPageSize
	}
	tr := newThrottle()
	afterID := ""
	scanned := 0
	for {
		if ctx.Err() != nil {
			return enqueued, skipped, true
		}
		page, err := s.assets.EligibleForBackupPage(ctx, afterID, pageSize, scope)
		if err != nil {
			if ctx.Err() != nil {
				cancelled = true
			} else {
				s.log.Error("backup backfill: read eligible page", "provider", providerID, "after", afterID, "error", err.Error())
			}
			return enqueued, skipped, cancelled
		}
		if len(page) == 0 {
			return enqueued, skipped, cancelled // drained: all in-scope eligible assets scanned
		}

		// Enqueue this page inside one transaction for insert throughput. Enqueue is
		// idempotent, so a partially-committed page re-runs harmlessly on resume.
		pageEnqueued, pageSkipped, err := s.backfillPage(ctx, providerID, pluginName, page)
		enqueued += pageEnqueued
		skipped += pageSkipped
		scanned += len(page)
		afterID = page[len(page)-1].ID
		if err != nil {
			if ctx.Err() != nil {
				cancelled = true
			} else {
				s.log.Error("backup backfill: enqueue page", "provider", providerID, "error", err.Error())
			}
			return enqueued, skipped, cancelled
		}

		s.bfMu.Lock()
		s.bfDone = scanned
		s.bfMu.Unlock()
		if tr.allow() {
			emitSafe(s.emitter, EventBackupBackfillProgress, BackupBackfillProgress{
				ProviderID: providerID, Done: scanned, Total: total, Running: true,
			})
		}

		if len(page) < pageSize {
			return enqueued, skipped, cancelled // last (short) page
		}
	}
}

// backfillPage enqueues one page of assets for the provider inside a single
// transaction, returning how many jobs were newly created versus skipped
// (already-enqueued/completed pairs). The SortKey mirrors the importer's.
func (s *BackupService) backfillPage(ctx context.Context, providerID, pluginName string, page []domain.Asset) (enqueued, skipped int, err error) {
	err = s.db.Transaction(func(tx *gorm.DB) error {
		q := s.jobs.WithTx(tx)
		for i := range page {
			asset := page[i]
			sortKey := backup.SortKeyForAsset(asset)
			_, created, eErr := q.Enqueue(ctx, asset.ID, pluginName, providerID, sortKey)
			if eErr != nil {
				return fmt.Errorf("enqueue asset %q: %w", asset.ID, eErr)
			}
			if created {
				enqueued++
			} else {
				skipped++
			}
		}
		return nil
	})
	if err != nil {
		// Counts inside the closure are not committed on error; report zero so the
		// caller does not double-count a rolled-back page.
		return 0, 0, err
	}
	return enqueued, skipped, nil
}
