// Package metadata extracts technical and descriptive metadata from media files
// for PAIM. It wraps the exiftool command-line tool run as a single persistent
// -stay_open batch process (the preferred, high-fidelity path) and degrades
// gracefully to a stdlib-only Fallback when exiftool is not installed.
//
// The package is split so the JSON-parsing layer (parseExifJSON) is independent
// of the subprocess-management layer (ExifTool): parsing is exercised by golden
// fixtures without needing exiftool on the machine, while process management is
// covered by an integration test that self-skips when exiftool is absent.
//
// exiftool is always invoked with "-json -n": -json for machine-readable output
// and -n to disable print-conversion so GPS coordinates, durations, exposure and
// orientation arrive as raw numbers rather than human-formatted strings. The few
// values that are nicer as display strings (shutter speed, aperture, colour
// space) are reconstructed from those numbers in the parse layer.
package metadata

import (
	"context"
	"log/slog"
	"time"
)

// AssetMetadata is the normalized, orientation-corrected metadata extracted from
// a single media file. All fields are best-effort: a missing value yields the
// zero value (or nil for the pointer fields) rather than an error, so partial
// metadata never blocks an import.
type AssetMetadata struct {
	// SourceFile echoes the path exiftool reported for this record. It is the key
	// used to demultiplex ExtractBatch results.
	SourceFile string

	// CaptureDate is the moment the media was captured, chosen by priority
	// SubSecDateTimeOriginal > DateTimeOriginal > CreateDate. Values carrying a
	// timezone offset (e.g. "2026:07:17 10:04:05.123-07:00") are parsed as such;
	// naive values (e.g. "2026:07:17 10:04:05") are interpreted as local time.
	// nil means no capture date was available (the importer falls back to mtime).
	CaptureDate *time.Time

	CameraMake  string
	CameraModel string
	Lens        string

	ISO          int
	ShutterSpeed string // reconstructed display form, e.g. "1/500" or "2.5"
	Aperture     string // reconstructed display form, e.g. "f/2.8"

	// GPSLatitude / GPSLongitude are signed decimal degrees (N/E positive), or nil
	// when absent.
	GPSLatitude  *float64
	GPSLongitude *float64

	// Width and Height are orientation-corrected: when Orientation indicates a
	// 90/270-degree rotation (values 5–8) the stored dimensions are swapped so
	// they describe the displayed image.
	Width       int
	Height      int
	Orientation int

	DurationSeconds float64
	FrameRate       float64
	Codec           string
	ColorSpace      string

	// ContentIdentifier is Apple's Live Photo identifier (exiftool tag
	// ContentIdentifier); the still and its paired movie share the same value.
	ContentIdentifier string
}

// Extractor is the metadata-extraction contract consumed by the importer. It is
// satisfied by ExifTool (full fidelity) and Fallback (degraded, stdlib-only).
type Extractor interface {
	// Extract reads metadata for a single file. It honors ctx (see the ExifTool
	// implementation for the cancellation semantics of the stay_open protocol).
	Extract(ctx context.Context, path string) (*AssetMetadata, error)

	// ExtractBatch reads metadata for many files in one or more batched exiftool
	// invocations. The result is keyed by each record's SourceFile. A per-file
	// read failure is not fatal to the whole batch; a fatal error (dead process)
	// is returned alongside any results gathered so far.
	ExtractBatch(ctx context.Context, paths []string) (map[string]*AssetMetadata, error)

	// Available reports whether real metadata extraction is possible. false means
	// the caller is running in degraded mode and should surface that in the UI.
	Available() bool

	// Close releases any subprocess resources. It is safe to call more than once.
	Close() error
}

// defaultExiftoolPath is used when exiftool is not found on PATH; it matches the
// Homebrew install location on Apple Silicon.
const defaultExiftoolPath = "/opt/homebrew/bin/exiftool"

// loggerOrDefault normalizes a possibly-nil logger to the process default.
func loggerOrDefault(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
