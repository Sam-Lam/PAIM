package services

import (
	"context"
	"time"
)

// quitGracePeriod bounds how long ConfirmQuit waits, after cancelling every
// active operation, for the current per-file transactions to settle before it
// quits anyway. Safety does not depend on it (quit is a controlled crash the
// restart-safe architecture recovers from) — it only lets an in-flight file
// finish cleanly when it can within a few seconds.
const quitGracePeriod = 5 * time.Second

// quitPollInterval is how often ConfirmQuit re-checks whether the active
// operations have drained during the grace period.
const quitPollInterval = 100 * time.Millisecond

// AppService is the Wails-bound application-lifecycle service backing the quit
// guard. ActiveOperations exposes the running long operations (for the guard
// dialog and future UI); ConfirmQuit cancels them, waits a bounded grace period,
// then quits through the normal graceful-shutdown path.
//
// The quit function and clock are injected so ConfirmQuit is unit-testable
// without a running Wails app or real time: main.go assigns a quit closure that
// flags the interception hook and calls app.Quit; tests assign a fake. Quit is an
// exported field (not a setter method) deliberately — the Wails binding generator
// binds exported methods but not fields, so this stays off the frontend API.
type AppService struct {
	tracker *ActivityTracker

	// Quit is assigned by main.go after the app is constructed (it needs the app).
	// It sets the ShouldQuit interception hook's confirmed flag and triggers the
	// real app quit, so the re-entrant quit passes straight through.
	Quit func()

	grace time.Duration
	poll  time.Duration
	now   func() time.Time
	sleep func(time.Duration)
}

// NewAppService constructs an AppService over the shared activity tracker. The
// Quit function is assigned later by main.go (it needs the constructed app).
func NewAppService(tracker *ActivityTracker) *AppService {
	return &AppService{
		tracker: tracker,
		grace:   quitGracePeriod,
		poll:    quitPollInterval,
		now:     time.Now,
		sleep:   time.Sleep,
	}
}

// ActiveOperations returns the long operations currently running across the app.
// It is bound to the frontend so the quit dialog (and future activity UI) can
// read live progress on demand, independent of the app:quit-requested event.
func (s *AppService) ActiveOperations(ctx context.Context) ([]OperationInfo, error) {
	if s.tracker == nil {
		return nil, nil
	}
	return s.tracker.Snapshot(), nil
}

// ConfirmQuit is invoked by the guard dialog's "Quit anyway" action. It cancels
// every active operation via its existing cancel path, waits up to the grace
// period for the operations to drain (so the current per-file transaction can
// settle), and then quits. It quits even if the grace period elapses with work
// still in flight — that is a safe controlled crash the next launch recovers
// from. It returns nil once the quit has been triggered.
func (s *AppService) ConfirmQuit(ctx context.Context) error {
	if s.tracker != nil {
		s.tracker.CancelAll()
		deadline := s.now().Add(s.grace)
		for len(s.tracker.Snapshot()) > 0 && s.now().Before(deadline) {
			s.sleep(s.poll)
		}
	}
	if s.Quit != nil {
		s.Quit()
	}
	return nil
}
