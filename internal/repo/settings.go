package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SettingsRepo persists key/value application settings. Values are stored as JSON
// in the Setting.ValueJSON column.
type SettingsRepo struct {
	db *gorm.DB
}

// NewSettingsRepo constructs a SettingsRepo.
func NewSettingsRepo(db *gorm.DB) *SettingsRepo { return &SettingsRepo{db: db} }

// WithTx binds the repo to a transaction handle.
func (r *SettingsRepo) WithTx(tx *gorm.DB) *SettingsRepo { return &SettingsRepo{db: tx} }

// Set marshals value to JSON and upserts it under key.
func (r *SettingsRepo) Set(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("repo: marshal setting %q: %w", key, err)
	}
	setting := domain.Setting{Key: key, ValueJSON: string(raw)}
	err = r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value_json", "updated_at"}),
	}).Create(&setting).Error
	if err != nil {
		return fmt.Errorf("repo: set setting %q: %w", key, err)
	}
	return nil
}

// Get unmarshals the stored value for key into dest. It returns found=false (and
// leaves dest untouched) when the key does not exist.
func (r *SettingsRepo) Get(ctx context.Context, key string, dest any) (found bool, err error) {
	var setting domain.Setting
	err = r.db.WithContext(ctx).First(&setting, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("repo: get setting %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(setting.ValueJSON), dest); err != nil {
		return false, fmt.Errorf("repo: unmarshal setting %q: %w", key, err)
	}
	return true, nil
}

// GetString returns the string value for key, or def if the key is absent.
func (r *SettingsRepo) GetString(ctx context.Context, key, def string) (string, error) {
	var v string
	found, err := r.Get(ctx, key, &v)
	if err != nil {
		return def, err
	}
	if !found {
		return def, nil
	}
	return v, nil
}

// GetInt returns the int value for key, or def if the key is absent.
func (r *SettingsRepo) GetInt(ctx context.Context, key string, def int) (int, error) {
	var v int
	found, err := r.Get(ctx, key, &v)
	if err != nil {
		return def, err
	}
	if !found {
		return def, nil
	}
	return v, nil
}

// GetBool returns the bool value for key, or def if the key is absent.
func (r *SettingsRepo) GetBool(ctx context.Context, key string, def bool) (bool, error) {
	var v bool
	found, err := r.Get(ctx, key, &v)
	if err != nil {
		return def, err
	}
	if !found {
		return def, nil
	}
	return v, nil
}
