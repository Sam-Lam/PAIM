package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/cleanup"
)

// ErrCleanupInProgress is returned by StartCleanupAnalyze when an analysis is
// already running (only one cleanup analysis is permitted at a time).
var ErrCleanupInProgress = errors.New("services: a cleanup analysis is already running")

// cleanupReportTTL bounds how long a completed cleanup report is retained for
// re-attachment (ActiveCleanupAnalyze) after the analysis finishes.
const cleanupReportTTL = 15 * time.Minute

// cleanupRun is the in-memory state of a single background cleanup analysis.
// running is true while the goroutine is live; once it finishes, report/err/
// cancelled hold the terminal outcome and at records the completion time.
type cleanupRun struct {
	root      string
	progress  *CleanupProgress
	report    *CleanupReportDTO
	err       string
	cancelled bool
	running   bool
	at        time.Time
}

// CleanupService runs the read-only Cleanup Assistant analysis over a folder.
//
// v1 is advisory only: there is deliberately NO server-side delete method. The
// Analyze result carries a recommendation the UI presents; any actual deletion
// is the user's responsibility in Finder. A DeleteAnalyzedFolder method is
// intentionally not implemented.
//
// Analysis is a re-attachable background job (StartCleanupAnalyze /
// ActiveCleanupAnalyze / CancelCleanupAnalyze) because hashing a large folder
// against the archive takes minutes; the synchronous Analyze remains for simple
// callers and tests.
type CleanupService struct {
	gated
	sleepAware
	analyzer *cleanup.Analyzer
	dialog   Dialoger
	emitter  Emitter
	log      *slog.Logger

	mu     sync.Mutex
	active bool
	cancel context.CancelFunc
	run    *cleanupRun
}

// Bind wires the CleanupService to an open library's analyzer in place.
func (s *CleanupService) Bind(core *AppCore) {
	s.analyzer = core.Analyzer
}

// NewCleanupService constructs a CleanupService.
func NewCleanupService(analyzer *cleanup.Analyzer, dialog Dialoger, emitter Emitter, logger *slog.Logger) *CleanupService {
	if logger == nil {
		logger = slog.Default()
	}
	return &CleanupService{analyzer: analyzer, dialog: dialog, emitter: emitter, log: logger.With(slog.String("subsystem", "cleanup"))}
}

// PickFolder opens a native directory chooser for the folder to analyze.
func (s *CleanupService) PickFolder(ctx context.Context) (string, error) {
	if s.dialog == nil {
		return "", fmt.Errorf("services: no dialog provider configured")
	}
	return s.dialog.PickFolder(ctx, "Choose a folder to analyze")
}

// ClassStatDTO is the per-class rollup in a cleanup report.
type ClassStatDTO struct {
	Class     string   `json:"class"`
	Count     int      `json:"count"`
	Bytes     int64    `json:"bytes"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// RecommendationDTO is the delete-safety verdict.
type RecommendationDTO struct {
	SafeToDelete bool     `json:"safeToDelete"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Reasons      []string `json:"reasons"`
}

// CleanupReportDTO is the JSON-friendly cleanup analysis result.
type CleanupReportDTO struct {
	Root                string            `json:"root"`
	Classes             []ClassStatDTO    `json:"classes"`
	TotalFiles          int               `json:"totalFiles"`
	MediaFiles          int               `json:"mediaFiles"`
	NonMedia            int               `json:"nonMedia"`
	UnreadableMedia     int               `json:"unreadableMedia"`
	ArchivedNotVerified int               `json:"archivedNotVerified"`
	BackupIncomplete    int               `json:"backupIncomplete"`
	DBInconsistencies   int               `json:"dbInconsistencies"`
	Recommendation      RecommendationDTO `json:"recommendation"`
}

// Analyze performs a strictly read-only classification of every media file under
// root against the archive and returns the report plus its delete-safety
// recommendation. It is synchronous but honors ctx cancellation between files.
func (s *CleanupService) Analyze(ctx context.Context, root string) (CleanupReportDTO, error) {
	if err := s.guard(); err != nil {
		return CleanupReportDTO{}, err
	}
	if root == "" {
		return CleanupReportDTO{}, fmt.Errorf("services: cleanup analyze: empty root")
	}
	report, err := s.analyzer.Analyze(ctx, root, nil)
	if err != nil {
		return CleanupReportDTO{}, err
	}
	return newCleanupReportDTO(report), nil
}

// newCleanupReportDTO projects a cleanup.Report into the JSON-friendly DTO,
// including its delete-safety recommendation. Shared by the synchronous Analyze
// and the background StartCleanupAnalyze so the report shape is identical.
func newCleanupReportDTO(report *cleanup.Report) CleanupReportDTO {
	rec := report.Recommendation()
	classes := make([]ClassStatDTO, 0, len(cleanup.AllClasses()))
	for _, c := range cleanup.AllClasses() {
		stat := report.Class(c)
		classes = append(classes, ClassStatDTO{
			Class:     string(c),
			Count:     stat.Count,
			Bytes:     stat.Bytes,
			Files:     stat.Files,
			Truncated: stat.Truncated,
		})
	}
	return CleanupReportDTO{
		Root:                report.Root,
		Classes:             classes,
		TotalFiles:          report.TotalFiles,
		MediaFiles:          report.MediaFiles,
		NonMedia:            report.NonMedia,
		UnreadableMedia:     report.UnreadableMedia,
		ArchivedNotVerified: report.ArchivedNotVerified,
		BackupIncomplete:    report.BackupIncomplete,
		DBInconsistencies:   report.DBInconsistencies,
		Recommendation: RecommendationDTO{
			SafeToDelete: rec.SafeToDelete,
			Title:        rec.Title,
			Summary:      rec.Summary,
			Reasons:      rec.Reasons,
		},
	}
}

// StartCleanupAnalyzeResult is returned immediately from StartCleanupAnalyze once
// the background analysis has been launched. Root echoes the analyzed folder; the
// analysis is re-attached via ActiveCleanupAnalyze (only one runs at a time).
type StartCleanupAnalyzeResult struct {
	Root string `json:"root"`
}

// ActiveCleanupAnalyzeDTO is the re-attachment snapshot returned by
// ActiveCleanupAnalyze. State is "running" (Progress holds the latest snapshot),
// "completed" (Report, or Cancelled/Error, is populated), or "none". Root echoes
// the request so the frontend can restore context after navigation.
type ActiveCleanupAnalyzeDTO struct {
	State     string            `json:"state"`
	Root      string            `json:"root"`
	Progress  *CleanupProgress  `json:"progress"`
	Report    *CleanupReportDTO `json:"report"`
	Cancelled bool              `json:"cancelled"`
	Error     string            `json:"error"`
}

// StartCleanupAnalyze launches a background cleanup analysis of root. Only one may
// run at a time (ErrCleanupInProgress otherwise). It emits throttled
// cleanup:progress (verbose per-file count + current file; the total is unknown
// during the single-pass walk-and-classify, so FilesTotal stays 0) and a terminal
// cleanup:completed when it finishes; the completed report is retained for
// re-attachment (ActiveCleanupAnalyze) for cleanupReportTTL. Cancel via
// CancelCleanupAnalyze.
func (s *CleanupService) StartCleanupAnalyze(ctx context.Context, root string) (StartCleanupAnalyzeResult, error) {
	if err := s.guard(); err != nil {
		return StartCleanupAnalyzeResult{}, err
	}
	if root == "" {
		return StartCleanupAnalyzeResult{}, fmt.Errorf("services: cleanup analyze: empty root")
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartCleanupAnalyzeResult{}, ErrCleanupInProgress
	}
	s.active = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.run = &cleanupRun{root: root, running: true, at: time.Now()}
	s.mu.Unlock()

	s.sleep.Acquire()
	go s.runCleanupAnalyze(runCtx, root)
	return StartCleanupAnalyzeResult{Root: root}, nil
}

// runCleanupAnalyze performs the analysis, emitting throttled progress and a
// terminal event.
func (s *CleanupService) runCleanupAnalyze(ctx context.Context, root string) {
	defer func() {
		s.mu.Lock()
		s.active = false
		s.cancel = nil
		s.mu.Unlock()
		s.sleep.Release()
	}()

	tr := newThrottle()
	progressFn := func(processed int, path string) {
		p := CleanupProgress{FilesDone: processed, CurrentFile: path}
		s.mu.Lock()
		if s.run != nil {
			s.run.progress = &p
		}
		s.mu.Unlock()
		if tr.allow() {
			emitSafe(s.emitter, EventCleanupProgress, p)
		}
	}

	report, err := s.analyzer.Analyze(ctx, root, progressFn)
	if err != nil {
		s.finishCleanup(root, nil, err)
		return
	}
	dto := newCleanupReportDTO(report)
	s.finishCleanup(root, &dto, nil)
}

// finishCleanup records the terminal state and emits cleanup:completed. A
// context-cancelled run is reported as Cancelled (not an error).
func (s *CleanupService) finishCleanup(root string, report *CleanupReportDTO, runErr error) {
	cancelled := false
	errMsg := ""
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			cancelled = true
		} else {
			errMsg = runErr.Error()
			s.log.Warn("cleanup analyze failed", "root", root, "error", errMsg)
		}
	}
	s.mu.Lock()
	if s.run != nil {
		s.run.running = false
		s.run.report = report
		s.run.cancelled = cancelled
		s.run.err = errMsg
		s.run.at = time.Now()
	}
	s.mu.Unlock()

	emitSafe(s.emitter, EventCleanupCompleted, CleanupCompleted{
		Root:      root,
		Report:    report,
		Cancelled: cancelled,
		Error:     errMsg,
	})
}

// ActiveCleanupAnalyze returns the current analysis state for re-attachment:
// "running" with the latest progress snapshot, "completed" with the report (or a
// cancelled/failed marker), or "none". A completed snapshot lapses to "none"
// after cleanupReportTTL.
func (s *CleanupService) ActiveCleanupAnalyze(ctx context.Context) (ActiveCleanupAnalyzeDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run
	if r == nil {
		return ActiveCleanupAnalyzeDTO{State: "none"}, nil
	}
	if r.running {
		dto := ActiveCleanupAnalyzeDTO{State: "running", Root: r.root}
		if r.progress != nil {
			p := *r.progress
			dto.Progress = &p
		}
		return dto, nil
	}
	if time.Since(r.at) > cleanupReportTTL {
		s.run = nil
		return ActiveCleanupAnalyzeDTO{State: "none"}, nil
	}
	return ActiveCleanupAnalyzeDTO{
		State:     "completed",
		Root:      r.root,
		Report:    r.report,
		Cancelled: r.cancelled,
		Error:     r.err,
	}, nil
}

// CancelCleanupAnalyze cancels the active cleanup analysis (if any). It is a
// no-op when nothing is running.
func (s *CleanupService) CancelCleanupAnalyze(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// activeOps reports a running cleanup analysis for the quit guard. FilesTotal is
// 0 during the single-pass walk-and-classify (the total is not known until it
// completes), so the guard renders an indeterminate count.
func (s *CleanupService) activeOps() []OperationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.run == nil {
		return nil
	}
	info := OperationInfo{Kind: OpKindCleanup, Label: "Analyzing a folder for cleanup"}
	if s.run.progress != nil {
		info.FilesDone, info.FilesTotal = s.run.progress.FilesDone, s.run.progress.FilesTotal
	}
	return []OperationInfo{info}
}

// cancelActive cancels a running cleanup analysis via the existing
// CancelCleanupAnalyze path. It is a no-op when nothing is running.
func (s *CleanupService) cancelActive() { _ = s.CancelCleanupAnalyze(context.Background()) }
