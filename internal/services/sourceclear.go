package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Clear-source typed errors. The frontend distinguishes them to render the right
// guidance ("evaluate first" vs "already running" vs "cannot clear the archive").
var (
	// ErrClearGateNotMet means there is no fresh green safe-to-erase evaluation for
	// exactly this source root: the user must (re)evaluate before clearing.
	ErrClearGateNotMet = errors.New("services: clear source: no fresh green safe-to-erase evaluation for this source — evaluate first")
	// ErrClearInProgress means a clear is already running.
	ErrClearInProgress = errors.New("services: clear source: a clear operation is already running")
	// ErrClearInsideLibrary means the requested root is the open library root (or
	// overlaps it); PAIM never "clears" the archive itself.
	ErrClearInsideLibrary = errors.New("services: clear source: refusing to clear the open library root")
)

// clearSourceSubsystem is the logging subsystem for per-file clear logging.
const clearSourceSubsystem = "source-clear"

// clearSourceRun is the in-memory state of a single background clear-source job,
// mirroring safeEraseRun: running while the goroutine is live, then the terminal
// result/cancelled/err with a completion time for the retention TTL.
type clearSourceRun struct {
	root      string
	progress  *SourceProgress
	result    *ClearSourceResultDTO
	err       string
	cancelled bool
	running   bool
	at        time.Time
}

// ClearSourceResultDTO summarizes a completed clear: how many evaluated-safe
// files were moved to the timestamped trash directory, how many unsafe files were
// deliberately left untouched, and how many moves errored. TrashDir is the exact
// destination so the UI can tell the user where the files went.
type ClearSourceResultDTO struct {
	Root          string `json:"root"`
	TrashDir      string `json:"trashDir"`
	Moved         int    `json:"moved"`
	SkippedUnsafe int    `json:"skippedUnsafe"`
	Errors        int    `json:"errors"`
	Cancelled     bool   `json:"cancelled"`
}

// StartClearSourceResult is returned immediately once the clear job launches.
type StartClearSourceResult struct {
	Root     string `json:"root"`
	TrashDir string `json:"trashDir"`
	Files    int    `json:"files"`
}

// ClearSourcePreviewDTO is what the confirmation dialog needs before a clear:
// the number of evaluated-safe files, their total size (best-effort stat), and
// the trash destination parent. It is produced only when the gate is satisfied;
// otherwise the preview call returns a typed gate error.
type ClearSourcePreviewDTO struct {
	Root       string `json:"root"`
	FileCount  int    `json:"fileCount"`
	TotalBytes int64  `json:"totalBytes"`
	TrashDir   string `json:"trashDir"`
}

// ActiveClearSourceDTO is the re-attachment snapshot for a clear job: "running"
// (Progress holds the latest snapshot), "completed" (Result, or Cancelled/Error),
// or "none". A completed snapshot lapses to "none" after safeEraseReportTTL.
type ActiveClearSourceDTO struct {
	State     string                `json:"state"`
	Root      string                `json:"root"`
	Progress  *SourceProgress       `json:"progress"`
	Result    *ClearSourceResultDTO `json:"result"`
	Cancelled bool                  `json:"cancelled"`
	Error     string                `json:"error"`
}

// validateClearGate re-validates, server-side, that clearing root is allowed and
// returns the evaluated-safe file list to move. It never trusts frontend state:
// the gate is the service's own cached safe-to-erase report — GREEN, for exactly
// this root, fresh within safeEraseReportTTL. It also refuses when root overlaps
// the open library root. Callers hold s.mu.
func (s *SourcesService) validateClearGate(root string) ([]string, *SafeToEraseDTO, error) {
	if root == "" {
		return nil, nil, fmt.Errorf("services: clear source: empty root")
	}
	if s.root != "" && pathsOverlap(root, s.root) {
		return nil, nil, ErrClearInsideLibrary
	}
	r := s.run
	if r == nil || r.running || r.cancelled || r.err != "" || r.report == nil {
		return nil, nil, ErrClearGateNotMet
	}
	if !r.report.Safe {
		return nil, nil, ErrClearGateNotMet
	}
	if !samePath(r.mountPoint, root) {
		return nil, nil, ErrClearGateNotMet
	}
	if time.Since(r.at) > safeEraseReportTTL {
		return nil, nil, ErrClearGateNotMet
	}
	return r.safeFiles, r.report, nil
}

// ClearSourcePreview validates the clear gate for root and returns the confirm-
// dialog inputs (file count, total bytes, trash destination). It returns a typed
// gate error when the evaluation is missing, stale, red, for a different root, or
// when root overlaps the library.
func (s *SourcesService) ClearSourcePreview(ctx context.Context, root string) (ClearSourcePreviewDTO, error) {
	if err := s.guard(); err != nil {
		return ClearSourcePreviewDTO{}, err
	}
	root = normalizePath(root)

	s.mu.Lock()
	safeFiles, _, err := s.validateClearGate(root)
	files := append([]string(nil), safeFiles...)
	s.mu.Unlock()
	if err != nil {
		return ClearSourcePreviewDTO{}, err
	}

	var total int64
	for _, p := range files {
		if info, serr := os.Stat(p); serr == nil {
			total += info.Size()
		}
	}
	return ClearSourcePreviewDTO{
		Root:       root,
		FileCount:  len(files),
		TotalBytes: total,
		TrashDir:   filepath.Join(root, trashDirName),
	}, nil
}

// StartClearSource launches a background job that moves every evaluated-safe media
// file under root into <root>/.paim-trash/<yyyymmdd-hhmmss>/, preserving each
// file's path relative to root (so identically-named files across subfolders never
// collide). It is gated: a fresh GREEN safe-to-erase evaluation for exactly this
// root must exist (validateClearGate), else a typed error the UI renders as
// "evaluate first". PAIM never hard-deletes — the user formats the card or empties
// the trash folder afterward. Files that are not evaluated-safe are never touched.
//
// The job is cancellable between files: cancelling stops cleanly before the next
// move; files already moved stay moved (they are provably safe — that is the whole
// point). It is activity-tracked (quit guard) and sleep-guarded.
func (s *SourcesService) StartClearSource(ctx context.Context, root string) (StartClearSourceResult, error) {
	if err := s.guard(); err != nil {
		return StartClearSourceResult{}, err
	}
	root = normalizePath(root)

	s.mu.Lock()
	if s.clearing {
		s.mu.Unlock()
		return StartClearSourceResult{}, ErrClearInProgress
	}
	safeFiles, report, err := s.validateClearGate(root)
	if err != nil {
		s.mu.Unlock()
		return StartClearSourceResult{}, err
	}
	files := append([]string(nil), safeFiles...)
	skippedUnsafe := report.New + report.Unverified + report.BackupIncomplete
	stamp := timeNow().Format("20060102-150405")
	trashDir := filepath.Join(root, trashDirName, stamp)

	s.clearing = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.clearCancel = cancel
	s.clearRun = &clearSourceRun{root: root, running: true, at: timeNow()}
	s.mu.Unlock()

	s.log.Info("source clear starting",
		slog.String("subsystem", clearSourceSubsystem),
		"root", root, "trashDir", trashDir, "files", len(files))

	s.sleep.Acquire()
	go s.runClearSource(runCtx, root, trashDir, files, skippedUnsafe)
	return StartClearSourceResult{Root: root, TrashDir: trashDir, Files: len(files)}, nil
}

// runClearSource performs the moves, emitting throttled source:progress (kind
// "clear") and a terminal source:cleared. Already-moved files remain moved on
// cancellation.
func (s *SourcesService) runClearSource(ctx context.Context, root, trashDir string, files []string, skippedUnsafe int) {
	defer func() {
		s.mu.Lock()
		s.clearing = false
		s.clearCancel = nil
		s.mu.Unlock()
		s.sleep.Release()
	}()

	clog := s.log.With(slog.String("subsystem", clearSourceSubsystem))
	tr := newThrottle()
	total := len(files)

	emitProgress := func(done int, current string) {
		p := SourceProgress{Kind: "clear", MountPoint: root, FilesDone: done, FilesTotal: total, CurrentFile: current}
		s.mu.Lock()
		if s.clearRun != nil {
			s.clearRun.progress = &p
		}
		s.mu.Unlock()
		if done == total || tr.allow() {
			emitSafe(s.emitter, EventSourceProgress, p)
		}
	}
	emitProgress(0, "")

	moved, errCount := 0, 0
	cancelled := false
	for i, path := range files {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if err := moveIntoTrash(root, trashDir, path); err != nil {
			errCount++
			clog.Warn("clear: could not move file to trash", "file", path, "error", err.Error())
		} else {
			moved++
			clog.Info("clear: moved file to trash", "file", path)
		}
		emitProgress(i+1, path)
	}
	if ctx.Err() != nil {
		cancelled = true
	}

	result := &ClearSourceResultDTO{
		Root:          root,
		TrashDir:      trashDir,
		Moved:         moved,
		SkippedUnsafe: skippedUnsafe,
		Errors:        errCount,
		Cancelled:     cancelled,
	}
	s.mu.Lock()
	if s.clearRun != nil {
		s.clearRun.running = false
		s.clearRun.result = result
		s.clearRun.cancelled = cancelled
		s.clearRun.at = timeNow()
	}
	s.mu.Unlock()

	clog.Info("source clear finished",
		"root", root, "moved", moved, "skippedUnsafe", skippedUnsafe, "errors", errCount, "cancelled", cancelled)
	emitSafe(s.emitter, EventSourceCleared, SourceCleared{
		Root:          root,
		TrashDir:      trashDir,
		Moved:         moved,
		SkippedUnsafe: skippedUnsafe,
		Errors:        errCount,
		Cancelled:     cancelled,
	})
}

// moveIntoTrash moves path into trashDir preserving its path relative to root, so
// files sharing a basename across different subfolders never collide. The move is
// a same-volume atomic rename (source and trash share the volume by construction).
func moveIntoTrash(root, trashDir, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		// Fall back to the basename rather than escaping the trash tree.
		rel = filepath.Base(path)
	}
	dst := filepath.Join(trashDir, rel)
	if err := ensureDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return renameFile(path, dst)
}

// ActiveClearSource returns the current clear job's state for re-attachment.
func (s *SourcesService) ActiveClearSource(ctx context.Context) (ActiveClearSourceDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.clearRun
	if r == nil {
		return ActiveClearSourceDTO{State: "none"}, nil
	}
	if r.running {
		dto := ActiveClearSourceDTO{State: "running", Root: r.root}
		if r.progress != nil {
			p := *r.progress
			dto.Progress = &p
		}
		return dto, nil
	}
	if time.Since(r.at) > safeEraseReportTTL {
		s.clearRun = nil
		return ActiveClearSourceDTO{State: "none"}, nil
	}
	return ActiveClearSourceDTO{
		State:     "completed",
		Root:      r.root,
		Result:    r.result,
		Cancelled: r.cancelled,
		Error:     r.err,
	}, nil
}

// CancelClearSource cancels a running clear (if any). Files already moved stay
// moved; the job stops cleanly before the next file.
func (s *SourcesService) CancelClearSource(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.clearCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// normalizePath returns a cleaned absolute form of p for stable comparison.
func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// samePath reports whether two paths refer to the same location after
// normalization.
func samePath(a, b string) bool {
	return normalizePath(a) == normalizePath(b)
}

// pathsOverlap reports whether a and b are the same directory or one contains the
// other, used to refuse clearing a root that is (or holds, or sits inside) the
// library.
func pathsOverlap(a, b string) bool {
	a, b = normalizePath(a), normalizePath(b)
	if a == b {
		return true
	}
	return isWithin(a, b) || isWithin(b, a)
}

// isWithin reports whether child is inside parent (a strict descendant).
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." && !filepath.IsAbs(rel)
}
