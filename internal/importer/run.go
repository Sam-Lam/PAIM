package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/autolinepro/paim/internal/archive"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/hashing"
	"github.com/autolinepro/paim/internal/mediatype"
	"github.com/autolinepro/paim/internal/metadata"
	"github.com/autolinepro/paim/internal/repo"
	"gorm.io/gorm"
)

// sessionState is the JSON blob stored in ImportSession.Notes. It captures
// everything Resume needs to re-run the import plus any human-readable notes
// accumulated during the run (e.g. cross-volume reorganize fallbacks).
type sessionState struct {
	Mode            Mode     `json:"mode"`
	SourceRoot      string   `json:"sourceRoot"`
	DestinationRoot string   `json:"destinationRoot"`
	EventName       string   `json:"eventName"`
	SourceID        string   `json:"sourceId"`
	Reorganize      bool     `json:"reorganize"`
	Concurrency     int      `json:"concurrency"`
	Notes           []string `json:"notes,omitempty"`
}

func (s sessionState) options() Options {
	return Options{
		Mode:            s.Mode,
		SourceRoot:      s.SourceRoot,
		DestinationRoot: s.DestinationRoot,
		EventName:       s.EventName,
		SourceID:        s.SourceID,
		Reorganize:      s.Reorganize,
		Concurrency:     s.Concurrency,
	}
}

func stateFromOptions(opts Options) sessionState {
	return sessionState{
		Mode:            opts.mode(),
		SourceRoot:      opts.SourceRoot,
		DestinationRoot: opts.DestinationRoot,
		EventName:       opts.EventName,
		SourceID:        opts.SourceID,
		Reorganize:      opts.Reorganize,
		Concurrency:     opts.Concurrency,
	}
}

// Run executes a full import: it creates an ImportSession (status running),
// scans SourceRoot, then imports every file per the copy/adopt protocol,
// finalizing the session status (completed, cancelled, or interrupted). It
// returns the final session record.
func (p *Pipeline) Run(ctx context.Context, opts Options, progressFn ProgressFunc) (*domain.ImportSession, error) {
	state := stateFromOptions(opts)
	notesJSON, _ := json.Marshal(state)

	session := &domain.ImportSession{
		StartedAt:       time.Now(),
		SourceID:        opts.SourceID,
		DestinationRoot: opts.DestinationRoot,
		Status:          domain.SessionStatusRunning,
		Notes:           string(notesJSON),
	}
	if err := p.sessions.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("run: create session: %w", err)
	}
	p.log.Info("import session started", "sessionId", session.ID, "mode", opts.mode(), "source", opts.SourceRoot)

	scan, err := p.Scan(ctx, opts.SourceRoot, progressFn)
	if err != nil {
		// Scan failure (including cancellation) leaves the session interrupted.
		_ = p.sessions.SetStatus(context.Background(), session.ID, domain.SessionStatusInterrupted)
		return p.reload(session.ID), fmt.Errorf("run: scan: %w", err)
	}
	if err := p.sessions.IncScanned(ctx, session.ID, len(scan.Files)); err != nil {
		p.log.Warn("run: could not record scanned count", "error", err.Error())
	}

	return p.runImport(ctx, session, scan, opts, &state, progressFn)
}

// runImport performs the per-file import loop against an existing session and a
// completed scan. It is shared by Run and ResumeSession.
func (p *Pipeline) runImport(ctx context.Context, session *domain.ImportSession, scan *ScanResult, opts Options, state *sessionState, progressFn ProgressFunc) (*domain.ImportSession, error) {
	lay := p.effectiveLayout(opts.DestinationRoot)

	// Metadata for every file, batched. ContentIdentifier feeds pairing; capture
	// dates feed the layout. Degrades gracefully to an empty map.
	metaByPath := p.extractMetadata(ctx, scan.Files)

	// Stage-2 (authoritative) Live Photo pairing using ContentIdentifier.
	partnerOf := reconcilePairs(scan, metaByPath)

	// Reuse precomputed quick hashes where the file is unchanged.
	quick := p.reuseQuickHashes(opts.Precomputed, scan.Files)

	assetIDByPath := make(map[string]string, len(scan.Files))
	var bytesDone int64
	errorCount := 0

	for i, fi := range scan.Files {
		if err := ctx.Err(); err != nil {
			p.finishSession(session.ID, domain.SessionStatusCancelled, state)
			p.log.Warn("import cancelled", "sessionId", session.ID, "filesProcessed", i)
			return p.reload(session.ID), nil
		}

		progressFn.emit(Progress{
			Phase:       PhaseImporting,
			FilesDone:   i,
			FilesTotal:  len(scan.Files),
			BytesDone:   bytesDone,
			BytesTotal:  scan.TotalBytes,
			CurrentFile: fi.Path,
			Errors:      errorCount,
		})

		outcome := p.processFile(ctx, session.ID, fi, opts, lay, quick[fi.Path], metaByPath[fi.Path], state, &bytesDone)
		if outcome.abort {
			p.finishSession(session.ID, domain.SessionStatusInterrupted, state)
			p.log.Error("import aborted", "sessionId", session.ID, "error", outcome.abortErr.Error())
			return p.reload(session.ID), outcome.abortErr
		}
		if outcome.failed {
			errorCount++
		}
		if outcome.assetID != "" {
			assetIDByPath[fi.Path] = outcome.assetID
		}
	}

	// Link confirmed Live Photo pairs both ways, now that all files are imported.
	p.linkPairs(ctx, partnerOf, assetIDByPath)

	p.finishSession(session.ID, domain.SessionStatusCompleted, state)
	progressFn.emit(Progress{
		Phase:      PhaseDone,
		FilesDone:  len(scan.Files),
		FilesTotal: len(scan.Files),
		BytesDone:  bytesDone,
		BytesTotal: scan.TotalBytes,
		Errors:     errorCount,
	})
	p.log.Info("import session completed", "sessionId", session.ID, "errors", errorCount)
	return p.reload(session.ID), nil
}

// fileOutcome summarizes what happened to one file.
type fileOutcome struct {
	assetID  string
	failed   bool
	abort    bool
	abortErr error
}

// processFile imports a single file in either copy or adopt mode. Any recoverable
// failure increments the session Failures counter, logs, and returns
// failed=true; unrecoverable destination conditions (disk full, destination root
// gone) return abort=true.
func (p *Pipeline) processFile(ctx context.Context, sessionID string, fi FileInfo, opts Options, lay *archive.Layout, quickHash string, meta *metadata.AssetMetadata, state *sessionState, bytesDone *int64) fileOutcome {
	// Source may have vanished (e.g. the drive was pulled).
	if _, err := statSize(fi.Path); err != nil {
		return p.fail(ctx, sessionID, fi.Path, "stat", err)
	}

	cls, err := p.classify(ctx, fi.Path, quickHash)
	if err != nil {
		return p.fail(ctx, sessionID, fi.Path, "classify", err)
	}

	switch cls.Disposition {
	case DispositionAlreadyImported:
		if err := p.sessions.IncSkipped(ctx, sessionID, 1); err != nil {
			p.log.Warn("processFile: inc skipped", "error", err.Error())
		}
		p.log.Debug("skip already-imported file", "path", fi.Path, "assetId", cls.MatchedAssetID)
		return fileOutcome{}
	case DispositionDuplicate:
		return p.recordDuplicate(ctx, sessionID, fi, opts, lay, cls, meta, state)
	default:
		if opts.mode() == ModeAdopt {
			return p.adoptFile(ctx, sessionID, fi, opts, lay, cls, meta, state, false)
		}
		return p.copyFile(ctx, sessionID, fi, opts, lay, cls, meta, bytesDone)
	}
}

// copyFile implements the copy-mode verification & copy protocol for one new
// file.
func (p *Pipeline) copyFile(ctx context.Context, sessionID string, fi FileInfo, opts Options, lay *archive.Layout, cls classification, meta *metadata.AssetMetadata, bytesDone *int64) fileOutcome {
	// Guard: destination root must exist before we attempt any copy.
	if opts.DestinationRoot != "" {
		if _, err := os.Stat(opts.DestinationRoot); err != nil {
			return fileOutcome{abort: true, abortErr: fmt.Errorf("copy %q: destination root unavailable: %w", opts.DestinationRoot, err)}
		}
	}

	captureDate, fromMtime := effectiveCaptureDate(meta, fi)
	if fromMtime {
		p.log.Info("capture date from file mtime (no EXIF)", "path", fi.Path, "date", captureDate.Format(time.RFC3339))
	}
	destPath := computeDestination(lay, captureDate, opts.EventName, fi)
	destDir := filepath.Dir(destPath)

	partialPath, err := p.copyToPartial(ctx, fi.Path, destDir, func(n int64) {
		*bytesDone += n
	})
	if err != nil {
		if errors.Is(err, errDestinationFull) {
			return fileOutcome{abort: true, abortErr: fmt.Errorf("copy %q: %w", fi.Path, err)}
		}
		if ctx.Err() != nil {
			// Cancelled mid-copy: partial already removed by copyToPartial.
			return fileOutcome{}
		}
		return p.fail(ctx, sessionID, fi.Path, "copy", err)
	}

	// Test hook: allow corruption of the partial before verification.
	if p.afterCopyHook != nil {
		p.afterCopyHook(partialPath)
	}

	ok, err := hashing.VerifyCopy(ctx, cls.QuickHash, cls.FullHash, partialPath)
	if err != nil {
		_ = os.Remove(partialPath)
		return p.fail(ctx, sessionID, fi.Path, "verify", err)
	}
	if !ok {
		_ = os.Remove(partialPath)
		return p.fail(ctx, sessionID, fi.Path, "verify",
			fmt.Errorf("destination copy does not match source"))
	}

	finalName, err := archive.ResolveCollision(destDir, filepath.Base(destPath))
	if err != nil {
		_ = os.Remove(partialPath)
		return p.fail(ctx, sessionID, fi.Path, "resolve-collision", err)
	}
	finalPath := filepath.Join(destDir, finalName)
	if err := os.Rename(partialPath, finalPath); err != nil {
		_ = os.Remove(partialPath)
		return p.fail(ctx, sessionID, fi.Path, "rename", err)
	}
	_ = fsyncDir(destDir)

	asset := p.buildAsset(sessionID, fi, cls, meta, finalPath, captureDate, nil)
	asset.BackupStatus = domain.BackupStatusPending
	if err := p.recordAsset(ctx, asset, repo.SessionCounters{Imported: 1}, true); err != nil {
		// The bytes are safely on disk but the DB write failed; remove the file so
		// a later resume re-imports cleanly (nothing was recorded).
		_ = os.Remove(finalPath)
		return p.fail(ctx, sessionID, fi.Path, "record", err)
	}
	p.log.Info("imported (copy, verified)", "path", fi.Path, "dest", finalPath, "assetId", asset.ID)
	return fileOutcome{assetID: asset.ID}
}

// adoptFile registers an existing file in place (optionally reorganizing it via
// same-volume rename), computing a full BLAKE3 integrity baseline. duplicate
// indicates the file is a flagged in-library duplicate (still registered).
func (p *Pipeline) adoptFile(ctx context.Context, sessionID string, fi FileInfo, opts Options, lay *archive.Layout, cls classification, meta *metadata.AssetMetadata, state *sessionState, duplicate bool) fileOutcome {
	// Full BLAKE3 is the in-place verification baseline; always computed.
	fullHash := cls.FullHash
	if fullHash == "" {
		fh, err := hashing.FullHash(ctx, fi.Path)
		if err != nil {
			return p.fail(ctx, sessionID, fi.Path, "baseline-hash", err)
		}
		fullHash = fh
	}
	cls.FullHash = fullHash

	currentPath := fi.Path
	// Optional reorganize: same-volume atomic rename into the standard layout. We
	// never reorganize a flagged duplicate (its canonical original owns the slot).
	if opts.Reorganize && !duplicate {
		captureDate, fromMtime := effectiveCaptureDate(meta, fi)
		if fromMtime {
			p.log.Info("capture date from file mtime (no EXIF)", "path", fi.Path)
		}
		destPath := computeDestination(lay, captureDate, opts.EventName, fi)
		if destPath != fi.Path {
			moved, newPath, err := p.reorganizeInPlace(ctx, fi.Path, destPath, cls.QuickHash, state)
			if err != nil {
				return p.fail(ctx, sessionID, fi.Path, "reorganize", err)
			}
			if moved {
				currentPath = newPath
			}
		}
	}

	var dupOf *string
	if duplicate && cls.MatchedAssetID != "" {
		dupOf = &cls.MatchedAssetID
	}
	captureDate, _ := effectiveCaptureDate(meta, fi)
	asset := p.buildAsset(sessionID, fi, cls, meta, currentPath, captureDate, dupOf)
	asset.FullHash = fullHash
	asset.BackupStatus = domain.BackupStatusPending
	// A flagged in-library duplicate is still adopted and still backed up, but is
	// additionally counted in the Duplicates tally.
	counters := repo.SessionCounters{Imported: 1}
	if duplicate {
		counters.Duplicates = 1
	}
	if err := p.recordAsset(ctx, asset, counters, true); err != nil {
		return p.fail(ctx, sessionID, fi.Path, "record", err)
	}
	p.log.Info("adopted in place (in-place baseline verified)",
		"path", currentPath, "assetId", asset.ID, "duplicate", duplicate)
	return fileOutcome{assetID: asset.ID}
}

// reorganizeInPlace moves src to destPath via a same-volume atomic rename,
// resolving name collisions and re-verifying the quick hash at the new path. A
// cross-volume destination is refused (never copy+delete in adopt mode): the
// file is left in place and a note is recorded. It returns whether a move
// happened and the resulting path.
func (p *Pipeline) reorganizeInPlace(ctx context.Context, src, destPath, srcQuick string, state *sessionState) (bool, string, error) {
	destDir := filepath.Dir(destPath)
	same, err := sameVolume(src, destDir)
	if err != nil {
		return false, "", err
	}
	if !same {
		note := fmt.Sprintf("left in place (cross-volume): %s", src)
		state.Notes = append(state.Notes, note)
		p.log.Warn("adopt reorganize skipped: cross-volume", "path", src, "dest", destPath)
		return false, src, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return false, "", fmt.Errorf("reorganize: mkdir %q: %w", destDir, err)
	}
	finalName, err := archive.ResolveCollision(destDir, filepath.Base(destPath))
	if err != nil {
		return false, "", err
	}
	finalPath := filepath.Join(destDir, finalName)
	if err := os.Rename(src, finalPath); err != nil {
		return false, "", fmt.Errorf("reorganize: rename %q -> %q: %w", src, finalPath, err)
	}
	_ = fsyncDir(destDir)

	// Re-verify the quick hash at the new location before recording it.
	newQuick, err := hashing.QuickHash(finalPath)
	if err != nil {
		return false, "", fmt.Errorf("reorganize: re-hash %q: %w", finalPath, err)
	}
	if newQuick != srcQuick {
		return false, "", fmt.Errorf("reorganize: quick hash changed after move %q", finalPath)
	}
	return true, finalPath, nil
}

// recordDuplicate records a content duplicate. In copy mode the bytes are NOT
// copied: a placeholder Asset with DuplicateOfAssetID and an empty archive path
// is inserted. In adopt mode the in-place file is registered and flagged.
func (p *Pipeline) recordDuplicate(ctx context.Context, sessionID string, fi FileInfo, opts Options, lay *archive.Layout, cls classification, meta *metadata.AssetMetadata, state *sessionState) fileOutcome {
	if opts.mode() == ModeAdopt {
		return p.adoptFile(ctx, sessionID, fi, opts, lay, cls, meta, state, true)
	}
	captureDate, _ := effectiveCaptureDate(meta, fi)
	dupOf := cls.MatchedAssetID
	asset := p.buildAsset(sessionID, fi, cls, meta, "", captureDate, &dupOf)
	asset.BackupStatus = domain.BackupStatusNone
	if err := p.recordAsset(ctx, asset, repo.SessionCounters{Duplicates: 1}, false); err != nil {
		return p.fail(ctx, sessionID, fi.Path, "record-duplicate", err)
	}
	p.log.Info("recorded duplicate (not copied)", "path", fi.Path, "duplicateOf", dupOf, "assetId", asset.ID)
	return fileOutcome{assetID: asset.ID}
}

// buildAsset constructs an Asset from a file, its classification, and metadata.
// captureDate is the effective (EXIF-or-mtime) date. archivePath is the file's
// recorded location (empty for a not-copied duplicate). duplicateOf, if
// non-nil, flags this asset as a duplicate.
func (p *Pipeline) buildAsset(sessionID string, fi FileInfo, cls classification, meta *metadata.AssetMetadata, archivePath string, captureDate time.Time, duplicateOf *string) *domain.Asset {
	cd := captureDate
	asset := &domain.Asset{
		OriginalFilename:   filepath.Base(fi.Path),
		OriginalExtension:  fi.Ext,
		OriginalFullPath:   fi.Path,
		SourceID:           "",
		SessionID:          sessionID,
		QuickHash:          cls.QuickHash,
		FullHash:           cls.FullHash,
		FileSize:           fi.Size,
		CaptureDate:        &cd,
		ImportDate:         time.Now(),
		MediaType:          mediatype.MediaTypeFor(fi.Ext),
		CurrentArchivePath: archivePath,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
		DuplicateOfAssetID: duplicateOf,
	}
	applyMetadata(asset, meta)
	return asset
}

// applyMetadata copies extracted metadata fields onto an asset (best effort).
func applyMetadata(asset *domain.Asset, meta *metadata.AssetMetadata) {
	if meta == nil {
		return
	}
	asset.Width = meta.Width
	asset.Height = meta.Height
	asset.DurationSeconds = meta.DurationSeconds
	asset.CameraMake = meta.CameraMake
	asset.CameraModel = meta.CameraModel
	asset.Lens = meta.Lens
	asset.ISO = meta.ISO
	asset.ShutterSpeed = meta.ShutterSpeed
	asset.Aperture = meta.Aperture
	asset.GPSLatitude = meta.GPSLatitude
	asset.GPSLongitude = meta.GPSLongitude
}

// recordAsset inserts an asset, applies session counters, and (optionally)
// enqueues backup work, all in one transaction so the import is atomic.
func (p *Pipeline) recordAsset(ctx context.Context, asset *domain.Asset, counters repo.SessionCounters, enqueue bool) error {
	return p.db.Transaction(func(tx *gorm.DB) error {
		if err := p.assets.WithTx(tx).Create(ctx, asset); err != nil {
			return err
		}
		if err := p.sessions.WithTx(tx).AddCounters(ctx, asset.SessionID, counters); err != nil {
			return err
		}
		if enqueue {
			if err := p.backup.EnqueueForAsset(ctx, tx, asset.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// fail records a per-file failure: increments the session Failures counter and
// logs a wrapped, path+op error. It never aborts the pipeline.
func (p *Pipeline) fail(ctx context.Context, sessionID, path, op string, err error) fileOutcome {
	wrapped := fmt.Errorf("import %s %q: %w", op, path, err)
	if e := p.sessions.IncFailures(context.Background(), sessionID, 1); e != nil {
		p.log.Warn("fail: inc failures", "error", e.Error())
	}
	p.log.Error("import file failed", "path", path, "op", op, "error", wrapped.Error())
	return fileOutcome{failed: true}
}

// finishSession finalizes a session's status and persists the accumulated notes
// state. It uses a background context so finalization survives cancellation.
func (p *Pipeline) finishSession(sessionID string, status domain.SessionStatus, state *sessionState) {
	if state != nil {
		if raw, err := json.Marshal(state); err == nil {
			if e := p.db.Model(&domain.ImportSession{}).Where("id = ?", sessionID).Update("notes", string(raw)).Error; e != nil {
				p.log.Warn("finishSession: persist notes", "error", e.Error())
			}
		}
	}
	if err := p.sessions.Complete(context.Background(), sessionID, status, time.Now()); err != nil {
		p.log.Warn("finishSession: complete", "sessionId", sessionID, "error", err.Error())
	}
}

// reload fetches the latest session record for returning to the caller.
func (p *Pipeline) reload(sessionID string) *domain.ImportSession {
	s, err := p.sessions.GetByID(context.Background(), sessionID)
	if err != nil {
		p.log.Warn("reload session", "sessionId", sessionID, "error", err.Error())
		return nil
	}
	return s
}

// extractMetadata batch-extracts metadata for all files, keyed by path. A batch
// failure degrades to an empty map (import proceeds with mtime capture dates).
func (p *Pipeline) extractMetadata(ctx context.Context, files []FileInfo) map[string]*metadata.AssetMetadata {
	if len(files) == 0 || p.extractor == nil {
		return map[string]*metadata.AssetMetadata{}
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	byPath, err := p.extractor.ExtractBatch(ctx, paths)
	if err != nil {
		p.log.Warn("metadata batch extraction failed; proceeding degraded", "error", err.Error())
	}
	if byPath == nil {
		byPath = map[string]*metadata.AssetMetadata{}
	}
	return byPath
}

// reconcilePairs resolves the authoritative Live Photo pairs using
// ContentIdentifier from metadata (stage 2 of two-stage pairing) and returns a
// symmetric path->partner-path map.
func reconcilePairs(scan *ScanResult, meta map[string]*metadata.AssetMetadata) map[string]string {
	candidates := make([]mediatype.Candidate, 0, len(scan.Files))
	for _, f := range scan.Files {
		cid := ""
		if m := meta[f.Path]; m != nil {
			cid = m.ContentIdentifier
		}
		candidates = append(candidates, mediatype.Candidate{
			Path:              f.Path,
			Ext:               f.Ext,
			ContentIdentifier: cid,
		})
	}
	pairs := mediatype.PairLivePhotos(candidates)
	partner := make(map[string]string, len(pairs)*2)
	for _, pr := range pairs {
		partner[pr.Still.Path] = pr.Motion.Path
		partner[pr.Motion.Path] = pr.Still.Path
	}
	return partner
}

// linkPairs sets LivePhotoPartnerID both ways and marks both rows as a
// live_photo_pair, but only when BOTH components imported successfully. An
// orphaned component (its partner failed) is logged and left unlinked.
func (p *Pipeline) linkPairs(ctx context.Context, partnerOf map[string]string, assetIDByPath map[string]string) {
	linked := map[string]bool{}
	for stillPath, motionPath := range partnerOf {
		if linked[stillPath] {
			continue
		}
		stillID, okS := assetIDByPath[stillPath]
		motionID, okM := assetIDByPath[motionPath]
		if !okS || !okM {
			if okS != okM {
				p.log.Warn("live photo pair orphaned: one component failed",
					"still", stillPath, "motion", motionPath)
			}
			continue
		}
		if err := p.setPartner(ctx, stillID, motionID); err != nil {
			p.log.Warn("linkPairs: set still partner", "error", err.Error())
			continue
		}
		if err := p.setPartner(ctx, motionID, stillID); err != nil {
			p.log.Warn("linkPairs: set motion partner", "error", err.Error())
			continue
		}
		linked[stillPath] = true
		linked[motionPath] = true
		p.log.Info("linked live photo pair", "stillId", stillID, "motionId", motionID)
	}
}

// setPartner links assetID to partnerID and marks it as a live_photo_pair.
func (p *Pipeline) setPartner(ctx context.Context, assetID, partnerID string) error {
	err := p.db.WithContext(ctx).Model(&domain.Asset{}).Where("id = ?", assetID).Updates(map[string]any{
		"live_photo_partner_id": partnerID,
		"media_type":            domain.MediaTypeLivePhotoPair,
	}).Error
	if err != nil {
		return fmt.Errorf("set partner %q: %w", assetID, err)
	}
	return nil
}

// reuseQuickHashes returns a path->quick-hash map, reusing a prior DryRun's
// hashes when the file path and size still match, otherwise leaving the entry
// empty so classify recomputes it.
func (p *Pipeline) reuseQuickHashes(report *DryRunReport, files []FileInfo) map[string]string {
	out := make(map[string]string, len(files))
	if report == nil {
		return out
	}
	for _, f := range files {
		if h, ok := report.QuickHashes[f.Path]; ok {
			out[f.Path] = h
		}
	}
	return out
}

// effectiveCaptureDate returns the capture date to use for layout and storage:
// the EXIF capture date when present, otherwise the file mtime. The second
// return reports whether the mtime fallback was used (so the caller can log it).
func effectiveCaptureDate(meta *metadata.AssetMetadata, fi FileInfo) (time.Time, bool) {
	if meta != nil && meta.CaptureDate != nil {
		return *meta.CaptureDate, false
	}
	return time.Unix(fi.ModTime, 0), true
}
