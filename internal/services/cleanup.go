package services

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/autolinepro/paim/internal/cleanup"
)

// CleanupService runs the read-only Cleanup Assistant analysis over a folder.
//
// v1 is advisory only: there is deliberately NO server-side delete method. The
// Analyze result carries a recommendation the UI presents; any actual deletion
// is the user's responsibility in Finder. A DeleteAnalyzedFolder method is
// intentionally not implemented.
type CleanupService struct {
	gated
	analyzer *cleanup.Analyzer
	dialog   Dialoger
	log      *slog.Logger
}

// Bind wires the CleanupService to an open library's analyzer in place.
func (s *CleanupService) Bind(core *AppCore) {
	s.analyzer = core.Analyzer
}

// NewCleanupService constructs a CleanupService.
func NewCleanupService(analyzer *cleanup.Analyzer, dialog Dialoger, logger *slog.Logger) *CleanupService {
	if logger == nil {
		logger = slog.Default()
	}
	return &CleanupService{analyzer: analyzer, dialog: dialog, log: logger.With(slog.String("subsystem", "cleanup"))}
}

// PickFolder opens a native directory chooser for the folder to analyze.
func (s *CleanupService) PickFolder(ctx context.Context) (string, error) {
	if s.dialog == nil {
		return "", fmt.Errorf("services: no dialog provider configured")
	}
	return s.dialog.PickFolder(ctx, "Choose a folder to analyze")
}

// ClassStatDTO is the per-class rollup in a cleanup report.
type ClassStatDTO struct {
	Class     string   `json:"class"`
	Count     int      `json:"count"`
	Bytes     int64    `json:"bytes"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// RecommendationDTO is the delete-safety verdict.
type RecommendationDTO struct {
	SafeToDelete bool     `json:"safeToDelete"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Reasons      []string `json:"reasons"`
}

// CleanupReportDTO is the JSON-friendly cleanup analysis result.
type CleanupReportDTO struct {
	Root                string            `json:"root"`
	Classes             []ClassStatDTO    `json:"classes"`
	TotalFiles          int               `json:"totalFiles"`
	MediaFiles          int               `json:"mediaFiles"`
	NonMedia            int               `json:"nonMedia"`
	UnreadableMedia     int               `json:"unreadableMedia"`
	ArchivedNotVerified int               `json:"archivedNotVerified"`
	BackupIncomplete    int               `json:"backupIncomplete"`
	DBInconsistencies   int               `json:"dbInconsistencies"`
	Recommendation      RecommendationDTO `json:"recommendation"`
}

// Analyze performs a strictly read-only classification of every media file under
// root against the archive and returns the report plus its delete-safety
// recommendation. It is synchronous but honors ctx cancellation between files.
func (s *CleanupService) Analyze(ctx context.Context, root string) (CleanupReportDTO, error) {
	if err := s.guard(); err != nil {
		return CleanupReportDTO{}, err
	}
	if root == "" {
		return CleanupReportDTO{}, fmt.Errorf("services: cleanup analyze: empty root")
	}
	report, err := s.analyzer.Analyze(ctx, root, nil)
	if err != nil {
		return CleanupReportDTO{}, err
	}
	rec := report.Recommendation()

	classes := make([]ClassStatDTO, 0, len(cleanup.AllClasses()))
	for _, c := range cleanup.AllClasses() {
		stat := report.Class(c)
		classes = append(classes, ClassStatDTO{
			Class:     string(c),
			Count:     stat.Count,
			Bytes:     stat.Bytes,
			Files:     stat.Files,
			Truncated: stat.Truncated,
		})
	}

	return CleanupReportDTO{
		Root:                report.Root,
		Classes:             classes,
		TotalFiles:          report.TotalFiles,
		MediaFiles:          report.MediaFiles,
		NonMedia:            report.NonMedia,
		UnreadableMedia:     report.UnreadableMedia,
		ArchivedNotVerified: report.ArchivedNotVerified,
		BackupIncomplete:    report.BackupIncomplete,
		DBInconsistencies:   report.DBInconsistencies,
		Recommendation: RecommendationDTO{
			SafeToDelete: rec.SafeToDelete,
			Title:        rec.Title,
			Summary:      rec.Summary,
			Reasons:      rec.Reasons,
		},
	}, nil
}
