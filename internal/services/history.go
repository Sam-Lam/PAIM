package services

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/repo"
)

// HistoryService lists past import sessions and the log entries belonging to one.
type HistoryService struct {
	gated
	sessions *repo.SessionRepo
	logs     *repo.LogRepo
	log      *slog.Logger
}

// Bind wires the HistoryService to an open library's repos in place.
func (s *HistoryService) Bind(core *AppCore) {
	s.sessions = core.Sessions
	s.logs = core.Logs
}

// NewHistoryService constructs a HistoryService.
func NewHistoryService(sessions *repo.SessionRepo, logs *repo.LogRepo, logger *slog.Logger) *HistoryService {
	if logger == nil {
		logger = slog.Default()
	}
	return &HistoryService{sessions: sessions, logs: logs, log: logger}
}

// ListSessions returns a page of import sessions, newest first. SessionRepo
// exposes only a limit-based recent listing, so pagination is implemented by
// fetching up to (offset+limit) recent rows and slicing the window; Total is the
// true count of all sessions so the History table renders correct pagination.
func (s *HistoryService) ListSessions(ctx context.Context, page, pageSize int) (PageResult[SessionDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[SessionDTO]{}, err
	}
	limit, offset := normalizePage(page, pageSize)
	rows, err := s.sessions.ListRecent(ctx, offset+limit)
	if err != nil {
		return PageResult[SessionDTO]{}, err
	}
	total, err := s.sessions.Count(ctx)
	if err != nil {
		return PageResult[SessionDTO]{}, err
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	items := make([]SessionDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, toSessionDTO(r))
	}
	return PageResult[SessionDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// SessionDetail bundles a session with the log entries produced during its run.
type SessionDetail struct {
	Session SessionDTO    `json:"session"`
	Events  []LogEntryDTO `json:"events"`
}

// SessionEvents returns the session plus the log entries that reference it. The
// LogRepo cannot query MetadataJSON directly, so entries are gathered from the
// import subsystem within the session's time window and filtered in memory to
// those whose MetadataJSON mentions the session ID.
func (s *HistoryService) SessionEvents(ctx context.Context, sessionID string) (SessionDetail, error) {
	if err := s.guard(); err != nil {
		return SessionDetail{}, err
	}
	session, err := s.sessions.GetByID(ctx, sessionID)
	if err != nil {
		return SessionDetail{}, err
	}

	until := time.Now()
	if session.CompletedAt != nil {
		until = session.CompletedAt.Add(time.Second)
	}
	q := repo.LogQuery{
		Subsystem: "import",
		Since:     session.StartedAt.Add(-time.Second),
		Until:     until,
	}
	entries, err := s.logs.ListForExport(ctx, q)
	if err != nil {
		return SessionDetail{}, err
	}

	events := make([]LogEntryDTO, 0)
	for _, e := range entries {
		if strings.Contains(e.MetadataJSON, sessionID) {
			events = append(events, toLogEntryDTO(e))
		}
	}
	return SessionDetail{Session: toSessionDTO(*session), Events: events}, nil
}
