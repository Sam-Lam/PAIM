package services

import (
	"context"
	"errors"
	"time"

	"github.com/Sam-Lam/PAIM/internal/library"
)

// ErrBulkResolveInProgress is returned by StartBulkResolve when a bulk resolve is
// already running (only one may run at a time).
var ErrBulkResolveInProgress = errors.New("services: a bulk duplicate resolve is already running")

// bulkResolveRun is the in-memory state of a single background bulk-resolve job,
// mirroring the safeEraseRun/clearSourceRun pattern: running while the goroutine
// is live, then the terminal summary/cancelled with a completion time for the
// re-attachment TTL (safeEraseReportTTL).
type bulkResolveRun struct {
	action    string
	progress  *BulkResolveProgress
	summary   *BulkResolveSummaryDTO
	cancelled bool
	running   bool
	at        time.Time
}

// StartBulkResolveDTO is returned immediately once a bulk resolve launches.
type StartBulkResolveDTO struct {
	Action string `json:"action"`
	Total  int    `json:"total"`
}

// ActiveBulkResolveDTO is the re-attachment snapshot for a bulk resolve: "running"
// (Progress holds the latest snapshot), "completed" (Summary, possibly Cancelled),
// or "none". A completed snapshot lapses to "none" after safeEraseReportTTL.
type ActiveBulkResolveDTO struct {
	State     string                 `json:"state"`
	Progress  *BulkResolveProgress   `json:"progress"`
	Summary   *BulkResolveSummaryDTO `json:"summary"`
	Cancelled bool                   `json:"cancelled"`
}

// StartBulkResolve launches a background job that applies action to every
// duplicate in ids (delete → trash + soft-delete, move → relocate, ignore /
// keep_both → clear the flag), reusing the exact per-pair resolve logic. Only one
// bulk resolve may run at a time (ErrBulkResolveInProgress otherwise). It is
// cancellable between items (CancelBulkResolve), activity-tracked (quit guard),
// and sleep-guarded; per-item failures are collected and never abort the batch.
// Files are never hard-deleted (delete moves to the trash dir) and DB rows are
// only soft-deleted, exactly as the per-pair action does.
func (s *DuplicateService) StartBulkResolve(ctx context.Context, ids []string, action, destFolder string) (StartBulkResolveDTO, error) {
	if err := s.guard(); err != nil {
		return StartBulkResolveDTO{}, err
	}
	switch action {
	case DuplicateActionDelete, DuplicateActionMove, DuplicateActionIgnore, DuplicateActionKeepBoth:
	default:
		return StartBulkResolveDTO{}, errors.New("services: bulk resolve: unknown action " + action)
	}
	if action == DuplicateActionMove && destFolder == "" {
		return StartBulkResolveDTO{}, errors.New("services: bulk resolve: move requires a destination folder")
	}
	ids = dedupeNonEmpty(ids)
	if len(ids) == 0 {
		return StartBulkResolveDTO{}, errors.New("services: bulk resolve: no duplicates selected")
	}

	s.mu.Lock()
	if s.bulkActive {
		s.mu.Unlock()
		return StartBulkResolveDTO{}, ErrBulkResolveInProgress
	}
	s.bulkActive = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.bulkCancel = cancel
	s.bulkRun = &bulkResolveRun{action: action, running: true, at: time.Now()}
	s.mu.Unlock()

	s.log.Info("bulk duplicate resolve starting", "action", action, "count", len(ids))
	s.sleep.Acquire()
	go s.runBulkResolve(runCtx, ids, action, destFolder)
	return StartBulkResolveDTO{Action: action, Total: len(ids)}, nil
}

// runBulkResolve applies action to each id in turn, honoring cancellation between
// items, emitting throttled duplicates:progress, and a terminal duplicates:completed
// carrying the summary (including every per-item failure).
func (s *DuplicateService) runBulkResolve(ctx context.Context, ids []string, action, destFolder string) {
	defer func() {
		s.mu.Lock()
		s.bulkActive = false
		s.bulkCancel = nil
		s.mu.Unlock()
		s.sleep.Release()
	}()

	tr := newThrottle()
	total := len(ids)
	failures := make([]BulkResolveFailure, 0)
	succeeded := 0

	setProgress := func(done int, current string) {
		p := BulkResolveProgress{
			Action:      action,
			Done:        done,
			Total:       total,
			Succeeded:   succeeded,
			Failed:      len(failures),
			CurrentFile: current,
		}
		s.mu.Lock()
		if s.bulkRun != nil {
			s.bulkRun.progress = &p
		}
		s.mu.Unlock()
		if done == total || tr.allow() {
			emitSafe(s.emitter, EventBulkResolveProgress, p)
		}
	}
	setProgress(0, "")

	cancelled := false
	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		asset, err := s.assets.GetByID(ctx, id)
		if err != nil {
			failures = append(failures, BulkResolveFailure{AssetID: id, Error: err.Error()})
			s.log.Warn("bulk resolve: load asset failed", "assetId", id, "error", err.Error())
			setProgress(i+1, id)
			continue
		}
		archiveAbs := library.ResolvePath(s.root, asset.CurrentArchivePath)
		if err := s.applyResolution(ctx, id, archiveAbs, action, destFolder); err != nil {
			failures = append(failures, BulkResolveFailure{
				AssetID:  id,
				Filename: asset.OriginalFilename,
				Path:     archiveAbs,
				Error:    err.Error(),
			})
			s.log.Warn("bulk resolve: item failed", "assetId", id, "action", action, "error", err.Error())
		} else {
			succeeded++
		}
		setProgress(i+1, asset.OriginalFilename)
	}
	if ctx.Err() != nil {
		cancelled = true
	}

	summary := &BulkResolveSummaryDTO{
		Action:    action,
		Total:     total,
		Succeeded: succeeded,
		Failed:    len(failures),
		Cancelled: cancelled,
		Failures:  failures,
	}
	s.mu.Lock()
	if s.bulkRun != nil {
		s.bulkRun.running = false
		s.bulkRun.summary = summary
		s.bulkRun.cancelled = cancelled
		s.bulkRun.at = time.Now()
	}
	s.mu.Unlock()

	s.log.Info("bulk duplicate resolve finished",
		"action", action, "total", total, "succeeded", succeeded, "failed", len(failures), "cancelled", cancelled)
	emitSafe(s.emitter, EventBulkResolveCompleted, *summary)
}

// ActiveBulkResolve returns the current bulk-resolve state for re-attachment:
// "running" with the latest progress snapshot, "completed" with the summary (or a
// cancelled marker), or "none". A completed snapshot lapses after safeEraseReportTTL.
func (s *DuplicateService) ActiveBulkResolve(ctx context.Context) (ActiveBulkResolveDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.bulkRun
	if r == nil {
		return ActiveBulkResolveDTO{State: "none"}, nil
	}
	if r.running {
		dto := ActiveBulkResolveDTO{State: "running"}
		if r.progress != nil {
			p := *r.progress
			dto.Progress = &p
		}
		return dto, nil
	}
	if time.Since(r.at) > safeEraseReportTTL {
		s.bulkRun = nil
		return ActiveBulkResolveDTO{State: "none"}, nil
	}
	return ActiveBulkResolveDTO{
		State:     "completed",
		Summary:   r.summary,
		Cancelled: r.cancelled,
	}, nil
}

// CancelBulkResolve cancels a running bulk resolve (if any). Items already
// resolved stay resolved; the job stops cleanly before the next item.
func (s *DuplicateService) CancelBulkResolve(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.bulkCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// activeOps reports a running bulk resolve for the quit guard.
func (s *DuplicateService) activeOps() []OperationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bulkActive || s.bulkRun == nil {
		return nil
	}
	info := OperationInfo{Kind: OpKindDuplicateResolve, Label: "Resolving duplicates in bulk"}
	if s.bulkRun.progress != nil {
		info.FilesDone = s.bulkRun.progress.Done
		info.FilesTotal = s.bulkRun.progress.Total
	}
	return []OperationInfo{info}
}

// cancelActive cancels a running bulk resolve via its existing cancel path.
func (s *DuplicateService) cancelActive() { _ = s.CancelBulkResolve(context.Background()) }

// dedupeNonEmpty returns ids with empty strings dropped and duplicates removed,
// preserving first-seen order.
func dedupeNonEmpty(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
