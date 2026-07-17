// Package logging provides a slog.Handler that tees every record to a console
// text handler and to asynchronous, batched inserts into the LogEntry table via
// repo.LogRepo. Callers are never blocked on database I/O: records are handed to
// a buffered channel and a single background goroutine flushes them in batches
// (every 250ms or every 100 entries). On Close the remaining buffered records are
// flushed.
//
// The mandatory "subsystem" attribute convention identifies which part of PAIM
// produced a record. Use For to obtain a logger already tagged with a subsystem.
package logging

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
)

const (
	// subsystemKey is the attribute key that carries the subsystem name.
	subsystemKey = "subsystem"

	// bufferSize is the capacity of the record channel. It is large enough that
	// callers effectively never block under normal load; if it ever fills, the
	// caller blocks (backpressure) rather than dropping records — dropping is not
	// permitted.
	bufferSize = 10000

	// flushInterval and flushBatch bound how long a record waits and how many
	// accumulate before a database write.
	flushInterval = 250 * time.Millisecond
	flushBatch    = 100
)

// sink holds the shared, handler-instance-wide state: the destination repo, the
// record channel, and the background flush goroutine's lifecycle. It is shared by
// all Handler values derived via WithAttrs/WithGroup.
type sink struct {
	repo    *repo.LogRepo
	level   slog.Level
	entryCh chan domain.LogEntry
	done    chan struct{}
	wg      sync.WaitGroup
	closeOnce sync.Once
}

// Handler is an slog.Handler that tees to the console and to the LogEntry table.
type Handler struct {
	sink    *sink
	console slog.Handler
	attrs   []slog.Attr
	groups  []string
}

var _ slog.Handler = (*Handler)(nil)

// New constructs a Handler that writes at or above level. It returns the handler
// and a close function that flushes and stops the background goroutine. The close
// function is idempotent and should be called during application shutdown (e.g.
// via defer).
func New(logRepo *repo.LogRepo, level slog.Level) (*Handler, func()) {
	s := &sink{
		repo:    logRepo,
		level:   level,
		entryCh: make(chan domain.LogEntry, bufferSize),
		done:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()

	console := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	h := &Handler{sink: s, console: console}

	closeFn := func() {
		s.closeOnce.Do(func() {
			close(s.done)
			s.wg.Wait()
		})
	}
	return h, closeFn
}

// run is the background flush loop. It batches records and writes them via the
// repo, flushing on a full batch, on the ticker, and (draining fully) when done
// is closed. Backpressure from a slow database is absorbed here, not by callers.
func (s *sink) run() {
	defer s.wg.Done()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]domain.LogEntry, 0, flushBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Best-effort: a logging failure must not crash the app, and there is no
		// higher place to report it than the console handler already used.
		_ = s.repo.BatchInsert(context.Background(), batch)
		batch = batch[:0]
	}

	for {
		select {
		case e := <-s.entryCh:
			batch = append(batch, e)
			if len(batch) >= flushBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			// Drain everything currently buffered, then flush and exit. The
			// channel is never closed, so concurrent Handle calls that race with
			// shutdown safely take their own done branch instead of panicking.
			for {
				select {
				case e := <-s.entryCh:
					batch = append(batch, e)
					if len(batch) >= flushBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// Enabled reports whether records at the given level should be handled.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.sink.level
}

// Handle writes the record to the console immediately and enqueues a LogEntry for
// asynchronous persistence. It does not block on the database.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	// Console output is synchronous and authoritative even if the DB is down.
	consoleErr := h.console.Handle(ctx, r)

	entry := h.buildEntry(r)
	select {
	case h.sink.entryCh <- entry:
	case <-h.sink.done:
		// Handler is closing; skip the async DB write. Console output already
		// happened above.
	}

	return consoleErr
}

// buildEntry converts a slog.Record (plus this handler's accumulated attrs) into
// a domain.LogEntry, extracting the mandatory subsystem attribute and folding any
// remaining attributes into MetadataJSON.
func (h *Handler) buildEntry(r slog.Record) domain.LogEntry {
	metadata := map[string]any{}
	subsystem := ""

	collect := func(a slog.Attr) {
		if a.Key == subsystemKey {
			subsystem = a.Value.Resolve().String()
			return
		}
		metadata[h.qualify(a.Key)] = a.Value.Resolve().Any()
	}

	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		collect(a)
		return true
	})

	entry := domain.LogEntry{
		Timestamp: r.Time,
		Level:     r.Level.String(),
		Subsystem: subsystem,
		Message:   r.Message,
	}
	if len(metadata) > 0 {
		if raw, err := json.Marshal(metadata); err == nil {
			entry.MetadataJSON = string(raw)
		}
	}
	return entry
}

// qualify prefixes an attribute key with any active group names so nested groups
// do not collide in MetadataJSON.
func (h *Handler) qualify(key string) string {
	if len(h.groups) == 0 {
		return key
	}
	out := ""
	for _, g := range h.groups {
		out += g + "."
	}
	return out + key
}

// WithAttrs returns a new Handler with the given attributes added. The console
// handler and the shared sink are preserved.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	newAttrs := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	newAttrs = append(newAttrs, h.attrs...)
	newAttrs = append(newAttrs, attrs...)
	return &Handler{
		sink:    h.sink,
		console: h.console.WithAttrs(attrs),
		attrs:   newAttrs,
		groups:  h.groups,
	}
}

// WithGroup returns a new Handler that qualifies subsequent attributes under
// name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	newGroups := make([]string, 0, len(h.groups)+1)
	newGroups = append(newGroups, h.groups...)
	newGroups = append(newGroups, name)
	return &Handler{
		sink:    h.sink,
		console: h.console.WithGroup(name),
		attrs:   h.attrs,
		groups:  newGroups,
	}
}

// For returns a logger tagged with the given subsystem, derived from the default
// slog logger. Install the PAIM handler as the default (slog.SetDefault) at
// startup so these loggers persist to the LogEntry table.
func For(subsystem string) *slog.Logger {
	return slog.Default().With(slog.String(subsystemKey, subsystem))
}
