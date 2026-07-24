package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/importer"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// HistoryService lists past import sessions and the log entries belonging to one.
type HistoryService struct {
	gated
	sessions *repo.SessionRepo
	logs     *repo.LogRepo
	sources  *repo.SourceRepo
	failures *repo.ImportFailureRepo
	log      *slog.Logger
}

// Bind wires the HistoryService to an open library's repos in place.
func (s *HistoryService) Bind(core *AppCore) {
	s.sessions = core.Sessions
	s.logs = core.Logs
	s.sources = core.Sources
	s.failures = core.Failures
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
	s.enrichSourceLabels(ctx, items)
	return PageResult[SessionDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// enrichSourceLabels replaces the display-only SourceLabel with the linked
// volume's label + type for every copy-mode session that recorded a SourceID.
// It is best-effort: a missing source or a lookup error leaves the notes-derived
// fallback (source-root basename) in place. Lookups are memoized per SourceID so
// a page of sessions from one card issues a single query. Adopt sessions keep
// their "Library (adopt)" label untouched.
func (s *HistoryService) enrichSourceLabels(ctx context.Context, items []SessionDTO) {
	if s.sources == nil {
		return
	}
	cache := make(map[string]string)
	for i := range items {
		d := &items[i]
		if d.SourceID == "" || d.Mode == string(importer.ModeAdopt) {
			continue
		}
		label, ok := cache[d.SourceID]
		if !ok {
			label = s.sourceLabelFor(ctx, d.SourceID)
			cache[d.SourceID] = label
		}
		if label != "" {
			d.SourceLabel = label
		}
	}
}

// sourceLabelFor returns "<volume label> (<type>)" for a linked source, or ""
// when it cannot be resolved. The volume label falls back through model /
// manufacturer / serial so a card with no filesystem label is still named.
func (s *HistoryService) sourceLabelFor(ctx context.Context, sourceID string) string {
	src, err := s.sources.GetByID(ctx, sourceID)
	if err != nil || src == nil {
		return ""
	}
	label := firstNonEmptyStr(src.VolumeLabel, src.Model, src.Manufacturer, src.HardwareSerial)
	typ := string(src.SourceType)
	switch {
	case label != "" && typ != "":
		return fmt.Sprintf("%s (%s)", label, typ)
	case label != "":
		return label
	case typ != "":
		return typ
	default:
		return ""
	}
}

// firstNonEmptyStr returns the first non-empty string in vals, or "".
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SessionDetail bundles a session with the log entries produced during its run.
// Truncated reports whether the event list was capped (sessionEventsCap); the
// counts on the session itself always reflect the full run.
type SessionDetail struct {
	Session   SessionDTO    `json:"session"`
	Events    []LogEntryDTO `json:"events"`
	Truncated bool          `json:"truncated"`
	// Approximate is true when the events could not be matched to this session by
	// ID (an older session imported before per-file logs carried a sessionId) and
	// were instead gathered by the import subsystem's time window. The UI notes
	// this so the user knows the list is a best-effort time-window match.
	Approximate bool `json:"approximate"`
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

	// Fallback for pre-existing sessions whose per-file logs predate sessionId
	// tagging: when the ID match finds nothing, gather the import subsystem's
	// events within this session's time window and flag them as approximate so the
	// UI can say the match is by time, not identity.
	approximate := false
	if len(entries) == 0 {
		fq := q
		fq.MetadataText = ""
		fbEntries, fbTruncated, ferr := s.logs.ListForSession(ctx, fq, sessionEventsCap)
		if ferr != nil {
			return SessionDetail{}, ferr
		}
		if len(fbEntries) > 0 {
			entries, truncated, approximate = fbEntries, fbTruncated, true
		}
	}

	events := make([]LogEntryDTO, 0, len(entries))
	for _, e := range entries {
		events = append(events, toLogEntryDTO(e))
	}
	dto := toSessionDTO(*session)
	if s.sources != nil && dto.SourceID != "" && dto.Mode != string(importer.ModeAdopt) {
		if label := s.sourceLabelFor(ctx, dto.SourceID); label != "" {
			dto.SourceLabel = label
		}
	}
	return SessionDetail{Session: dto, Events: events, Truncated: truncated, Approximate: approximate}, nil
}

// ErrFailureAlreadyResolved is returned by DismissFailure when the record is not
// open (already retried or dismissed) — the UI should refresh rather than act on
// stale state.
var ErrFailureAlreadyResolved = errors.New("services: import failure is already resolved")

// ListSessionFailures returns a page of the structured per-file failure records
// for a session (oldest first, so the list reads in import order). Total is the
// count of ALL failure records for the session (any status); a session whose
// Failures counter is > 0 but whose Total here is 0 is a legacy session imported
// before structured records existed, and the UI keeps the log-only view for it.
func (s *HistoryService) ListSessionFailures(ctx context.Context, sessionID string, page, pageSize int) (PageResult[ImportFailureDTO], error) {
	if err := s.guard(); err != nil {
		return PageResult[ImportFailureDTO]{}, err
	}
	if s.failures == nil {
		return PageResult[ImportFailureDTO]{Items: []ImportFailureDTO{}, Page: page, PageSize: pageSize}, nil
	}
	limit, offset := normalizePage(page, pageSize)
	rows, total, err := s.failures.ListForSession(ctx, sessionID, repo.Page{Limit: limit, Offset: offset})
	if err != nil {
		return PageResult[ImportFailureDTO]{}, err
	}
	items := make([]ImportFailureDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, toImportFailureDTO(r))
	}
	return PageResult[ImportFailureDTO]{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// DismissFailure resolves a failure by marking it dismissed (with an optional
// reason) and stamping ResolvedAt. It is a soft state change — the row is never
// hard-deleted — used for the "file vanished before import" cases that can never
// be retried. It refuses (ErrFailureAlreadyResolved) a record that is not open.
func (s *HistoryService) DismissFailure(ctx context.Context, failureID, reason string) (ImportFailureDTO, error) {
	if err := s.guard(); err != nil {
		return ImportFailureDTO{}, err
	}
	if s.failures == nil {
		return ImportFailureDTO{}, ErrNoLibrary
	}
	f, err := s.failures.GetByID(ctx, failureID)
	if err != nil {
		return ImportFailureDTO{}, err
	}
	if f.Status != domain.ImportFailureStatusOpen {
		return ImportFailureDTO{}, ErrFailureAlreadyResolved
	}
	if err := s.failures.Dismiss(ctx, failureID, reason, timeNow()); err != nil {
		return ImportFailureDTO{}, err
	}
	s.log.Info("import failure dismissed", "failureId", failureID, "sessionId", f.SessionID, "path", f.Path)
	updated, err := s.failures.GetByID(ctx, failureID)
	if err != nil {
		return ImportFailureDTO{}, err
	}
	return toImportFailureDTO(*updated), nil
}
