package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/backup/plugins/rclone"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// ProviderService manages configured backup destinations (BackupProvider rows)
// and reports the plugins available to back them. Provider CRUD is performed
// directly over GORM (repo has no provider repository); the registry is used to
// validate a provider's config via a plugin Initialize probe and to enumerate
// available plugins with their capabilities.
type ProviderService struct {
	gated
	db       *gorm.DB
	assets   *repo.AssetRepo
	registry *backup.Registry
	log      *slog.Logger
}

// Bind wires the ProviderService to an open library's catalog in place.
func (s *ProviderService) Bind(core *AppCore) {
	s.db = core.DB
	s.assets = core.Assets
}

// NewProviderService constructs a ProviderService.
func NewProviderService(db *gorm.DB, registry *backup.Registry, logger *slog.Logger) *ProviderService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ProviderService{db: db, registry: registry, log: logger.With(slog.String("subsystem", "backup"))}
}

// ProviderDTO is the JSON-friendly projection of a BackupProvider.
type ProviderDTO struct {
	ID          string `json:"id"`
	PluginName  string `json:"pluginName"`
	ConfigJSON  string `json:"configJson"`
	Enabled     bool   `json:"enabled"`
	Mirror      bool   `json:"mirror"`
	UploadOrder string `json:"uploadOrder"`
	// MissingBackupCount is how many eligible library assets have NO backup job for
	// this destination yet — the count that powers the "Queue N backups" auto-offer
	// and the per-card badge. It is populated only for ENABLED providers (backfill
	// is refused for disabled ones); 0 otherwise or when the catalog is unbound.
	// Assets deliberately opted out of this destination are NOT counted here (they
	// are surfaced separately as OptedOutCount).
	MissingBackupCount int64 `json:"missingBackupCount"`
	// OptedOutCount is how many assets the user deliberately excluded from this
	// destination at import time (opted_out jobs). It drives the card's "N skipped
	// by choice · Queue anyway" reversal line. Populated for enabled providers.
	OptedOutCount int64 `json:"optedOutCount"`

	// LastError is this destination's most recent still-failing job (its error
	// message and when it was recorded), or nil when it has no currently-failed
	// job. LastSuccessAt is when its most recent job completed, or nil if none has.
	// The card shows a "Failing — <error>" amber state (in place of the green
	// Enabled dot) when LastError is set and is more recent than LastSuccessAt.
	LastError     *ProviderErrorDTO `json:"lastError"`
	LastSuccessAt *time.Time        `json:"lastSuccessAt"`
}

// ProviderErrorDTO is a destination's most recent failure: the job's error
// message and the time it was recorded (the failed job's UpdatedAt).
type ProviderErrorDTO struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

func toProviderDTO(p domain.BackupProvider) ProviderDTO {
	order := string(p.UploadOrder)
	if order == "" {
		order = string(domain.UploadOrderOldestFirst)
	}
	return ProviderDTO{
		ID:          p.ID,
		PluginName:  p.PluginName,
		ConfigJSON:  p.ConfigJSON,
		Enabled:     p.Enabled,
		Mirror:      p.Mirror,
		UploadOrder: order,
	}
}

// withMissingCount fills a DTO's MissingBackupCount for an enabled provider (the
// cheap NOT EXISTS count of eligible assets lacking a job for it). A disabled
// provider, an unbound catalog, or a count error leaves the field at 0 — the field
// is a UI convenience, never load-bearing, so a failure degrades silently.
func (s *ProviderService) withMissingCount(ctx context.Context, dto ProviderDTO) ProviderDTO {
	if !dto.Enabled || s.assets == nil {
		return dto
	}
	if n, err := s.assets.CountEligibleMissingBackup(ctx, dto.ID); err == nil {
		dto.MissingBackupCount = n
	} else {
		s.log.Warn("provider missing-backup count failed", "provider", dto.ID, "error", err.Error())
	}
	if s.db != nil {
		var n int64
		if err := s.db.WithContext(ctx).
			Model(&domain.BackupJob{}).
			Where("destination = ? AND status = ?", dto.ID, domain.JobStatusOptedOut).
			Count(&n).Error; err == nil {
			dto.OptedOutCount = n
		} else {
			s.log.Warn("provider opted-out count failed", "provider", dto.ID, "error", err.Error())
		}
	}
	return dto
}

// withHealth derives a provider's recent-outcome health from its jobs: the most
// recent still-failed job (LastError) and the most recent completed job
// (LastSuccessAt), both keyed by destination = provider ID over indexed columns.
// The catalog being unbound or a query error leaves the fields nil — health is a
// UI convenience, never load-bearing.
func (s *ProviderService) withHealth(ctx context.Context, dto ProviderDTO) ProviderDTO {
	if s.db == nil {
		return dto
	}
	var failed domain.BackupJob
	err := s.db.WithContext(ctx).
		Where("destination = ? AND status = ?", dto.ID, domain.JobStatusFailed).
		Order("updated_at DESC").Limit(1).First(&failed).Error
	if err == nil {
		dto.LastError = &ProviderErrorDTO{Message: failed.ErrorMessage, At: failed.UpdatedAt}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		s.log.Warn("provider last-error lookup failed", "provider", dto.ID, "error", err.Error())
	}

	var completed domain.BackupJob
	err = s.db.WithContext(ctx).
		Where("destination = ? AND status = ? AND completed_at IS NOT NULL", dto.ID, domain.JobStatusCompleted).
		Order("completed_at DESC").Limit(1).First(&completed).Error
	if err == nil {
		dto.LastSuccessAt = completed.CompletedAt
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		s.log.Warn("provider last-success lookup failed", "provider", dto.ID, "error", err.Error())
	}
	return dto
}

// normalizeUploadOrder maps a requested order string to a valid UploadOrder,
// defaulting to oldest_first for anything unrecognized (including empty).
func normalizeUploadOrder(order string) domain.UploadOrder {
	if domain.UploadOrder(order) == domain.UploadOrderNewestFirst {
		return domain.UploadOrderNewestFirst
	}
	return domain.UploadOrderOldestFirst
}

// injectRcloneMirror stamps the mirror flag into an rclone config JSON so the
// plugin's Initialize can enforce pool rules (a >1 remote pool is only valid on a
// mirror provider). It is a no-op for non-rclone plugins and tolerates a config
// that is not a JSON object (Initialize will reject that separately).
func injectRcloneMirror(pluginName, configJSON string, mirror bool) string {
	if pluginName != rclone.PluginName {
		return configJSON
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil || m == nil {
		return configJSON
	}
	m["mirror"] = mirror
	b, err := json.Marshal(m)
	if err != nil {
		return configJSON
	}
	return string(b)
}

// PluginDTO describes an available backup plugin and its capabilities.
type PluginDTO struct {
	Name           string `json:"name"`
	SupportsVerify bool   `json:"supportsVerify"`
	SupportsDelete bool   `json:"supportsDelete"`
	SupportsResume bool   `json:"supportsResume"`
	MaxFileSize    int64  `json:"maxFileSize"`
}

// List returns every configured provider (including disabled ones).
func (s *ProviderService) List(ctx context.Context) ([]ProviderDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var rows []domain.BackupProvider
	if err := s.db.WithContext(ctx).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: list providers: %w", err)
	}
	out := make([]ProviderDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.withHealth(ctx, s.withMissingCount(ctx, toProviderDTO(r))))
	}
	return out, nil
}

// Add validates configJSON against the named plugin (via an Initialize probe)
// and, on success, creates an enabled provider. mirror marks a quality-of-life
// destination (its jobs never block a safety verdict); uploadOrder controls claim
// order (oldest_first FIFO or newest_first). The mirror flag is also stamped into
// the rclone config so the plugin can enforce its multi-remote-pool rules.
func (s *ProviderService) Add(ctx context.Context, pluginName, configJSON string, mirror bool, uploadOrder string) (ProviderDTO, error) {
	if err := s.guard(); err != nil {
		return ProviderDTO{}, err
	}
	configJSON = injectRcloneMirror(pluginName, configJSON, mirror)
	if err := s.probe(ctx, pluginName, configJSON); err != nil {
		return ProviderDTO{}, err
	}
	p := &domain.BackupProvider{
		PluginName:  pluginName,
		ConfigJSON:  configJSON,
		Enabled:     true,
		Mirror:      mirror,
		UploadOrder: normalizeUploadOrder(uploadOrder),
	}
	if err := s.db.WithContext(ctx).Create(p).Error; err != nil {
		return ProviderDTO{}, fmt.Errorf("services: create provider: %w", err)
	}
	s.log.Info("backup provider added", "id", p.ID, "plugin", pluginName, "mirror", mirror, "uploadOrder", p.UploadOrder)
	// A brand-new provider has no jobs yet, so MissingBackupCount is the full
	// eligible set — the UI reads it to auto-offer "Back up your existing library?".
	return s.withMissingCount(ctx, toProviderDTO(*p)), nil
}

// Update revalidates configJSON against the provider's plugin and persists the
// new config, enabled flag, mirror flag, and upload order.
func (s *ProviderService) Update(ctx context.Context, id, configJSON string, enabled, mirror bool, uploadOrder string) (ProviderDTO, error) {
	if err := s.guard(); err != nil {
		return ProviderDTO{}, err
	}
	var p domain.BackupProvider
	if err := s.db.WithContext(ctx).First(&p, "id = ?", id).Error; err != nil {
		return ProviderDTO{}, fmt.Errorf("services: get provider %q: %w", id, err)
	}
	configJSON = injectRcloneMirror(p.PluginName, configJSON, mirror)
	if err := s.probe(ctx, p.PluginName, configJSON); err != nil {
		return ProviderDTO{}, err
	}
	order := normalizeUploadOrder(uploadOrder)
	res := s.db.WithContext(ctx).Model(&domain.BackupProvider{}).Where("id = ?", id).Updates(map[string]any{
		"config_json":  configJSON,
		"enabled":      enabled,
		"mirror":       mirror,
		"upload_order": order,
	})
	if res.Error != nil {
		return ProviderDTO{}, fmt.Errorf("services: update provider %q: %w", id, res.Error)
	}
	p.ConfigJSON = configJSON
	p.Enabled = enabled
	p.Mirror = mirror
	p.UploadOrder = order
	s.log.Info("backup provider updated", "id", id, "enabled", enabled, "mirror", mirror, "uploadOrder", order)
	// The returned DTO carries the current missing-job count so the UI can offer to
	// queue them after an enable (a disabled→enabled flip surfaces the whole gap).
	return s.withMissingCount(ctx, toProviderDTO(p)), nil
}

// RcloneRemotesDTO reports rclone's install status and the remotes it has
// configured, for the Add-destination UI. When Installed is false the UI shows
// install guidance (brew install rclone) instead of a remotes dropdown; when
// Installed is true but Error is set, rclone is present but listing its remotes
// failed (surfaced inline). This is deliberately NOT gated on an open library —
// discovering rclone remotes is independent of the catalog.
type RcloneRemotesDTO struct {
	Installed bool     `json:"installed"`
	Binary    string   `json:"binary"`
	Remotes   []string `json:"remotes"`
	Error     string   `json:"error"`
}

// RcloneRemotes resolves the rclone binary and lists its configured remotes so
// the Add flow can present a dropdown. A missing binary is reported as a typed
// "not installed" state (Installed=false) rather than an error, so the UI can
// render setup guidance.
func (s *ProviderService) RcloneRemotes(ctx context.Context) (RcloneRemotesDTO, error) {
	binary, err := rclone.ResolveBinary("")
	if err != nil {
		if errors.Is(err, rclone.ErrNotInstalled) {
			return RcloneRemotesDTO{Installed: false, Error: err.Error()}, nil
		}
		return RcloneRemotesDTO{}, fmt.Errorf("services: resolve rclone binary: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	remotes, err := rclone.ListRemotes(cctx, binary)
	if err != nil {
		return RcloneRemotesDTO{Installed: true, Binary: binary, Error: err.Error()}, nil
	}
	return RcloneRemotesDTO{Installed: true, Binary: binary, Remotes: remotes}, nil
}

// RcloneRemoteInfoDTO reports a chosen rclone remote's backend type and whether
// PAIM can verify uploads to it. The Add-destination UI uses SupportsChecksum to
// auto-suggest the Mirror toggle and warn that uploads cannot be verified when the
// backend (e.g. Google Photos) exposes no content hash.
type RcloneRemoteInfoDTO struct {
	Remote           string `json:"remote"`
	BackendType      string `json:"backendType"`
	SupportsChecksum bool   `json:"supportsChecksum"`
	Error            string `json:"error"`
}

// RcloneRemoteInfo probes one rclone remote's backend type and checksum support.
// It is not gated on an open library (it inspects rclone config, not the catalog).
// A probe failure is returned inline (Error set) rather than as a hard error so
// the UI degrades to the manual Mirror toggle.
func (s *ProviderService) RcloneRemoteInfo(ctx context.Context, remote string) (RcloneRemoteInfoDTO, error) {
	if remote == "" {
		return RcloneRemoteInfoDTO{}, fmt.Errorf("services: rclone remote info: empty remote")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	backendType, supportsChecksum, err := rclone.RemoteInfo(cctx, "", remote)
	if err != nil {
		// Optimistic default so the UI still lets the user proceed (manual toggle).
		return RcloneRemoteInfoDTO{Remote: remote, SupportsChecksum: true, Error: err.Error()}, nil
	}
	return RcloneRemoteInfoDTO{
		Remote:           remote,
		BackendType:      backendType,
		SupportsChecksum: supportsChecksum,
	}, nil
}

// AvailablePlugins lists the registered plugins with their capabilities.
func (s *ProviderService) AvailablePlugins(ctx context.Context) ([]PluginDTO, error) {
	names := s.registry.Names()
	out := make([]PluginDTO, 0, len(names))
	for _, name := range names {
		plugin, ok := s.registry.New(name)
		if !ok {
			continue
		}
		caps := plugin.Capabilities()
		out = append(out, PluginDTO{
			Name:           name,
			SupportsVerify: caps.SupportsVerify,
			SupportsDelete: caps.SupportsDelete,
			SupportsResume: caps.SupportsResume,
			MaxFileSize:    caps.MaxFileSize,
		})
	}
	return out, nil
}

// probe constructs the plugin and runs Initialize to validate configJSON without
// persisting anything.
func (s *ProviderService) probe(ctx context.Context, pluginName, configJSON string) error {
	plugin, ok := s.registry.New(pluginName)
	if !ok {
		return fmt.Errorf("services: unknown backup plugin %q", pluginName)
	}
	if err := plugin.Initialize(ctx, configJSON); err != nil {
		return fmt.Errorf("services: invalid configuration for plugin %q: %w", pluginName, err)
	}
	return nil
}
