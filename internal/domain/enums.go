// Package domain holds the persistent data model for PAIM (Photo Archive
// Integrity Manager): GORM models, enum string constants, and shared embedded
// structs. It contains no business logic and imports no other internal package,
// per the architecture specification.
package domain

// MediaType classifies the kind of media an Asset represents.
type MediaType string

// MediaType values. RAW photos and Live Photo pairs are distinguished from
// ordinary photos so they can be laid out and counted according to their own
// rules.
const (
	MediaTypePhoto         MediaType = "photo"
	MediaTypeRawPhoto      MediaType = "raw_photo"
	MediaTypeVideo         MediaType = "video"
	MediaTypeLivePhotoPair MediaType = "live_photo_pair"
)

// VerificationStatus tracks whether an Asset's archived copy has been verified
// byte-for-byte against its source.
type VerificationStatus string

// VerificationStatus values.
const (
	VerificationStatusPending   VerificationStatus = "pending"
	VerificationStatusVerifying VerificationStatus = "verifying"
	VerificationStatusVerified  VerificationStatus = "verified"
	VerificationStatusFailed    VerificationStatus = "failed"
)

// BackupStatus is the aggregate backup state recorded on an Asset (as opposed to
// the state of an individual BackupJob).
type BackupStatus string

// BackupStatus values.
const (
	BackupStatusNone     BackupStatus = "none"
	BackupStatusPending  BackupStatus = "pending"
	BackupStatusPartial  BackupStatus = "partial"
	BackupStatusComplete BackupStatus = "complete"
	BackupStatusFailed   BackupStatus = "failed"
)

// SessionStatus is the lifecycle state of an ImportSession.
type SessionStatus string

// SessionStatus values. interrupted marks a session that was running when the
// app stopped and is therefore resumable.
const (
	SessionStatusScanning    SessionStatus = "scanning"
	SessionStatusDryRun      SessionStatus = "dry_run"
	SessionStatusRunning     SessionStatus = "running"
	SessionStatusPaused      SessionStatus = "paused"
	SessionStatusCompleted   SessionStatus = "completed"
	SessionStatusFailed      SessionStatus = "failed"
	SessionStatusCancelled   SessionStatus = "cancelled"
	SessionStatusInterrupted SessionStatus = "interrupted"
)

// JobStatus is the lifecycle state of a single BackupJob in the persisted queue.
type JobStatus string

// JobStatus values.
const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusPaused    JobStatus = "paused"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// UploadOrder controls the order in which a backup provider's pending jobs are
// claimed by the worker pool. OldestFirst (the default / zero-equivalent) keeps
// strict FIFO; NewestFirst drains the newest media first so a quota-limited
// mirror spends each day's budget on the most recently imported memories and new
// imports jump the queue.
type UploadOrder string

// UploadOrder values. An empty string is treated as UploadOrderOldestFirst.
const (
	UploadOrderOldestFirst UploadOrder = "oldest_first"
	UploadOrderNewestFirst UploadOrder = "newest_first"
)

// SourceType classifies the physical or logical origin of imported media.
type SourceType string

// SourceType values. A volume label is never used as identity; these types are
// determined from hardware/volume metadata (see internal/source).
const (
	SourceTypeSDCard         SourceType = "sd_card"
	SourceTypeUSBSSD         SourceType = "usb_ssd"
	SourceTypeExternalHDD    SourceType = "external_hdd"
	SourceTypeInternalFolder SourceType = "internal_folder"
	SourceTypeNASFolder      SourceType = "nas_folder"
	SourceTypeSMBShare       SourceType = "smb_share"
)

// LogLevel string constants matching slog.Level.String() output, used for the
// LogEntry.Level column and for filtering in the Logs page.
const (
	LogLevelDebug = "DEBUG"
	LogLevelInfo  = "INFO"
	LogLevelWarn  = "WARN"
	LogLevelError = "ERROR"
)
