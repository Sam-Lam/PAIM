package services

import (
	"path/filepath"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
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
	// SourceLabel is a human-readable description of the import source for the
	// History "Source" column: for adopt runs "Library (adopt)"; for a copy run
	// with a linked source, the volume label + type (enriched by HistoryService);
	// otherwise a display-only fallback to the source root folder's basename. It is
	// never empty for a session whose notes recorded a source root.
	SourceLabel string `json:"sourceLabel"`
	// SourceRoot is the absolute source tree recorded in the session's resume-state
	// notes. It backs the Source column's tooltip (and the basename fallback).
	SourceRoot string `json:"sourceRoot"`
}

// toSessionDTO maps a domain.ImportSession to its DTO, extracting the import mode
// from the resume-state JSON stored in Notes (defaulting to "copy") and deriving
// a display-only SourceLabel. HistoryService replaces the label with the linked
// volume's label+type when the session records a SourceID.
func toSessionDTO(s domain.ImportSession) SessionDTO {
	st, _ := decodeResumeState(s.Notes)
	mode := "copy"
	if st.Mode != "" {
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
		SourceLabel:     defaultSourceLabel(mode, st.SourceRoot),
		SourceRoot:      st.SourceRoot,
	}
}

// defaultSourceLabel derives the display-only source label from a session's mode
// and recorded source root, used when no ImportSource is linked (or before
// enrichment): adopt runs are labeled "Library (adopt)" (the source is the
// library itself); copy runs fall back to the source root folder's basename.
func defaultSourceLabel(mode, sourceRoot string) string {
	if mode == string(importer.ModeAdopt) {
		return "Library (adopt)"
	}
	if sourceRoot != "" {
		return filepath.Base(sourceRoot)
	}
	return ""
}

// ImportFailureDTO is the JSON-friendly projection of a structured per-file
// import failure. Op names the pipeline stage that failed; Status is open,
// retried, or dismissed. It backs the Import completion panel and Import History
// "Failed files" panel, whose per-file Retry/Dismiss actions resolve it.
type ImportFailureDTO struct {
	ID            string     `json:"id"`
	SessionID     string     `json:"sessionId"`
	Path          string     `json:"path"`
	Op            string     `json:"op"`
	ErrorMessage  string     `json:"errorMessage"`
	Status        string     `json:"status"`
	ResolvedAt    *time.Time `json:"resolvedAt"`
	DismissReason string     `json:"dismissReason"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// toImportFailureDTO maps a domain.ImportFailure to its DTO.
func toImportFailureDTO(f domain.ImportFailure) ImportFailureDTO {
	return ImportFailureDTO{
		ID:            f.ID,
		SessionID:     f.SessionID,
		Path:          f.Path,
		Op:            string(f.Op),
		ErrorMessage:  f.ErrorMessage,
		Status:        string(f.Status),
		ResolvedAt:    f.ResolvedAt,
		DismissReason: f.DismissReason,
		CreatedAt:     f.CreatedAt,
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

// QueueSummaryDTO reports the number of backup jobs in each status, plus any
// provider quota cooldowns currently in effect (so the Backup Queue can show a
// "uploads resume ~HH:MM" banner; the cooling jobs stay visible as pending).
type QueueSummaryDTO struct {
	Pending   int64 `json:"pending"`
	Running   int64 `json:"running"`
	Paused    int64 `json:"paused"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	// OptedOut counts jobs the user deliberately excluded per-import (opted_out).
	// They are never claimed and never gate a safety verdict; shown subtly in the
	// queue summary so the count is visible without implying pending work.
	OptedOut int64 `json:"optedOut"`
	Total    int64 `json:"total"`

	Cooldowns []ProviderCooldownDTO `json:"cooldowns"`

	// JobsPerMinute is the rolling backup completion rate (completed jobs per
	// minute) over a recent trailing window; 0 when there is not enough recent
	// activity to estimate. It drives the queue header's rate line.
	JobsPerMinute float64 `json:"jobsPerMinute"`
	// BytesRemaining is the outstanding upload workload in bytes — the summed file
	// size of assets behind pending/paused/running jobs (per provider). SQL
	// aggregate; never a full row scan.
	BytesRemaining int64 `json:"bytesRemaining"`
	// LastCompletedAt is the most recent backup-job completion, or nil when nothing
	// has completed yet (the UI shows a relative "last backup 2m ago").
	LastCompletedAt *time.Time `json:"lastCompletedAt"`
	// EtaSeconds estimates how many seconds until the active queue (pending+running)
	// drains at the current rate; EtaAt is the corresponding wall-clock instant.
	// Both are zero/nil when the rate is 0, the queue is empty, or backups are
	// paused/yielding/cooling — the frontend then renders "—" and surfaces the
	// paused state instead of a stale ETA, never "done ~Infinity".
	EtaSeconds int64      `json:"etaSeconds"`
	EtaAt      *time.Time `json:"etaAt"`

	// Yielding is true when the backup manager is currently withholding new job
	// claims because a foreground operation (import/analyze/reorganize/…) is
	// running; in-flight uploads still finish and pending jobs resume
	// automatically when the foreground work ends. Drives the Backup Queue's
	// "paused while an import runs" banner.
	Yielding bool `json:"yielding"`
}

// ProviderCooldownDTO describes one provider's active quota cooldown.
type ProviderCooldownDTO struct {
	ProviderID string    `json:"providerId"`
	Until      time.Time `json:"until"`
	Reason     string    `json:"reason"`
}

// cooldownDTOs projects manager cooldown snapshots into DTOs.
func cooldownDTOs(cds []backup.ProviderCooldown) []ProviderCooldownDTO {
	out := make([]ProviderCooldownDTO, 0, len(cds))
	for _, c := range cds {
		out = append(out, ProviderCooldownDTO{ProviderID: c.ProviderID, Until: c.Until, Reason: c.Reason})
	}
	return out
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
		case domain.JobStatusOptedOut:
			out.OptedOut = n
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
