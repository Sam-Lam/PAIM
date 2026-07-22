package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// reorgSubsystem tags reorganize log records (spec: log everything under the
// "reorganize" subsystem). It overrides the pipeline's default "import" tag.
const reorgSubsystem = "reorganize"

// MoveKind classifies a single reorganize plan entry.
type MoveKind string

const (
	// MoveInPlace: the asset already sits at its computed destination.
	MoveInPlace MoveKind = "in_place"
	// MoveMove: the asset will be moved (same-volume atomic rename) to a new path.
	MoveMove MoveKind = "move"
	// MoveSkip: the asset cannot be reorganized (see Reason) and is left untouched.
	MoveSkip MoveKind = "skip"
)

// Reorganize skip reasons (stable, machine-readable strings).
const (
	ReorgSkipMissing     = "missing-on-disk"
	ReorgSkipCrossVolume = "cross-volume"
	ReorgSkipDuplicate   = "duplicate"
)

// ReorganizeOptions configures a reorganize plan/run. EventName is the event
// folder segment applied to the standard layout (default empty → date-only
// YYYY-MM-DD folders); it lets the caller reorganize into
// YYYY/YYYY-MM-DD Event/ when desired.
type ReorganizeOptions struct {
	EventName string
}

// ReorganizeEntry is one asset's disposition in a reorganize plan. From and To
// are absolute paths (To == From for an in-place asset or a skip). QuickHash is
// carried so RunReorganize can re-verify the file at its new location without a
// second catalog read.
type ReorganizeEntry struct {
	AssetID   string
	QuickHash string
	From      string
	To        string
	Filename  string
	Kind      MoveKind
	Reason    string
}

// ReorganizePlan is the non-mutating prediction of a reorganize run: aggregate
// counts plus the full per-asset entry list that RunReorganize executes.
type ReorganizePlan struct {
	EventName   string
	TotalAssets int
	InPlace     int
	Moves       int
	Skipped     int
	Entries     []ReorganizeEntry
}

// reorgLog returns the pipeline logger retagged with the reorganize subsystem.
func (p *Pipeline) reorgLog() *slog.Logger {
	return p.log.With(slog.String("subsystem", reorgSubsystem))
}

// PlanReorganize builds a non-mutating reorganize plan straight from the catalog
// (never a filesystem scan): for every non-deleted, verified asset with an
// archive path it computes the standard-layout destination from the stored
// CaptureDate (falling back to ImportDate — EXIF and file contents are NEVER
// re-read), and classifies the asset as in-place, a move, or a skip. Collision
// resolution considers BOTH names already on disk AND names already claimed by
// earlier planned moves, so two planned moves can never target the same path.
// Nothing on disk or in the DB is modified.
func (p *Pipeline) PlanReorganize(ctx context.Context, opts ReorganizeOptions, progressFn func(done, total int)) (*ReorganizePlan, error) {
	assets, err := p.assets.ListActiveArchived(ctx)
	if err != nil {
		return nil, fmt.Errorf("reorganize: list assets: %w", err)
	}
	lay := p.effectiveLayout(p.libraryRoot)
	plan := &ReorganizePlan{EventName: opts.EventName, Entries: make([]ReorganizeEntry, 0, len(assets))}

	total := len(assets)
	if progressFn != nil {
		progressFn(0, total)
	}

	// claimed holds absolute destination paths already assigned to a planned move,
	// so inter-move collisions resolve to distinct names deterministically.
	claimed := make(map[string]bool)

	for i, a := range assets {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("reorganize: plan: %w", err)
		}
		if progressFn != nil {
			progressFn(i, total)
		}
		plan.TotalAssets++
		currentAbs := library.ResolvePath(p.libraryRoot, a.CurrentArchivePath)
		base := filepath.Base(currentAbs)
		entry := ReorganizeEntry{
			AssetID:   a.ID,
			QuickHash: a.QuickHash,
			From:      currentAbs,
			To:        currentAbs,
			Filename:  base,
		}

		// A flagged (adopted-in-place) duplicate is never moved: the canonical
		// original owns the layout slot. Report it as skipped.
		if a.DuplicateOfAssetID != nil && *a.DuplicateOfAssetID != "" {
			entry.Kind = MoveSkip
			entry.Reason = ReorgSkipDuplicate
			plan.Skipped++
			plan.Entries = append(plan.Entries, entry)
			continue
		}

		// The file must exist on disk to be moved. Missing files are reported, not
		// fatal (the Cleanup/Verify flows handle integrity separately).
		if _, err := os.Stat(currentAbs); err != nil {
			entry.Kind = MoveSkip
			entry.Reason = ReorgSkipMissing
			plan.Skipped++
			plan.Entries = append(plan.Entries, entry)
			continue
		}

		date := a.ImportDate
		if a.CaptureDate != nil {
			date = *a.CaptureDate
		}
		kind := mediatype.KindOf(a.OriginalExtension)
		rawDest := filepath.Join(lay.MasterRoot, lay.DestinationFor(date, opts.EventName, base, kind))

		if rawDest == currentAbs {
			entry.Kind = MoveInPlace
			plan.InPlace++
			plan.Entries = append(plan.Entries, entry)
			continue
		}

		destDir := filepath.Dir(rawDest)
		same, err := sameVolume(currentAbs, destDir)
		if err != nil {
			// Treat an unresolvable volume check as a skip (never a fatal plan error).
			entry.Kind = MoveSkip
			entry.Reason = ReorgSkipCrossVolume
			plan.Skipped++
			plan.Entries = append(plan.Entries, entry)
			continue
		}
		if !same {
			entry.Kind = MoveSkip
			entry.Reason = ReorgSkipCrossVolume
			plan.Skipped++
			plan.Entries = append(plan.Entries, entry)
			continue
		}

		name, err := resolvePlannedName(destDir, base, claimed)
		if err != nil {
			return nil, fmt.Errorf("reorganize: resolve target name for %q: %w", currentAbs, err)
		}
		finalDest := filepath.Join(destDir, name)
		claimed[finalDest] = true
		entry.Kind = MoveMove
		entry.To = finalDest
		plan.Moves++
		plan.Entries = append(plan.Entries, entry)
	}

	if progressFn != nil {
		progressFn(total, total)
	}
	p.reorgLog().Info("reorganize plan computed",
		"total", plan.TotalAssets, "moves", plan.Moves, "inPlace", plan.InPlace, "skipped", plan.Skipped)
	return plan, nil
}

// resolvePlannedName returns a filename in destDir that is free of BOTH an
// existing on-disk file AND any path already claimed by an earlier planned move.
// It mirrors archive.ResolveCollision's " (2)", " (3)" scheme but also consults
// the claimed set so two planned moves never collapse onto the same target.
func resolvePlannedName(destDir, filename string, claimed map[string]bool) (string, error) {
	free := func(name string) (bool, error) {
		full := filepath.Join(destDir, name)
		if claimed[full] {
			return false, nil
		}
		_, err := os.Lstat(full)
		if err == nil {
			return false, nil
		}
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if ok, err := free(filename); err != nil {
		return "", err
	} else if ok {
		return filename, nil
	}
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if ok, err := free(cand); err != nil {
			return "", err
		} else if ok {
			return cand, nil
		}
	}
}

// RunReorganize creates a fresh ImportSession (notes mode "reorganize") and
// executes plan against it. If plan is nil it is computed here. It is the
// convenience entry point used by tests; the service layer owns the session ID
// up front and calls RunReorganizeSession directly.
func (p *Pipeline) RunReorganize(ctx context.Context, plan *ReorganizePlan, progressFn ProgressFunc) (*domain.ImportSession, error) {
	notes := reorganizeNotes()
	session := &domain.ImportSession{
		StartedAt:       time.Now(),
		DestinationRoot: p.effectiveLayout(p.libraryRoot).MasterRoot,
		Status:          domain.SessionStatusRunning,
		Notes:           notes,
	}
	if err := p.sessions.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("reorganize: create session: %w", err)
	}
	return p.RunReorganizeSession(ctx, session.ID, plan, progressFn)
}

// reorganizeNotes returns the session Notes JSON marking a reorganize run. The
// empty SourceRoot makes ResumeSession refuse a reorganize session cleanly (it
// is re-runnable via a fresh plan, never resumable as an import).
func reorganizeNotes() string {
	raw, _ := json.Marshal(sessionState{Mode: ModeReorganize})
	return string(raw)
}

// RunReorganizeSession executes a reorganize plan against an existing session,
// moving each out-of-place asset via the adopt+reorganize safety protocol
// (same-volume atomic rename, exclusive link+remove publish, quick-hash
// re-verification), updating CurrentArchivePath (root-relative) and the session
// counters in one per-file transaction. It honors ctx cancellation between files
// (finalizing the session as cancelled), never aborts on a single failure, and
// sweeps directories left empty by moves afterward. If plan is nil it is
// computed here.
func (p *Pipeline) RunReorganizeSession(ctx context.Context, sessionID string, plan *ReorganizePlan, progressFn ProgressFunc) (*domain.ImportSession, error) {
	log := p.reorgLog()
	log.Info("reorganize session started", "sessionId", sessionID)

	if plan == nil {
		var err error
		plan, err = p.PlanReorganize(ctx, ReorganizeOptions{}, nil)
		if err != nil {
			p.finishReorg(sessionID, domain.SessionStatusFailed)
			return p.reload(sessionID), fmt.Errorf("reorganize: plan: %w", err)
		}
	}

	if err := p.sessions.IncScanned(ctx, sessionID, plan.TotalAssets); err != nil {
		log.Warn("reorganize: record scanned count", "error", err.Error())
	}

	lay := p.effectiveLayout(p.libraryRoot)
	moved, skipped, failures := 0, 0, 0
	// notesState accumulates cross-volume fallbacks recorded by reorganizeInPlace.
	notesState := &sessionState{Mode: ModeReorganize}

	for i, e := range plan.Entries {
		if err := ctx.Err(); err != nil {
			p.finishReorg(sessionID, domain.SessionStatusCancelled)
			log.Warn("reorganize cancelled", "sessionId", sessionID, "filesProcessed", i)
			return p.reload(sessionID), nil
		}

		progressFn.emit(Progress{
			Phase:       PhaseReorganizing,
			FilesDone:   i,
			FilesTotal:  len(plan.Entries),
			CurrentFile: e.From,
			Errors:      failures,
		})

		// Re-check after emit: a progress observer may have cancelled. Catch it here
		// so a move never begins on a cancelled context (a physical move is not
		// context-guarded once started).
		if err := ctx.Err(); err != nil {
			p.finishReorg(sessionID, domain.SessionStatusCancelled)
			log.Warn("reorganize cancelled", "sessionId", sessionID, "filesProcessed", i)
			return p.reload(sessionID), nil
		}

		switch e.Kind {
		case MoveInPlace:
			log.Debug("reorganize: already in place", "path", e.From, "assetId", e.AssetID)
			continue
		case MoveSkip:
			if err := p.sessions.IncSkipped(ctx, sessionID, 1); err != nil {
				log.Warn("reorganize: inc skipped", "error", err.Error())
			}
			skipped++
			log.Info("reorganize: skipped", "path", e.From, "reason", e.Reason, "assetId", e.AssetID)
			continue
		case MoveMove:
			ok, wasMoved := p.reorganizeOne(ctx, sessionID, e, notesState)
			switch {
			case !ok:
				failures++
			case !wasMoved:
				// A cross-volume condition surfaced at run time: left in place, noted.
				if err := p.sessions.IncSkipped(ctx, sessionID, 1); err != nil {
					log.Warn("reorganize: inc skipped", "error", err.Error())
				}
				skipped++
			default:
				moved++
			}
		}
	}

	swept := p.sweepEmptyDirs(lay.MasterRoot)
	if swept > 0 {
		log.Info("reorganize: swept empty directories", "count", swept)
	}

	status := domain.SessionStatusCompleted
	if moved == 0 && failures > 0 {
		status = domain.SessionStatusFailed
	}
	p.finishReorg(sessionID, status)
	progressFn.emit(Progress{
		Phase:      PhaseDone,
		FilesDone:  len(plan.Entries),
		FilesTotal: len(plan.Entries),
		Errors:     failures,
	})
	log.Info("reorganize session completed",
		"sessionId", sessionID, "moved", moved, "skipped", skipped, "failures", failures)
	return p.reload(sessionID), nil
}

// reorganizeOne executes a single planned move: it performs the same-volume
// atomic rename with quick-hash re-verification, then updates the asset's
// archive path and the session counter in one transaction. It returns (ok,
// moved): ok=false on a recoverable failure (counted as a failure by the
// caller), and moved=false when the move was refused as cross-volume (left in
// place). It never aborts the run.
func (p *Pipeline) reorganizeOne(ctx context.Context, sessionID string, e ReorganizeEntry, state *sessionState) (ok, moved bool) {
	log := p.reorgLog()
	didMove, newPath, err := p.reorganizeInPlace(ctx, e.From, e.To, e.QuickHash, state)
	if err != nil {
		if e := p.sessions.IncFailures(context.Background(), sessionID, 1); e != nil {
			log.Warn("reorganize: inc failures", "error", e.Error())
		}
		log.Error("reorganize move failed", "path", e.From, "dest", e.To, "assetId", e.AssetID, "error", err.Error())
		return false, false
	}
	if !didMove {
		// Cross-volume fallback: the file is untouched at its original path.
		return true, false
	}

	// The file is already physically moved. Record it with a background context so
	// a cancellation racing in AFTER the move can never strand the catalog with a
	// stale path — once the bytes have moved, the row must follow.
	rel := library.RelativizePath(p.libraryRoot, newPath)
	recCtx := context.Background()
	txErr := p.db.Transaction(func(tx *gorm.DB) error {
		if err := p.assets.WithTx(tx).UpdateArchivePath(recCtx, e.AssetID, rel); err != nil {
			return err
		}
		return p.sessions.WithTx(tx).AddCounters(recCtx, sessionID, repo.SessionCounters{Imported: 1})
	})
	if txErr != nil {
		// The file is safely at newPath (quick-hash verified) but the DB update
		// failed. Data is not lost; a subsequent plan reports the stale row's old
		// path as missing. Record a failure and continue.
		if e := p.sessions.IncFailures(context.Background(), sessionID, 1); e != nil {
			log.Warn("reorganize: inc failures", "error", e.Error())
		}
		log.Error("reorganize: record move failed (file moved, DB not updated)",
			"assetId", e.AssetID, "newPath", newPath, "error", txErr.Error())
		return false, true
	}
	log.Info("reorganized (moved, verified)", "from", e.From, "to", newPath, "assetId", e.AssetID)
	return true, true
}

// finishReorg finalizes a reorganize session's status.
func (p *Pipeline) finishReorg(sessionID string, status domain.SessionStatus) {
	if err := p.sessions.Complete(context.Background(), sessionID, status, time.Now()); err != nil {
		p.reorgLog().Warn("reorganize: complete session", "sessionId", sessionID, "error", err.Error())
	}
}

// sweepEmptyDirs removes directories left empty (bottom-up) strictly inside root,
// never removing root itself and never descending into or removing dotted dirs
// (".paim", ".paim-trash", …). It returns the number of directories removed.
func (p *Pipeline) sweepEmptyDirs(root string) int {
	if root == "" {
		return 0
	}
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || !d.IsDir() {
			return nil
		}
		if path == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			// Never touch PAIM bookkeeping / hidden dirs (or their contents).
			return fs.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	// Deepest paths first so a parent is only considered after its children are
	// removed.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	removed := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			continue
		}
		if err := os.Remove(dir); err != nil {
			p.reorgLog().Warn("reorganize: remove empty dir", "path", dir, "error", err.Error())
			continue
		}
		removed++
	}
	return removed
}
