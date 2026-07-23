package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/thumbs"
)

// ErrWarmupInProgress is returned when a thumbnail warm-up is requested while one
// is already running (only one runs at a time; a second request is refused
// politely rather than queued).
var ErrWarmupInProgress = errors.New("services: a thumbnail warm-up is already running")

// Thumbnail generation parallelism bounds (per-machine setting).
const (
	// DefaultThumbnailParallelism is the concurrency used when the per-machine
	// setting is unset. It is intentionally low (HDD-friendly): parallel qlmanage
	// renders on a spinning external drive thrash the heads and slow every tile.
	DefaultThumbnailParallelism = 2
	// MaxThumbnailParallelism caps the setting so a typo cannot spawn hundreds of
	// concurrent qlmanage processes.
	MaxThumbnailParallelism = 16
)

// clampThumbnailParallelism normalizes a stored/requested parallelism to the
// supported range, applying the default for 0/absent values.
func clampThumbnailParallelism(n int) int {
	if n < 1 {
		return DefaultThumbnailParallelism
	}
	if n > MaxThumbnailParallelism {
		return MaxThumbnailParallelism
	}
	return n
}

// ThumbnailService owns the disposable thumbnail cache's per-machine concerns:
// where the cache lives (in-library vs this Mac's local disk), clearing it, and
// warming it ahead of browsing. The cache location is stored in the per-machine
// library.Config (not the library DB) because it is a machine-local performance
// preference. Warm-up is a single-instance background job registered with the
// activity tracker (quit-guard aware) and the sleep guard; it deliberately does
// NOT share the import one-active guard, so it never blocks an import.
type ThumbnailService struct {
	gated
	sleepAware
	config  *library.ConfigStore
	emitter Emitter
	log     *slog.Logger

	// Bound per-library state.
	cache    *thumbs.Cache
	warmer   *thumbs.Warmer
	assets   *repo.AssetRepo
	sessions *repo.SessionRepo
	settings *repo.SettingsRepo
	root     string

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    int
	total   int
	label   string
}

// NewThumbnailService constructs a ThumbnailService over the per-machine config
// store. Per-library dependencies are wired later by Bind.
func NewThumbnailService(config *library.ConfigStore, emitter Emitter, logger *slog.Logger) *ThumbnailService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ThumbnailService{
		config:  config,
		emitter: emitter,
		log:     logger.With(slog.String("subsystem", "thumbs")),
	}
}

// Bind wires the ThumbnailService to an open library's cache, resolver, and repos
// in place.
func (s *ThumbnailService) Bind(core *AppCore) {
	s.cache = core.Thumbs
	s.warmer = thumbs.NewWarmer(core.Thumbs, core.ThumbResolver(), 0, s.log)
	s.assets = core.Assets
	s.sessions = core.Sessions
	s.settings = core.Settings
	s.root = core.Root
}

/* ------------------------------ cache location ----------------------------- */

// ThumbCacheDTO describes where the thumbnail cache currently lives and the two
// possible locations, so the Settings UI can render the choice and the active
// path.
type ThumbCacheDTO struct {
	Location   string `json:"location"`   // "library" | "local"
	ActiveDir  string `json:"activeDir"`  // where thumbnails are written today
	LibraryDir string `json:"libraryDir"` // <root>/.paim/thumbs
	LocalDir   string `json:"localDir"`   // app-support/thumbs/<libraryId>
}

// currentLocation returns the stored location preference, normalized to one of
// the two tokens (empty → library).
func (s *ThumbnailService) currentLocation() (string, error) {
	cfg, err := s.config.Load()
	if err != nil {
		return "", err
	}
	if cfg.ThumbnailCacheLocation == library.ThumbLocationLocal {
		return library.ThumbLocationLocal, nil
	}
	return library.ThumbLocationLibrary, nil
}

// cacheDTO builds the ThumbCacheDTO for the given location.
func (s *ThumbnailService) cacheDTO(location string) (ThumbCacheDTO, error) {
	local, err := library.LocalThumbsDir(s.root)
	if err != nil {
		return ThumbCacheDTO{}, err
	}
	active := s.cache.Dir()
	return ThumbCacheDTO{
		Location:   location,
		ActiveDir:  active,
		LibraryDir: library.LibraryThumbsDir(s.root),
		LocalDir:   local,
	}, nil
}

// ThumbnailCacheLocation returns the current cache location and paths.
func (s *ThumbnailService) ThumbnailCacheLocation(ctx context.Context) (ThumbCacheDTO, error) {
	if err := s.guard(); err != nil {
		return ThumbCacheDTO{}, err
	}
	loc, err := s.currentLocation()
	if err != nil {
		return ThumbCacheDTO{}, err
	}
	return s.cacheDTO(loc)
}

// SetThumbnailCacheLocation persists the per-machine cache-location preference and
// re-points the live cache immediately. The old location is left in place (its
// contents are disposable). location must be "library" or "local".
func (s *ThumbnailService) SetThumbnailCacheLocation(ctx context.Context, location string) (ThumbCacheDTO, error) {
	if err := s.guard(); err != nil {
		return ThumbCacheDTO{}, err
	}
	if location != library.ThumbLocationLibrary && location != library.ThumbLocationLocal {
		return ThumbCacheDTO{}, fmt.Errorf("services: invalid thumbnail cache location %q", location)
	}
	dir, err := library.ResolveThumbCacheDir(s.root, location)
	if err != nil {
		return ThumbCacheDTO{}, err
	}
	cfg, err := s.config.Load()
	if err != nil {
		return ThumbCacheDTO{}, err
	}
	cfg.ThumbnailCacheLocation = location
	if err := s.config.Save(cfg); err != nil {
		return ThumbCacheDTO{}, err
	}
	s.cache.Repoint(dir)
	s.log.Info("thumbnail cache location changed", "location", location, "dir", dir)
	return s.cacheDTO(location)
}

// ClearThumbnailCache deletes the contents of the ACTIVE cache directory. It
// verifies the active dir is one of the two known cache roots for this library
// before removing anything, so a mis-set path can never delete unrelated files.
func (s *ThumbnailService) ClearThumbnailCache(ctx context.Context) error {
	if err := s.guard(); err != nil {
		return err
	}
	known, err := library.KnownThumbCacheDirs(s.root)
	if err != nil {
		return err
	}
	active := s.cache.Dir()
	safe := false
	for _, k := range known {
		if k == active {
			safe = true
			break
		}
	}
	if !safe {
		return fmt.Errorf("services: refusing to clear cache: %q is not a known cache root", active)
	}
	return s.cache.Clear()
}

/* --------------------------- generation parallelism ------------------------ */

// ThumbnailParallelism returns the per-machine thumbnail generation parallelism,
// normalized to the supported range (the default when unset).
func (s *ThumbnailService) ThumbnailParallelism(ctx context.Context) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	cfg, err := s.config.Load()
	if err != nil {
		return 0, err
	}
	return clampThumbnailParallelism(cfg.ThumbnailParallelism), nil
}

// SetThumbnailParallelism persists the per-machine generation parallelism and
// applies it to the live cache immediately (shared by interactive browsing and
// the warm-up). The requested value is clamped to [1, MaxThumbnailParallelism];
// the clamped value is returned.
func (s *ThumbnailService) SetThumbnailParallelism(ctx context.Context, n int) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	n = clampThumbnailParallelism(n)
	cfg, err := s.config.Load()
	if err != nil {
		return 0, err
	}
	cfg.ThumbnailParallelism = n
	if err := s.config.Save(cfg); err != nil {
		return 0, err
	}
	if s.cache != nil {
		s.cache.SetParallelism(n)
	}
	s.log.Info("thumbnail generation parallelism changed", "parallelism", n)
	return n, nil
}

/* -------------------------------- warm-up ---------------------------------- */

// WarmupStatusDTO is the re-attachment snapshot of the current warm-up.
type WarmupStatusDTO struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Label   string `json:"label"`
}

// WarmupStatus returns the current warm-up state so the Library header can render
// compact progress and decide whether to offer the "Pre-generate all" action.
func (s *ThumbnailService) WarmupStatus(ctx context.Context) (WarmupStatusDTO, error) {
	if err := s.guard(); err != nil {
		return WarmupStatusDTO{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return WarmupStatusDTO{Running: s.running, Done: s.done, Total: s.total, Label: s.label}, nil
}

// StartWarmupAll pre-generates 512px thumbnails for the whole catalog. It is
// resumable (cache hits are skipped instantly), so re-running after a partial run
// finishes the rest. Refused with ErrWarmupInProgress if one is already running.
func (s *ThumbnailService) StartWarmupAll(ctx context.Context) (WarmupStatusDTO, error) {
	if err := s.guard(); err != nil {
		return WarmupStatusDTO{}, err
	}
	ids, err := s.assets.ArchivedIDs(ctx)
	if err != nil {
		return WarmupStatusDTO{}, err
	}
	return s.start(ids, "Generating thumbnails")
}

// WarmSessionIfEnabled warms exactly the assets a just-completed import/adopt
// session added, when the "Generate thumbnails after import" setting is on. It is
// the after-import trigger, invoked by the composition root when import:completed
// is emitted; it never blocks the import (it runs after completion) and quietly
// no-ops when the setting is off, the session added no archived assets (e.g. a
// reorganize session), or a warm-up is already running. Errors are returned for
// the caller to log but are non-fatal.
func (s *ThumbnailService) WarmSessionIfEnabled(ctx context.Context, sessionID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	on, err := s.settings.GetBool(ctx, KeyGenerateThumbsAfterImport, DefaultGenerateThumbsAfterImport)
	if err != nil {
		return err
	}
	if !on {
		return nil
	}
	ids, err := s.assets.IDsForSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.start(ids, "Generating thumbnails"); err != nil {
		if errors.Is(err, ErrWarmupInProgress) {
			return nil // a warm-up is already running; nothing to do
		}
		return err
	}
	return nil
}

// start launches a background warm-up over ids. Only one runs at a time.
func (s *ThumbnailService) start(ids []string, label string) (WarmupStatusDTO, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return WarmupStatusDTO{}, ErrWarmupInProgress
	}
	if len(ids) == 0 {
		// Nothing to do; report an immediately-complete status without starting.
		st := WarmupStatusDTO{Running: false, Done: 0, Total: 0, Label: label}
		s.mu.Unlock()
		return st, nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.cancel = cancel
	s.done = 0
	s.total = len(ids)
	s.label = label
	s.mu.Unlock()

	s.sleep.Acquire()
	go s.run(runCtx, ids, label)
	return WarmupStatusDTO{Running: true, Done: 0, Total: len(ids), Label: label}, nil
}

// run executes the warm-up, emitting throttled thumbs:progress and a terminal
// event when it finishes (or is cancelled).
func (s *ThumbnailService) run(ctx context.Context, ids []string, label string) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		done, total := s.done, s.total
		s.mu.Unlock()
		s.sleep.Release()
		// Terminal event so the UI clears its running indicator.
		emitSafe(s.emitter, EventThumbsProgress, ThumbsProgress{Done: done, Total: total, Label: label, Running: false})
	}()

	tr := newThrottle()
	err := s.warmer.Warm(ctx, ids, func(done, total int) {
		s.mu.Lock()
		s.done = done
		s.total = total
		s.mu.Unlock()
		if tr.allow() {
			emitSafe(s.emitter, EventThumbsProgress, ThumbsProgress{Done: done, Total: total, Label: label, Running: true})
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		s.log.Warn("thumbnail warm-up failed", "error", err.Error())
	}
}

// CancelWarmup cancels the active warm-up (if any). It is a no-op when nothing is
// running.
func (s *ThumbnailService) CancelWarmup(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// activeOps reports a running warm-up to the quit guard so quitting mid-warm-up
// is surfaced (it can be safely cancelled and re-run — thumbnails are disposable).
func (s *ThumbnailService) activeOps() []OperationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return nil
	}
	return []OperationInfo{{
		Kind:       "thumbnail_warmup",
		Label:      "Generating thumbnails",
		FilesDone:  s.done,
		FilesTotal: s.total,
	}}
}

// cancelActive cancels a running warm-up via the existing cancel path.
func (s *ThumbnailService) cancelActive() { _ = s.CancelWarmup(context.Background()) }
