package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/hashing"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/source"
	"github.com/Sam-Lam/PAIM/internal/volumes"
)

// ErrSafeToEraseInProgress is returned by StartSafeToErase when an evaluation is
// already running (only one safe-to-erase evaluation is permitted at a time).
var ErrSafeToEraseInProgress = errors.New("services: a safe-to-erase evaluation is already running")

// safeEraseReportTTL bounds how long a completed safe-to-erase report is retained
// for re-attachment (ActiveSafeToErase) after the evaluation finishes.
const safeEraseReportTTL = 15 * time.Minute

// safeEraseRun is the in-memory state of a single background safe-to-erase
// evaluation. running is true while the goroutine is live; once it finishes,
// report/err/cancelled hold the terminal outcome and at records the completion
// time for the TTL.
type safeEraseRun struct {
	mountPoint string
	sourceID   string
	progress   *SourceProgress // latest throttled snapshot
	report     *SafeToEraseDTO // populated on success
	safeFiles  []string        // absolute paths of evaluated-safe media (clear-source input)
	err        string          // populated on failure
	cancelled  bool            // populated when cancelled
	running    bool
	at         time.Time // completion time (for TTL) once running is false
}

// Hasher adapts internal/hashing to the source package's FileHasher and
// FullHasher interfaces (QuickHash(path) / FullHash(path)). source.Identifier is
// constructed with one of these in main.go; the FullHash method (with an internal
// background context) lets safe-to-erase disambiguate quick-hash collisions.
type Hasher struct{}

// QuickHash returns the BLAKE3 quick hash of path.
func (Hasher) QuickHash(path string) (string, error) { return hashing.QuickHash(path) }

// FullHash returns the BLAKE3 full hash of path. The source package's FullHasher
// interface takes no context, so a background context is used internally.
func (Hasher) FullHash(path string) (string, error) {
	return hashing.FullHash(context.Background(), path)
}

// assetLookupAdapter adapts *repo.AssetRepo to source.AssetLookup, projecting
// domain assets into the minimal source.ArchivedAsset view (with backup
// completeness folded in).
type assetLookupAdapter struct {
	assets *repo.AssetRepo
}

// FindByQuickHash returns archived-asset views for every non-deleted asset with
// the given quick hash.
func (a assetLookupAdapter) FindByQuickHash(ctx context.Context, quickHash string) ([]source.ArchivedAsset, error) {
	rows, err := a.assets.FindByQuickHash(ctx, quickHash)
	if err != nil {
		return nil, err
	}
	return toArchivedAssets(rows), nil
}

// FindByOriginalPath returns archived-asset views for every non-deleted asset
// recorded at the given original source path (the safe-to-erase fast-path key).
func (a assetLookupAdapter) FindByOriginalPath(ctx context.Context, path string) ([]source.ArchivedAsset, error) {
	rows, err := a.assets.FindByOriginalPath(ctx, path)
	if err != nil {
		return nil, err
	}
	return toArchivedAssets(rows), nil
}

// toArchivedAssets projects domain assets into the minimal source.ArchivedAsset
// view (with backup completeness folded in and the fast-path fields populated).
func toArchivedAssets(rows []domain.Asset) []source.ArchivedAsset {
	out := make([]source.ArchivedAsset, 0, len(rows))
	for _, r := range rows {
		out = append(out, source.ArchivedAsset{
			ID:               r.ID,
			QuickHash:        r.QuickHash,
			FullHash:         r.FullHash,
			Verified:         r.VerificationStatus == domainVerified,
			BackupComplete:   r.BackupStatus == domainBackupComplete,
			HasArchiveCopy:   r.CurrentArchivePath != "",
			OriginalFullPath: r.OriginalFullPath,
			FileSize:         r.FileSize,
			ImportDate:       r.ImportDate,
		})
	}
	return out
}

// SourcesService lists volumes, identifies import sources, and evaluates
// safe-to-erase.
type SourcesService struct {
	gated
	sleepAware
	collector  *volumes.Collector
	identifier *source.Identifier
	sources    *repo.SourceRepo
	assets     *repo.AssetRepo
	watcher    *volumes.Watcher
	emitter    Emitter
	log        *slog.Logger
	// root is the open library root, used to refuse clearing the archive itself.
	root string

	mu             sync.Mutex
	active         bool
	cancel         context.CancelFunc
	run            *safeEraseRun
	identifyCancel context.CancelFunc

	clearing    bool
	clearCancel context.CancelFunc
	clearRun    *clearSourceRun
}

// Bind wires the SourcesService to an open library's identifier and repos in
// place.
func (s *SourcesService) Bind(core *AppCore) {
	s.identifier = core.Identifier
	s.sources = core.Sources
	s.assets = core.Assets
	s.root = core.Root
}

// NewSourcesService constructs a SourcesService.
func NewSourcesService(collector *volumes.Collector, identifier *source.Identifier, sources *repo.SourceRepo, assets *repo.AssetRepo, watcher *volumes.Watcher, emitter Emitter, logger *slog.Logger) *SourcesService {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourcesService{
		collector:  collector,
		identifier: identifier,
		sources:    sources,
		assets:     assets,
		watcher:    watcher,
		emitter:    emitter,
		log:        logger.With(slog.String("subsystem", "source")),
	}
}

// ListVolumes enumerates and describes every mounted volume under /Volumes.
func (s *SourcesService) ListVolumes(ctx context.Context) ([]VolumeDTO, error) {
	infos, err := s.collector.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VolumeDTO, 0, len(infos))
	for _, v := range infos {
		out = append(out, toVolumeDTO(v))
	}
	return out, nil
}

// MatchDTO is the JSON-friendly result of identifying a volume.
type MatchDTO struct {
	SourceID                   string    `json:"sourceId"`
	Source                     SourceDTO `json:"source"`
	Confidence                 int       `json:"confidence"`
	Reasons                    []string  `json:"reasons"`
	IsKnown                    bool      `json:"isKnown"`
	ContentsPreviouslyImported bool      `json:"contentsPreviouslyImported"`
}

// IdentifyVolume identifies the volume at mountPoint, persists the resulting
// source (creating a new record or updating the matched one and its LastSeen),
// emits source:identified, and returns the match with confidence and reasons.
func (s *SourcesService) IdentifyVolume(ctx context.Context, mountPoint string) (MatchDTO, error) {
	if err := s.guard(); err != nil {
		return MatchDTO{}, err
	}

	// Wire a cancellable context so a Cancel link can abort the (walk-bound)
	// fingerprint scan. The call is synchronous, but CancelIdentify cancels this
	// derived context to unwind the walk promptly.
	idCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.identifyCancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.identifyCancel = nil
		s.mu.Unlock()
	}()

	tr := newThrottle()
	progressFn := func(scanned int) {
		if tr.allow() {
			emitSafe(s.emitter, EventSourceProgress, SourceProgress{
				Kind:       "identify",
				MountPoint: mountPoint,
				Scanned:    scanned,
			})
		}
	}

	match, err := s.identifier.Identify(idCtx, mountPoint, progressFn)
	if err != nil {
		return MatchDTO{}, err
	}

	rec := match.SourceRecord
	now := timeNow()
	if match.IsKnown && rec.ID != "" {
		rec.LastSeenAt = now
		if err := s.sources.Update(ctx, rec); err != nil {
			return MatchDTO{}, err
		}
	} else {
		rec.LastSeenAt = now
		if err := s.sources.Create(ctx, rec); err != nil {
			return MatchDTO{}, err
		}
	}

	// A content-fingerprint match proves this volume's contents were imported
	// before, so sessions that recorded this mount as their root but predate
	// source auto-linking can safely be attributed to this source. Path match
	// alone is NOT identity (mount names like "Untitled" recycle) — the
	// fingerprint corroboration is what makes the adoption sound.
	if match.ContentsPreviouslyImported && rec.ID != "" {
		if adopted, err := s.sources.AdoptOrphanSessions(ctx, rec.ID, mountPoint); err != nil {
			s.log.Warn("adopt orphan sessions", "sourceId", rec.ID, "error", err.Error())
		} else if adopted > 0 {
			s.log.Info("adopted orphan import sessions", "sourceId", rec.ID, "count", adopted, "mount", mountPoint)
		}
	}

	emitSafe(s.emitter, EventSourceIdentified, SourceIdentified{
		MountPoint: mountPoint,
		SourceID:   rec.ID,
		Confidence: match.Confidence,
		IsKnown:    match.IsKnown,
	})

	dto := toSourceDTO(*rec)
	if counts, err := s.sources.SessionCounts(ctx); err == nil {
		dto.ImportCount = int(counts[rec.ID])
	}
	return MatchDTO{
		SourceID:                   rec.ID,
		Source:                     dto,
		Confidence:                 match.Confidence,
		Reasons:                    match.Reasons,
		IsKnown:                    match.IsKnown,
		ContentsPreviouslyImported: match.ContentsPreviouslyImported,
	}, nil
}

// ListKnownSources returns the most recently seen persisted sources.
func (s *SourcesService) ListKnownSources(ctx context.Context) ([]SourceDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.sources.ListRecent(ctx, 200)
	if err != nil {
		return nil, err
	}
	counts, err := s.sources.SessionCounts(ctx)
	if err != nil {
		s.log.Warn("session counts by source", "error", err.Error())
		counts = map[string]int64{}
	}
	out := make([]SourceDTO, 0, len(rows))
	for _, r := range rows {
		dto := toSourceDTO(r)
		// Import counts are derived from linked sessions, never a stored counter.
		dto.ImportCount = int(counts[r.ID])
		out = append(out, dto)
	}
	return out, nil
}

// SafeToEraseDTO is the JSON-friendly safe-to-erase report. FastPath/Hashed
// report how many media files were classified from the catalog without hashing
// vs by (re)hashing, so the UI can show how cheap a just-imported evaluation was.
type SafeToEraseDTO struct {
	SourceID         string `json:"sourceId"`
	Safe             bool   `json:"safe"`
	Reason           string `json:"reason"`
	TotalMedia       int    `json:"totalMedia"`
	Archived         int    `json:"archived"`
	New              int    `json:"new"`
	Unverified       int    `json:"unverified"`
	BackupIncomplete int    `json:"backupIncomplete"`
	FastPath         int    `json:"fastPath"`
	Hashed           int    `json:"hashed"`
}

// StartSafeToEraseResult is returned immediately from StartSafeToErase once the
// background evaluation has been launched. MountPoint echoes the volume; the
// evaluation is re-attached via ActiveSafeToErase (keyed by nothing — only one
// runs at a time).
type StartSafeToEraseResult struct {
	MountPoint string `json:"mountPoint"`
}

// ActiveSafeToEraseDTO is the re-attachment snapshot returned by
// ActiveSafeToErase. State is "running" (Progress holds the latest snapshot),
// "completed" (Report, or Cancelled/Error, is populated), or "none". MountPoint
// and SourceID echo the request so the frontend can restore the volume card.
type ActiveSafeToEraseDTO struct {
	State      string          `json:"state"`
	MountPoint string          `json:"mountPoint"`
	SourceID   string          `json:"sourceId"`
	Progress   *SourceProgress `json:"progress"`
	Report     *SafeToEraseDTO `json:"report"`
	Cancelled  bool            `json:"cancelled"`
	Error      string          `json:"error"`
}

// StartSafeToErase launches a background safe-to-erase evaluation of the volume
// at mountPoint. Only one evaluation may run at a time (ErrSafeToEraseInProgress
// otherwise). It emits throttled source:progress (kind "safe-to-erase") while
// hashing and a terminal source:evaluated when it finishes; the completed report
// is retained for re-attachment (ActiveSafeToErase) for safeEraseReportTTL. A
// running evaluation is cancelled via CancelSafeToErase.
func (s *SourcesService) StartSafeToErase(ctx context.Context, sourceID, mountPoint string) (StartSafeToEraseResult, error) {
	if err := s.guard(); err != nil {
		return StartSafeToEraseResult{}, err
	}
	if mountPoint == "" {
		return StartSafeToEraseResult{}, fmt.Errorf("services: start safe-to-erase: empty mount point")
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return StartSafeToEraseResult{}, ErrSafeToEraseInProgress
	}
	s.active = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.run = &safeEraseRun{mountPoint: mountPoint, sourceID: sourceID, running: true, at: time.Now()}
	s.mu.Unlock()

	s.sleep.Acquire()
	go s.runSafeToErase(runCtx, sourceID, mountPoint)
	return StartSafeToEraseResult{MountPoint: mountPoint}, nil
}

// runSafeToErase performs the evaluation, emitting throttled progress and a
// terminal event, and persists the conclusion on the source (when sourceID set).
func (s *SourcesService) runSafeToErase(ctx context.Context, sourceID, mountPoint string) {
	defer func() {
		s.mu.Lock()
		s.active = false
		s.cancel = nil
		s.mu.Unlock()
		s.sleep.Release()
	}()

	tr := newThrottle()
	progressFn := func(filesDone, filesTotal int, currentFile string) {
		p := SourceProgress{
			Kind:        "safe-to-erase",
			MountPoint:  mountPoint,
			FilesDone:   filesDone,
			FilesTotal:  filesTotal,
			CurrentFile: currentFile,
		}
		s.mu.Lock()
		if s.run != nil {
			s.run.progress = &p
		}
		s.mu.Unlock()
		if tr.allow() {
			emitSafe(s.emitter, EventSourceProgress, p)
		}
	}

	report, err := s.identifier.EvaluateSafeToErase(ctx, sourceID, mountPoint, assetLookupAdapter{assets: s.assets}, mediatype.IsMedia, progressFn)
	if err != nil {
		s.finishSafeToErase(mountPoint, sourceID, nil, nil, err)
		return
	}
	if sourceID != "" {
		if err := s.sources.SetSafeToErase(context.Background(), sourceID, report.Safe, report.Reason); err != nil {
			s.log.Warn("could not persist safe-to-erase", "sourceId", sourceID, "error", err.Error())
		}
	}
	s.log.Info("safe-to-erase evaluated",
		"mountPoint", mountPoint, "safe", report.Safe,
		"totalMedia", report.TotalMedia, "fastPath", report.FastPath, "hashed", report.Hashed)
	dto := SafeToEraseDTO{
		SourceID:         report.SourceID,
		Safe:             report.Safe,
		Reason:           report.Reason,
		TotalMedia:       report.TotalMedia,
		Archived:         report.Archived,
		New:              report.New,
		Unverified:       report.Unverified,
		BackupIncomplete: report.BackupIncomplete,
		FastPath:         report.FastPath,
		Hashed:           report.Hashed,
	}
	s.finishSafeToErase(mountPoint, sourceID, &dto, report.SafeFiles, nil)
}

// finishSafeToErase records the terminal state and emits source:evaluated. A
// context-cancelled run is reported as Cancelled (not an error).
func (s *SourcesService) finishSafeToErase(mountPoint, sourceID string, report *SafeToEraseDTO, safeFiles []string, runErr error) {
	cancelled := false
	errMsg := ""
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			cancelled = true
		} else {
			errMsg = runErr.Error()
			s.log.Warn("safe-to-erase failed", "mountPoint", mountPoint, "error", errMsg)
		}
	}
	s.mu.Lock()
	if s.run != nil {
		s.run.running = false
		s.run.report = report
		s.run.safeFiles = safeFiles
		s.run.cancelled = cancelled
		s.run.err = errMsg
		s.run.at = time.Now()
	}
	s.mu.Unlock()

	emitSafe(s.emitter, EventSourceEvaluated, SourceEvaluated{
		MountPoint: mountPoint,
		SourceID:   sourceID,
		Report:     report,
		Cancelled:  cancelled,
		Error:      errMsg,
	})
}

// ActiveSafeToErase returns the current evaluation state for re-attachment:
// "running" with the latest progress snapshot, "completed" with the report (or a
// cancelled/failed marker), or "none". A completed snapshot lapses to "none"
// after safeEraseReportTTL.
func (s *SourcesService) ActiveSafeToErase(ctx context.Context) (ActiveSafeToEraseDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run
	if r == nil {
		return ActiveSafeToEraseDTO{State: "none"}, nil
	}
	if r.running {
		dto := ActiveSafeToEraseDTO{State: "running", MountPoint: r.mountPoint, SourceID: r.sourceID}
		if r.progress != nil {
			p := *r.progress
			dto.Progress = &p
		}
		return dto, nil
	}
	if time.Since(r.at) > safeEraseReportTTL {
		s.run = nil
		return ActiveSafeToEraseDTO{State: "none"}, nil
	}
	return ActiveSafeToEraseDTO{
		State:      "completed",
		MountPoint: r.mountPoint,
		SourceID:   r.sourceID,
		Report:     r.report,
		Cancelled:  r.cancelled,
		Error:      r.err,
	}, nil
}

// CancelSafeToErase cancels the active safe-to-erase evaluation (if any). It is a
// no-op when nothing is running.
func (s *SourcesService) CancelSafeToErase(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// CancelIdentify aborts an in-flight IdentifyVolume call (if any) by cancelling
// its derived context, unwinding the fingerprint walk. It is a no-op when no
// identification is running.
func (s *SourcesService) CancelIdentify(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.identifyCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// activeOps reports a running safe-to-erase evaluation for the quit guard. The
// identify walk is deliberately excluded: it is short and not a re-attachable
// background run, matching the activity-registry spec (sources contributes its
// safe-to-erase run only).
func (s *SourcesService) activeOps() []OperationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ops []OperationInfo
	if s.active && s.run != nil {
		info := OperationInfo{Kind: OpKindSafeToErase, Label: "Checking whether a card is safe to erase"}
		if s.run.progress != nil {
			info.FilesDone, info.FilesTotal = s.run.progress.FilesDone, s.run.progress.FilesTotal
		}
		ops = append(ops, info)
	}
	if s.clearing && s.clearRun != nil {
		info := OperationInfo{Kind: OpKindClearSource, Label: "Clearing an imported source"}
		if s.clearRun.progress != nil {
			info.FilesDone, info.FilesTotal = s.clearRun.progress.FilesDone, s.clearRun.progress.FilesTotal
		}
		ops = append(ops, info)
	}
	return ops
}

// cancelActive cancels a running safe-to-erase evaluation and/or clear via their
// existing cancel paths. It is a no-op when nothing is running.
func (s *SourcesService) cancelActive() {
	_ = s.CancelSafeToErase(context.Background())
	_ = s.CancelClearSource(context.Background())
}

// StartWatching runs the volume watcher until ctx is cancelled, emitting
// volume:mounted / volume:unmounted for each change. It is invoked once from
// main.go in a background goroutine; the watcher establishes its baseline from
// the volumes already mounted at start (which produce no events).
func (s *SourcesService) StartWatching(ctx context.Context) error {
	if s.watcher == nil {
		return nil
	}
	events, err := s.watcher.Start(ctx)
	if err != nil {
		return err
	}
	go func() {
		for ev := range events {
			switch ev.Type {
			case volumes.EventMounted:
				s.log.Info("volume mounted", "mountPoint", ev.MountPoint)
				emitSafe(s.emitter, EventVolumeMounted, VolumeEvent{MountPoint: ev.MountPoint})
			case volumes.EventUnmounted:
				s.log.Info("volume unmounted", "mountPoint", ev.MountPoint)
				emitSafe(s.emitter, EventVolumeUnmounted, VolumeEvent{MountPoint: ev.MountPoint})
			}
		}
	}()
	return nil
}
