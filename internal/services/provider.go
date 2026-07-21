package services

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
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
	registry *backup.Registry
	log      *slog.Logger
}

// Bind wires the ProviderService to an open library's catalog in place.
func (s *ProviderService) Bind(core *AppCore) {
	s.db = core.DB
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
	ID         string `json:"id"`
	PluginName string `json:"pluginName"`
	ConfigJSON string `json:"configJson"`
	Enabled    bool   `json:"enabled"`
}

func toProviderDTO(p domain.BackupProvider) ProviderDTO {
	return ProviderDTO{ID: p.ID, PluginName: p.PluginName, ConfigJSON: p.ConfigJSON, Enabled: p.Enabled}
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
		out = append(out, toProviderDTO(r))
	}
	return out, nil
}

// Add validates configJSON against the named plugin (via an Initialize probe)
// and, on success, creates an enabled provider.
func (s *ProviderService) Add(ctx context.Context, pluginName, configJSON string) (ProviderDTO, error) {
	if err := s.guard(); err != nil {
		return ProviderDTO{}, err
	}
	if err := s.probe(ctx, pluginName, configJSON); err != nil {
		return ProviderDTO{}, err
	}
	p := &domain.BackupProvider{PluginName: pluginName, ConfigJSON: configJSON, Enabled: true}
	if err := s.db.WithContext(ctx).Create(p).Error; err != nil {
		return ProviderDTO{}, fmt.Errorf("services: create provider: %w", err)
	}
	s.log.Info("backup provider added", "id", p.ID, "plugin", pluginName)
	return toProviderDTO(*p), nil
}

// Update revalidates configJSON against the provider's plugin and persists the
// new config and enabled flag.
func (s *ProviderService) Update(ctx context.Context, id, configJSON string, enabled bool) (ProviderDTO, error) {
	if err := s.guard(); err != nil {
		return ProviderDTO{}, err
	}
	var p domain.BackupProvider
	if err := s.db.WithContext(ctx).First(&p, "id = ?", id).Error; err != nil {
		return ProviderDTO{}, fmt.Errorf("services: get provider %q: %w", id, err)
	}
	if err := s.probe(ctx, p.PluginName, configJSON); err != nil {
		return ProviderDTO{}, err
	}
	res := s.db.WithContext(ctx).Model(&domain.BackupProvider{}).Where("id = ?", id).Updates(map[string]any{
		"config_json": configJSON,
		"enabled":     enabled,
	})
	if res.Error != nil {
		return ProviderDTO{}, fmt.Errorf("services: update provider %q: %w", id, res.Error)
	}
	p.ConfigJSON = configJSON
	p.Enabled = enabled
	s.log.Info("backup provider updated", "id", id, "enabled", enabled)
	return toProviderDTO(p), nil
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
