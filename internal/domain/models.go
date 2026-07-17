package domain

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UUIDModel is embedded by every model whose primary key is a string UUID (v4).
// The ID is generated in BeforeCreate when left empty. CreatedAt and UpdatedAt
// are maintained automatically by GORM.
type UUIDModel struct {
	ID        string    `gorm:"primaryKey;type:text" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// BeforeCreate assigns a fresh UUID when one has not been supplied. Because the
// method has a pointer receiver on the embedded UUIDModel, it is promoted to
// every embedding model and satisfies GORM's BeforeCreate hook interface.
func (m *UUIDModel) BeforeCreate(*gorm.DB) error {
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	return nil
}

// SoftDelete is embedded by every model. PAIM never hard-deletes rows: "delete"
// flows set the Deleted flag and populate DeletedAt (via GORM's soft-delete
// mechanism), so history and recoverability are preserved. Default GORM queries
// automatically exclude rows whose DeletedAt is set.
type SoftDelete struct {
	Deleted   bool           `json:"deleted"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deletedAt"`
}

// Asset is a single imported, verified media file (or one logical Live Photo
// pair). Identity is established solely by content hashes, never by filename,
// timestamp, or EXIF.
type Asset struct {
	UUIDModel

	OriginalFilename  string `json:"originalFilename"`
	OriginalExtension string `json:"originalExtension"` // lowercase, no leading dot
	OriginalFullPath  string `json:"originalFullPath"`

	SourceID  string `gorm:"index" json:"sourceId"`  // FK -> ImportSource.ID
	SessionID string `gorm:"index" json:"sessionId"` // FK -> ImportSession.ID

	// QuickHash is the BLAKE3 quick hash (size + head/tail); always present.
	QuickHash string `gorm:"index" json:"quickHash"`
	// FullHash is the BLAKE3 of the whole file. It is computed lazily (only on a
	// quick-hash collision or when verification requires it); empty means "not
	// yet computed".
	FullHash string `gorm:"index" json:"fullHash"`

	FileSize    int64      `json:"fileSize"`
	CaptureDate *time.Time `json:"captureDate"`
	ImportDate  time.Time  `json:"importDate"`

	MediaType MediaType `gorm:"index" json:"mediaType"`

	Width           int     `json:"width"`
	Height          int     `json:"height"`
	DurationSeconds float64 `json:"durationSeconds"`

	CameraMake   string `json:"cameraMake"`
	CameraModel  string `json:"cameraModel"`
	Lens         string `json:"lens"`
	ISO          int    `json:"iso"`
	ShutterSpeed string `json:"shutterSpeed"`
	Aperture     string `json:"aperture"`

	GPSLatitude  *float64 `json:"gpsLatitude"`
	GPSLongitude *float64 `json:"gpsLongitude"`

	CurrentArchivePath string             `json:"currentArchivePath"`
	VerificationStatus VerificationStatus `gorm:"index" json:"verificationStatus"`
	BackupStatus       BackupStatus       `gorm:"index" json:"backupStatus"`

	// DuplicateOfAssetID, when set, points at the original (canonical) asset this
	// one duplicates (full-hash match).
	DuplicateOfAssetID *string `gorm:"index" json:"duplicateOfAssetId"`
	// LivePhotoPartnerID links the still/motion halves of a Live Photo pair.
	LivePhotoPartnerID *string `gorm:"index" json:"livePhotoPartnerId"`

	SoftDelete
}

// ImportSession records one import run (scan, dry-run, or copy) and its rolling
// counters. Sessions left running when the app stops are marked interrupted and
// are resumable.
type ImportSession struct {
	UUIDModel

	StartedAt       time.Time  `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
	DurationSeconds float64    `json:"durationSeconds"`

	SourceID        string `gorm:"index" json:"sourceId"`
	DestinationRoot string `json:"destinationRoot"`

	FilesScanned  int `json:"filesScanned"`
	FilesImported int `json:"filesImported"`
	Duplicates    int `json:"duplicates"`
	Failures      int `json:"failures"`
	Skipped       int `json:"skipped"`

	Status SessionStatus `gorm:"index" json:"status"`
	Notes  string        `json:"notes"`

	SoftDelete
}

// ImportSource is a physical or logical origin of media, identified by hardware
// and volume metadata plus a content fingerprint (never by volume label).
type ImportSource struct {
	UUIDModel

	SourceType SourceType `gorm:"index" json:"sourceType"`

	HardwareSerial string `gorm:"index" json:"hardwareSerial"`
	FilesystemUUID string `gorm:"index" json:"filesystemUuid"`
	FilesystemType string `json:"filesystemType"`
	VolumeUUID     string `gorm:"index" json:"volumeUuid"`
	VolumeLabel    string `json:"volumeLabel"`

	Manufacturer   string `json:"manufacturer"`
	Model          string `json:"model"`
	CapacityBytes  int64  `json:"capacityBytes"`
	ConnectionType string `json:"connectionType"`

	// ContentFingerprint is JSON: total file count, total bytes, representative
	// path hash, representative content hash.
	ContentFingerprint string `json:"contentFingerprint"`

	Confidence       int    `json:"confidence"` // 0..100
	ConfidenceReason string `json:"confidenceReason"`

	LastSeenAt  time.Time `json:"lastSeenAt"`
	ImportCount int       `json:"importCount"`

	SafeToErase       bool   `json:"safeToErase"`
	SafeToEraseReason string `json:"safeToEraseReason"`

	SoftDelete
}

// BackupJob is one unit of the SQLite-persisted backup queue. The rows are the
// queue, which makes the backup system restart-safe by construction.
type BackupJob struct {
	UUIDModel

	AssetID     string `gorm:"index" json:"assetId"` // FK -> Asset.ID
	Plugin      string `gorm:"index" json:"plugin"`
	Destination string `json:"destination"`

	Status JobStatus `gorm:"index" json:"status"`

	Retries      int        `json:"retries"`
	StartedAt    *time.Time `json:"startedAt"`
	CompletedAt  *time.Time `json:"completedAt"`
	ErrorMessage string     `json:"errorMessage"`

	SoftDelete
}

// BackupProvider is a configured backup destination/plugin instance.
type BackupProvider struct {
	UUIDModel

	PluginName string `json:"pluginName"`
	ConfigJSON string `json:"configJson"`
	Enabled    bool   `json:"enabled"`

	SoftDelete
}

// Setting is a single key/value application setting. Values are stored as JSON
// in ValueJSON so any serializable type can be persisted.
type Setting struct {
	Key       string    `gorm:"primaryKey" json:"key"`
	ValueJSON string    `json:"valueJson"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	SoftDelete
}

// LogEntry is one structured log record persisted for the Logs page. Unlike the
// other models it uses an auto-increment integer primary key because log volume
// is high and ordering by insertion is useful.
type LogEntry struct {
	ID int64 `gorm:"primaryKey;autoIncrement" json:"id"`

	Timestamp time.Time `gorm:"index" json:"timestamp"`
	Level     string    `gorm:"index" json:"level"`
	Subsystem string    `gorm:"index" json:"subsystem"`
	Message   string    `json:"message"`
	// MetadataJSON holds any extra structured slog attributes as a JSON object.
	MetadataJSON string `json:"metadataJson"`

	CreatedAt time.Time `json:"createdAt"`

	SoftDelete
}

// AllModels returns pointers to one zero value of every model, in dependency
// order, for AutoMigrate and index creation. Keeping this list here (rather than
// in internal/db) keeps the canonical model set next to the definitions.
func AllModels() []any {
	return []any{
		&ImportSource{},
		&ImportSession{},
		&Asset{},
		&BackupProvider{},
		&BackupJob{},
		&Setting{},
		&LogEntry{},
	}
}
