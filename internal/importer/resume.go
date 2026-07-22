package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// ResumeSession re-runs an interrupted, cancelled, or crashed session. It
// reloads the original options from the session notes, deletes any stray
// ".paim-partial-*" files left under the destination root (they were never
// recorded as assets), re-scans the source root, and re-imports — skipping files
// whose content already maps to a verified asset (so nothing is duplicated). A
// crash mid-rename during adopt+reorganize is recoverable because rename is
// atomic: the file is at exactly one path and the re-scan finds and reconciles
// it.
func (p *Pipeline) ResumeSession(ctx context.Context, sessionID string, progressFn ProgressFunc) (*domain.ImportSession, error) {
	return p.resumeSession(ctx, sessionID, nil, progressFn)
}

// ResumeSessionPrecomputed is ResumeSession with a dry-run report threaded in so
// the import reuses its quick/full hashes (staleness-gated) instead of
// re-hashing every file. It is the path StartImport drives after an Analyze; the
// plain ResumeSession (nil report) remains the correct crash-resume path where
// re-hashing from scratch is intended. A nil report here behaves exactly like
// ResumeSession.
func (p *Pipeline) ResumeSessionPrecomputed(ctx context.Context, sessionID string, precomputed *DryRunReport, progressFn ProgressFunc) (*domain.ImportSession, error) {
	return p.resumeSession(ctx, sessionID, precomputed, progressFn)
}

func (p *Pipeline) resumeSession(ctx context.Context, sessionID string, precomputed *DryRunReport, progressFn ProgressFunc) (*domain.ImportSession, error) {
	session, err := p.sessions.GetByID(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("resume: load session %q: %w", sessionID, err)
	}

	var state sessionState
	if session.Notes != "" {
		if err := json.Unmarshal([]byte(session.Notes), &state); err != nil {
			return nil, fmt.Errorf("resume: session %q has no resumable state: %w", sessionID, err)
		}
	}
	if state.SourceRoot == "" {
		return nil, fmt.Errorf("resume: session %q has no recorded source root", sessionID)
	}
	opts := state.options()
	if opts.DestinationRoot == "" {
		opts.DestinationRoot = session.DestinationRoot
	}
	// Runtime-only hash handoff: never persisted in Notes, so a genuine crash
	// resume (which reloads from Notes with no precomputed) correctly re-hashes.
	opts.Precomputed = precomputed

	p.log.Info("resuming import session", "sessionId", sessionID, "mode", opts.mode(), "source", opts.SourceRoot)

	// Remove stray partials from a prior crash before doing anything else. This
	// walks the whole destination archive, which on a large library is a
	// multi-second step with no per-item progress — surface it as an
	// indeterminate "preparing" state so the UI never looks hung before scanning
	// begins. (StartImport also runs through ResumeSession, so a fresh import
	// hits this too.)
	if root := opts.DestinationRoot; root != "" {
		progressFn.emit(Progress{Phase: PhasePreparing})
		removed, err := p.cleanStrayPartials(ctx, root, progressFn)
		if err != nil {
			p.log.Warn("resume: cleaning stray partials", "error", err.Error())
		} else if removed > 0 {
			p.log.Info("resume: removed stray partial files", "count", removed)
		}
	}

	if err := p.sessions.SetStatus(ctx, sessionID, domain.SessionStatusRunning); err != nil {
		return nil, fmt.Errorf("resume: set running %q: %w", sessionID, err)
	}

	scan, err := p.Scan(ctx, opts.SourceRoot, progressFn)
	if err != nil {
		_ = p.sessions.SetStatus(context.Background(), sessionID, domain.SessionStatusInterrupted)
		return p.reload(sessionID), fmt.Errorf("resume: scan: %w", err)
	}

	// Counters are preserved across resume; already-imported files are skipped by
	// the classifier, so we do not re-add the scanned count.
	return p.runImport(ctx, session, scan, opts, &state, progressFn)
}

// strayPartialProgressInterval is how often (in files checked) the stray-partial
// sweep reports a "checked N files" progress update, so a multi-second walk over
// a large archive never looks hung before scanning begins.
const strayPartialProgressInterval = 512

// cleanStrayPartials deletes every ".paim-partial-*" file under root, logging
// each removal, and returns the count removed. It emits a periodic checked-file
// count into the existing PhasePreparing progress (progressFn may be nil) so the
// UI shows forward motion during the walk.
func (p *Pipeline) cleanStrayPartials(ctx context.Context, root string, progressFn ProgressFunc) (int, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("clean partials: stat root %q: %w", root, err)
	}
	removed := 0
	checked := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		checked++
		if checked%strayPartialProgressInterval == 0 {
			progressFn.emit(Progress{Phase: PhasePreparing, FilesDone: checked, CurrentFile: path})
		}
		if strings.HasPrefix(d.Name(), partialPrefix) {
			if rmErr := os.Remove(path); rmErr != nil {
				p.log.Warn("clean partials: remove failed", "path", path, "error", rmErr.Error())
				return nil
			}
			p.log.Info("clean partials: removed stray partial", "path", path)
			removed++
		}
		return nil
	})
	if walkErr != nil {
		return removed, fmt.Errorf("clean partials: walk %q: %w", root, walkErr)
	}
	return removed, nil
}
