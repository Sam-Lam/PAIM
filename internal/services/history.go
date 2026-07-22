package services

import (
	"context"
	"log/slog"
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
// Truncated reports whether the event list was capped (sessionEventsCap); the
// counts on the session itself always reflect the full run.
type SessionDetail struct {
	Session   SessionDTO    `json:"session"`
	Events    []LogEntryDTO `json:"events"`
	Truncated bool          `json:"truncated"`
}

// sessionEventsCap bounds how many log entries SessionEvents returns for one
// session; beyond it the DTO's Truncated flag is set.
const sessionEventsCap = 5000

// SessionEvents returns the session plus the log entries that reference it. The
// sessionID match is pushed into SQL (metadata_json LIKE) and bounded by the
// session's time window, so a large log never has to be loaded and string-
// scanned in memory. The result is capped at sessionEventsCap with a truncation
// flag in the DTO.
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
		Subsystem:    "import",
		MetadataText: sessionID,
		Since:        session.StartedAt.Add(-time.Second),
		Until:        until,
	}
	entries, truncated, err := s.logs.ListForSession(ctx, q, sessionEventsCap)
	if err != nil {
		return SessionDetail{}, err
	}

	events := make([]LogEntryDTO, 0, len(entries))
	for _, e := range entries {
		events = append(events, toLogEntryDTO(e))
	}
	return SessionDetail{Session: toSessionDTO(*session), Events: events, Truncated: truncated}, nil
}
