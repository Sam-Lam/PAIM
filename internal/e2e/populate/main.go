// Command populate runs essentially the full PAIM e2e scenario against a REAL
// on-disk database and working directory, leaving all data in place so the Wails
// GUI can be launched against it for manual inspection. It reuses the same
// fixture builders as the automated scenario test (internal/e2e).
//
// Usage:
//
//	go run ./internal/e2e/populate <db-path> <workdir>
//
// It builds a dummy source tree and a separate "existing library" adopt tree
// under <workdir>, then wires the real components (exiftool extractor, archive
// layout under <workdir>/Master Library, an enabled localfs backup provider
// rooted at <workdir>/Backup, and the backup manager) exactly like the test, and:
//
//   - dry-runs + copy-imports the source tree (event "Test Shoot"),
//   - waits for backups to complete,
//   - re-imports the same tree (an "already-imported" session in History),
//   - adopts the separate tree in place (reorganizing into the layout), and
//   - runs one import that ends in failure (an unreadable file) for History variety.
//
// It is safe to run repeatedly against the same DB: a second run simply adds
// already-imported/skipped sessions.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/autolinepro/paim/internal/archive"
	"github.com/autolinepro/paim/internal/backup"
	"github.com/autolinepro/paim/internal/backup/plugins/localfs"
	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/e2e"
	"github.com/autolinepro/paim/internal/importer"
	"github.com/autolinepro/paim/internal/metadata"
	"github.com/autolinepro/paim/internal/repo"
	"gorm.io/gorm"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: populate <db-path> <workdir>")
		os.Exit(2)
	}
	dbPath, workdir := os.Args[1], os.Args[2]
	if err := run(dbPath, workdir); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(dbPath, workdir string) error {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	bin := lookupExiftool()
	if bin == "" {
		return fmt.Errorf("exiftool not found on PATH or at /opt/homebrew/bin/exiftool")
	}

	srcRoot := filepath.Join(workdir, "SD Card")
	adoptRoot := filepath.Join(workdir, "Existing Library")
	masterRoot := filepath.Join(workdir, "Master Library")
	backupRoot := filepath.Join(workdir, "Backup")
	for _, d := range []string{masterRoot, backupRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", d, err)
		}
	}

	// Fixtures (deterministic content, so re-runs hash-match -> already imported).
	srcTree, err := e2e.BuildSourceTree(srcRoot, bin)
	if err != nil {
		return fmt.Errorf("build source tree: %w", err)
	}
	adoptTree, err := e2e.BuildAdoptTree(adoptRoot, bin)
	if err != nil {
		return fmt.Errorf("build adopt tree: %w", err)
	}

	// Database + repositories.
	gdb, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db %q: %w", dbPath, err)
	}
	assets := repo.NewAssetRepo(gdb)
	sessions := repo.NewSessionRepo(gdb)
	backupRepo := repo.NewBackupRepo(gdb)

	// Metadata + backup subsystem + import pipeline (main.go-style wiring).
	extractor := metadata.NewExtractor(logger)
	defer extractor.Close()

	registry := backup.NewRegistry()
	registry.Register(localfs.PluginName, localfs.New)
	jobQueue := backup.NewRepoJobQueue(gdb)
	providerStore := backup.NewRepoProviderStore(gdb)
	manager := backup.NewManager(jobQueue, assets, providerStore, registry, logger, backup.Options{
		Workers:      2,
		MaxRetries:   3,
		BaseBackoff:  10 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		LibraryRoot:  masterRoot,
	})
	pipeline := importer.New(importer.Config{
		DB:        gdb,
		Assets:    assets,
		Sessions:  sessions,
		Extractor: extractor,
		Layout:    archive.New(masterRoot),
		Logger:    logger,
		Backup:    manager,
	})

	// Enable a localfs backup provider (idempotent: reuse an existing enabled one).
	if err := ensureProvider(gdb, backupRoot); err != nil {
		return err
	}

	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("start backup manager: %w", err)
	}
	defer manager.Stop()

	// 1) Dry run + copy import of the source tree.
	copyOpts := importer.Options{Mode: importer.ModeCopy, SourceRoot: srcRoot, DestinationRoot: masterRoot, EventName: "Test Shoot"}
	scan, err := pipeline.Scan(ctx, srcRoot, nil)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	dry, err := pipeline.DryRun(ctx, scan, copyOpts, nil)
	if err != nil {
		return fmt.Errorf("dry run: %w", err)
	}
	sess1, err := pipeline.Run(ctx, copyOpts, nil)
	if err != nil {
		return fmt.Errorf("copy import: %w", err)
	}

	// 2) Wait for backups to complete.
	if err := waitForBackups(ctx, backupRepo, 60*time.Second); err != nil {
		return fmt.Errorf("wait for backups: %w", err)
	}

	// 3) Re-import the same tree -> an already-imported (skipped) session.
	sess2, err := pipeline.Run(ctx, copyOpts, nil)
	if err != nil {
		return fmt.Errorf("re-import: %w", err)
	}

	// 4) Adopt the separate tree in place, reorganizing into the layout.
	adoptOpts := importer.Options{Mode: importer.ModeAdopt, SourceRoot: adoptRoot, DestinationRoot: adoptRoot, EventName: "Legacy", Reorganize: true}
	sess3, err := pipeline.Run(ctx, adoptOpts, nil)
	if err != nil {
		return fmt.Errorf("adopt: %w", err)
	}

	// 5) One import that ends in failure (an unreadable file) for History variety.
	sess4, failErr := importFailing(ctx, pipeline, bin, workdir, masterRoot)
	if failErr != nil {
		// Non-fatal: this is a best-effort variety session.
		logger.Warn("failing-import session skipped", "error", failErr.Error())
	}

	// Summary.
	fmt.Println("PAIM test data populated.")
	fmt.Printf("  db:            %s\n", dbPath)
	fmt.Printf("  workdir:       %s\n", workdir)
	fmt.Printf("  source tree:   %s (%d media, %d duplicate)\n", srcRoot, srcTree.MediaCount(), srcTree.DuplicateCount())
	fmt.Printf("  adopt tree:    %s (%d media, %d duplicate)\n", adoptRoot, adoptTree.MediaCount(), adoptTree.DuplicateCount())
	fmt.Printf("  master lib:    %s\n", masterRoot)
	fmt.Printf("  backup root:   %s\n", backupRoot)
	fmt.Println("  dry run:       files=", dry.Files, "new=", dry.New, "photos=", dry.Photos, "videos=", dry.Videos)
	printSession("  copy import ", sess1)
	printSession("  re-import   ", sess2)
	printSession("  adopt       ", sess3)
	if sess4 != nil {
		printSession("  failing     ", sess4)
	}
	var total int64
	gdb.Model(&domain.Asset{}).Count(&total)
	fmt.Printf("  total assets in DB: %d\n", total)
	return nil
}

// ensureProvider makes sure exactly one enabled localfs provider (rooted at
// backupRoot) exists, so re-runs do not pile up providers.
func ensureProvider(gdb *gorm.DB, backupRoot string) error {
	cfg := fmt.Sprintf(`{"root":%q}`, backupRoot)
	var existing domain.BackupProvider
	err := gdb.Where("plugin_name = ? AND enabled = ?", localfs.PluginName, true).First(&existing).Error
	if err == nil {
		return nil
	}
	if err != gorm.ErrRecordNotFound {
		return fmt.Errorf("look up provider: %w", err)
	}
	p := &domain.BackupProvider{PluginName: localfs.PluginName, ConfigJSON: cfg, Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		return fmt.Errorf("create provider: %w", err)
	}
	return nil
}

// waitForBackups polls until no backup job is pending or running (all reached a
// terminal state), or the timeout elapses.
func waitForBackups(ctx context.Context, backupRepo *repo.BackupRepo, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pending := domain.JobStatusPending
		running := domain.JobStatusRunning
		_, nPending, err := backupRepo.ListJobs(ctx, &pending, repo.Page{})
		if err != nil {
			return err
		}
		_, nRunning, err := backupRepo.ListJobs(ctx, &running, repo.Page{})
		if err != nil {
			return err
		}
		if nPending == 0 && nRunning == 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("backups did not settle within %s", timeout)
}

// importFailing creates a tiny tree with one deliberately-unreadable media file
// and imports it, producing a session that ends in failure. Best effort.
func importFailing(ctx context.Context, pipeline *importer.Pipeline, bin, workdir, masterRoot string) (*domain.ImportSession, error) {
	root := filepath.Join(workdir, "Corrupt Import")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	bad := filepath.Join(root, "UNREADABLE.JPG")
	if err := os.WriteFile(bad, []byte{0xff, 0xd8, 0xff, 0xd9}, 0o644); err != nil {
		return nil, err
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		return nil, err
	}
	opts := importer.Options{Mode: importer.ModeCopy, SourceRoot: root, DestinationRoot: masterRoot, EventName: "Corrupt"}
	sess, err := pipeline.Run(ctx, opts, nil)
	if err != nil {
		return sess, err
	}
	return sess, nil
}

func printSession(label string, s *domain.ImportSession) {
	if s == nil {
		return
	}
	fmt.Printf("%s status=%-9s scanned=%d imported=%d dup=%d skipped=%d failures=%d\n",
		label, s.Status, s.FilesScanned, s.FilesImported, s.Duplicates, s.Skipped, s.Failures)
}

func lookupExiftool() string {
	if p, err := exec.LookPath("exiftool"); err == nil {
		return p
	}
	const brew = "/opt/homebrew/bin/exiftool"
	if _, err := os.Stat(brew); err == nil {
		return brew
	}
	return ""
}
