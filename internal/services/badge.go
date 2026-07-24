package services

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// BadgeSetter is the minimal macOS dock-badge surface the BadgeController drives.
// It is satisfied directly by Wails v3's *dock.DockService (SetBadge/RemoveBadge),
// wired in main.go — so the services layer never imports the Wails packages. A nil
// setter (or one whose calls error because the dock icon is unavailable) degrades
// to a silent no-op.
type BadgeSetter interface {
	SetBadge(label string) error
	RemoveBadge() error
}

// badgeableKinds are the long-operation kinds whose overall progress is worth
// surfacing as a dock badge percentage. These are the file-count-based operations
// the user watches run for minutes: import/adopt, analyze, reorganize, the backup
// backfill (queueing missing jobs), and the thumbnail warm-up. Per-file backup
// UPLOADS (OpKindBackupUpload) are deliberately excluded — an individual upload's
// byte percentage says nothing about the queue as a whole, whose progress the
// Backup Queue ETA already covers; likewise the byte-based source operations are
// left out to keep the policy a simple files-done/files-total percentage.
var badgeableKinds = map[string]bool{
	OpKindImport:          true,
	OpKindAnalyze:         true,
	OpKindReorganize:      true,
	OpKindBackupBackfill:  true,
	OpKindThumbnailWarmup: true,
}

// BadgeLabel computes the dock badge label for the currently-running operations,
// returning "" to clear the badge. Policy (kept deliberately simple and
// documented): among the badgeable file-based operations with a known total, the
// PRIMARY one is the job with the most total files — the biggest piece of work —
// and its completion percentage (FilesDone/FilesTotal, clamped to 0..100) is shown
// as "NN%". Indeterminate operations (FilesTotal == 0) and non-badgeable kinds are
// ignored; when none qualify the badge is cleared.
func BadgeLabel(ops []OperationInfo) string {
	primaryTotal := 0
	pct := -1
	for _, op := range ops {
		if !badgeableKinds[op.Kind] || op.FilesTotal <= 0 {
			continue
		}
		if op.FilesTotal > primaryTotal {
			primaryTotal = op.FilesTotal
			p := op.FilesDone * 100 / op.FilesTotal
			if p < 0 {
				p = 0
			}
			if p > 100 {
				p = 100
			}
			pct = p
		}
	}
	if pct < 0 {
		return ""
	}
	return fmt.Sprintf("%d%%", pct)
}

// badgePollInterval is how often the controller samples the activity tracker and,
// at most, updates the dock badge — satisfying the "throttle updates (<=1/sec)"
// requirement. Sampling on its own goroutine means dock updates never block a
// service's progress-reporting path.
const badgePollInterval = time.Second

// BadgeController periodically observes the ActivityTracker and reflects the
// primary running operation's progress as a macOS dock badge (e.g. "42%"),
// clearing it when nothing badgeable runs. Updates are throttled to the poll
// interval and de-duplicated (an unchanged label is not re-pushed), and a failing
// setter is logged exactly once then treated as a no-op — the badge is pure UI
// polish and must never crash or stall real work.
type BadgeController struct {
	tracker  *ActivityTracker
	setter   BadgeSetter
	log      *slog.Logger
	interval time.Duration

	mu          sync.Mutex
	applied     bool   // whether any label has been pushed yet
	lastApplied string // last label pushed ("" == cleared)
	loggedFail  bool   // whether a setter failure has already been logged
}

// NewBadgeController constructs a controller over the tracker and setter. A nil
// setter is tolerated (the controller becomes an inert no-op), which keeps tests
// and non-macOS builds simple.
func NewBadgeController(tracker *ActivityTracker, setter BadgeSetter, logger *slog.Logger) *BadgeController {
	if logger == nil {
		logger = slog.Default()
	}
	return &BadgeController{
		tracker:  tracker,
		setter:   setter,
		log:      logger.With(slog.String("subsystem", "dock")),
		interval: badgePollInterval,
	}
}

// Run samples the tracker every interval and applies the derived badge label until
// ctx is cancelled, clearing the badge on exit. It blocks, so callers launch it on
// a goroutine.
func (c *BadgeController) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.apply("") // clear on shutdown
			return
		case <-t.C:
			c.tick()
		}
	}
}

// tick samples the tracker once and applies the resulting label. It is separated
// from Run so tests can drive a single update deterministically.
func (c *BadgeController) tick() {
	var ops []OperationInfo
	if c.tracker != nil {
		ops = c.tracker.Snapshot()
	}
	c.apply(BadgeLabel(ops))
}

// apply pushes label to the setter, skipping the call when it matches the last
// applied label (de-dup). An empty label clears the badge via RemoveBadge (Wails's
// SetBadge("") would show a dot rather than clearing). Setter errors are swallowed
// after a single log line.
func (c *BadgeController) apply(label string) {
	c.mu.Lock()
	if c.applied && label == c.lastApplied {
		c.mu.Unlock()
		return
	}
	c.applied = true
	c.lastApplied = label
	c.mu.Unlock()

	if c.setter == nil {
		return
	}
	var err error
	if label == "" {
		err = c.setter.RemoveBadge()
	} else {
		err = c.setter.SetBadge(label)
	}
	if err != nil {
		c.mu.Lock()
		first := !c.loggedFail
		c.loggedFail = true
		c.mu.Unlock()
		if first {
			c.log.Debug("dock badge unavailable; badges disabled", "error", err.Error())
		}
	}
}
