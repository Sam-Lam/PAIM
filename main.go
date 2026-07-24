// Command paim is the Photo Archive Integrity Manager desktop application. This
// file is the composition root: it constructs the library-independent
// collaborators (logging, dialogs, events, backup registry, exiftool, volume
// watcher), registers the Wails service layer, and then either opens the last
// portable library on launch or lands in the Welcome state until the user
// creates/opens/migrates one. Opening a library builds the per-library object
// graph (services.BuildCore) and binds it into the already-registered services in
// place, so a cold-start open needs no restart. Switching away from an open
// library updates config and prompts an explicit relaunch.
package main

import (
	"context"
	"embed"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/backup/plugins/localfs"
	"github.com/Sam-Lam/PAIM/internal/backup/plugins/rclone"
	"github.com/Sam-Lam/PAIM/internal/db"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/logging"
	"github.com/Sam-Lam/PAIM/internal/metadata"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/services"
	"github.com/Sam-Lam/PAIM/internal/thumbs"
	"github.com/Sam-Lam/PAIM/internal/version"
	"github.com/Sam-Lam/PAIM/internal/volumes"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/dock"
)

//go:embed all:frontend/dist
var assets embed.FS

// init registers every service event with its typed payload so the binding
// generator emits a strongly-typed TS API and Emit validates payloads at runtime.
func init() {
	application.RegisterEvent[services.ImportProgress](services.EventImportProgress)
	application.RegisterEvent[services.ImportCompleted](services.EventImportCompleted)
	application.RegisterEvent[services.AnalyzeCompleted](services.EventAnalyzeCompleted)
	application.RegisterEvent[services.BackupProgress](services.EventBackupProgress)
	application.RegisterEvent[services.BackupQueueChanged](services.EventBackupQueueChanged)
	application.RegisterEvent[services.BackupBackfillProgress](services.EventBackupBackfillProgress)
	application.RegisterEvent[services.BackupBackfillCompleted](services.EventBackupBackfillCompleted)
	application.RegisterEvent[services.BackupReconcileCompleted](services.EventBackupReconcileCompleted)
	application.RegisterEvent[services.BackupProviderFailing](services.EventBackupProviderFailing)
	application.RegisterEvent[services.VolumeEvent](services.EventVolumeMounted)
	application.RegisterEvent[services.VolumeEvent](services.EventVolumeUnmounted)
	application.RegisterEvent[services.SourceIdentified](services.EventSourceIdentified)
	application.RegisterEvent[services.SourceProgress](services.EventSourceProgress)
	application.RegisterEvent[services.SourceEvaluated](services.EventSourceEvaluated)
	application.RegisterEvent[services.SourceCleared](services.EventSourceCleared)
	application.RegisterEvent[services.CleanupProgress](services.EventCleanupProgress)
	application.RegisterEvent[services.CleanupCompleted](services.EventCleanupCompleted)
	application.RegisterEvent[services.ReorganizePlanProgress](services.EventReorganizePlan)
	application.RegisterEvent[services.LogExportProgress](services.EventLogExportProgress)
	application.RegisterEvent[services.DuplicateProgress](services.EventDuplicateProgress)
	application.RegisterEvent[services.BulkResolveProgress](services.EventBulkResolveProgress)
	application.RegisterEvent[services.BulkResolveSummaryDTO](services.EventBulkResolveCompleted)
	application.RegisterEvent[services.LibraryProgress](services.EventLibraryProgress)
	application.RegisterEvent[services.QuitRequested](services.EventQuitRequested)
	application.RegisterEvent[services.ThumbsProgress](services.EventThumbsProgress)
}

// wailsEmitter adapts the Wails event manager to services.Emitter. Its app
// pointer is filled in after the application is constructed but before it runs,
// so Emit (only ever called at runtime) always has a live app.
type wailsEmitter struct{ app *application.App }

func (e *wailsEmitter) Emit(name string, data any) {
	if e.app != nil {
		e.app.Event.Emit(name, data)
	}
}

// wailsDialoger adapts the Wails dialog manager to services.Dialoger.
type wailsDialoger struct{ app *application.App }

func (d *wailsDialoger) PickFolder(_ context.Context, title string) (string, error) {
	if d.app == nil {
		return "", nil
	}
	return d.app.Dialog.OpenFile().
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true).
		SetTitle(title).
		PromptForSingleSelection()
}

func (d *wailsDialoger) SaveFile(_ context.Context, defaultName string) (string, error) {
	if d.app == nil {
		return "", nil
	}
	return d.app.Dialog.SaveFile().
		SetFilename(defaultName).
		CanCreateDirectories(true).
		PromptForSingleSelection()
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// composition holds the library-independent collaborators, the registered
// services, and the mutable state of the currently open library. It implements
// services.LibraryOpener so the LibraryService (and startup) route every open
// through one code path.
type composition struct {
	logger     *slog.Logger
	emitter    *wailsEmitter
	dialoger   *wailsDialoger
	registry   *backup.Registry
	extractor  metadata.Extractor
	collector  *volumes.Collector
	watcher    *volumes.Watcher
	gate       *services.LibraryGate
	sleep      *services.SleepGuard
	tracker    *services.ActivityTracker
	yield      *services.ForegroundYield
	badge      *services.BadgeController
	config     *library.ConfigStore
	appVersion string
	rootCtx    context.Context

	// quitConfirmed is set by ConfirmQuit (via the injected quit closure) so the
	// re-entrant quit it triggers passes the ShouldQuit interception hook straight
	// through to the normal graceful shutdown.
	quitConfirmed atomic.Bool

	libSvc       *services.LibraryService
	dashSvc      *services.DashboardService
	importSvc    *services.ImportService
	sourcesSvc   *services.SourcesService
	historySvc   *services.HistoryService
	dupSvc       *services.DuplicateService
	cleanupSvc   *services.CleanupService
	backupSvc    *services.BackupService
	providerSvc  *services.ProviderService
	logSvc       *services.LogService
	settingsSvc  *services.SettingsService
	browserSvc   *services.BrowserService
	thumbnailSvc *services.ThumbnailService
	snapshotSvc  *services.SnapshotService
	appSvc       *services.AppService

	mu       sync.Mutex
	core     *services.AppCore
	lock     *library.Lock
	closeLog func()
}

func run() error {
	appVersion := version.Version

	// Logging: tee slog to console and (once a library opens) the LogEntry table.
	// Until a library is open there is no catalog to persist into; a console-only
	// default keeps early messages visible. logging.New requires a LogRepo, so the
	// persistent tee is installed when the library opens (see bindAll).
	logger := slog.Default()
	logger.Info("PAIM starting", "version", version.Full())

	configStore, err := library.NewConfigStore("")
	if err != nil {
		return err
	}

	emitter := &wailsEmitter{}
	dialoger := &wailsDialoger{}

	registry := backup.NewRegistry()
	registry.Register(localfs.PluginName, localfs.New)
	registry.Register(rclone.PluginName, rclone.New)

	extractor := metadata.NewExtractor(logger)
	collector := volumes.NewCollector(logger)
	watcher := volumes.NewWatcher(logger)
	gate := services.NewLibraryGate()
	sleep := services.NewSleepGuard(logger)
	tracker := services.NewActivityTracker()

	// Foreground-yield gate: while a foreground operation (import/analyze/…) runs,
	// the backup manager stops claiming new upload jobs so its reads don't
	// seek-compete on spinning media. Seed the live enabled flag from the
	// per-machine PauseBackupsDuringForeground preference (default true).
	pausePref := true
	if cfg, cerr := configStore.Load(); cerr == nil {
		pausePref = cfg.PauseBackupsDuringForegroundEnabled()
	}
	yield := services.NewForegroundYield(tracker, pausePref)

	// macOS dock badge: reflect the primary running long operation's progress as a
	// dock badge (e.g. "42%"), cleared when nothing runs. Wails v3's built-in dock
	// service wraps NSDockTile setBadgeLabel:; the controller observes the same
	// activity tracker the quit guard uses and throttles updates. If the dock icon
	// is unavailable the setter's calls simply error and the controller no-ops.
	dockService := dock.New()
	badgeCtrl := services.NewBadgeController(tracker, dockService, logger)

	rootCtx, rootCancel := context.WithCancel(context.Background())

	comp := &composition{
		logger:     logger,
		emitter:    emitter,
		dialoger:   dialoger,
		registry:   registry,
		extractor:  extractor,
		collector:  collector,
		watcher:    watcher,
		gate:       gate,
		sleep:      sleep,
		tracker:    tracker,
		yield:      yield,
		badge:      badgeCtrl,
		config:     configStore,
		appVersion: appVersion,
		rootCtx:    rootCtx,
	}

	// Construct the services once. DB-backed services start with nil catalog deps
	// and a closed gate; they become live when a library is bound in place.
	comp.libSvc = services.NewLibraryService(configStore, dialoger, comp, appVersion, logger)
	comp.dashSvc = services.NewDashboardService(nil, nil, nil, nil, logger)
	comp.importSvc = services.NewImportService(nil, nil, nil, dialoger, emitter, logger)
	comp.sourcesSvc = services.NewSourcesService(collector, nil, nil, nil, watcher, emitter, logger)
	comp.historySvc = services.NewHistoryService(nil, nil, logger)
	comp.dupSvc = services.NewDuplicateService(nil, nil, nil, nil, emitter, logger)
	comp.cleanupSvc = services.NewCleanupService(nil, dialoger, emitter, logger)
	comp.backupSvc = services.NewBackupService(nil, nil, nil, configStore, yield, emitter, logger)
	comp.providerSvc = services.NewProviderService(nil, registry, logger)
	comp.logSvc = services.NewLogService(nil, nil, dialoger, emitter, logger)
	comp.settingsSvc = services.NewSettingsService(nil, extractor.Available())
	comp.browserSvc = services.NewBrowserService(nil, nil, nil, nil, logger)
	comp.thumbnailSvc = services.NewThumbnailService(configStore, emitter, logger)
	comp.snapshotSvc = services.NewSnapshotService(configStore, dialoger, logger)
	comp.appSvc = services.NewAppService(tracker)

	for _, g := range []interface{ SetGate(*services.LibraryGate) }{
		comp.dashSvc, comp.importSvc, comp.sourcesSvc, comp.historySvc, comp.dupSvc,
		comp.cleanupSvc, comp.backupSvc, comp.providerSvc, comp.logSvc, comp.settingsSvc,
		comp.browserSvc, comp.thumbnailSvc, comp.snapshotSvc,
	} {
		g.SetGate(gate)
	}

	// Long-running operations hold a macOS sleep assertion (see SleepGuard) so an
	// unattended import/analyze/reorganize/safe-to-erase/cleanup does not stall
	// when the Mac sleeps. Only the services that run such jobs need it.
	for _, sa := range []interface{ SetSleepGuard(*services.SleepGuard) }{
		comp.importSvc, comp.sourcesSvc, comp.cleanupSvc, comp.thumbnailSvc, comp.backupSvc, comp.dupSvc,
	} {
		sa.SetSleepGuard(sleep)
	}

	// Quit guard: the activity tracker aggregates every service's running long
	// operation so the ShouldQuit hook (below) can veto a quit with work in flight
	// and ConfirmQuit can cancel it. Backup contributes only its currently-
	// uploading jobs (not the pending queue). Registration is once, before Run.
	tracker.Register(comp.importSvc)
	tracker.Register(comp.sourcesSvc)
	tracker.Register(comp.cleanupSvc)
	tracker.Register(comp.backupSvc)
	tracker.Register(comp.thumbnailSvc)
	tracker.Register(comp.dupSvc)

	// The browser's event-folder rename refuses while any long operation is in
	// flight (renaming under a running move is unsafe), so it reads the same
	// activity tracker the quit guard uses.
	comp.browserSvc.SetActivity(tracker)

	// The coverage view's bulk "Queue N to <provider>" action emits
	// backup:queue-changed so the Backup Queue and provider cards refresh.
	comp.browserSvc.SetEmitter(emitter)

	// After an import/adopt session completes, warm that session's thumbnails when
	// the setting is on (default). This runs after completion — never inline during
	// the import — and quietly no-ops for reorganize sessions or when disabled.
	comp.importSvc.OnCompleted = func(sessionID string) {
		if err := comp.thumbnailSvc.WarmSessionIfEnabled(context.Background(), sessionID); err != nil {
			logger.Warn("post-import thumbnail warm-up", "sessionId", sessionID, "error", err.Error())
		}
	}

	app := application.New(application.Options{
		Name:        "Photo Archive Integrity Manager",
		Description: "Photo/video import, archive, verification, backup, and storage reclamation.",
		Services: []application.Service{
			application.NewService(comp.libSvc),
			application.NewService(comp.dashSvc),
			application.NewService(comp.importSvc),
			application.NewService(comp.sourcesSvc),
			application.NewService(comp.historySvc),
			application.NewService(comp.dupSvc),
			application.NewService(comp.cleanupSvc),
			application.NewService(comp.backupSvc),
			application.NewService(comp.providerSvc),
			application.NewService(comp.logSvc),
			application.NewService(comp.settingsSvc),
			application.NewService(comp.browserSvc),
			application.NewService(comp.thumbnailSvc),
			application.NewService(comp.snapshotSvc),
			application.NewService(comp.appSvc),
			application.NewService(dockService),
		},
		Assets: application.AssetOptions{
			Handler:    application.AssetFileServerFS(assets),
			Middleware: comp.thumbMiddleware(),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		// ShouldQuit is the single quit-interception chokepoint. On macOS AppKit's
		// applicationShouldTerminate: routes Cmd+Q, the menu Quit, AND (because
		// ApplicationShouldTerminateAfterLastWindowClosed is true) the last-window
		// close all through here. Returning false vetoes the quit (NSTerminateCancel);
		// true lets AppKit run cleanup() -> OnShutdown exactly once. With no active
		// operations (or once ConfirmQuit has set quitConfirmed) the quit proceeds
		// immediately; otherwise we veto and emit app:quit-requested with the live
		// operations so the frontend can offer "Quit anyway". It runs on the main
		// thread and must not block, so it only snapshots and emits.
		ShouldQuit: func() bool {
			if comp.quitConfirmed.Load() {
				return true
			}
			ops := tracker.Snapshot()
			if len(ops) == 0 {
				return true
			}
			logger.Info("quit requested with active operations; vetoing", "operations", len(ops))
			emitter.Emit(services.EventQuitRequested, services.QuitRequested{Operations: ops})
			return false
		},
		OnShutdown: func() {
			logger.Info("PAIM shutting down")
			rootCancel()
			comp.shutdown()
		},
	})

	emitter.app = app
	dialoger.app = app

	// Wire ConfirmQuit's quit action now that the app exists: flag the interception
	// hook so the re-entrant quit passes straight through, then trigger the quit on
	// a goroutine (app.Quit blocks until the app tears down) so ConfirmQuit returns.
	comp.appSvc.Quit = func() {
		comp.quitConfirmed.Store(true)
		go app.Quit()
	}

	// Startup open: the PAIM_DB_PATH dev escape hatch bypasses library mode
	// entirely (plain db.Open, absolute paths, no lock/config). Otherwise try the
	// last library; if it opens and locks cleanly the app starts on the Dashboard,
	// else it lands in the Welcome state.
	if devPath := os.Getenv("PAIM_DB_PATH"); devPath != "" {
		if err := comp.openDev(devPath); err != nil {
			return err
		}
	} else if cfg, err := configStore.Load(); err == nil && cfg.LastLibrary != "" {
		if _, oerr := comp.Open(rootCtx, cfg.LastLibrary, false, false); oerr != nil {
			logger.Warn("could not open last library; starting in Welcome state",
				"library", cfg.LastLibrary, "error", oerr.Error())
		}
	}

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Photo Archive Integrity Manager",
		Width:  1280,
		Height: 800,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(9, 9, 11),
		URL:              "/",
	})

	// Reflect long-operation progress on the dock badge for the life of the app
	// (throttled; clears itself when idle and on rootCtx cancellation at shutdown).
	go comp.badge.Run(rootCtx)

	return app.Run()
}

// Open implements services.LibraryOpener. When a library is already open it
// treats the request as a switch (updates config, signals relaunch). Otherwise it
// (optionally migrates the legacy catalog then) acquires the single-writer lock,
// builds the per-library object graph, binds it into the services in place,
// starts the engines, opens the gate, and records the open in config.
func (c *composition) Open(ctx context.Context, root string, force, migrateLegacy bool) (services.OpenOutcome, error) {
	c.mu.Lock()
	alreadyOpen := c.core != nil
	c.mu.Unlock()

	if alreadyOpen {
		// A different library is already open: services cannot be re-registered at
		// runtime, so switching requires an explicit relaunch. Update config so the
		// next launch opens the chosen library.
		if err := c.config.RecordOpened(root, ""); err != nil {
			return services.OpenOutcome{}, err
		}
		c.logger.Info("library switch requested; relaunch required", "library", root)
		return services.OpenOutcome{NeedsRelaunch: true}, nil
	}

	if migrateLegacy {
		// Copying + verifying the legacy catalog can take a while on a large DB.
		// Surface a labeled phase so Welcome can show progress and a "don't quit"
		// warning. Not cancellable: a half-installed catalog must never be left.
		c.emitLibraryProgress("installing-legacy", "Copying and verifying legacy catalog…")
		if err := library.InstallLegacy(ctx, "", root); err != nil {
			return services.OpenOutcome{}, err
		}
	}

	lockPath := library.LockPath(root)
	var lock *library.Lock
	var lockErr error
	// The lock file records the FULL build string (semver + commit + date); the
	// library_meta bookkeeping keeps just the semver (see c.appVersion below).
	lockVersion := version.Full()
	if force {
		lock, lockErr = library.ForceAcquireLock(lockPath, lockVersion)
	} else {
		lock, lockErr = library.AcquireLock(lockPath, lockVersion)
	}
	if lockErr != nil {
		if held, ok := library.AsLockHeld(lockErr); ok {
			return services.OpenOutcome{LockConflict: held}, nil
		}
		return services.OpenOutcome{}, lockErr
	}

	// Opening the catalog runs the migration framework (backup + pending
	// migrations) when the library is behind. Emit a determinate-ish "migrating"
	// phase so the Welcome screen shows the upgrade is underway and warns the user
	// not to quit; migrations are not cancellable (they run to completion or roll
	// back atomically).
	c.emitLibraryProgress("migrating", "Upgrading library catalog — don't quit PAIM")

	// Resolve the per-machine thumbnail preferences so the cache opens correctly
	// from the start: the cache-location directory (in-library or this Mac's local
	// disk) and the generation parallelism. A config error falls back to defaults.
	thumbDir := ""
	thumbParallelism := 0
	if cfg, cerr := c.config.Load(); cerr == nil {
		if d, derr := library.ResolveThumbCacheDir(root, cfg.ThumbnailCacheLocation); derr == nil {
			thumbDir = d
		}
		thumbParallelism = cfg.ThumbnailParallelism
	}

	core, err := services.BuildCore(services.CoreDeps{
		Root:             root,
		AppVersion:       c.appVersion,
		Emitter:          c.emitter,
		Registry:         c.registry,
		Extractor:        c.extractor,
		Collector:        c.collector,
		Watcher:          c.watcher,
		Logger:           c.logger,
		ThumbCacheDir:    thumbDir,
		ThumbParallelism: thumbParallelism,
		ForegroundGate:   c.yield.Gate,
	})
	if err != nil {
		_ = lock.Release()
		return services.OpenOutcome{}, err
	}

	schema := db.LatestSchemaVersion()
	if core.Meta != nil {
		schema = core.Meta.SchemaVersion
	}
	c.activate(core, lock)
	if err := c.config.RecordOpened(root, library.DefaultName(root)); err != nil {
		c.logger.Warn("could not record library in config", "error", err.Error())
	}

	cur := &services.CurrentLibraryDTO{
		Path:          root,
		Name:          library.DefaultName(root),
		SchemaVersion: schema,
		AppVersion:    c.appVersion,
		OpenedAt:      time.Now(),
	}
	c.libSvc.SetCurrent(cur)
	c.emitLibraryProgress("done", "Library ready")
	c.logger.Info("library opened", "library", root, "schema", schema)
	return services.OpenOutcome{Current: cur}, nil
}

// emitLibraryProgress emits a library:progress event describing a phase of a
// library open that runs migrations / legacy installation, so the Welcome screen
// can render labeled phases and a "don't quit" warning instead of a bare spinner.
func (c *composition) emitLibraryProgress(phase, message string) {
	c.emitter.Emit(services.EventLibraryProgress, services.LibraryProgress{Phase: phase, Message: message})
}

// thumbMiddleware routes GET /thumb/{assetID} to the thumbnail handler and
// passes everything else through to the default Wails asset server. The handler
// is constructed once; it reads the currently-open library per request via
// thumbBinding, so it always serves from the live catalog and cache.
func (c *composition) thumbMiddleware() application.Middleware {
	handler := thumbs.NewHandler(c.thumbBinding, c.logger)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, thumbs.URLPrefix) {
				handler.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// thumbBinding returns the currently-open library's thumbnail dependencies, or
// ok=false when no library is open (the handler then responds 503).
func (c *composition) thumbBinding(_ context.Context) (thumbs.Binding, bool) {
	c.mu.Lock()
	core := c.core
	c.mu.Unlock()
	if core == nil || core.Thumbs == nil {
		return thumbs.Binding{}, false
	}
	return thumbs.Binding{Assets: core.ThumbResolver(), Cache: core.Thumbs}, true
}

// openDev opens an explicit database file (PAIM_DB_PATH) in library-less dev mode.
func (c *composition) openDev(dbPath string) error {
	gdb, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	core, err := services.BuildCore(services.CoreDeps{
		OpenedDB:       gdb,
		Emitter:        c.emitter,
		Registry:       c.registry,
		Extractor:      c.extractor,
		Collector:      c.collector,
		Logger:         c.logger,
		ForegroundGate: c.yield.Gate,
	})
	if err != nil {
		return err
	}
	c.activate(core, nil)
	c.libSvc.SetCurrent(&services.CurrentLibraryDTO{
		Path:          filepath.Dir(dbPath),
		Name:          "(dev) " + filepath.Base(dbPath),
		SchemaVersion: db.LatestSchemaVersion(),
		AppVersion:    c.appVersion,
		OpenedAt:      time.Now(),
	})
	c.logger.Info("PAIM_DB_PATH dev mode: opened database directly", "db", dbPath)
	return nil
}

// activate installs the persistent logging tee, runs startup recovery, binds the
// core into every service, starts the engines, and opens the gate.
func (c *composition) activate(core *services.AppCore, lock *library.Lock) {
	// Persistent logging tee into this library's LogEntry table.
	logHandler, closeLog := logging.New(repo.NewLogRepo(core.DB), slog.LevelInfo)
	slog.SetDefault(slog.New(logHandler))
	c.mu.Lock()
	c.closeLog = closeLog
	c.mu.Unlock()

	c.recover(core)

	c.dashSvc.Bind(core)
	c.importSvc.Bind(core)
	c.sourcesSvc.Bind(core)
	c.historySvc.Bind(core)
	c.dupSvc.Bind(core)
	c.cleanupSvc.Bind(core)
	c.backupSvc.Bind(core)
	c.providerSvc.Bind(core)
	c.logSvc.Bind(core)
	c.settingsSvc.Bind(core)
	c.browserSvc.Bind(core)
	c.thumbnailSvc.Bind(core)
	c.snapshotSvc.Bind(core)

	if err := core.Manager.Start(c.rootCtx); err != nil {
		c.logger.Error("could not start backup manager", "error", err.Error())
	}
	if err := c.sourcesSvc.StartWatching(c.rootCtx); err != nil {
		c.logger.Warn("could not start volume watcher", "error", err.Error())
	}
	// Arm the catalog-snapshot timer for the life of the app (idles when no
	// destination/interval is configured; re-arms when the setting changes).
	c.snapshotSvc.StartTimer(c.rootCtx)

	c.mu.Lock()
	c.core = core
	c.lock = lock
	c.mu.Unlock()
	c.gate.Open()
}

// recover marks still-running sessions interrupted and reverts orphaned running
// backup jobs to pending, as at every startup.
func (c *composition) recover(core *services.AppCore) {
	ctx := context.Background()
	if n, err := core.Sessions.MarkInterruptedOnStartup(ctx); err != nil {
		c.logger.Warn("startup: mark interrupted sessions", "error", err.Error())
	} else if n > 0 {
		c.logger.Info("startup: marked interrupted sessions", "count", n)
	}
	if n, err := core.Backups.ResetRunningOnStartup(ctx); err != nil {
		c.logger.Warn("startup: reset running backup jobs", "error", err.Error())
	} else if n > 0 {
		c.logger.Info("startup: reset running backup jobs", "count", n)
	}
}

// shutdown stops the current library's engines and releases its lock.
func (c *composition) shutdown() {
	c.mu.Lock()
	core := c.core
	lock := c.lock
	closeLog := c.closeLog
	c.mu.Unlock()

	if core != nil {
		core.Manager.Stop()
		// Best-effort final catalog snapshot on graceful quit (when configured and
		// the interval is not "off"). Bounded so it never wedges shutdown.
		snapCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c.snapshotSvc.SnapshotOnQuit(snapCtx)
		cancel()
	}
	// Force-stop the sleep assertion so no caffeinate child outlives the app.
	c.sleep.Shutdown()
	if err := c.extractor.Close(); err != nil {
		c.logger.Warn("shutdown: close extractor", "error", err.Error())
	}
	if lock != nil {
		if err := lock.Release(); err != nil {
			c.logger.Warn("shutdown: release library lock", "error", err.Error())
		}
	}
	if closeLog != nil {
		closeLog()
	}
}
