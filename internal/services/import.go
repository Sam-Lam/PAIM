package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/importer"
	"github.com/autolinepro/paim/internal/mediatype"
	"github.com/autolinepro/paim/internal/repo"
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
	pipeline *importer.Pipeline
	sessions *repo.SessionRepo
	dialog   Dialoger
	emitter  Emitter
	settings *repo.SettingsRepo
	log      *slog.Logger

	mu        sync.Mutex
	active    bool
	cancel    context.CancelFunc
	sessionID string
	current   *ImportProgress
	cache     map[string]scanEntry
}

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
	}, nil
}

// StartImport creates a session (returning its ID immediately) and launches the
// import in a background goroutine, emitting import:progress throughout and
// import:completed at the end. Only one import may run at a time; a concurrent
// start returns ErrImportInProgress.
//
// The run is driven through pipeline.ResumeSession against the freshly created
// session so the service owns the session ID up front and can stamp it on every
// emitted progress event. Quick hashes are recomputed at import time (they are
// cheap relative to the copy); the dry-run cache is used to validate the request
// and to power instant UI stats, not to seed hashes.
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
	return s.launch(ctx, state)
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
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.sessionID = session.ID
	s.current = nil
	s.mu.Unlock()

	go s.run(runCtx, session.ID)
	return StartImportResult{SessionID: session.ID}, nil
}

// launch creates a session from state and starts the background run. It performs
// the one-active guard atomically with session creation so two concurrent
// StartImport calls cannot both create a session.
func (s *ImportService) launch(ctx context.Context, state resumeState) (StartImportResult, error) {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartImportResult{}, ErrImportInProgress
	}
	// Reserve the active slot before the (fast) session insert so no second start
	// slips in; release it if creation fails.
	s.active = true
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
	s.mu.Unlock()

	go s.run(runCtx, session)
	return StartImportResult{SessionID: session}, nil
}

// run executes the import against sessionID and emits import:completed when done.
func (s *ImportService) run(ctx context.Context, sessionID string) {
	defer func() {
		s.mu.Lock()
		s.active = false
		s.cancel = nil
		s.mu.Unlock()
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

	if _, err := s.pipeline.ResumeSession(ctx, sessionID, progress); err != nil {
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
