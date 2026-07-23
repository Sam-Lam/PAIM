package services

import (
	"context"
	"fmt"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
)

// ReorganizeMoveDTO is one planned same-drive move in a reorganize preview.
type ReorganizeMoveDTO struct {
	AssetID  string `json:"assetId"`
	Filename string `json:"filename"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// ReorganizeSkipDTO is one asset the reorganize will not move, with the reason
// (missing-on-disk, cross-volume, duplicate).
type ReorganizeSkipDTO struct {
	AssetID  string `json:"assetId"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
}

// ReorganizePlanDTO is the JSON-friendly reorganize preview: aggregate counts
// plus a display-capped sample of moves and skips. Truncated reports whether the
// move sample was cut to DisplayCap (the counts remain exact).
type ReorganizePlanDTO struct {
	TotalAssets   int                 `json:"totalAssets"`
	InPlace       int                 `json:"inPlace"`
	Moves         int                 `json:"moves"`
	Skipped       int                 `json:"skipped"`
	DisplayCap    int                 `json:"displayCap"`
	Truncated     bool                `json:"truncated"`
	MovesSample   []ReorganizeMoveDTO `json:"movesSample"`
	SkippedSample []ReorganizeSkipDTO `json:"skippedSample"`
}

// PlanReorganize computes a non-mutating reorganize preview from the catalog and
// caches it so a subsequent StartReorganize reuses the exact plan. eventName is
// the (optional) event folder segment applied to the standard layout; empty
// yields date-only YYYY-MM-DD folders (subject to the labels/sticky rules).
// useSourceFolderLabels turns on "labels survive reorganize": when eventName is
// empty, each file's label is derived from its current parent folder name. The
// returned DTO caps its move/skip sample at reorgDisplayCap while the counts
// reflect the full plan.
func (s *ImportService) PlanReorganize(ctx context.Context, eventName string, useSourceFolderLabels bool) (*ReorganizePlanDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Emit throttled determinate progress while planning. The call stays
	// synchronous — the events reach the frontend over the event bridge during the
	// in-flight call, independent of the returned promise — so the Settings page
	// can render a live bar and a Cancel (via CancelImport-style ctx cancellation
	// of this call) without a background job.
	tr := newThrottle()
	progressFn := func(done, total int) {
		if done == total || tr.allow() {
			emitSafe(s.emitter, EventReorganizePlan, ReorganizePlanProgress{Done: done, Total: total})
		}
	}
	plan, err := s.pipeline.PlanReorganize(ctx, importer.ReorganizeOptions{
		EventName:             eventName,
		UseSourceFolderLabels: useSourceFolderLabels,
	}, progressFn)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.reorgPlan = plan
	s.reorgPlanAt = time.Now()
	s.reorgEvent = eventName
	s.reorgLabels = useSourceFolderLabels
	s.mu.Unlock()

	return reorganizePlanDTO(plan), nil
}

// reorganizePlanDTO projects an importer plan into the display DTO.
func reorganizePlanDTO(plan *importer.ReorganizePlan) *ReorganizePlanDTO {
	dto := &ReorganizePlanDTO{
		TotalAssets: plan.TotalAssets,
		InPlace:     plan.InPlace,
		Moves:       plan.Moves,
		Skipped:     plan.Skipped,
		DisplayCap:  reorgDisplayCap,
		Truncated:   plan.Moves > reorgDisplayCap,
	}
	for _, e := range plan.Entries {
		switch e.Kind {
		case importer.MoveMove:
			if len(dto.MovesSample) < reorgDisplayCap {
				dto.MovesSample = append(dto.MovesSample, ReorganizeMoveDTO{
					AssetID:  e.AssetID,
					Filename: e.Filename,
					From:     e.From,
					To:       e.To,
				})
			}
		case importer.MoveSkip:
			if len(dto.SkippedSample) < reorgDisplayCap {
				dto.SkippedSample = append(dto.SkippedSample, ReorganizeSkipDTO{
					AssetID:  e.AssetID,
					Filename: e.Filename,
					Path:     e.From,
					Reason:   e.Reason,
				})
			}
		}
	}
	return dto
}

// StartReorganize creates a reorganize session (returning its ID immediately)
// and runs it in the background under the SAME one-active-operation guard as
// imports, emitting import:progress (phase "reorganizing") and import:completed.
// It reuses the plan cached by a recent PlanReorganize; a stale or missing plan
// is recomputed. Cancel via CancelImport.
func (s *ImportService) StartReorganize(ctx context.Context) (StartImportResult, error) {
	if err := s.guard(); err != nil {
		return StartImportResult{}, err
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartImportResult{}, ErrImportInProgress
	}
	// Reserve the active slot before the (fast) session insert so no second start
	// slips in; release it if creation fails.
	s.active = true
	s.opKind = OpKindReorganize
	plan := s.reorgPlan
	event := s.reorgEvent
	labels := s.reorgLabels
	fresh := plan != nil && time.Since(s.reorgPlanAt) < reorgPlanTTL
	s.mu.Unlock()

	if !fresh {
		// Recompute from the catalog with the last-previewed event/labels (or empty).
		recomputed, err := s.pipeline.PlanReorganize(ctx, importer.ReorganizeOptions{
			EventName:             event,
			UseSourceFolderLabels: labels,
		}, nil)
		if err != nil {
			s.mu.Lock()
			s.active = false
			s.mu.Unlock()
			return StartImportResult{}, err
		}
		plan = recomputed
	}

	sessionID, err := s.newReorgSession(ctx)
	if err != nil {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
		return StartImportResult{}, err
	}

	s.mu.Lock()
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.sessionID = sessionID
	s.current = nil
	s.analyze = nil   // a new reorganize supersedes any retained analyze snapshot
	s.reorgPlan = nil // consumed
	s.mu.Unlock()

	s.sleep.Acquire()
	go s.runReorganize(runCtx, sessionID, plan)
	return StartImportResult{SessionID: sessionID}, nil
}

// newReorgSession creates the ImportSession row a reorganize run drives, marking
// its Notes with mode "reorganize" so Import History badges it distinctly.
func (s *ImportService) newReorgSession(ctx context.Context) (string, error) {
	session := &domain.ImportSession{
		StartedAt: time.Now(),
		Status:    domain.SessionStatusRunning,
		Notes:     resumeState{Mode: string(importer.ModeReorganize)}.encode(),
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return "", fmt.Errorf("services: create reorganize session: %w", err)
	}
	s.log.Info("reorganize session created", "sessionId", session.ID)
	return session.ID, nil
}

// runReorganize executes the reorganize against sessionID and emits
// import:completed when done, mirroring run() but with the reorganizing phase.
func (s *ImportService) runReorganize(ctx context.Context, sessionID string, plan *importer.ReorganizePlan) {
	defer func() {
		s.mu.Lock()
		s.active = false
		s.cancel = nil
		s.mu.Unlock()
		s.sleep.Release()
	}()

	tr := newThrottle()
	progress := func(p importer.Progress) {
		dto := ImportProgress{
			SessionID:   sessionID,
			Phase:       string(p.Phase),
			FilesDone:   p.FilesDone,
			FilesTotal:  p.FilesTotal,
			BytesDone:   p.BytesDone,
			BytesTotal:  p.BytesTotal,
			CurrentFile: p.CurrentFile,
			Errors:      p.Errors,
			Percent:     percent(int64(p.FilesDone), int64(p.FilesTotal)),
			Done:        p.Phase == importer.PhaseDone,
		}
		s.mu.Lock()
		s.current = &dto
		s.mu.Unlock()

		if dto.Done || tr.allow() {
			emitSafe(s.emitter, EventImportProgress, dto)
		}
	}

	if _, err := s.pipeline.RunReorganizeSession(ctx, sessionID, plan, progress); err != nil {
		s.log.Error("reorganize run failed", "sessionId", sessionID, "error", err.Error())
	}

	s.emitCompleted(sessionID)
}
