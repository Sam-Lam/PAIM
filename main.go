// Command paim is the Photo Archive Integrity Manager desktop application. This
// file is the composition root: it opens the database, wires the logging tee,
// constructs every engine (importer, backup, source, volumes, cleanup, metadata,
// archive) and the service layer that binds them to the Wails frontend, then runs
// the application window. All dependency construction and lifecycle management
// live here; the packages under internal/ contain no global wiring.
package main

import (
	"context"
	"embed"
	"log"
	"log/slog"
	"os"

	"github.com/autolinepro/paim/internal/archive"
	"github.com/autolinepro/paim/internal/backup"
	"github.com/autolinepro/paim/internal/backup/plugins/localfs"
	"github.com/autolinepro/paim/internal/cleanup"
	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/importer"
	"github.com/autolinepro/paim/internal/logging"
	"github.com/autolinepro/paim/internal/mediatype"
	"github.com/autolinepro/paim/internal/metadata"
	"github.com/autolinepro/paim/internal/repo"
	"github.com/autolinepro/paim/internal/services"
	"github.com/autolinepro/paim/internal/source"
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
	application.RegisterEvent[services.LogEntryEvent](services.EventLogEntry)
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

// wailsDialoger adapts the Wails dialog manager to services.Dialoger. Like
// wailsEmitter, its app pointer is set after construction.
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

func run() error {
	ctx := context.Background()

	// Database: PAIM_DB_PATH overrides the default location (used by tests/tools).
	dbPath := os.Getenv("PAIM_DB_PATH")
	if dbPath == "" {
		var err error
		if dbPath, err = db.DefaultPath(); err != nil {
			return err
		}
	}
	gdb, err := db.Open(dbPath)
	if err != nil {
		return err
	}

	// Repositories.
	assetRepo := repo.NewAssetRepo(gdb)
	sessionRepo := repo.NewSessionRepo(gdb)
	sourceRepo := repo.NewSourceRepo(gdb)
	backupRepo := repo.NewBackupRepo(gdb)
	logRepo := repo.NewLogRepo(gdb)
	settingsRepo := repo.NewSettingsRepo(gdb)

	// Logging: tee slog to the console and the LogEntry table, installed as the
	// process default so logging.For(subsystem) persists.
	logHandler, closeLog := logging.New(logRepo, slog.LevelInfo)
	slog.SetDefault(slog.New(logHandler))
	logger := slog.Default()
	logger.Info("PAIM starting", "db", dbPath)

	// Settings-driven configuration.
	cfg, err := services.LoadSettings(ctx, settingsRepo)
	if err != nil {
		closeLog()
		return err
	}
	masterRoot := cfg.MasterLibraryRoot
	layout := archive.New(masterRoot)

	// Metadata extraction (exiftool with graceful degradation).
	extractor := metadata.NewExtractor(logger)

	// Backup subsystem: registry + localfs plugin + persisted queue + manager.
	registry := backup.NewRegistry()
	registry.Register(localfs.PluginName, localfs.New)
	jobQueue := backup.NewRepoJobQueue(gdb)
	providerStore := backup.NewRepoProviderStore(gdb)

	emitter := &wailsEmitter{}
	dialoger := &wailsDialoger{}

	manager := backup.NewManager(jobQueue, assetRepo, providerStore, registry, logger, backup.Options{
		Workers:     cfg.BackupWorkers,
		MaxRetries:  cfg.MaxRetries,
		LibraryRoot: masterRoot,
		ProgressFn:  services.NewBackupProgressEmitter(emitter),
	})

	// Import pipeline (backup manager wired as the atomic backup enqueuer).
	pipeline := importer.New(importer.Config{
		DB:        gdb,
		Assets:    assetRepo,
		Sessions:  sessionRepo,
		Extractor: extractor,
		Layout:    layout,
		Logger:    logger,
		Backup:    manager,
	})

	// Volume enumeration/watching and source identification.
	collector := volumes.NewCollector(logger)
	watcher := volumes.NewWatcher(logger)
	identifier := source.NewIdentifier(collector, sourceRepo, services.Hasher{}, mediatype.IsMedia)

	// Cleanup Assistant.
	analyzer := cleanup.NewAnalyzer(assetRepo, nil, nil, logger)

	// Startup recovery: mark still-running sessions interrupted (resumable) and
	// revert orphaned running backup jobs to pending.
	if n, err := sessionRepo.MarkInterruptedOnStartup(ctx); err != nil {
		logger.Warn("startup: mark interrupted sessions", "error", err.Error())
	} else if n > 0 {
		logger.Info("startup: marked interrupted sessions", "count", n)
	}
	if n, err := backupRepo.ResetRunningOnStartup(ctx); err != nil {
		logger.Warn("startup: reset running backup jobs", "error", err.Error())
	} else if n > 0 {
		logger.Info("startup: reset running backup jobs", "count", n)
	}

	// Services.
	dashboardSvc := services.NewDashboardService(gdb, assetRepo, backupRepo, sourceRepo, logger)
	importSvc := services.NewImportService(pipeline, sessionRepo, settingsRepo, dialoger, emitter, logger)
	sourcesSvc := services.NewSourcesService(collector, identifier, sourceRepo, assetRepo, watcher, emitter, logger)
	historySvc := services.NewHistoryService(sessionRepo, logRepo, logger)
	duplicateSvc := services.NewDuplicateService(gdb, assetRepo, settingsRepo, logger)
	cleanupSvc := services.NewCleanupService(analyzer, dialoger, logger)
	backupSvc := services.NewBackupService(manager, backupRepo, assetRepo, emitter, logger)
	providerSvc := services.NewProviderService(gdb, registry, logger)
	logSvc := services.NewLogService(gdb, logRepo, dialoger, logger)
	settingsSvc := services.NewSettingsService(settingsRepo, extractor.Available())

	// Long-running lifecycle context, cancelled on shutdown.
	rootCtx, rootCancel := context.WithCancel(context.Background())

	app := application.New(application.Options{
		Name:        "Photo Archive Integrity Manager",
		Description: "Photo/video import, archive, verification, backup, and storage reclamation.",
		Services: []application.Service{
			application.NewService(dashboardSvc),
			application.NewService(importSvc),
			application.NewService(sourcesSvc),
			application.NewService(historySvc),
			application.NewService(duplicateSvc),
			application.NewService(cleanupSvc),
			application.NewService(backupSvc),
			application.NewService(providerSvc),
			application.NewService(logSvc),
			application.NewService(settingsSvc),
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
			manager.Stop()
			if err := extractor.Close(); err != nil {
				logger.Warn("shutdown: close extractor", "error", err.Error())
			}
			closeLog()
		},
	})

	// Wire the deferred adapters now that the app exists.
	emitter.app = app
	dialoger.app = app

	// Start the backup worker pool and the volume watcher.
	if err := manager.Start(rootCtx); err != nil {
		logger.Error("could not start backup manager", "error", err.Error())
	}
	if err := sourcesSvc.StartWatching(rootCtx); err != nil {
		logger.Warn("could not start volume watcher", "error", err.Error())
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
