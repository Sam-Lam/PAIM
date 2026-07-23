package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// ErrImportInProgress is returned by StartImport/ResumeSession when an import is
// already running (only one active import is permitted at a time).
var ErrImportInProgress = errors.New("services: an import is already running")

// scanEntry caches a completed scan (and any subsequent dry run) for a root.
// Entries live in memory for the process lifetime, keyed by absolute source
// root, and are superseded whenever the same root is re-scanned. They exist so
// the Import page can render dry-run stats instantly and so StartImport can
// validate the requested root against a known scan; they are dropped on restart.
type scanEntry struct {
	scan   *importer.ScanResult
	report *importer.DryRunReport
	at     time.Time
}

// ImportService drives the scan -> dry-run -> import workflow. It enforces a
// single active import: StartImport and ResumeSession reject concurrent starts.
type ImportService struct {
	gated
	sleepAware
	pipeline *importer.Pipeline
	sessions *repo.SessionRepo
	dialog   Dialoger
	emitter  Emitter
	settings *repo.SettingsRepo
	log      *slog.Logger

	// OnCompleted, when set, is invoked (synchronously, after import:completed is
	// emitted) with the finished session ID. main.go wires it to trigger the
	// post-import thumbnail warm-up. It is an exported field, not a bound method,
	// so it stays off the Wails frontend API.
	OnCompleted func(sessionID string)

	mu        sync.Mutex
	active    bool
	opKind    string // "import" | "analyze" | "reorganize" while active; for the quit guard
	cancel    context.CancelFunc
	sessionID string
	current   *ImportProgress
	cache     map[string]scanEntry

	// analyze holds the state of the most recent background analyze (scan +
	// dry run). It is running while the goroutine is live, then retained as a
	// completed/cancelled/failed snapshot until a new operation starts or
	// analyzeReportTTL elapses (see ActiveAnalyze). An analyze shares the single
	// active-operation slot with imports and reorganizes: while one runs the
	// other cannot start, and CancelImport cancels whichever is active.
	analyze *analyzeRun

	// Reorganize plan cache, filled by PlanReorganize and consumed by
	// StartReorganize so the run reuses the just-previewed plan. It is recomputed
	// when stale (see reorgPlanTTL) so a Start long after a Plan never acts on an
	// out-of-date view of the catalog.
	reorgPlan   *importer.ReorganizePlan
	reorgPlanAt time.Time
	reorgEvent  string
}

// reorgPlanTTL bounds how long a cached reorganize plan is reused between
// PlanReorganize and StartReorganize before it is recomputed.
const reorgPlanTTL = 5 * time.Minute

// analyzeReportTTL bounds how long a completed background-analyze report is
// retained for re-attachment (ActiveAnalyze) after the analyze finishes.
const analyzeReportTTL = 15 * time.Minute

// analyzeRun is the in-memory state of a single background analyze. running is
// true while the goroutine is live; once it finishes, report/err/cancelled hold
// the terminal outcome and at records the completion time for the TTL.
type analyzeRun struct {
	opts      ImportOptions    // normalized echo of the request
	progress  *ImportProgress  // latest throttled snapshot (sessionless)
	report    *DryRunReportDTO // populated on success
	err       string           // populated on failure
	cancelled bool             // populated when cancelled via CancelImport
	running   bool
	at        time.Time // completion time (for TTL) once running is false
}

// reorgDisplayCap bounds how many move/skip entries a plan DTO carries for
// display; the aggregate counts always reflect the full plan.
const reorgDisplayCap = 500

// NewImportService constructs an ImportService.
func NewImportService(pipeline *importer.Pipeline, sessions *repo.SessionRepo, settings *repo.SettingsRepo, dialog Dialoger, emitter Emitter, logger *slog.Logger) *ImportService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ImportService{
		pipeline: pipeline,
		sessions: sessions,
		dialog:   dialog,
		emitter:  emitter,
		settings: settings,
		log:      logger.With(slog.String("subsystem", "import")),
		cache:    make(map[string]scanEntry),
	}
}

// Bind wires the ImportService to an open library's importer and repos in place.
func (s *ImportService) Bind(core *AppCore) {
	s.pipeline = core.Pipeline
	s.sessions = core.Sessions
	s.settings = core.Settings
}

// ScanSummary is the provisional result of scanning a source tree, before any
// hashing or duplicate detection.
type ScanSummary struct {
	Root             string `json:"root"`
	Files            int    `json:"files"`
	Photos           int    `json:"photos"`
	Videos           int    `json:"videos"`
	RawPhotos        int    `json:"rawPhotos"`
	ProvisionalPairs int    `json:"provisionalPairs"`
	TotalBytes       int64  `json:"totalBytes"`
}

// DryRunReportDTO is the JSON-friendly projection of importer.DryRunReport. It
// omits the carried-forward hash maps (an internal optimization) and exposes the
// mode-aware planning counts.
type DryRunReportDTO struct {
	Mode             string  `json:"mode"`
	Reorganize       bool    `json:"reorganize"`
	Files            int     `json:"files"`
	Photos           int     `json:"photos"`
	Videos           int     `json:"videos"`
	AlreadyImported  int     `json:"alreadyImported"`
	Duplicates       int     `json:"duplicates"`
	New              int     `json:"new"`
	TotalImportBytes int64   `json:"totalImportBytes"`
	EstimatedSeconds float64 `json:"estimatedSeconds"`
	PlannedAdoptions int     `json:"plannedAdoptions"`
	PlannedMoves     int     `json:"plannedMoves"`
}

// ImportOptions configures StartImport. Root is the source tree. DestinationRoot
// defaults to the configured Master Library when empty. Mode is "copy" or
// "adopt"; Reorganize (adopt only) moves files into the standard layout.
type ImportOptions struct {
	Root            string `json:"root"`
	DestinationRoot string `json:"destinationRoot"`
	EventName       string `json:"eventName"`
	Mode            string `json:"mode"`
	Reorganize      bool   `json:"reorganize"`
	SourceID        string `json:"sourceId"`
}

// StartImportResult is returned immediately from StartImport once the session is
// created and the background run has been launched.
type StartImportResult struct {
	SessionID string `json:"sessionId"`
}

// StartAnalyzeResult is returned immediately from StartAnalyze once the
// background analyze has been launched. Root echoes the resolved source root; an
// analyze is sessionless (no ImportSession is created), so it is re-attached via
// ActiveAnalyze rather than by a session ID.
type StartAnalyzeResult struct {
	Root string `json:"root"`
}

// ActiveAnalyzeDTO is the re-attachment snapshot returned by ActiveAnalyze.
// State is "running" (Progress holds the latest snapshot), "completed" (Report,
// or Cancelled/Error for a cancelled/failed run, is populated), or "none".
// Opts echoes the normalized request so the frontend can restore the whole
// step-2 context (root, mode, reorganize, event) after navigation.
type ActiveAnalyzeDTO struct {
	State     string           `json:"state"`
	Opts      ImportOptions    `json:"opts"`
	Progress  *ImportProgress  `json:"progress"`
	Report    *DryRunReportDTO `json:"report"`
	Cancelled bool             `json:"cancelled"`
	Error     string           `json:"error"`
}

// PickFolder opens a native directory chooser and returns the selected path, or
// "" if the user cancelled.
func (s *ImportService) PickFolder(ctx context.Context) (string, error) {
	if s.dialog == nil {
		return "", fmt.Errorf("services: no dialog provider configured")
	}
	return s.dialog.PickFolder(ctx, "Choose a folder to import")
}

// ScanSource walks root, caches the scan, and returns provisional counts (no
// hashing or duplicate detection yet).
func (s *ImportService) ScanSource(ctx context.Context, root string) (ScanSummary, error) {
	if err := s.guard(); err != nil {
		return ScanSummary{}, err
	}
	if root == "" {
		return ScanSummary{}, fmt.Errorf("services: scan source: empty root")
	}
	scan, err := s.pipeline.Scan(ctx, root, nil)
	if err != nil {
		return ScanSummary{}, err
	}
	s.putScan(scan.Root, scanEntry{scan: scan, at: time.Now()})

	summary := ScanSummary{
		Root:             scan.Root,
		Files:            len(scan.Files),
		ProvisionalPairs: len(scan.ProvisionalPairs),
		TotalBytes:       scan.TotalBytes,
	}
	for _, f := range scan.Files {
		switch f.Kind {
		case mediatype.Photo:
			summary.Photos++
		case mediatype.RawPhoto:
			summary.RawPhotos++
			summary.Photos++
		case mediatype.Video:
			summary.Videos++
		}
	}
	return summary, nil
}

// DryRun computes hashes and classifies every file against the archive, returning
// the mode-aware prediction. It reuses a cached scan for root when present and
// caches the resulting report so StartImport can reuse it.
func (s *ImportService) DryRun(ctx context.Context, root string, opts ImportOptions) (DryRunReportDTO, error) {
	if err := s.guard(); err != nil {
		return DryRunReportDTO{}, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return DryRunReportDTO{}, fmt.Errorf("services: dry run: resolve %q: %w", root, err)
	}

	entry, ok := s.getScan(absRoot)
	if !ok || entry.scan == nil {
		scan, err := s.pipeline.Scan(ctx, absRoot, nil)
		if err != nil {
			return DryRunReportDTO{}, err
		}
		entry = scanEntry{scan: scan, at: time.Now()}
	}

	iopts, err := s.buildOptions(ctx, opts, absRoot)
	if err != nil {
		return DryRunReportDTO{}, err
	}

	report, err := s.pipeline.DryRun(ctx, entry.scan, iopts, nil)
	if err != nil {
		return DryRunReportDTO{}, err
	}
	entry.report = report
	entry.at = time.Now()
	s.putScan(absRoot, entry)

	return newDryRunReportDTO(iopts, report), nil
}

// newDryRunReportDTO projects an importer.DryRunReport into the JSON-friendly
// DTO, echoing the mode/reorganize flags the planning ran under. Shared by the
// synchronous DryRun and the background StartAnalyze so the report shape is
// identical regardless of which produced it.
func newDryRunReportDTO(iopts importer.Options, report *importer.DryRunReport) DryRunReportDTO {
	return DryRunReportDTO{
		Mode:             string(iopts.Mode),
		Reorganize:       iopts.Reorganize,
		Files:            report.Files,
		Photos:           report.Photos,
		Videos:           report.Videos,
		AlreadyImported:  report.AlreadyImported,
		Duplicates:       report.Duplicates,
		New:              report.New,
		TotalImportBytes: report.TotalImportBytes,
		EstimatedSeconds: report.EstimatedSeconds,
		PlannedAdoptions: report.PlannedAdoptions,
		PlannedMoves:     report.PlannedMoves,
	}
}

// StartAnalyze runs scan + dry-run ("Analyze") in a background goroutine under
// the SAME one-active-operation guard as imports and reorganizes: while an
// analyze runs, StartImport/ResumeSession/StartReorganize are refused with
// ErrImportInProgress, and vice versa. A running analyze is cancelled via
// CancelImport (the same cancel plumbing imports use), which resolves it as a
// cancelled analyze:completed.
//
// It emits throttled import:progress with phase "scanning" (FilesTotal=0,
// FilesDone=discovered count), "hashing", and "classifying". Analyze progress
// carries an empty SessionID so the frontend can distinguish it from an import
// or reorganize. On finish it emits analyze:completed (success/cancelled/failed)
// and caches the scan+report exactly as DryRun does, so a later StartImport is
// unaffected. No ImportSession is created.
func (s *ImportService) StartAnalyze(ctx context.Context, opts ImportOptions) (StartAnalyzeResult, error) {
	if err := s.guard(); err != nil {
		return StartAnalyzeResult{}, err
	}
	if opts.Root == "" {
		return StartAnalyzeResult{}, fmt.Errorf("services: start analyze: empty root")
	}
	absRoot, err := filepath.Abs(opts.Root)
	if err != nil {
		return StartAnalyzeResult{}, fmt.Errorf("services: start analyze: resolve %q: %w", opts.Root, err)
	}
	iopts, err := s.buildOptions(ctx, opts, absRoot)
	if err != nil {
		return StartAnalyzeResult{}, err
	}
	if iopts.Mode == importer.ModeCopy && iopts.DestinationRoot == "" {
		return StartAnalyzeResult{}, fmt.Errorf("services: start analyze: no destination and no Master Library configured")
	}

	echo := ImportOptions{
		Root:            absRoot,
		DestinationRoot: iopts.DestinationRoot,
		EventName:       iopts.EventName,
		Mode:            string(iopts.Mode),
		Reorganize:      iopts.Reorganize,
		SourceID:        iopts.SourceID,
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartAnalyzeResult{}, ErrImportInProgress
	}
	s.active = true
	s.opKind = "analyze"
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.sessionID = ""
	s.current = nil
	s.analyze = &analyzeRun{opts: echo, running: true, at: time.Now()}
	s.mu.Unlock()

	s.sleep.Acquire()
	go s.runAnalyze(runCtx, absRoot, echo, iopts)
	return StartAnalyzeResult{Root: absRoot}, nil
}

// runAnalyze performs the scan + dry-run and records/emits the outcome. It
// mirrors run()/runReorganize() but is sessionless and terminates on
// analyze:completed rather than import:completed.
func (s *ImportService) runAnalyze(ctx context.Context, absRoot string, echo ImportOptions, iopts importer.Options) {
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
			SessionID:   "", // analyze is sessionless; empty ID marks it as analyze
			Phase:       string(p.Phase),
			FilesDone:   p.FilesDone,
			FilesTotal:  p.FilesTotal,
			BytesDone:   p.BytesDone,
			BytesTotal:  p.BytesTotal,
			CurrentFile: p.CurrentFile,
			Errors:      p.Errors,
			Percent:     percent(int64(p.FilesDone), int64(p.FilesTotal)),
			Done:        false,
		}
		s.mu.Lock()
		if s.analyze != nil {
			s.analyze.progress = &dto
		}
		s.mu.Unlock()
		if tr.allow() {
			emitSafe(s.emitter, EventImportProgress, dto)
		}
	}

	scan, err := s.pipeline.Scan(ctx, absRoot, progress)
	if err != nil {
		s.finishAnalyze(echo, nil, err)
		return
	}
	report, err := s.pipeline.DryRun(ctx, scan, iopts, progress)
	if err != nil {
		s.finishAnalyze(echo, nil, err)
		return
	}
	// Cache handoff: write the same scan+report cache DryRun writes (keyed by
	// absolute root) so a subsequent StartImport is identical to one after a
	// synchronous DryRun. StartImport still recomputes quick hashes at import
	// time (cheap relative to the copy) — the cache validates the request and
	// powers instant UI stats; the analyze does not change that path.
	s.putScan(absRoot, scanEntry{scan: scan, report: report, at: time.Now()})
	dto := newDryRunReportDTO(iopts, report)
	s.finishAnalyze(echo, &dto, nil)
}

// finishAnalyze records the terminal analyze state and emits analyze:completed.
// A context-cancelled run is reported as Cancelled (not an error); any other
// error carries its message.
func (s *ImportService) finishAnalyze(echo ImportOptions, report *DryRunReportDTO, runErr error) {
	cancelled := false
	errMsg := ""
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			cancelled = true
		} else {
			errMsg = runErr.Error()
			s.log.Warn("analyze failed", "root", echo.Root, "error", errMsg)
		}
	}
	s.mu.Lock()
	if s.analyze != nil {
		s.analyze.running = false
		s.analyze.report = report
		s.analyze.cancelled = cancelled
		s.analyze.err = errMsg
		s.analyze.at = time.Now()
	}
	s.mu.Unlock()

	emitSafe(s.emitter, EventAnalyzeCompleted, AnalyzeCompleted{
		Root:      echo.Root,
		Opts:      echo,
		Report:    report,
		Cancelled: cancelled,
		Error:     errMsg,
	})
}

// ActiveAnalyze returns the current analyze state for re-attachment: "running"
// with the latest progress snapshot, "completed" with the report (or a
// cancelled/failed marker), or "none". A completed snapshot is retained until a
// new operation starts (see StartImport/ResumeSession/StartReorganize) or
// analyzeReportTTL elapses, after which it lapses to "none".
func (s *ImportService) ActiveAnalyze(ctx context.Context) (ActiveAnalyzeDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.analyze
	if a == nil {
		return ActiveAnalyzeDTO{State: "none"}, nil
	}
	if a.running {
		dto := ActiveAnalyzeDTO{State: "running", Opts: a.opts}
		if a.progress != nil {
			p := *a.progress
			dto.Progress = &p
		}
		return dto, nil
	}
	if time.Since(a.at) > analyzeReportTTL {
		s.analyze = nil
		return ActiveAnalyzeDTO{State: "none"}, nil
	}
	return ActiveAnalyzeDTO{
		State:     "completed",
		Opts:      a.opts,
		Report:    a.report,
		Cancelled: a.cancelled,
		Error:     a.err,
	}, nil
}

// StartImport creates a session (returning its ID immediately) and launches the
// import in a background goroutine, emitting import:progress throughout and
// import:completed at the end. Only one import may run at a time; a concurrent
// start returns ErrImportInProgress.
//
// The run is driven through pipeline.ResumeSession against the freshly created
// session so the service owns the session ID up front and can stamp it on every
// emitted progress event. When a fresh Analyze/dry-run for this exact root is
// cached (see precomputedFor), its report is threaded into the run so quick and
// full hashes are reused rather than recomputed — eliminating a full second hash
// pass over the library. Without a cache the import re-hashes, which is correct.
func (s *ImportService) StartImport(ctx context.Context, opts ImportOptions) (StartImportResult, error) {
	if err := s.guard(); err != nil {
		return StartImportResult{}, err
	}
	iopts, err := s.buildOptions(ctx, opts, opts.Root)
	if err != nil {
		return StartImportResult{}, err
	}
	if iopts.SourceRoot == "" {
		return StartImportResult{}, fmt.Errorf("services: start import: empty root")
	}
	if iopts.Mode == importer.ModeCopy && iopts.DestinationRoot == "" {
		return StartImportResult{}, fmt.Errorf("services: start import: no destination and no Master Library configured")
	}

	state := resumeState{
		Mode:            string(iopts.Mode),
		SourceRoot:      iopts.SourceRoot,
		DestinationRoot: iopts.DestinationRoot,
		EventName:       iopts.EventName,
		SourceID:        iopts.SourceID,
		Reorganize:      iopts.Reorganize,
		Concurrency:     iopts.Concurrency,
	}
	return s.launch(ctx, state, s.precomputedFor(iopts.SourceRoot))
}

// precomputedFor returns the cached dry-run report for absRoot when a scan+report
// for that exact root was produced recently enough to trust (analyzeReportTTL,
// the same 15-minute freshness the analyze snapshot uses). The report is only a
// hash-reuse hint: every reused hash is still size+mtime gated inside the
// pipeline, so a slightly stale report can never carry a wrong hash into an
// import. A cache miss (or a stale entry) returns nil and the import re-hashes.
func (s *ImportService) precomputedFor(absRoot string) *importer.DryRunReport {
	entry, ok := s.getScan(absRoot)
	if !ok || entry.report == nil {
		return nil
	}
	if time.Since(entry.at) > analyzeReportTTL {
		return nil
	}
	return entry.report
}

// ResumeSession resumes an interrupted or cancelled session by ID, in a
// background goroutine identical to StartImport. Only one import may run at a
// time.
func (s *ImportService) ResumeSession(ctx context.Context, sessionID string) (StartImportResult, error) {
	if err := s.guard(); err != nil {
		return StartImportResult{}, err
	}
	session, err := s.sessions.GetByID(ctx, sessionID)
	if err != nil {
		return StartImportResult{}, err
	}
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartImportResult{}, ErrImportInProgress
	}
	s.active = true
	s.opKind = "import"
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.sessionID = session.ID
	s.current = nil
	s.analyze = nil // a resumed import supersedes any retained analyze snapshot
	s.mu.Unlock()

	s.sleep.Acquire()
	// A genuine crash/interrupt resume reloads options from the session Notes and
	// re-hashes from scratch: nil precomputed is the correct, safe behavior here.
	go s.run(runCtx, session.ID, nil)
	return StartImportResult{SessionID: session.ID}, nil
}

// launch creates a session from state and starts the background run. It performs
// the one-active guard atomically with session creation so two concurrent
// StartImport calls cannot both create a session. precomputed (may be nil) is the
// dry-run report whose hashes the run reuses.
func (s *ImportService) launch(ctx context.Context, state resumeState, precomputed *importer.DryRunReport) (StartImportResult, error) {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartImportResult{}, ErrImportInProgress
	}
	// Reserve the active slot before the (fast) session insert so no second start
	// slips in; release it if creation fails.
	s.active = true
	s.opKind = "import"
	s.mu.Unlock()

	session, err := s.newSession(ctx, state)
	if err != nil {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
		return StartImportResult{}, err
	}

	s.mu.Lock()
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.sessionID = session
	s.current = nil
	s.analyze = nil // a new import supersedes any retained analyze snapshot
	s.mu.Unlock()

	s.sleep.Acquire()
	go s.run(runCtx, session, precomputed)
	return StartImportResult{SessionID: session}, nil
}

// run executes the import against sessionID and emits import:completed when done.
// precomputed (may be nil) threads a prior dry run's hashes into the pipeline so
// they are reused rather than recomputed.
func (s *ImportService) run(ctx context.Context, sessionID string, precomputed *importer.DryRunReport) {
	defer func() {
		s.mu.Lock()
		s.active = false
		s.cancel = nil
		s.mu.Unlock()
		s.sleep.Release()
	}()

	tr := newThrottle()
	scannedRecorded := false
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

		// ResumeSession intentionally does not record FilesScanned (it assumes a
		// prior run already did). For a service-created session this is the first
		// run, so record the scanned count once, when it becomes known.
		if !scannedRecorded && p.FilesTotal > 0 &&
			(p.Phase == importer.PhaseImporting || p.Phase == importer.PhaseDone) {
			scannedRecorded = true
			if err := s.sessions.IncScanned(context.Background(), sessionID, p.FilesTotal); err != nil {
				s.log.Warn("could not record scanned count", "error", err.Error())
			}
		}

		if dto.Done || tr.allow() {
			emitSafe(s.emitter, EventImportProgress, dto)
		}
	}

	if _, err := s.pipeline.ResumeSessionPrecomputed(ctx, sessionID, precomputed, progress); err != nil {
		s.log.Error("import run failed", "sessionId", sessionID, "error", err.Error())
	}

	s.emitCompleted(sessionID)
}

// emitCompleted reloads the finished session and emits import:completed.
func (s *ImportService) emitCompleted(sessionID string) {
	session, err := s.sessions.GetByID(context.Background(), sessionID)
	if err != nil {
		s.log.Warn("could not reload session for completion event", "sessionId", sessionID, "error", err.Error())
		return
	}
	emitSafe(s.emitter, EventImportCompleted, ImportCompleted{
		SessionID:     session.ID,
		Status:        string(session.Status),
		FilesScanned:  session.FilesScanned,
		FilesImported: session.FilesImported,
		Duplicates:    session.Duplicates,
		Failures:      session.Failures,
		Skipped:       session.Skipped,
	})
	if s.OnCompleted != nil {
		s.OnCompleted(session.ID)
	}
}

// CancelImport cancels the active import (if any). The pipeline finalizes the
// session as cancelled. It is a no-op when nothing is running.
func (s *ImportService) CancelImport(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// ActiveImport returns the latest progress snapshot of the running import, or
// nil when nothing is running.
func (s *ImportService) ActiveImport(ctx context.Context) (*ImportProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.current == nil {
		return nil, nil
	}
	snapshot := *s.current
	return &snapshot, nil
}

// activeOps reports the running import/analyze/reorganize (at most one, since
// they share the single active-operation slot) for the quit guard. It reads the
// latest progress snapshot; numbers come from s.analyze for an analyze and
// s.current for an import/reorganize.
func (s *ImportService) activeOps() []OperationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return nil
	}
	info := OperationInfo{Kind: s.opKind}
	switch s.opKind {
	case "analyze":
		info.Label = "Analyzing a source"
		if s.analyze != nil && s.analyze.progress != nil {
			p := s.analyze.progress
			info.FilesDone, info.FilesTotal = p.FilesDone, p.FilesTotal
			info.BytesDone, info.BytesTotal = p.BytesDone, p.BytesTotal
		}
	case "reorganize":
		info.Label = "Reorganizing the library"
		if s.current != nil {
			info.FilesDone, info.FilesTotal = s.current.FilesDone, s.current.FilesTotal
			info.BytesDone, info.BytesTotal = s.current.BytesDone, s.current.BytesTotal
		}
	default:
		info.Kind = "import"
		info.Label = "Importing files"
		if s.current != nil {
			info.FilesDone, info.FilesTotal = s.current.FilesDone, s.current.FilesTotal
			info.BytesDone, info.BytesTotal = s.current.BytesDone, s.current.BytesTotal
		}
	}
	return []OperationInfo{info}
}

// cancelActive cancels a running import/analyze/reorganize via the existing
// CancelImport path. It is a no-op when nothing is running.
func (s *ImportService) cancelActive() { _ = s.CancelImport(context.Background()) }

// newSession creates the ImportSession row that ResumeSession will drive,
// stamping the resume-state notes so the run can reload its options.
func (s *ImportService) newSession(ctx context.Context, state resumeState) (string, error) {
	session := &domain.ImportSession{
		StartedAt:       time.Now(),
		SourceID:        state.SourceID,
		DestinationRoot: state.DestinationRoot,
		Status:          domain.SessionStatusRunning,
		Notes:           state.encode(),
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return "", fmt.Errorf("services: create import session: %w", err)
	}
	s.log.Info("import session created", "sessionId", session.ID, "mode", state.Mode, "source", state.SourceRoot)
	return session.ID, nil
}

// buildOptions resolves an ImportOptions DTO into importer.Options, filling the
// destination from settings and the concurrency from settings when unset.
func (s *ImportService) buildOptions(ctx context.Context, opts ImportOptions, root string) (importer.Options, error) {
	absRoot := root
	if absRoot != "" {
		if a, err := filepath.Abs(absRoot); err == nil {
			absRoot = a
		}
	}

	cfg, err := LoadSettings(ctx, s.settings)
	if err != nil {
		return importer.Options{}, err
	}

	dest := opts.DestinationRoot
	if dest == "" {
		dest = cfg.MasterLibraryRoot
	}

	mode := importer.ModeCopy
	if opts.Mode == string(importer.ModeAdopt) {
		mode = importer.ModeAdopt
	}

	eventName := opts.EventName
	if eventName == "" {
		eventName = cfg.DefaultEventName
	}

	return importer.Options{
		Mode:            mode,
		SourceRoot:      absRoot,
		DestinationRoot: dest,
		EventName:       eventName,
		SourceID:        opts.SourceID,
		Reorganize:      opts.Reorganize,
		Concurrency:     cfg.ImportConcurrency,
	}, nil
}

func (s *ImportService) getScan(root string) (scanEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[root]
	return e, ok
}

func (s *ImportService) putScan(root string, e scanEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[root] = e
}
