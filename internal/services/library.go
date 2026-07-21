package services

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/version"
)

// CurrentLibraryDTO describes the library currently open, surfaced to the
// frontend for the Settings → Library card and the root-layout gate.
type CurrentLibraryDTO struct {
	Path          string    `json:"path"`
	Name          string    `json:"name"`
	SchemaVersion int       `json:"schemaVersion"`
	AppVersion    string    `json:"appVersion"`
	OpenedAt      time.Time `json:"openedAt"`
}

// RecentLibraryDTO is one entry in the recent-libraries list.
type RecentLibraryDTO struct {
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	LastOpenedAt time.Time `json:"lastOpenedAt"`
}

// LockConflictDTO carries the structured details of a refused lock so the
// frontend can present an informed Force Open confirmation.
type LockConflictDTO struct {
	Hostname            string `json:"hostname"`
	PID                 int    `json:"pid"`
	SameHostLive        bool   `json:"sameHostLive"`
	HeartbeatAgeSeconds int    `json:"heartbeatAgeSeconds"`
	Message             string `json:"message"`
}

// OpenResultDTO is returned by Create/Open/ForceOpen/MigrateLegacy. Exactly one
// of {Library set}, {LockConflict set}, or {NeedsRelaunch true} is meaningful:
//   - Library set, NeedsRelaunch false: opened in place, frontend can proceed.
//   - NeedsRelaunch true: a different library was already open; config was
//     updated and the app must relaunch to switch (frontend prompts).
//   - LockConflict set: the library is locked; frontend offers Force Open.
type OpenResultDTO struct {
	NeedsRelaunch bool               `json:"needsRelaunch"`
	Library       *CurrentLibraryDTO `json:"library"`
	LockConflict  *LockConflictDTO   `json:"lockConflict"`
}

// LegacyStatusDTO reports whether a pre-library per-machine catalog exists (so
// Welcome can offer to migrate it).
type LegacyStatusDTO struct {
	Exists bool   `json:"exists"`
	Path   string `json:"path"`
}

// OpenOutcome is the internal result the LibraryOpener returns to the service.
type OpenOutcome struct {
	NeedsRelaunch bool
	LockConflict  *library.LockHeldError
	Current       *CurrentLibraryDTO
}

// LibraryOpener performs the actual in-place open (or switch), implemented in
// main.go where the object graph and lifecycle live. force breaks a refused lock;
// migrateLegacy first installs the legacy catalog into root.
type LibraryOpener interface {
	Open(ctx context.Context, root string, force, migrateLegacy bool) (OpenOutcome, error)
}

// LibraryService manages portable libraries: reporting the current/recent
// libraries, and creating/opening/migrating them. It is the ONE service that is
// never gated — it is how the user gets a library open in the first place.
type LibraryService struct {
	config     *library.ConfigStore
	dialog     Dialoger
	opener     LibraryOpener
	appVersion string
	log        *slog.Logger

	mu      sync.RWMutex
	current *CurrentLibraryDTO
}

// NewLibraryService constructs a LibraryService. appVersion defaults to
// version.Version when empty.
func NewLibraryService(config *library.ConfigStore, dialog Dialoger, opener LibraryOpener, appVersion string, logger *slog.Logger) *LibraryService {
	if logger == nil {
		logger = slog.Default()
	}
	if appVersion == "" {
		appVersion = version.Version
	}
	return &LibraryService{
		config:     config,
		dialog:     dialog,
		opener:     opener,
		appVersion: appVersion,
		log:        logger.With(slog.String("subsystem", "library")),
	}
}

// SetCurrent records the currently open library. main.go calls it after a
// successful open (including the startup open) so Current() reflects reality.
func (s *LibraryService) SetCurrent(c *CurrentLibraryDTO) {
	s.mu.Lock()
	s.current = c
	s.mu.Unlock()
}

// Current returns the open library, or nil when none is open (the frontend
// redirects to Welcome on nil).
func (s *LibraryService) Current(ctx context.Context) (*CurrentLibraryDTO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil, nil
	}
	c := *s.current
	return &c, nil
}

// Recent returns the recently opened libraries (most recent first).
func (s *LibraryService) Recent(ctx context.Context) ([]RecentLibraryDTO, error) {
	cfg, err := s.config.Load()
	if err != nil {
		return nil, err
	}
	out := make([]RecentLibraryDTO, 0, len(cfg.RecentLibraries))
	for _, r := range cfg.RecentLibraries {
		out = append(out, RecentLibraryDTO{Path: r.Path, Name: r.Name, LastOpenedAt: r.LastOpenedAt})
	}
	return out, nil
}

// LegacyStatus reports whether a pre-library catalog exists to migrate.
func (s *LibraryService) LegacyStatus(ctx context.Context) (LegacyStatusDTO, error) {
	path, _ := library.LegacyDBPath()
	return LegacyStatusDTO{Exists: library.LegacyExists(), Path: path}, nil
}

// AppVersion returns the running application version (for Settings → About).
func (s *LibraryService) AppVersion(ctx context.Context) (string, error) {
	return s.appVersion, nil
}

// PickLibraryFolder opens a native directory chooser for a library root.
func (s *LibraryService) PickLibraryFolder(ctx context.Context) (string, error) {
	if s.dialog == nil {
		return "", fmt.Errorf("services: no dialog provider configured")
	}
	return s.dialog.PickFolder(ctx, "Choose a library folder")
}

// Create creates a new library at path (or opens it if one already exists there)
// and opens it. path must be an existing directory.
func (s *LibraryService) Create(ctx context.Context, path string) (OpenResultDTO, error) {
	if path == "" {
		return OpenResultDTO{}, fmt.Errorf("services: create library: empty path")
	}
	return s.open(ctx, path, false, false)
}

// Open opens the existing library at path. It validates that a catalog exists
// there and returns a clear error otherwise.
func (s *LibraryService) Open(ctx context.Context, path string) (OpenResultDTO, error) {
	if path == "" {
		return OpenResultDTO{}, fmt.Errorf("services: open library: empty path")
	}
	if !library.HasLibrary(path) {
		return OpenResultDTO{}, fmt.Errorf("services: no PAIM library found at %q (expected %s)", path, library.DBPath(path))
	}
	return s.open(ctx, path, false, false)
}

// ForceOpen opens the library at path, breaking an existing lock. It is the
// user-confirmed override for a lock conflict.
func (s *LibraryService) ForceOpen(ctx context.Context, path string) (OpenResultDTO, error) {
	if path == "" {
		return OpenResultDTO{}, fmt.Errorf("services: force open library: empty path")
	}
	return s.open(ctx, path, true, false)
}

// MigrateLegacy installs the pre-library per-machine catalog into targetRoot and
// opens it. targetRoot must be an existing directory with no library yet.
func (s *LibraryService) MigrateLegacy(ctx context.Context, targetRoot string) (OpenResultDTO, error) {
	if targetRoot == "" {
		return OpenResultDTO{}, fmt.Errorf("services: migrate legacy: empty target")
	}
	if library.HasLibrary(targetRoot) {
		return OpenResultDTO{}, fmt.Errorf("services: a library already exists at %q — open it instead", targetRoot)
	}
	return s.open(ctx, targetRoot, false, true)
}

// open delegates to the opener and maps the outcome to the DTO, recording the
// current library and updating the per-machine config on a successful in-place
// open.
func (s *LibraryService) open(ctx context.Context, path string, force, migrateLegacy bool) (OpenResultDTO, error) {
	outcome, err := s.opener.Open(ctx, path, force, migrateLegacy)
	if err != nil {
		return OpenResultDTO{}, err
	}
	if outcome.LockConflict != nil {
		lc := outcome.LockConflict
		return OpenResultDTO{LockConflict: &LockConflictDTO{
			Hostname:            lc.Info.Hostname,
			PID:                 lc.Info.PID,
			SameHostLive:        lc.SameHost && lc.LivePID,
			HeartbeatAgeSeconds: int(lc.HeartbeatAge.Seconds()),
			Message:             lc.Error(),
		}}, nil
	}
	if outcome.NeedsRelaunch {
		// A switch: config was updated by the opener; the frontend prompts relaunch.
		return OpenResultDTO{NeedsRelaunch: true}, nil
	}
	if outcome.Current != nil {
		s.SetCurrent(outcome.Current)
	}
	return OpenResultDTO{Library: outcome.Current}, nil
}
