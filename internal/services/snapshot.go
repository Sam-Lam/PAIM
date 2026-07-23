package services

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/library"
	"gorm.io/gorm"
)

// SnapshotService manages the one-way, disaster-recovery catalog snapshots: a
// per-machine destination folder + interval, a manual "Snapshot now", and the
// automatic triggers (a timer while the app is open + on graceful quit). The
// destination/interval are stored in the per-machine library.Config, never in the
// library DB. Snapshots are insurance only — they are never opened as the live
// catalog. Failures (destination missing/unplugged) are recorded and logged, then
// retried next interval; they never block anything.
type SnapshotService struct {
	gated
	config *library.ConfigStore
	dialog Dialoger
	log    *slog.Logger

	// Bound per-library state.
	db     *gorm.DB
	dbPath string
	name   string
	bound  bool

	// snapMu serializes snapshot runs (manual / timer / quit) so two never copy at
	// once. It is TryLock'd by callers that must not block.
	snapMu sync.Mutex

	mu          sync.Mutex
	running     bool
	lastAt      time.Time
	lastPath    string
	lastError   string
	reconfigure chan struct{}
}

// NewSnapshotService constructs a SnapshotService over the per-machine config
// store and native folder dialog.
func NewSnapshotService(config *library.ConfigStore, dialog Dialoger, logger *slog.Logger) *SnapshotService {
	if logger == nil {
		logger = slog.Default()
	}
	return &SnapshotService{
		config:      config,
		dialog:      dialog,
		log:         logger.With(slog.String("subsystem", "snapshot")),
		reconfigure: make(chan struct{}, 1),
	}
}

// Bind wires the SnapshotService to an open library's catalog handle and path.
func (s *SnapshotService) Bind(core *AppCore) {
	s.db = core.DB
	s.name = library.DefaultName(core.Root)
	if core.Root != "" {
		s.dbPath = library.DBPath(core.Root)
	} else if core.Meta != nil {
		s.dbPath = core.Meta.DBPath
	}
	s.bound = true
}

/* --------------------------------- config ---------------------------------- */

// SnapshotConfigDTO is the per-machine snapshot configuration.
type SnapshotConfigDTO struct {
	Dest     string `json:"dest"`
	Interval string `json:"interval"`
}

// SnapshotStatusDTO is the snapshot config plus the last-run result for the UI.
type SnapshotStatusDTO struct {
	Dest      string `json:"dest"`
	Interval  string `json:"interval"`
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	LastAt    string `json:"lastAt"`
	LastPath  string `json:"lastPath"`
	LastError string `json:"lastError"`
}

// normalizedInterval returns a valid interval token, defaulting to daily.
func normalizedInterval(v string) string {
	switch v {
	case library.SnapshotIntervalOff, library.SnapshotIntervalQuit,
		library.SnapshotInterval6h, library.SnapshotIntervalDaily:
		return v
	default:
		return library.SnapshotIntervalDaily
	}
}

// PickSnapshotDest opens a native directory chooser for the snapshot destination.
func (s *SnapshotService) PickSnapshotDest(ctx context.Context) (string, error) {
	if s.dialog == nil {
		return "", fmt.Errorf("services: no dialog provider configured")
	}
	return s.dialog.PickFolder(ctx, "Choose a folder for catalog snapshots")
}

// GetSnapshotConfig returns the current per-machine snapshot configuration.
func (s *SnapshotService) GetSnapshotConfig(ctx context.Context) (SnapshotConfigDTO, error) {
	if err := s.guard(); err != nil {
		return SnapshotConfigDTO{}, err
	}
	cfg, err := s.config.Load()
	if err != nil {
		return SnapshotConfigDTO{}, err
	}
	return SnapshotConfigDTO{Dest: cfg.SnapshotDest, Interval: normalizedInterval(cfg.SnapshotInterval)}, nil
}

// SetSnapshotConfig persists the snapshot destination/interval and re-arms the
// timer. An empty destination disables snapshots.
func (s *SnapshotService) SetSnapshotConfig(ctx context.Context, in SnapshotConfigDTO) (SnapshotStatusDTO, error) {
	if err := s.guard(); err != nil {
		return SnapshotStatusDTO{}, err
	}
	cfg, err := s.config.Load()
	if err != nil {
		return SnapshotStatusDTO{}, err
	}
	cfg.SnapshotDest = in.Dest
	cfg.SnapshotInterval = normalizedInterval(in.Interval)
	if err := s.config.Save(cfg); err != nil {
		return SnapshotStatusDTO{}, err
	}
	// Wake the timer loop so it re-reads the interval immediately.
	select {
	case s.reconfigure <- struct{}{}:
	default:
	}
	s.log.Info("snapshot config updated", "dest", cfg.SnapshotDest, "interval", cfg.SnapshotInterval)
	return s.status(cfg.SnapshotDest, cfg.SnapshotInterval), nil
}

// SnapshotStatus returns the config plus the last-run result.
func (s *SnapshotService) SnapshotStatus(ctx context.Context) (SnapshotStatusDTO, error) {
	if err := s.guard(); err != nil {
		return SnapshotStatusDTO{}, err
	}
	cfg, err := s.config.Load()
	if err != nil {
		return SnapshotStatusDTO{}, err
	}
	return s.status(cfg.SnapshotDest, cfg.SnapshotInterval), nil
}

// status assembles a SnapshotStatusDTO from the config plus last-run state.
func (s *SnapshotService) status(dest, interval string) SnapshotStatusDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	dto := SnapshotStatusDTO{
		Dest:      dest,
		Interval:  normalizedInterval(interval),
		Enabled:   dest != "",
		Running:   s.running,
		LastPath:  s.lastPath,
		LastError: s.lastError,
	}
	if !s.lastAt.IsZero() {
		dto.LastAt = s.lastAt.Format(time.RFC3339)
	}
	return dto
}

/* --------------------------------- runs ------------------------------------ */

// SnapshotNow runs a snapshot immediately (the manual "Snapshot now" button). It
// returns an error only when the destination is unconfigured; a copy failure is
// recorded in the returned status (LastError) and logged, not returned as an
// error, so the UI shows it inline.
func (s *SnapshotService) SnapshotNow(ctx context.Context) (SnapshotStatusDTO, error) {
	if err := s.guard(); err != nil {
		return SnapshotStatusDTO{}, err
	}
	cfg, err := s.config.Load()
	if err != nil {
		return SnapshotStatusDTO{}, err
	}
	if cfg.SnapshotDest == "" {
		return SnapshotStatusDTO{}, fmt.Errorf("services: no snapshot destination configured")
	}
	s.runSnapshot(ctx, cfg.SnapshotDest, "manual")
	return s.status(cfg.SnapshotDest, cfg.SnapshotInterval), nil
}

// SnapshotOnQuit runs a final snapshot during graceful shutdown when a
// destination is configured and the interval is not "off". It is best-effort and
// bounded by the caller's context; it never blocks the quit path on failure.
func (s *SnapshotService) SnapshotOnQuit(ctx context.Context) {
	if !s.bound {
		return
	}
	cfg, err := s.config.Load()
	if err != nil || cfg.SnapshotDest == "" || cfg.SnapshotInterval == library.SnapshotIntervalOff {
		return
	}
	s.runSnapshot(ctx, cfg.SnapshotDest, "quit")
}

// StartTimer launches the interval timer for the life of ctx. It re-reads the
// configured interval whenever SetSnapshotConfig signals reconfigure, snapshots
// on each tick, and idles (waiting only on reconfigure/ctx) when the interval has
// no timer (off / quit-only) or no destination is set.
func (s *SnapshotService) StartTimer(ctx context.Context) {
	go s.timerLoop(ctx)
}

func (s *SnapshotService) timerLoop(ctx context.Context) {
	for {
		cfg, err := s.config.Load()
		dur := time.Duration(0)
		armed := false
		if err == nil && cfg.SnapshotDest != "" {
			if d, ok := library.SnapshotIntervalDuration(cfg.SnapshotInterval); ok {
				dur, armed = d, true
			}
		}
		if !armed {
			select {
			case <-ctx.Done():
				return
			case <-s.reconfigure:
				continue
			}
		}
		timer := time.NewTimer(dur)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.reconfigure:
			timer.Stop()
			continue
		case <-timer.C:
			s.runSnapshot(ctx, cfg.SnapshotDest, "timer")
		}
	}
}

// runSnapshot performs one snapshot, serialized against other runs. If another
// run holds the lock it is skipped (a snapshot is already in flight). Result and
// any error are recorded for the status DTO and logged.
func (s *SnapshotService) runSnapshot(ctx context.Context, dest, trigger string) {
	if !s.snapMu.TryLock() {
		return // a snapshot is already running
	}
	defer s.snapMu.Unlock()

	s.mu.Lock()
	if s.db == nil || s.dbPath == "" {
		s.mu.Unlock()
		return
	}
	s.running = true
	db, dbPath, name := s.db, s.dbPath, s.name
	s.mu.Unlock()

	res, err := library.Snapshot(ctx, db, dbPath, name, dest, library.DefaultSnapshotKeep)

	s.mu.Lock()
	s.running = false
	if err != nil {
		s.lastError = err.Error()
		s.mu.Unlock()
		s.log.Warn("catalog snapshot failed", "trigger", trigger, "dest", dest, "error", err.Error())
		return
	}
	s.lastError = ""
	s.lastAt = res.CreatedAt
	s.lastPath = res.Path
	s.mu.Unlock()
	s.log.Info("catalog snapshot written", "trigger", trigger, "path", res.Path, "bytes", res.Bytes, "pruned", res.Pruned)
}
