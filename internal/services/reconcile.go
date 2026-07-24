package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"gorm.io/gorm"
)

// outOfScopeNote is stamped into ErrorMessage on a job a reconcile cancels because
// its asset's kind is no longer in the provider's scope. It explains the cancel in
// the Backup Queue rather than reading as a failure.
const outOfScopeNote = "out of provider scope"

// ReconcilePreviewDTO is the dry-run outcome of a queue reconcile for a provider:
// how many jobs would be cancelled (pending/paused jobs whose asset is now OUT of
// the provider's scope) and how many would be enqueued (in-scope eligible assets
// with no job yet). Both are computed from the provider's CURRENT stored scope, so
// the UI saves a scope change first, then previews.
type ReconcilePreviewDTO struct {
	ProviderID string `json:"providerId"`
	ToCancel   int    `json:"toCancel"`
	ToEnqueue  int    `json:"toEnqueue"`
}

// PreviewReconcile reports what a "recalculate queue" would do for the provider,
// without changing anything: ToCancel counts its pending/paused jobs now out of
// scope, ToEnqueue counts its in-scope eligible assets missing a job. It is the
// count behind the inline "Scope changed — recalculate? (X to remove, Y to add)"
// prompt. An empty ("all kinds") scope has nothing out of scope, so ToCancel is 0.
func (s *BackupService) PreviewReconcile(ctx context.Context, providerID string) (ReconcilePreviewDTO, error) {
	if err := s.guard(); err != nil {
		return ReconcilePreviewDTO{}, err
	}
	if providerID == "" {
		return ReconcilePreviewDTO{}, fmt.Errorf("services: preview reconcile: empty provider id")
	}
	provider, err := s.loadProvider(ctx, providerID)
	if err != nil {
		return ReconcilePreviewDTO{}, err
	}

	scopedExts := mediatype.ScopedExtensions(provider.MediaScope)
	toCancel, err := s.jobs.CountOutOfScopePending(ctx, providerID, scopedExts)
	if err != nil {
		return ReconcilePreviewDTO{}, err
	}
	toEnqueue, err := s.assets.CountEligibleMissingBackup(ctx, providerID, provider.MediaScope)
	if err != nil {
		return ReconcilePreviewDTO{}, err
	}
	return ReconcilePreviewDTO{
		ProviderID: providerID,
		ToCancel:   int(toCancel),
		ToEnqueue:  int(toEnqueue),
	}, nil
}

// StartReconcile brings the provider's queue into line with its current scope in
// the background: it (a) cancels its pending/paused jobs whose asset is now out of
// scope (batch transition, stamping outOfScopeNote), then (b) runs the scope-aware
// backfill path to enqueue every in-scope eligible asset that lacks a job. It
// reuses the backfill single-instance guard, so a reconcile and a backfill can
// never run concurrently (ErrBackfillInProgress otherwise). Progress reuses the
// backup:backfill-progress events (so a re-attaching UI shows the same inline
// bar); completion emits a backup:reconcile-completed event carrying {cancelled,
// enqueued} for the toast, plus backup:queue-changed.
//
// The out-of-scope cancel is a DERIVED reconciliation of jobs enqueued under a
// wider scope — it is NOT a per-import opt-out (those durable opted_out rows are
// left untouched; only pending/paused jobs are reclaimed). This mirrors the
// enqueue path, where scope exclusion is likewise derived, never recorded.
func (s *BackupService) StartReconcile(ctx context.Context, providerID string) (BackfillStatusDTO, error) {
	if err := s.guard(); err != nil {
		return BackfillStatusDTO{}, err
	}
	if providerID == "" {
		return BackfillStatusDTO{}, fmt.Errorf("services: start reconcile: empty provider id")
	}
	provider, err := s.loadProvider(ctx, providerID)
	if err != nil {
		return BackfillStatusDTO{}, err
	}
	if !provider.Enabled {
		return BackfillStatusDTO{}, fmt.Errorf("services: start reconcile: provider %q is disabled", providerID)
	}

	total, err := s.assets.CountEligibleForBackup(ctx, provider.MediaScope)
	if err != nil {
		return BackfillStatusDTO{}, err
	}

	s.bfMu.Lock()
	if s.bfRunning {
		s.bfMu.Unlock()
		return BackfillStatusDTO{}, ErrBackfillInProgress
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.bfRunning = true
	s.bfCancel = cancel
	s.bfProvider = providerID
	s.bfDone = 0
	s.bfTotal = int(total)
	s.bfMu.Unlock()

	s.sleep.Acquire()
	go s.runReconcile(runCtx, provider.ID, provider.PluginName, provider.MediaScope, int(total))

	return BackfillStatusDTO{Running: true, ProviderID: providerID, Done: 0, Total: int(total)}, nil
}

// runReconcile is the background driver for StartReconcile: cancel out-of-scope
// pending/paused jobs, then enqueue in-scope missing jobs (the shared scanAndEnqueue
// path). It emits a terminal backfill-progress tick (to clear the inline bar) and a
// reconcile-completed event with the outcome counts, and refreshes the queue summary
// when anything changed.
func (s *BackupService) runReconcile(ctx context.Context, providerID, pluginName, scope string, total int) {
	var cancelled, enqueued int
	aborted := false

	defer func() {
		s.bfMu.Lock()
		done := s.bfDone
		s.bfRunning = false
		s.bfCancel = nil
		s.bfMu.Unlock()
		s.sleep.Release()

		emitSafe(s.emitter, EventBackupBackfillProgress, BackupBackfillProgress{
			ProviderID: providerID, Done: done, Total: total, Running: false,
		})
		emitSafe(s.emitter, EventBackupReconcileCompleted, BackupReconcileCompleted{
			ProviderID: providerID, Cancelled: cancelled, Enqueued: enqueued, Aborted: aborted,
		})
		if (cancelled > 0 || enqueued > 0) && s.manager != nil {
			if summary, err := s.queueSummary(context.Background()); err == nil {
				emitSafe(s.emitter, EventBackupQueueChanged, BackupQueueChanged{Summary: summary})
			}
		}
		s.log.Info("backup reconcile finished",
			"provider", providerID, "cancelled", cancelled, "enqueued", enqueued, "aborted", aborted)
	}()

	// Phase 1: cancel out-of-scope pending/paused jobs (one batched UPDATE). Fast and
	// not resumable-sensitive; do it before the enqueue scan so the queue shrinks
	// first. An empty ("all kinds") scope has no out-of-scope jobs -> a no-op.
	if ctx.Err() == nil {
		n, err := s.jobs.CancelOutOfScopePending(ctx, providerID, outOfScopeNote, mediatype.ScopedExtensions(scope))
		if err != nil {
			s.log.Error("backup reconcile: cancel out-of-scope jobs", "provider", providerID, "error", err.Error())
		} else {
			cancelled = int(n)
		}
	}

	// Phase 2: enqueue in-scope missing jobs via the shared backfill scan.
	enqueued, _, aborted = s.scanAndEnqueue(ctx, providerID, pluginName, scope, total)
}

// loadProvider loads a provider by ID, mapping a missing row to a clear error. It
// is the shared lookup for the reconcile entry points.
func (s *BackupService) loadProvider(ctx context.Context, providerID string) (*domain.BackupProvider, error) {
	var provider domain.BackupProvider
	if err := s.db.WithContext(ctx).First(&provider, "id = ?", providerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("services: provider %q not found", providerID)
		}
		return nil, fmt.Errorf("services: load provider %q: %w", providerID, err)
	}
	return &provider, nil
}
