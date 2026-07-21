package services

import (
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/volumes"
)

// AssetDTO is the JSON-friendly projection of a domain.Asset returned across the
// binding boundary. It omits GORM internals (soft-delete plumbing) and normalizes
// the nullable pointer fields the frontend cares about.
type AssetDTO struct {
	ID                 string     `json:"id"`
	OriginalFilename   string     `json:"originalFilename"`
	OriginalExtension  string     `json:"originalExtension"`
	OriginalFullPath   string     `json:"originalFullPath"`
	SourceID           string     `json:"sourceId"`
	SessionID          string     `json:"sessionId"`
	QuickHash          string     `json:"quickHash"`
	FullHash           string     `json:"fullHash"`
	FileSize           int64      `json:"fileSize"`
	CaptureDate        *time.Time `json:"captureDate"`
	ImportDate         time.Time  `json:"importDate"`
	MediaType          string     `json:"mediaType"`
	Width              int        `json:"width"`
	Height             int        `json:"height"`
	DurationSeconds    float64    `json:"durationSeconds"`
	CameraMake         string     `json:"cameraMake"`
	CameraModel        string     `json:"cameraModel"`
	CurrentArchivePath string     `json:"currentArchivePath"`
	VerificationStatus string     `json:"verificationStatus"`
	BackupStatus       string     `json:"backupStatus"`
	DuplicateOfAssetID string     `json:"duplicateOfAssetId"`
}

// toAssetDTO maps a domain.Asset to its DTO, surfacing the RESOLVED absolute
// archive path (relative-to-root storage is an internal detail; the frontend and
// any file-open action need a real path). root is the library root; an empty root
// leaves stored paths as-is (dev/legacy absolute paths).
func toAssetDTO(a domain.Asset, root string) AssetDTO {
	dup := ""
	if a.DuplicateOfAssetID != nil {
		dup = *a.DuplicateOfAssetID
	}
	return AssetDTO{
		ID:                 a.ID,
		OriginalFilename:   a.OriginalFilename,
		OriginalExtension:  a.OriginalExtension,
		OriginalFullPath:   a.OriginalFullPath,
		SourceID:           a.SourceID,
		SessionID:          a.SessionID,
		QuickHash:          a.QuickHash,
		FullHash:           a.FullHash,
		FileSize:           a.FileSize,
		CaptureDate:        a.CaptureDate,
		ImportDate:         a.ImportDate,
		MediaType:          string(a.MediaType),
		Width:              a.Width,
		Height:             a.Height,
		DurationSeconds:    a.DurationSeconds,
		CameraMake:         a.CameraMake,
		CameraModel:        a.CameraModel,
		CurrentArchivePath: library.ResolvePath(root, a.CurrentArchivePath),
		VerificationStatus: string(a.VerificationStatus),
		BackupStatus:       string(a.BackupStatus),
		DuplicateOfAssetID: dup,
	}
}

// SessionDTO is the JSON-friendly projection of an ImportSession. Mode is decoded
// from the session's resume-state notes so the Import History can badge adopt
// runs distinctly from copy runs.
type SessionDTO struct {
	ID              string     `json:"id"`
	StartedAt       time.Time  `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
	DurationSeconds float64    `json:"durationSeconds"`
	SourceID        string     `json:"sourceId"`
	DestinationRoot string     `json:"destinationRoot"`
	FilesScanned    int        `json:"filesScanned"`
	FilesImported   int        `json:"filesImported"`
	Duplicates      int        `json:"duplicates"`
	Failures        int        `json:"failures"`
	Skipped         int        `json:"skipped"`
	Status          string     `json:"status"`
	Mode            string     `json:"mode"`
}

// toSessionDTO maps a domain.ImportSession to its DTO, extracting the import mode
// from the resume-state JSON stored in Notes (defaulting to "copy").
func toSessionDTO(s domain.ImportSession) SessionDTO {
	mode := "copy"
	if st, err := decodeResumeState(s.Notes); err == nil && st.Mode != "" {
		mode = st.Mode
	}
	return SessionDTO{
		ID:              s.ID,
		StartedAt:       s.StartedAt,
		CompletedAt:     s.CompletedAt,
		DurationSeconds: s.DurationSeconds,
		SourceID:        s.SourceID,
		DestinationRoot: s.DestinationRoot,
		FilesScanned:    s.FilesScanned,
		FilesImported:   s.FilesImported,
		Duplicates:      s.Duplicates,
		Failures:        s.Failures,
		Skipped:         s.Skipped,
		Status:          string(s.Status),
		Mode:            mode,
	}
}

// SourceDTO is the JSON-friendly projection of an ImportSource.
type SourceDTO struct {
	ID                string    `json:"id"`
	SourceType        string    `json:"sourceType"`
	HardwareSerial    string    `json:"hardwareSerial"`
	FilesystemUUID    string    `json:"filesystemUuid"`
	VolumeUUID        string    `json:"volumeUuid"`
	VolumeLabel       string    `json:"volumeLabel"`
	Manufacturer      string    `json:"manufacturer"`
	Model             string    `json:"model"`
	CapacityBytes     int64     `json:"capacityBytes"`
	ConnectionType    string    `json:"connectionType"`
	Confidence        int       `json:"confidence"`
	ConfidenceReason  string    `json:"confidenceReason"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
	ImportCount       int       `json:"importCount"`
	SafeToErase       bool      `json:"safeToErase"`
	SafeToEraseReason string    `json:"safeToEraseReason"`
}

// toSourceDTO maps a domain.ImportSource to its DTO.
func toSourceDTO(s domain.ImportSource) SourceDTO {
	return SourceDTO{
		ID:                s.ID,
		SourceType:        string(s.SourceType),
		HardwareSerial:    s.HardwareSerial,
		FilesystemUUID:    s.FilesystemUUID,
		VolumeUUID:        s.VolumeUUID,
		VolumeLabel:       s.VolumeLabel,
		Manufacturer:      s.Manufacturer,
		Model:             s.Model,
		CapacityBytes:     s.CapacityBytes,
		ConnectionType:    s.ConnectionType,
		Confidence:        s.Confidence,
		ConfidenceReason:  s.ConfidenceReason,
		LastSeenAt:        s.LastSeenAt,
		ImportCount:       s.ImportCount,
		SafeToErase:       s.SafeToErase,
		SafeToEraseReason: s.SafeToEraseReason,
	}
}

// VolumeDTO is the JSON-friendly projection of a mounted volume description.
type VolumeDTO struct {
	MountPoint      string   `json:"mountPoint"`
	VolumeName      string   `json:"volumeName"`
	VolumeUUID      string   `json:"volumeUuid"`
	FilesystemUUID  string   `json:"filesystemUuid"`
	FilesystemType  string   `json:"filesystemType"`
	CapacityBytes   int64    `json:"capacityBytes"`
	FreeBytes       int64    `json:"freeBytes"`
	Removable       bool     `json:"removable"`
	Internal        bool     `json:"internal"`
	Ejectable       bool     `json:"ejectable"`
	ConnectionType  string   `json:"connectionType"`
	HardwareSerial  string   `json:"hardwareSerial"`
	Manufacturer    string   `json:"manufacturer"`
	Model           string   `json:"model"`
	IsNetworkVolume bool     `json:"isNetworkVolume"`
	NetworkURL      string   `json:"networkUrl"`
	Warnings        []string `json:"warnings"`
}

// toVolumeDTO maps a volumes.Info to its DTO.
func toVolumeDTO(v volumes.Info) VolumeDTO {
	return VolumeDTO{
		MountPoint:      v.MountPoint,
		VolumeName:      v.VolumeName,
		VolumeUUID:      v.VolumeUUID,
		FilesystemUUID:  v.FilesystemUUID,
		FilesystemType:  v.FilesystemType,
		CapacityBytes:   v.CapacityBytes,
		FreeBytes:       v.FreeBytes,
		Removable:       v.Removable,
		Internal:        v.Internal,
		Ejectable:       v.Ejectable,
		ConnectionType:  string(v.ConnectionType),
		HardwareSerial:  v.HardwareSerial,
		Manufacturer:    v.Manufacturer,
		Model:           v.Model,
		IsNetworkVolume: v.IsNetworkVolume,
		NetworkURL:      v.NetworkURL,
		Warnings:        v.Warnings,
	}
}

// BackupJobDTO is the JSON-friendly projection of a BackupJob joined with its
// asset's display filename and archive path.
type BackupJobDTO struct {
	ID           string     `json:"id"`
	AssetID      string     `json:"assetId"`
	Filename     string     `json:"filename"`
	ArchivePath  string     `json:"archivePath"`
	Plugin       string     `json:"plugin"`
	Destination  string     `json:"destination"`
	Status       string     `json:"status"`
	Retries      int        `json:"retries"`
	StartedAt    *time.Time `json:"startedAt"`
	CompletedAt  *time.Time `json:"completedAt"`
	ErrorMessage string     `json:"errorMessage"`
}

// QueueSummaryDTO reports the number of backup jobs in each status.
type QueueSummaryDTO struct {
	Pending   int64 `json:"pending"`
	Running   int64 `json:"running"`
	Paused    int64 `json:"paused"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	Total     int64 `json:"total"`
}

// summaryFromCounts folds repo StatusCount rows into a QueueSummaryDTO.
func summaryFromCounts(counts []domain.JobStatus, values []int64) QueueSummaryDTO {
	var out QueueSummaryDTO
	for i, st := range counts {
		n := values[i]
		out.Total += n
		switch st {
		case domain.JobStatusPending:
			out.Pending = n
		case domain.JobStatusRunning:
			out.Running = n
		case domain.JobStatusPaused:
			out.Paused = n
		case domain.JobStatusCompleted:
			out.Completed = n
		case domain.JobStatusFailed:
			out.Failed = n
		case domain.JobStatusCancelled:
			out.Cancelled = n
		}
	}
	return out
}

// LogEntryDTO is the JSON-friendly projection of a persisted LogEntry.
type LogEntryDTO struct {
	ID           int64     `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	Level        string    `json:"level"`
	Subsystem    string    `json:"subsystem"`
	Message      string    `json:"message"`
	MetadataJSON string    `json:"metadataJson"`
}

// toLogEntryDTO maps a domain.LogEntry to its DTO.
func toLogEntryDTO(e domain.LogEntry) LogEntryDTO {
	return LogEntryDTO{
		ID:           e.ID,
		Timestamp:    e.Timestamp,
		Level:        e.Level,
		Subsystem:    e.Subsystem,
		Message:      e.Message,
		MetadataJSON: e.MetadataJSON,
	}
}

// PageResult wraps a paginated slice with its total match count so tables can
// render pagination controls.
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
}

// normalizePage clamps a 1-based page and page size to sane bounds and returns
// the resulting limit/offset. Pages are 1-based to match the frontend callers:
// page 1 is the first page (offset 0); page<=0 is clamped to 1.
func normalizePage(page, pageSize int) (limit, offset int) {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	if page <= 0 {
		page = 1
	}
	return pageSize, (page - 1) * pageSize
}
