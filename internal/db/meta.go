package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// metaSingletonID is the fixed primary key of the single library_meta row.
const metaSingletonID = 1

// LibraryMeta is the single-row table that records what wrote a library and at
// what schema version. It lets Open refuse databases from newer app versions and
// drives the ordered migration framework. Exactly one row (ID == metaSingletonID)
// ever exists.
type LibraryMeta struct {
	ID                     int       `gorm:"primaryKey"`
	SchemaVersion          int       `json:"schemaVersion"`
	CreatedByAppVersion    string    `json:"createdByAppVersion"`
	CreatedAt              time.Time `json:"createdAt"`
	LastOpenedByAppVersion string    `json:"lastOpenedByAppVersion"`
	LastOpenedAt           time.Time `json:"lastOpenedAt"`
}

// TableName pins the table name so it reads well in the schema.
func (LibraryMeta) TableName() string { return "library_meta" }

// ensureMetaTable creates the library_meta table if it does not exist.
func ensureMetaTable(gdb *gorm.DB) error {
	if err := gdb.AutoMigrate(&LibraryMeta{}); err != nil {
		return wrap("auto-migrate library_meta", err)
	}
	return nil
}

// readSchemaVersion returns the recorded schema version and whether a meta row
// exists. A database with no meta row (a legacy or plain-Open catalog) reports
// version 0 so every migration runs.
func readSchemaVersion(gdb *gorm.DB) (version int, exists bool, err error) {
	var meta LibraryMeta
	res := gdb.Take(&meta, "id = ?", metaSingletonID)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return 0, false, nil
		}
		return 0, false, wrap("read schema version", res.Error)
	}
	return meta.SchemaVersion, true, nil
}

// bumpSchemaVersion upserts the single meta row, setting its schema version to v.
// It preserves any existing creation stamp and only sets CreatedAt when the row
// is first written.
func bumpSchemaVersion(tx *gorm.DB, v int) error {
	meta := LibraryMeta{ID: metaSingletonID, SchemaVersion: v}
	err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"schema_version"}),
	}).Create(&meta).Error
	if err != nil {
		return wrap("bump schema version", err)
	}
	return nil
}

// stampCreatedIfEmpty records the creating app version/time on the meta row when
// they are not already set (a legacy DB adopted into a library has no creation
// stamp of its own).
func stampCreatedIfEmpty(gdb *gorm.DB, appVersion string) error {
	var meta LibraryMeta
	if err := gdb.Take(&meta, "id = ?", metaSingletonID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return wrap("load meta for creation stamp", err)
	}
	if meta.CreatedByAppVersion != "" && !meta.CreatedAt.IsZero() {
		return nil
	}
	updates := map[string]any{}
	if meta.CreatedByAppVersion == "" {
		updates["created_by_app_version"] = appVersion
	}
	if meta.CreatedAt.IsZero() {
		updates["created_at"] = time.Now()
	}
	if err := gdb.Model(&LibraryMeta{}).Where("id = ?", metaSingletonID).Updates(updates).Error; err != nil {
		return wrap("stamp creation", err)
	}
	return nil
}

// stampCreated sets the creating app version/time unconditionally (used when a
// brand-new library's meta row is first written).
func stampCreated(gdb *gorm.DB, appVersion string) error {
	err := gdb.Model(&LibraryMeta{}).Where("id = ?", metaSingletonID).Updates(map[string]any{
		"created_by_app_version": appVersion,
		"created_at":             time.Now(),
	}).Error
	if err != nil {
		return wrap("stamp creation", err)
	}
	return nil
}

// stampOpened records the app version and time of this successful open on the
// meta row.
func stampOpened(gdb *gorm.DB, appVersion string) error {
	err := gdb.Model(&LibraryMeta{}).Where("id = ?", metaSingletonID).Updates(map[string]any{
		"last_opened_by_app_version": appVersion,
		"last_opened_at":             time.Now(),
	}).Error
	if err != nil {
		return wrap("stamp opened", err)
	}
	return nil
}

// loadMeta returns the single meta row.
func loadMeta(gdb *gorm.DB) (LibraryMeta, error) {
	var meta LibraryMeta
	if err := gdb.Take(&meta, "id = ?", metaSingletonID).Error; err != nil {
		return LibraryMeta{}, wrap("load meta", err)
	}
	return meta, nil
}
