package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// maxRecent bounds how many recent libraries the per-machine config remembers.
const maxRecent = 10

// RecentLibrary is one entry in the per-machine most-recently-used list.
type RecentLibrary struct {
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	LastOpenedAt time.Time `json:"lastOpenedAt"`
}

// Config is the per-machine PAIM configuration. It is deliberately NOT stored in
// any library (the library DB stays machine-agnostic); it lives at
// "~/Library/Application Support/PAIM/config.json" and records which libraries
// this machine has opened plus this machine's local preferences (thumbnail cache
// location, catalog snapshot destination/interval) that must not travel with the
// portable library.
type Config struct {
	// LastLibrary is the root path of the library to try opening on launch.
	LastLibrary string `json:"lastLibrary"`
	// RecentLibraries is the MRU list (most recent first).
	RecentLibraries []RecentLibrary `json:"recentLibraries"`

	// ThumbnailCacheLocation selects where the disposable thumbnail cache lives:
	// "" / ThumbLocationLibrary keeps it inside the library (<root>/.paim/thumbs);
	// ThumbLocationLocal keeps it on this Mac's internal disk. Machine-local
	// because it is a performance preference tied to this machine's disks.
	ThumbnailCacheLocation string `json:"thumbnailCacheLocation,omitempty"`

	// ThumbnailParallelism bounds how many thumbnails PAIM generates at once
	// (shared by on-demand browsing and the background warm-up). Machine-local
	// because the right value depends on THIS Mac's disk: a low value suits a
	// spinning external HDD (parallel qlmanage renders cause seek thrash), a
	// higher value suits an SSD. 0/absent means the default (2).
	ThumbnailParallelism int `json:"thumbnailParallelism,omitempty"`

	// PauseBackupsDuringForeground, when true (the default when nil/absent), makes
	// the backup manager stop claiming NEW upload jobs while a foreground
	// operation (import, analyze, reorganize, safe-to-erase, cleanup,
	// clear-source) runs; any in-flight upload always finishes, and claiming
	// resumes automatically once the foreground work ends. On spinning media a
	// backup upload's reads seek-compete with the foreground work on the same
	// drive, degrading both, so backups yield as patient background work.
	// Machine-local because whether it helps depends on THIS Mac's drive (a
	// spinning HDD benefits; an SSD does not need it). A pointer so an absent
	// value defaults to true, distinct from an explicit false.
	PauseBackupsDuringForeground *bool `json:"pauseBackupsDuringForeground,omitempty"`

	// SnapshotDest is the folder catalog snapshots are copied to. Empty disables
	// snapshots (the default). SnapshotInterval is one of the SnapshotInterval*
	// tokens; it only matters when SnapshotDest is set.
	SnapshotDest     string `json:"snapshotDest,omitempty"`
	SnapshotInterval string `json:"snapshotInterval,omitempty"`
}

// ConfigStore reads and writes the per-machine Config with atomic (temp+rename)
// writes. The path is overridable so tests can point it at a temp dir.
type ConfigStore struct {
	path string
}

// DefaultConfigPath returns "~/Library/Application Support/PAIM/config.json".
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("library: resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "PAIM", "config.json"), nil
}

// NewConfigStore constructs a ConfigStore at path. An empty path resolves to
// DefaultConfigPath.
func NewConfigStore(path string) (*ConfigStore, error) {
	if path == "" {
		var err error
		if path, err = DefaultConfigPath(); err != nil {
			return nil, err
		}
	}
	return &ConfigStore{path: path}, nil
}

// PauseBackupsDuringForegroundEnabled resolves the tri-state
// PauseBackupsDuringForeground preference to a concrete bool, defaulting to true
// when the value is unset (nil/absent).
func (c Config) PauseBackupsDuringForegroundEnabled() bool {
	return c.PauseBackupsDuringForeground == nil || *c.PauseBackupsDuringForeground
}

// Path returns the config file path this store reads and writes.
func (s *ConfigStore) Path() string { return s.path }

// Load reads the config. A missing file yields a zero Config and no error (first
// run). A malformed file is a hard error so a corrupt config is never silently
// discarded.
func (s *ConfigStore) Load() (Config, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("library: read config %q: %w", s.path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("library: parse config %q: %w", s.path, err)
	}
	return cfg, nil
}

// Save writes cfg atomically: it marshals to a temp file in the same directory,
// fsyncs it, then renames it over the destination so a crash never leaves a
// partially written config.
func (s *ConfigStore) Save(cfg Config) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("library: create config dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("library: marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("library: create temp config: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("library: write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("library: fsync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("library: close temp config: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("library: publish config: %w", err)
	}
	cleanup = false
	return nil
}

// RecordOpened sets LastLibrary to root and moves root to the front of the MRU
// list (dedup by path), then persists the config atomically. name defaults to the
// library folder name when empty.
func (s *ConfigStore) RecordOpened(root, name string) error {
	cfg, err := s.Load()
	if err != nil {
		return err
	}
	if name == "" {
		name = DefaultName(root)
	}
	cfg.LastLibrary = root
	cfg.RecentLibraries = promote(cfg.RecentLibraries, RecentLibrary{
		Path:         root,
		Name:         name,
		LastOpenedAt: time.Now(),
	})
	return s.Save(cfg)
}

// promote inserts entry at the front of list, removing any existing entry with
// the same path, and caps the list at maxRecent.
func promote(list []RecentLibrary, entry RecentLibrary) []RecentLibrary {
	out := make([]RecentLibrary, 0, len(list)+1)
	out = append(out, entry)
	for _, e := range list {
		if e.Path == entry.Path {
			continue
		}
		out = append(out, e)
	}
	if len(out) > maxRecent {
		out = out[:maxRecent]
	}
	return out
}
