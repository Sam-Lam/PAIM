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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/autolinepro/paim/internal/backup"
	"github.com/autolinepro/paim/internal/backup/plugins/localfs"
	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/library"
	"github.com/autolinepro/paim/internal/logging"
	"github.com/autolinepro/paim/internal/metadata"
	"github.com/autolinepro/paim/internal/repo"
	"github.com/autolinepro/paim/internal/services"
	"github.com/autolinepro/paim/internal/version"
	"github.com/autolinepro/paim/internal/volumes"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

// init registers every service event with its typed payload so the binding
// generator emits a strongly-typed TS API and Emit validates payloads at runtime.
func init() {
	application.RegisterEvent[services.ImportProgress](services.EventImportProgress)
	application.RegisterEvent[services.ImportCompleted](services.EventImportCompleted)
	application.RegisterEvent[services.BackupProgress](services.EventBackupProgress)
	application.RegisterEvent[services.BackupQueueChanged](services.EventBackupQueueChanged)
	application.RegisterEvent[services.VolumeEvent](services.EventVolumeMounted)
	application.RegisterEvent[services.VolumeEvent](services.EventVolumeUnmounted)
	application.RegisterEvent[services.SourceIdentified](services.EventSourceIdentified)
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
	config     *library.ConfigStore
	appVersion string
	rootCtx    context.Context

	libSvc      *services.LibraryService
	dashSvc     *services.DashboardService
	importSvc   *services.ImportService
	sourcesSvc  *services.SourcesService
	historySvc  *services.HistoryService
	dupSvc      *services.DuplicateService
	cleanupSvc  *services.CleanupService
	backupSvc   *services.BackupService
	providerSvc *services.ProviderService
	logSvc      *services.LogService
	settingsSvc *services.SettingsService

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
	logger.Info("PAIM starting", "version", appVersion)

	configStore, err := library.NewConfigStore("")
	if err != nil {
		return err
	}

	emitter := &wailsEmitter{}
	dialoger := &wailsDialoger{}

	registry := backup.NewRegistry()
	registry.Register(localfs.PluginName, localfs.New)

	extractor := metadata.NewExtractor(logger)
	collector := volumes.NewCollector(logger)
	watcher := volumes.NewWatcher(logger)
	gate := services.NewLibraryGate()

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
	comp.dupSvc = services.NewDuplicateService(nil, nil, nil, logger)
	comp.cleanupSvc = services.NewCleanupService(nil, dialoger, logger)
	comp.backupSvc = services.NewBackupService(nil, nil, nil, emitter, logger)
	comp.providerSvc = services.NewProviderService(nil, registry, logger)
	comp.logSvc = services.NewLogService(nil, nil, dialoger, logger)
	comp.settingsSvc = services.NewSettingsService(nil, extractor.Available())

	for _, g := range []interface{ SetGate(*services.LibraryGate) }{
		comp.dashSvc, comp.importSvc, comp.sourcesSvc, comp.historySvc, comp.dupSvc,
		comp.cleanupSvc, comp.backupSvc, comp.providerSvc, comp.logSvc, comp.settingsSvc,
	} {
		g.SetGate(gate)
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
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		OnShutdown: func() {
			logger.Info("PAIM shutting down")
			rootCancel()
			comp.shutdown()
		},
	})

	emitter.app = app
	dialoger.app = app

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
		if err := library.InstallLegacy(ctx, "", root); err != nil {
			return services.OpenOutcome{}, err
		}
	}

	lockPath := library.LockPath(root)
	var lock *library.Lock
	var lockErr error
	if force {
		lock, lockErr = library.ForceAcquireLock(lockPath, c.appVersion)
	} else {
		lock, lockErr = library.AcquireLock(lockPath, c.appVersion)
	}
	if lockErr != nil {
		if held, ok := library.AsLockHeld(lockErr); ok {
			return services.OpenOutcome{LockConflict: held}, nil
		}
		return services.OpenOutcome{}, lockErr
	}

	core, err := services.BuildCore(services.CoreDeps{
		Root:       root,
		AppVersion: c.appVersion,
		Emitter:    c.emitter,
		Registry:   c.registry,
		Extractor:  c.extractor,
		Collector:  c.collector,
		Watcher:    c.watcher,
		Logger:     c.logger,
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
	c.logger.Info("library opened", "library", root, "schema", schema)
	return services.OpenOutcome{Current: cur}, nil
}

// openDev opens an explicit database file (PAIM_DB_PATH) in library-less dev mode.
func (c *composition) openDev(dbPath string) error {
	gdb, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	core, err := services.BuildCore(services.CoreDeps{
		OpenedDB:  gdb,
		Emitter:   c.emitter,
		Registry:  c.registry,
		Extractor: c.extractor,
		Collector: c.collector,
		Logger:    c.logger,
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

	if err := core.Manager.Start(c.rootCtx); err != nil {
		c.logger.Error("could not start backup manager", "error", err.Error())
	}
	if err := c.sourcesSvc.StartWatching(c.rootCtx); err != nil {
		c.logger.Warn("could not start volume watcher", "error", err.Error())
	}

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
	}
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
