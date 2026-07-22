package services

import (
	"log/slog"
	"os/exec"
	"sync"
)

// SleepGuard holds a macOS power-management assertion for the duration of any
// long-running PAIM operation (import, analyze, reorganize, safe-to-erase,
// cleanup analyze). While such work is in flight we run `/usr/bin/caffeinate -im`
// as a child process, which keeps the system (and, with -m, the disk) awake.
//
// WHY: if the Mac sleeps mid-import the filesystem I/O the pipeline depends on is
// paused — copies stall, hashing halts, and a multi-hour import of a large card
// can silently wedge until the machine is woken. Holding the assertion for the
// life of the operation keeps long jobs progressing on an unattended machine.
//
// It is refcounted so several concurrent operations share one assertion: the
// first Acquire starts the process, the last Release kills it. Shutdown force-
// kills it regardless of the count so no caffeinate child outlives the app.
//
// The command starter is injected so tests can assert the refcount transitions
// without spawning a real process.
type SleepGuard struct {
	mu    sync.Mutex
	count int
	proc  sleepProc
	start func() (sleepProc, error)
	log   *slog.Logger
}

// sleepProc is the minimal handle SleepGuard needs over the started process,
// satisfied by the real *exec.Cmd wrapper and by test fakes.
type sleepProc interface {
	Kill() error
	Wait() error
}

// sleepAware is embedded by every service that runs a long operation, sharing
// the SetSleepGuard plumbing (mirroring gated's SetGate). A nil guard is a no-op,
// so unit-test-constructed services never spawn caffeinate.
type sleepAware struct {
	sleep *SleepGuard
}

// SetSleepGuard injects the shared sleep guard. Called once by main.go after
// construction; left unset (no-op) in unit tests.
func (s *sleepAware) SetSleepGuard(g *SleepGuard) { s.sleep = g }

// NewSleepGuard constructs a SleepGuard that runs the real caffeinate binary.
// A nil logger falls back to slog.Default(). A nil *SleepGuard is a valid no-op
// (Acquire/Release/Shutdown all tolerate it), so services left without one in
// unit tests never spawn a process.
func NewSleepGuard(logger *slog.Logger) *SleepGuard {
	if logger == nil {
		logger = slog.Default()
	}
	g := &SleepGuard{log: logger.With(slog.String("subsystem", "power"))}
	g.start = startCaffeinate
	return g
}

// Acquire raises the reference count and, on the first holder, starts the sleep
// assertion. It never blocks the caller and never fails hard: if caffeinate
// cannot start (e.g. missing on a non-macOS dev box) the operation still runs,
// only without the assertion, and the failure is logged.
func (g *SleepGuard) Acquire() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.count++
	if g.count == 1 {
		proc, err := g.start()
		if err != nil {
			g.log.Warn("could not start sleep assertion; long operation may pause if the Mac sleeps", "error", err.Error())
			return
		}
		g.proc = proc
		g.log.Info("sleep assertion held (long operation active)")
	}
}

// Release lowers the reference count and, when it reaches zero, stops the sleep
// assertion. It is safe to call more times than Acquire (extra calls are no-ops).
func (g *SleepGuard) Release() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.count == 0 {
		return
	}
	g.count--
	if g.count == 0 {
		g.stopLocked("sleep assertion released (no long operation active)")
	}
}

// Shutdown force-stops the assertion regardless of the reference count, so no
// caffeinate child process outlives the app. Called from the composition root's
// shutdown hook.
func (g *SleepGuard) Shutdown() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.count = 0
	g.stopLocked("sleep assertion released (app shutting down)")
}

// stopLocked kills the running assertion process, if any. The caller holds mu.
func (g *SleepGuard) stopLocked(msg string) {
	if g.proc == nil {
		return
	}
	if err := g.proc.Kill(); err != nil {
		g.log.Warn("could not stop sleep assertion", "error", err.Error())
	}
	// Reap the process so it does not linger as a zombie.
	_ = g.proc.Wait()
	g.proc = nil
	g.log.Info(msg)
}

// cmdProc adapts *exec.Cmd to sleepProc.
type cmdProc struct{ cmd *exec.Cmd }

func (p *cmdProc) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *cmdProc) Wait() error { return p.cmd.Wait() }

// startCaffeinate launches `/usr/bin/caffeinate -im`: -i prevents idle system
// sleep, -m prevents the disk from sleeping. It returns immediately; the process
// runs until Kill.
func startCaffeinate() (sleepProc, error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-im")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdProc{cmd: cmd}, nil
}
