package services

import (
	"context"
	"fmt"
	"os"

	"github.com/Sam-Lam/PAIM/internal/repo"
)

// Setting keys persisted in the Setting table (as JSON values). They are the
// canonical configuration surface shared by main.go wiring and the frontend
// Settings page.
const (
	KeyMasterLibraryRoot         = "master_library_root"
	KeyBackupWorkers             = "backup_workers"
	KeyMaxRetries                = "max_retries"
	KeyImportConcurrency         = "import_concurrency"
	KeyDefaultEventName          = "default_event_name"
	KeyGenerateThumbsAfterImport = "generate_thumbs_after_import"
)

// Default setting values, applied when a key has never been written.
const (
	DefaultBackupWorkers             = 2
	DefaultMaxRetries                = 5
	DefaultImportConcurrency         = 0 // 0 means "use runtime.NumCPU()" in the importer
	DefaultEventName                 = ""
	DefaultGenerateThumbsAfterImport = true
)

// Settings is the typed, JSON-friendly view of PAIM's configuration returned by
// SettingsService.GetAll and consumed by main.go to drive the archive layout,
// backup worker pool, and import concurrency. MetadataAvailable is read-only,
// derived at runtime from whether exiftool (or the fallback) can extract data.
type Settings struct {
	MasterLibraryRoot         string `json:"masterLibraryRoot"`
	BackupWorkers             int    `json:"backupWorkers"`
	MaxRetries                int    `json:"maxRetries"`
	ImportConcurrency         int    `json:"importConcurrency"`
	DefaultEventName          string `json:"defaultEventName"`
	GenerateThumbsAfterImport bool   `json:"generateThumbsAfterImport"`
	MetadataAvailable         bool   `json:"metadataAvailable"`
}

// LoadSettings reads every setting (applying defaults for absent keys) from the
// repository. It is used by main.go at startup and by SettingsService.GetAll.
// MetadataAvailable is left false; callers that know the extractor's status set
// it separately.
func LoadSettings(ctx context.Context, settings *repo.SettingsRepo) (Settings, error) {
	root, err := settings.GetString(ctx, KeyMasterLibraryRoot, "")
	if err != nil {
		return Settings{}, err
	}
	workers, err := settings.GetInt(ctx, KeyBackupWorkers, DefaultBackupWorkers)
	if err != nil {
		return Settings{}, err
	}
	retries, err := settings.GetInt(ctx, KeyMaxRetries, DefaultMaxRetries)
	if err != nil {
		return Settings{}, err
	}
	concurrency, err := settings.GetInt(ctx, KeyImportConcurrency, DefaultImportConcurrency)
	if err != nil {
		return Settings{}, err
	}
	eventName, err := settings.GetString(ctx, KeyDefaultEventName, DefaultEventName)
	if err != nil {
		return Settings{}, err
	}
	genThumbs, err := settings.GetBool(ctx, KeyGenerateThumbsAfterImport, DefaultGenerateThumbsAfterImport)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		MasterLibraryRoot:         root,
		BackupWorkers:             workers,
		MaxRetries:                retries,
		ImportConcurrency:         concurrency,
		DefaultEventName:          eventName,
		GenerateThumbsAfterImport: genThumbs,
	}, nil
}

// SettingsService reads and writes application settings.
type SettingsService struct {
	gated
	settings          *repo.SettingsRepo
	metadataAvailable bool
}

// Bind wires the SettingsService to an open library's settings repo in place.
func (s *SettingsService) Bind(core *AppCore) {
	s.settings = core.Settings
}

// NewSettingsService constructs a SettingsService. metadataAvailable reflects the
// extractor's runtime status and is surfaced (read-only) in GetAll.
func NewSettingsService(settings *repo.SettingsRepo, metadataAvailable bool) *SettingsService {
	return &SettingsService{settings: settings, metadataAvailable: metadataAvailable}
}

// GetAll returns the typed settings with defaults applied and the read-only
// MetadataAvailable flag populated.
func (s *SettingsService) GetAll(ctx context.Context) (Settings, error) {
	if err := s.guard(); err != nil {
		return Settings{}, err
	}
	out, err := LoadSettings(ctx, s.settings)
	if err != nil {
		return Settings{}, err
	}
	out.MetadataAvailable = s.metadataAvailable
	return out, nil
}

// Update persists the writable settings. Changing MasterLibraryRoot revalidates
// that the path exists and is a directory before storing it (an empty root is
// allowed — the library has not been chosen yet). MetadataAvailable is read-only
// and ignored. Changes to worker/retry/concurrency counts take effect on the
// next application start (the running Manager/Pipeline are configured at boot).
func (s *SettingsService) Update(ctx context.Context, in Settings) (Settings, error) {
	if err := s.guard(); err != nil {
		return Settings{}, err
	}
	if in.MasterLibraryRoot != "" {
		info, err := os.Stat(in.MasterLibraryRoot)
		if err != nil {
			return Settings{}, fmt.Errorf("settings: master library root %q is not accessible: %w", in.MasterLibraryRoot, err)
		}
		if !info.IsDir() {
			return Settings{}, fmt.Errorf("settings: master library root %q is not a directory", in.MasterLibraryRoot)
		}
	}
	if in.BackupWorkers < 1 {
		in.BackupWorkers = DefaultBackupWorkers
	}
	if in.MaxRetries < 1 {
		in.MaxRetries = DefaultMaxRetries
	}
	if in.ImportConcurrency < 0 {
		in.ImportConcurrency = 0
	}

	pairs := []struct {
		key string
		val any
	}{
		{KeyMasterLibraryRoot, in.MasterLibraryRoot},
		{KeyBackupWorkers, in.BackupWorkers},
		{KeyMaxRetries, in.MaxRetries},
		{KeyImportConcurrency, in.ImportConcurrency},
		{KeyDefaultEventName, in.DefaultEventName},
		{KeyGenerateThumbsAfterImport, in.GenerateThumbsAfterImport},
	}
	for _, p := range pairs {
		if err := s.settings.Set(ctx, p.key, p.val); err != nil {
			return Settings{}, err
		}
	}
	return s.GetAll(ctx)
}
