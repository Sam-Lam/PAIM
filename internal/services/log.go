package services

import (
	"bufio"
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

// logExportPageSize is the DB page size used when streaming an export, so a large
// log never has to be materialized in memory all at once.
const logExportPageSize = 5000

// LogService searches, enumerates, and exports persisted log entries for the
// Logs page.
type LogService struct {
	gated
	db      *gorm.DB
	logs    *repo.LogRepo
	dialog  Dialoger
	emitter Emitter
	log     *slog.Logger
}

// Bind wires the LogService to an open library's catalog in place.
func (s *LogService) Bind(core *AppCore) {
	s.db = core.DB
	s.logs = core.Logs
}

// NewLogService constructs a LogService. The db handle backs the distinct
// subsystem query (LogRepo exposes no DISTINCT helper).
func NewLogService(db *gorm.DB, logs *repo.LogRepo, dialog Dialoger, emitter Emitter, logger *slog.Logger) *LogService {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogService{db: db, logs: logs, dialog: dialog, emitter: emitter, log: logger}
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

// Export streams all log entries matching the filters (ignoring pagination) to a
// user-chosen file in json or csv format via a native save dialog and returns
// the written path. It returns "" when the user cancels the dialog. Rows are
// pulled from the DB in pages (logExportPageSize) and written to disk one at a
// time — CSV row-by-row, JSON as a streamed array — so memory stays flat on a
// large log, and log:export-progress (rows written) is emitted throttled.
func (s *LogService) Export(ctx context.Context, query, level, subsystem, fromISO, toISO, format string) (string, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	q, err := buildQuery(query, level, subsystem, fromISO, toISO, 0, 0)
	if err != nil {
		return "", err
	}
	q.Page = repo.Page{}

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

	tr := newThrottle()
	rows := 0
	progress := func() {
		rows++
		if tr.allow() {
			emitSafe(s.emitter, EventLogExportProgress, LogExportProgress{RowsWritten: rows})
		}
	}

	if format == "json" {
		err = s.streamJSON(ctx, path, q, progress)
	} else {
		err = s.streamCSV(ctx, path, q, progress)
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	emitSafe(s.emitter, EventLogExportProgress, LogExportProgress{RowsWritten: rows})
	s.log.Info("logs exported", "path", path, "format", format, "count", rows)
	return path, nil
}

// streamCSV writes the query's matching entries to path as CSV, one row at a
// time, flushing periodically so memory stays flat.
func (s *LogService) streamCSV(ctx context.Context, path string, q repo.LogQuery, progress func()) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("services: create %q: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"timestamp", "level", "subsystem", "message", "metadata"}); err != nil {
		return fmt.Errorf("services: write csv header: %w", err)
	}
	n := 0
	streamErr := s.logs.StreamForExport(ctx, q, logExportPageSize, func(e domain.LogEntry) error {
		if err := w.Write([]string{
			e.Timestamp.Format(time.RFC3339),
			e.Level,
			e.Subsystem,
			e.Message,
			e.MetadataJSON,
		}); err != nil {
			return fmt.Errorf("services: write csv row: %w", err)
		}
		progress()
		if n++; n%1000 == 0 {
			w.Flush()
			if err := w.Error(); err != nil {
				return fmt.Errorf("services: flush csv: %w", err)
			}
		}
		return nil
	})
	if streamErr != nil {
		return streamErr
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("services: flush csv: %w", err)
	}
	return nil
}

// streamJSON writes the query's matching entries to path as a streamed JSON
// array of LogEntryDTO, marshaling each entry individually so the whole result
// set is never held in memory at once.
func (s *LogService) streamJSON(ctx context.Context, path string, q repo.LogQuery, progress func()) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("services: create %q: %w", path, err)
	}
	defer f.Close()

	bw := bufio.NewWriter(f)
	if _, err := bw.WriteString("[\n"); err != nil {
		return fmt.Errorf("services: write %q: %w", path, err)
	}
	first := true
	streamErr := s.logs.StreamForExport(ctx, q, logExportPageSize, func(e domain.LogEntry) error {
		raw, err := json.Marshal(toLogEntryDTO(e))
		if err != nil {
			return fmt.Errorf("services: marshal log entry: %w", err)
		}
		if !first {
			if _, err := bw.WriteString(",\n"); err != nil {
				return err
			}
		}
		first = false
		if _, err := bw.WriteString("  "); err != nil {
			return err
		}
		if _, err := bw.Write(raw); err != nil {
			return err
		}
		progress()
		return nil
	})
	if streamErr != nil {
		return streamErr
	}
	if _, err := bw.WriteString("\n]\n"); err != nil {
		return fmt.Errorf("services: write %q: %w", path, err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("services: flush %q: %w", path, err)
	}
	return nil
}
