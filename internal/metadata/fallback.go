package metadata

import (
	"context"
	"log/slog"
)

// Fallback is the degraded Extractor used when exiftool is not installed. It
// reads no file contents and returns zero-value metadata (never an error), so an
// import can still proceed — the importer supplies a capture date from the file
// mtime and the UI surfaces the degraded state. Availability is false so callers
// can detect and report the degradation.
type Fallback struct {
	logger *slog.Logger
}

var _ Extractor = (*Fallback)(nil)

// newFallback constructs a Fallback with the given logger.
func newFallback(logger *slog.Logger) *Fallback {
	return &Fallback{logger: loggerOrDefault(logger)}
}

// Available always reports false: metadata extraction is degraded.
func (f *Fallback) Available() bool { return false }

// Extract returns zero-value metadata carrying only the requested path. The
// CaptureDate is deliberately nil so the importer falls back to the file mtime.
func (f *Fallback) Extract(_ context.Context, path string) (*AssetMetadata, error) {
	return &AssetMetadata{SourceFile: path}, nil
}

// ExtractBatch returns a zero-value metadata entry for each requested path.
func (f *Fallback) ExtractBatch(_ context.Context, paths []string) (map[string]*AssetMetadata, error) {
	result := make(map[string]*AssetMetadata, len(paths))
	for _, p := range paths {
		result[p] = &AssetMetadata{SourceFile: p}
	}
	return result, nil
}

// Close is a no-op; there is no subprocess to release.
func (f *Fallback) Close() error { return nil }

// NewExtractor returns the best available Extractor: an ExifTool when the
// exiftool binary can be located, otherwise a Fallback. When it degrades to the
// Fallback it logs prominently so the degradation is visible in the console and
// the Logs table.
func NewExtractor(logger *slog.Logger) Extractor {
	logger = loggerOrDefault(logger)
	if path := lookExiftool(); path != "" {
		logger.Info("metadata extraction enabled via exiftool", "path", path)
		return newExifTool(path, logger)
	}
	logger.Warn("exiftool not found: metadata extraction is DEGRADED — " +
		"imports will proceed with capture date from file modification time and no EXIF metadata")
	return newFallback(logger)
}
