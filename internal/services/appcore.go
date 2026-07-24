package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/cleanup"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/metadata"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/source"
	"github.com/Sam-Lam/PAIM/internal/thumbs"
	"github.com/Sam-Lam/PAIM/internal/volumes"
	"gorm.io/gorm"
)

// ErrNoLibrary is returned by gated service methods when no library is open. The
// frontend detects it (and the null LibraryService.Current) to show the Welcome
// screen instead of a page that has no catalog to read.
var ErrNoLibrary = errors.New("services: no library open")

// LibraryGate is the shared open/closed switch every DB-backed service consults.
// It is closed until a library is opened (at startup or from Welcome) and reopens
// atomically once the object graph is bound in place. A nil *LibraryGate is
// treated as always-open, so services constructed directly in unit tests (which
// pass no gate) are never gated.
type LibraryGate struct {
	mu   sync.RWMutex
	open bool
}

// NewLibraryGate returns a closed gate.
func NewLibraryGate() *LibraryGate { return &LibraryGate{} }

// Guard returns ErrNoLibrary when the gate is closed. A nil gate never blocks.
func (g *LibraryGate) Guard() error {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.open {
		return ErrNoLibrary
	}
	return nil
}

// IsOpen reports whether a library is open. A nil gate reports true.
func (g *LibraryGate) IsOpen() bool {
	if g == nil {
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.open
}

// setOpen flips the gate.
func (g *LibraryGate) setOpen(v bool) {
	g.mu.Lock()
	g.open = v
	g.mu.Unlock()
}

// Open marks a library as open (called by main.go after the object graph is
// bound in place).
func (g *LibraryGate) Open() { g.setOpen(true) }

// Close marks the library as closed (no library open).
func (g *LibraryGate) Close() { g.setOpen(false) }

// gated is embedded by every DB-backed service to share the gate plumbing. In
// production main.go injects the shared gate via SetGate before the app runs; in
// unit tests the gate is left nil (never gated). guard is the one-line check each
// public method performs before touching the catalog.
type gated struct {
	gate *LibraryGate
}

// SetGate injects the shared library gate. Called once by main.go after
// construction; never called in unit tests (leaving the service ungated).
func (g *gated) SetGate(gate *LibraryGate) { g.gate = gate }

// guard returns ErrNoLibrary when no library is open.
func (g *gated) guard() error { return g.gate.Guard() }

// AppCore is the per-library object graph: the opened catalog, its repositories,
// and the engines that operate on it. It is built by BuildCore when a library is
// opened and bound into the (already Wails-registered) services in place, so a
// cold-start open needs no application restart.
type AppCore struct {
	Root string
	Meta *db.Meta

	DB       *gorm.DB
	Assets   *repo.AssetRepo
	Sessions *repo.SessionRepo
	Sources  *repo.SourceRepo
	Backups  *repo.BackupRepo
	Logs     *repo.LogRepo
	Settings *repo.SettingsRepo

	Manager    *backup.Manager
	Pipeline   *importer.Pipeline
	Analyzer   *cleanup.Analyzer
	Identifier *source.Identifier
	// Collector enumerates/describes mounted volumes. It is library-independent
	// (owned by main.go, passed through CoreDeps) but carried here so services
	// bound to the core — e.g. the ImportService source auto-link — can reach it
	// without a separate wiring path.
	Collector *volumes.Collector
	// Thumbs is this library's disposable thumbnail cache
	// (<root>/.paim/thumbs). The thumbnail HTTP handler serves from it.
	Thumbs *thumbs.Cache
}

// CoreDeps carries the library-independent collaborators BuildCore needs. They
// are owned by main.go (constructed once) and reused across opens.
type CoreDeps struct {
	Root       string
	AppVersion string
	Emitter    Emitter
	Registry   *backup.Registry
	Extractor  metadata.Extractor
	Collector  *volumes.Collector
	Watcher    *volumes.Watcher
	Logger     *slog.Logger

	// ThumbCacheDir, when non-empty, is the directory the thumbnail cache writes
	// to (resolved by main.go from the per-machine location preference). Empty
	// falls back to the in-library "<root>/.paim/thumbs".
	ThumbCacheDir string

	// ThumbParallelism is the per-machine "Thumbnail generation parallelism"
	// setting (shared by interactive browsing and the warm-up). < 1 falls back to
	// DefaultThumbnailParallelism.
	ThumbParallelism int

	// ForegroundGate, when set, is passed to the backup Manager as its
	// Options.ForegroundGate: while it reports true the manager stops claiming new
	// upload jobs (in-flight uploads finish). main.go builds it from the activity
	// tracker filtered to foreground kinds plus the live PauseBackupsDuringForeground
	// preference (see ForegroundYield). Nil disables yielding.
	ForegroundGate func() bool

	// OpenedDB, when non-nil, is used instead of opening a library catalog under
	// Root. It backs the PAIM_DB_PATH dev escape hatch (plain db.Open, absolute
	// archive paths, no lock/config). Root is left empty in that mode.
	OpenedDB *gorm.DB
}

// BuildCore opens the library catalog at deps.Root and constructs the full
// per-library object graph (repositories + engines), wiring the backup manager
// and importer to store/resolve archive paths relative to the root. It does NOT
// start the manager/watcher or run startup recovery — main.go does that after
// binding, so the lifecycle stays in the composition root.
func BuildCore(deps CoreDeps) (*AppCore, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var (
		gdb  *gorm.DB
		meta *db.Meta
		err  error
	)
	if deps.OpenedDB != nil {
		gdb = deps.OpenedDB
	} else {
		gdb, meta, err = db.OpenLibrary(deps.Root, deps.AppVersion)
		if err != nil {
			return nil, err
		}
	}

	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	sources := repo.NewSourceRepo(gdb)
	backups := repo.NewBackupRepo(gdb)
	logs := repo.NewLogRepo(gdb)
	settings := repo.NewSettingsRepo(gdb)

	// Persist the derived (read-only) master root so existing settings-driven code
	// paths (import destination default, trash root) resolve to the library root.
	// In the dev escape hatch (Root empty) leave the stored setting untouched.
	cfg, err := LoadSettings(context.Background(), settings)
	if err != nil {
		return nil, err
	}
	if deps.Root != "" && cfg.MasterLibraryRoot != deps.Root {
		if err := settings.Set(context.Background(), KeyMasterLibraryRoot, deps.Root); err != nil {
			return nil, err
		}
		cfg.MasterLibraryRoot = deps.Root
	}

	layout := archive.New(deps.Root)
	jobQueue := backup.NewRepoJobQueue(gdb)
	providerStore := backup.NewRepoProviderStore(gdb)
	// The queue-changed emitter reads live provider cooldowns from the Manager, but
	// the Manager needs the emitter at construction; a var captured by the closure
	// (assigned just below) breaks the cycle — the closure only runs at emit time.
	var manager *backup.Manager
	manager = backup.NewManager(jobQueue, assets, providerStore, deps.Registry, logger, backup.Options{
		Workers:           cfg.BackupWorkers,
		MaxRetries:        cfg.MaxRetries,
		LibraryRoot:       deps.Root,
		ProgressFn:        NewBackupProgressEmitter(deps.Emitter),
		ForegroundGate:    deps.ForegroundGate,
		OnProviderFailing: NewProviderFailingEmitter(deps.Emitter, gdb),
		OnQueueChanged: NewBackupQueueChangedEmitter(deps.Emitter, backups, func() []ProviderCooldownDTO {
			if manager == nil {
				return nil
			}
			return cooldownDTOs(manager.Cooldowns())
		}, func() bool {
			return manager != nil && manager.Yielding()
		}, func() (float64, time.Time) {
			if manager == nil {
				return 0, time.Time{}
			}
			return manager.CompletionStats()
		}),
	})

	pipeline := importer.New(importer.Config{
		DB:          gdb,
		Assets:      assets,
		Sessions:    sessions,
		Extractor:   deps.Extractor,
		Layout:      layout,
		Logger:      logger,
		Backup:      manager,
		LibraryRoot: deps.Root,
	})

	analyzer := cleanup.NewAnalyzer(assets, nil, nil, logger)
	analyzer.Root = deps.Root

	identifier := source.NewIdentifier(deps.Collector, sources, Hasher{}, mediatype.IsMedia)

	// The thumbnail cache lives under the library root's .paim/thumbs by default
	// (disposable, travels with the library), or at an explicit ThumbCacheDir when
	// the per-machine preference moves it to this Mac's local disk. In the dev
	// escape hatch (Root empty, no dir) it falls back to a cwd-relative cache.
	// Generation concurrency is the per-machine parallelism setting (default 2).
	thumbConc := deps.ThumbParallelism
	if thumbConc < 1 {
		thumbConc = DefaultThumbnailParallelism
	}
	var thumbCache *thumbs.Cache
	if deps.ThumbCacheDir != "" {
		thumbCache = thumbs.NewInDir(deps.ThumbCacheDir, thumbConc, logger)
	} else {
		thumbCache = thumbs.New(deps.Root, thumbConc, logger)
	}

	return &AppCore{
		Root:       deps.Root,
		Meta:       meta,
		DB:         gdb,
		Assets:     assets,
		Sessions:   sessions,
		Sources:    sources,
		Backups:    backups,
		Logs:       logs,
		Settings:   settings,
		Manager:    manager,
		Pipeline:   pipeline,
		Analyzer:   analyzer,
		Identifier: identifier,
		Collector:  deps.Collector,
		Thumbs:     thumbCache,
	}, nil
}
