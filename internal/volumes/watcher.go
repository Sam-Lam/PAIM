package volumes

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher reports mount and unmount events for volumes appearing under a
// watched directory (default /Volumes). It unions two signals — fsnotify
// filesystem events and a periodic poll — and reconciles them against the set
// of currently-present mounts, so an event is emitted only on a real change.
// This design deduplicates the multiple filesystem events a single mount emits
// and covers the case where fsnotify misses an event.
type Watcher struct {
	dir          string
	debounce     time.Duration
	pollInterval time.Duration
	log          *slog.Logger
}

// WatcherOption configures a Watcher.
type WatcherOption func(*Watcher)

// WithWatchDir sets the directory watched for mounts (default /Volumes). Making
// this configurable lets tests stand a temp dir in for /Volumes.
func WithWatchDir(dir string) WatcherOption { return func(w *Watcher) { w.dir = dir } }

// WithDebounce sets how long fsnotify activity is coalesced before reconciling
// (default 400ms). Mounts commonly emit several filesystem events in a burst.
func WithDebounce(d time.Duration) WatcherOption { return func(w *Watcher) { w.debounce = d } }

// WithPollInterval sets the fallback poll cadence (default 10s).
func WithPollInterval(d time.Duration) WatcherOption {
	return func(w *Watcher) { w.pollInterval = d }
}

// NewWatcher constructs a Watcher. A nil logger falls back to the default slog
// logger tagged with the volumes subsystem.
func NewWatcher(log *slog.Logger, opts ...WatcherOption) *Watcher {
	w := &Watcher{
		dir:          "/Volumes",
		debounce:     400 * time.Millisecond,
		pollInterval: 10 * time.Second,
		log:          log,
	}
	if w.log == nil {
		w.log = slog.Default().With(slog.String("subsystem", "volumes"))
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Start begins watching and returns a channel of Events. The initial set of
// present mounts is treated as the baseline (no events are emitted for volumes
// already mounted at Start). Watching stops — and the channel is closed — when
// ctx is cancelled. The returned error covers only setup failures.
func (w *Watcher) Start(ctx context.Context) (<-chan Event, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("volumes: create fsnotify watcher: %w", err)
	}
	if err := fsw.Add(w.dir); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("volumes: watch %q: %w", w.dir, err)
	}

	// Establish the baseline synchronously, before returning, so volumes already
	// mounted at Start are not reported and there is no race with a caller that
	// mounts something immediately after Start returns.
	baseline := w.snapshot()

	out := make(chan Event)
	go w.loop(ctx, fsw, out, baseline)
	return out, nil
}

// loop is the reconciliation goroutine. It owns the known-mount set and the
// output channel and is the only writer to both.
func (w *Watcher) loop(ctx context.Context, fsw *fsnotify.Watcher, out chan<- Event, known map[string]struct{}) {
	defer close(out)
	defer fsw.Close()

	poll := time.NewTicker(w.pollInterval)
	defer poll.Stop()

	// debounceTimer coalesces bursts of fsnotify events; it is nil while idle.
	var debounceTimer *time.Timer
	var debounceC <-chan time.Time
	arm := func() {
		if debounceTimer == nil {
			debounceTimer = time.NewTimer(w.debounce)
			debounceC = debounceTimer.C
			return
		}
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
		debounceTimer.Reset(w.debounce)
	}

	reconcile := func() {
		current := w.snapshot()
		for _, ev := range diffMounts(known, current) {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		known = current
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-fsw.Events:
			if !ok {
				return
			}
			arm()
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			w.log.WarnContext(ctx, "fsnotify error", slog.Any("err", err))
		case <-debounceC:
			debounceTimer = nil
			debounceC = nil
			reconcile()
		case <-poll.C:
			reconcile()
		}
	}
}

// snapshot returns the set of mount points currently present under the watched
// directory (skipping hidden/Time Machine/root entries), keyed by full path.
func (w *Watcher) snapshot() map[string]struct{} {
	set := make(map[string]struct{})
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		w.log.Warn("watcher: read dir failed", slog.String("dir", w.dir), slog.Any("err", err))
		return set
	}
	for _, e := range entries {
		name := e.Name()
		if skipVolumeEntry(name) {
			continue
		}
		mount := filepath.Join(w.dir, name)
		if resolvesToRoot(mount) {
			continue
		}
		set[mount] = struct{}{}
	}
	return set
}

// diffMounts computes the mount/unmount events between an old and new mount set,
// returned in a deterministic order (unmounts and mounts each sorted by path).
func diffMounts(old, current map[string]struct{}) []Event {
	var events []Event
	for mp := range old {
		if _, ok := current[mp]; !ok {
			events = append(events, Event{MountPoint: mp, Type: EventUnmounted})
		}
	}
	for mp := range current {
		if _, ok := old[mp]; !ok {
			events = append(events, Event{MountPoint: mp, Type: EventMounted})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Type != events[j].Type {
			return events[i].Type < events[j].Type
		}
		return events[i].MountPoint < events[j].MountPoint
	})
	return events
}
