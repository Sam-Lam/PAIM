package services

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// LogService searches, enumerates, and exports persisted log entries for the
// Logs page.
type LogService struct {
	gated
	db     *gorm.DB
	logs   *repo.LogRepo
	dialog Dialoger
	log    *slog.Logger
}

// Bind wires the LogService to an open library's catalog in place.
func (s *LogService) Bind(core *AppCore) {
	s.db = core.DB
	s.logs = core.Logs
}

// NewLogService constructs a LogService. The db handle backs the distinct
// subsystem query (LogRepo exposes no DISTINCT helper).
func NewLogService(db *gorm.DB, logs *repo.LogRepo, dialog Dialoger, logger *slog.Logger) *LogService {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogService{db: db, logs: logs, dialog: dialog, log: logger}
}

// buildQuery assembles a repo.LogQuery from the raw filter parameters, parsing
// the ISO-8601 time bounds (empty bounds are ignored).
func buildQuery(query, level, subsystem, fromISO, toISO string, page, pageSize int) (repo.LogQuery, error) {
	q := repo.LogQuery{Text: query, Level: level, Subsystem: subsystem}
	if fromISO != "" {
		t, err := time.Parse(time.RFC3339, fromISO)
		if err != nil {
			return repo.LogQuery{}, fmt.Errorf("services: parse from time %q: %w", fromISO, err)
		}
		q.Since = t
	}
	if toISO != "" {
		t, err := time.Parse(time.RFC3339, toISO)
		if err != nil {
			return repo.LogQuery{}, fmt.Errorf("services: parse to time %q: %w", toISO, err)
		}
		q.Until = t
	}
	limit, offset := normalizePage(page, pageSize)
	q.Page = repo.Page{Limit: limit, Offset: offset}
	return q, nil
}

// Search returns a page of log entries matching the filters, newest first.
func (s *LogService) Search(ctx context.Context, query, level, subsystem, fromISO, toISO string, page, pageSize int) (PageResult[LogEntryDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[LogEntryDTO]{}, err
	}
	q, err := buildQuery(query, level, subsystem, fromISO, toISO, page, pageSize)
	if err != nil {
		return PageResult[LogEntryDTO]{}, err
	}
	rows, total, err := s.logs.Search(ctx, q)
	if err != nil {
		return PageResult[LogEntryDTO]{}, err
	}
	items := make([]LogEntryDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, toLogEntryDTO(r))
	}
	return PageResult[LogEntryDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// Subsystems returns the distinct subsystem names present in the log, sorted.
func (s *LogService) Subsystems(ctx context.Context) ([]string, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var names []string
	err := s.db.WithContext(ctx).
		Model(&domain.LogEntry{}).
		Distinct().
		Order("subsystem ASC").
		Pluck("subsystem", &names).Error
	if err != nil {
		return nil, fmt.Errorf("services: list subsystems: %w", err)
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// Export writes all log entries matching the filters (ignoring pagination) to a
// user-chosen file in json or csv format via a native save dialog and returns
// the written path. It returns "" when the user cancels the dialog.
func (s *LogService) Export(ctx context.Context, query, level, subsystem, fromISO, toISO, format string) (string, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	q, err := buildQuery(query, level, subsystem, fromISO, toISO, 0, 0)
	if err != nil {
		return "", err
	}
	q.Page = repo.Page{}
	entries, err := s.logs.ListForExport(ctx, q)
	if err != nil {
		return "", err
	}

	format = strings.ToLower(format)
	if format != "json" && format != "csv" {
		return "", fmt.Errorf("services: unsupported export format %q", format)
	}

	if s.dialog == nil {
		return "", fmt.Errorf("services: no dialog provider configured")
	}
	defaultName := "paim-logs." + format
	path, err := s.dialog.SaveFile(ctx, defaultName)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil // cancelled
	}

	if format == "json" {
		if err := writeJSON(path, entries); err != nil {
			return "", err
		}
	} else {
		if err := writeCSV(path, entries); err != nil {
			return "", err
		}
	}
	s.log.Info("logs exported", "path", path, "format", format, "count", len(entries))
	return path, nil
}

func writeJSON(path string, entries []domain.LogEntry) error {
	dtos := make([]LogEntryDTO, 0, len(entries))
	for _, e := range entries {
		dtos = append(dtos, toLogEntryDTO(e))
	}
	raw, err := json.MarshalIndent(dtos, "", "  ")
	if err != nil {
		return fmt.Errorf("services: marshal logs: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("services: write %q: %w", path, err)
	}
	return nil
}

func writeCSV(path string, entries []domain.LogEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("services: create %q: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"timestamp", "level", "subsystem", "message", "metadata"}); err != nil {
		return fmt.Errorf("services: write csv header: %w", err)
	}
	for _, e := range entries {
		row := []string{
			e.Timestamp.Format(time.RFC3339),
			e.Level,
			e.Subsystem,
			e.Message,
			e.MetadataJSON,
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("services: write csv row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("services: flush csv: %w", err)
	}
	return nil
}
