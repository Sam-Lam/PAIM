package source

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// ArchivedAsset is the minimal view of a stored asset that safe-to-erase needs.
// It is returned by an AssetLookup and lets this package decide whether a source
// file is already safely archived without depending on the repo or GORM models.
type ArchivedAsset struct {
	// ID identifies the asset (for diagnostics only).
	ID string
	// QuickHash is the asset's BLAKE3 quick hash.
	QuickHash string
	// FullHash is the asset's BLAKE3 full hash, empty if not yet computed.
	FullHash string
	// Verified is true when VerificationStatus == verified.
	Verified bool
	// BackupComplete is true when all required backups for the asset are complete.
	BackupComplete bool
	// HasArchiveCopy is true when the asset has its own archived copy (a non-empty
	// CurrentArchivePath). It gates the fast path: a copy-mode duplicate placeholder
	// (no archive copy, never backed up on its own) is NOT authoritative for its
	// source file's status — the canonical original is — so such a match falls back
	// to the hashing path, which resolves the canonical asset by content hash.
	HasArchiveCopy bool
	// OriginalFullPath is the absolute source path recorded when the asset was
	// imported. It is the key of the catalog-informed fast path (a source file at
	// this exact path may be the same file PAIM already verified at import).
	OriginalFullPath string
	// FileSize is the asset's recorded byte size, the fast path's size gate.
	FileSize int64
	// ImportDate is when PAIM imported (and verified) the asset. The fast path
	// trusts a source file whose mtime is at or before this instant — i.e. it has
	// not been modified since PAIM last read and verified its bytes.
	ImportDate time.Time
}

// AssetLookup finds archived assets by content hash or original source path.
// Implemented over the asset repo and wired in main.go/services; kept narrow so
// this package stays testable with fakes.
type AssetLookup interface {
	// FindByQuickHash returns every non-deleted asset sharing the quick hash.
	FindByQuickHash(ctx context.Context, quickHash string) ([]ArchivedAsset, error)
	// FindByOriginalPath returns every non-deleted asset whose recorded original
	// source path equals path. It powers the catalog-informed fast path so a
	// just-imported card can be evaluated without re-hashing. An implementation
	// that cannot answer (or has no such asset) returns an empty slice, which
	// simply forces the (correct) hashing path for that file.
	FindByOriginalPath(ctx context.Context, path string) ([]ArchivedAsset, error)
}

// FullHasher optionally provides a file's full hash, used to confirm a match
// when several archived assets share one quick hash. The injected FileHasher may
// also implement this (internal/hashing does); if it does not, colliding
// quick-hash matches are treated conservatively as unverified.
type FullHasher interface {
	FullHash(path string) (string, error)
}

// SafeToEraseReport is the outcome of evaluating whether a source volume can be
// erased. Safe is true only when every media file maps to a verified, fully
// backed-up archived asset. The Reason always explains the conclusion.
type SafeToEraseReport struct {
	SourceID string `json:"sourceId"`
	Safe     bool   `json:"safe"`
	Reason   string `json:"reason"`

	// NoBackupDestination is true when the verdict is not-safe SPECIFICALLY
	// because every file is archived and verified but no backup destination is
	// configured (zero enabled required, non-mirror providers) — the archive is
	// the only copy. It is a distinct, non-alarming state (the UI renders it amber,
	// not the red reserved for genuinely not-archived New/Unverified files). It is
	// never set alongside New/Unverified problems.
	NoBackupDestination bool `json:"noBackupDestination"`

	// TotalMedia is the number of media files examined on the volume.
	TotalMedia int `json:"totalMedia"`
	// Archived counts files mapping to a verified, fully backed-up asset.
	Archived int `json:"archived"`
	// New counts files with no matching archived asset (not yet imported).
	New int `json:"new"`
	// Unverified counts files whose matching asset is not verified.
	Unverified int `json:"unverified"`
	// BackupIncomplete counts files whose verified asset lacks complete backups.
	BackupIncomplete int `json:"backupIncomplete"`

	// FastPath counts files classified from the catalog without hashing (the
	// file's path+size matched an imported asset and its mtime proved it
	// unmodified since import). Hashed counts files that had to be (re)hashed
	// because they missed the catalog lookup, size-mismatched, or were modified
	// after import. FastPath+Hashed == TotalMedia. The pair makes the speedup
	// honest and visible; it does not change the evaluation's semantics.
	FastPath int `json:"fastPath"`
	Hashed   int `json:"hashed"`

	// SafeFiles is the absolute path of every media file classified as Archived
	// (verified + fully backed up) — exactly the set a source-clear operation may
	// move to trash. It is memory-bounded (paths only) and never serialized to the
	// frontend: the clear action re-reads it server-side, never re-deciding on its
	// own.
	SafeFiles []string `json:"-"`
}

// SafeToEraseProgress reports safe-to-erase evaluation progress: how many media
// files have been hashed/classified so far, the total discovered by enumeration,
// and the file currently being examined. FilesTotal is known before the first
// hash (enumeration completes up front) so the UI can render a determinate bar.
type SafeToEraseProgress func(filesDone, filesTotal int, currentFile string)

// walkedFile is a media file discovered by the enumeration phase, carrying the
// size and mtime the fast path needs so classification does not stat again.
type walkedFile struct {
	path    string
	size    int64
	modTime time.Time
}

// classification is the single bucket a media file falls into.
type classification int

const (
	classArchived classification = iota
	classNew
	classUnverified
	classBackupIncomplete
)

// EvaluateSafeToErase walks the media files under root and reports whether the
// source is safe to erase. Every media file must map — by quick hash, confirmed
// by full hash on a quick-hash collision, OR by the catalog-informed fast path —
// to a verified asset whose required backups are complete. Any New, Unverified,
// or BackupIncomplete file makes the volume unsafe.
//
// It runs in two phases so progress is determinate: first it enumerates every
// media file under root (fast, no hashing) recording each file's size and mtime,
// then it classifies each one, invoking progressFn (which may be nil) per file.
//
// Fast path: before hashing a file, it asks the lookup for an asset recorded with
// this exact original source path. If one exists whose FileSize matches the
// file's size AND whose ImportDate is at or after the file's mtime (the file has
// not been touched since PAIM verified it at import), the asset's own
// verification+backup status classifies the file WITHOUT hashing. This mirrors
// the size+mtime staleness gate the analyze→import hash reuse already relies on:
// the same trust posture (unchanged size+mtime ⇒ unchanged bytes). Any file that
// misses the lookup, size-mismatches, or was modified after import falls back to
// the full hashing path for that file only. The result semantics are identical to
// hashing every file; only the compute path differs. FastPath/Hashed counts
// record how many files took each path.
//
// hasBackupDestination reports whether at least one enabled required (non-mirror)
// backup provider is configured. When false, the archive is the only copy of an
// asset, so the volume can NEVER be classified safe to erase — regardless of each
// asset's stored aggregate BackupStatus (which can be stale "complete" after a
// destination is removed). See finalizeReport.
//
// lookup and isMedia are injected (rather than being constructor dependencies of
// the Identifier) because they are only needed for this evaluation.
func (id *Identifier) EvaluateSafeToErase(
	ctx context.Context,
	sourceID string,
	root string,
	hasBackupDestination bool,
	lookup AssetLookup,
	isMedia func(ext string) bool,
	progressFn SafeToEraseProgress,
) (*SafeToEraseReport, error) {
	if lookup == nil {
		return nil, fmt.Errorf("source: evaluate safe-to-erase %q: nil asset lookup", sourceID)
	}
	if isMedia == nil {
		return nil, fmt.Errorf("source: evaluate safe-to-erase %q: nil isMedia", sourceID)
	}
	if id.hasher == nil {
		return nil, fmt.Errorf("source: evaluate safe-to-erase %q: nil hasher", sourceID)
	}

	report := &SafeToEraseReport{SourceID: sourceID}
	fullHasher, _ := id.hasher.(FullHasher)

	// Phase 1: enumerate media files (no content hashing) recording size+mtime so
	// the total is known before classification and the fast path need not re-stat.
	var mediaFiles []walkedFile
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if !isMedia(normaliseExt(name)) {
			return nil
		}
		wf := walkedFile{path: path}
		if info, ierr := d.Info(); ierr == nil {
			wf.size = info.Size()
			wf.modTime = info.ModTime()
		}
		mediaFiles = append(mediaFiles, wf)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("source: walk %q for safe-to-erase: %w", root, walkErr)
	}

	total := len(mediaFiles)
	report.TotalMedia = total
	if progressFn != nil {
		progressFn(0, total, "")
	}

	// Phase 2: classify each media file (fast path first, hashing as fallback),
	// reporting progress per file.
	for i, wf := range mediaFiles {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if progressFn != nil {
			progressFn(i, total, wf.path)
		}
		cls, hashed := classifyFile(ctx, id.hasher, fullHasher, lookup, wf)
		switch cls {
		case classArchived:
			report.Archived++
			report.SafeFiles = append(report.SafeFiles, wf.path)
		case classNew:
			report.New++
		case classUnverified:
			report.Unverified++
		case classBackupIncomplete:
			report.BackupIncomplete++
		}
		if hashed {
			report.Hashed++
		} else {
			report.FastPath++
		}
	}
	if progressFn != nil {
		progressFn(total, total, "")
	}

	finalizeReport(report, hasBackupDestination)
	return report, nil
}

// classifyFile buckets a single media file into exactly one classification,
// returning whether the file had to be hashed (false = classified via the
// catalog-informed fast path).
func classifyFile(
	ctx context.Context,
	hasher FileHasher,
	fullHasher FullHasher,
	lookup AssetLookup,
	wf walkedFile,
) (classification, bool) {
	if asset, ok := fastPathAsset(ctx, lookup, wf); ok {
		return bucketFor(asset), false
	}
	return classifyByHash(ctx, hasher, fullHasher, lookup, wf.path), true
}

// fastPathAsset returns the imported asset that lets wf be classified without
// hashing: one recorded at wf's exact original path whose size matches and whose
// ImportDate is at or after wf's mtime (proving wf is unmodified since PAIM
// verified it). It returns ok=false — forcing the hashing path — when the file
// misses the lookup, size-mismatches, or was modified after import.
func fastPathAsset(ctx context.Context, lookup AssetLookup, wf walkedFile) (ArchivedAsset, bool) {
	candidates, err := lookup.FindByOriginalPath(ctx, wf.path)
	if err != nil || len(candidates) == 0 {
		return ArchivedAsset{}, false
	}
	for _, c := range candidates {
		if c.HasArchiveCopy && c.FileSize == wf.size && !wf.modTime.After(c.ImportDate) {
			return c, true
		}
	}
	return ArchivedAsset{}, false
}

// classifyByHash buckets a media file by (re)hashing it: quick hash, confirmed by
// full hash on a quick-hash collision, then the matched asset's verification and
// backup status.
func classifyByHash(
	ctx context.Context,
	hasher FileHasher,
	fullHasher FullHasher,
	lookup AssetLookup,
	path string,
) classification {
	quick, err := hasher.QuickHash(path)
	if err != nil {
		// Unreadable/unhashable source file: cannot prove it is archived.
		return classUnverified
	}
	candidates, err := lookup.FindByQuickHash(ctx, quick)
	if err != nil || len(candidates) == 0 {
		return classNew
	}

	// Narrow to the asset that actually corresponds to this file. With a single
	// candidate the quick hash is accepted; with several (a quick-hash collision)
	// the full hash disambiguates.
	asset, resolved := resolveAsset(fullHasher, path, candidates)
	if !resolved {
		// Could not confirm which colliding asset this file is: be conservative.
		return classUnverified
	}
	return bucketFor(asset)
}

// bucketFor classifies an already-resolved archived asset by its verification and
// backup status.
func bucketFor(asset ArchivedAsset) classification {
	switch {
	case !asset.Verified:
		return classUnverified
	case !asset.BackupComplete:
		return classBackupIncomplete
	default:
		return classArchived
	}
}

// resolveAsset picks the archived asset corresponding to path. A single
// candidate is returned directly. On a quick-hash collision it computes the
// file's full hash (when a FullHasher is available) and returns the asset whose
// full hash matches; if disambiguation is impossible it reports resolved=false.
func resolveAsset(fullHasher FullHasher, path string, candidates []ArchivedAsset) (ArchivedAsset, bool) {
	if len(candidates) == 1 {
		return candidates[0], true
	}
	if fullHasher == nil {
		return ArchivedAsset{}, false
	}
	full, err := fullHasher.FullHash(path)
	if err != nil || full == "" {
		return ArchivedAsset{}, false
	}
	for _, c := range candidates {
		if c.FullHash != "" && c.FullHash == full {
			return c, true
		}
	}
	return ArchivedAsset{}, false
}

// finalizeReport sets Safe and a human-readable Reason from the tallied counts.
// hasBackupDestination is whether any enabled required (non-mirror) backup
// provider exists; when false the archive is the only copy, so a would-be-safe
// verdict is downgraded to the distinct NoBackupDestination state.
func finalizeReport(r *SafeToEraseReport, hasBackupDestination bool) {
	if r.TotalMedia == 0 {
		r.Safe = true
		r.Reason = "No media files found on the volume — nothing would be lost by erasing it."
		return
	}

	// No backup destination configured: with zero enabled required (non-mirror)
	// providers the archive is the sole copy, so erasing sources can never be safe
	// even when every file is archived and verified. This supersedes the all-clear
	// branch below (which would otherwise pass for assets left carrying a stale
	// "complete" BackupStatus after their only destination was removed) and folds
	// the backup-pending files in — with no destination, "backup pending" and
	// "backed up" are indistinguishable. It applies only when there are no
	// genuinely-not-archived files (New/Unverified); those keep the alarming
	// wording below because adding a destination would not make them safe.
	if r.New == 0 && r.Unverified == 0 && !hasBackupDestination {
		r.Safe = false
		r.NoBackupDestination = true
		r.Reason = fmt.Sprintf(
			"All %d files are archived and verified, but no backup destination is configured — the archive is the only copy. Add a backup destination before erasing sources.",
			r.TotalMedia)
		return
	}

	if r.New == 0 && r.Unverified == 0 && r.BackupIncomplete == 0 {
		r.Safe = true
		r.Reason = fmt.Sprintf(
			"All %d media file(s) are archived, verified, and fully backed up — safe to erase.",
			r.TotalMedia)
		return
	}

	// Backups-only blocking case: every media file is already archived and
	// verified against the archive, and the ONLY thing not yet done is backing
	// some of them up. Nothing here is at risk of loss, so the message leads with
	// reassurance rather than the alarming "NOT recommended" wording reserved for
	// New/Unverified files (which genuinely are not safely archived).
	if r.New == 0 && r.Unverified == 0 && r.BackupIncomplete > 0 {
		r.Safe = false
		r.Reason = fmt.Sprintf(
			"All %d media file(s) are archived and verified. Deletion is not recommended yet: backups are still pending for %d.",
			r.TotalMedia, r.BackupIncomplete)
		return
	}

	var problems []string
	if r.New > 0 {
		problems = append(problems, fmt.Sprintf("%d not yet imported", r.New))
	}
	if r.Unverified > 0 {
		problems = append(problems, fmt.Sprintf("%d not verified against the archive", r.Unverified))
	}
	if r.BackupIncomplete > 0 {
		problems = append(problems, fmt.Sprintf("%d with incomplete backups", r.BackupIncomplete))
	}
	r.Safe = false
	r.Reason = fmt.Sprintf(
		"Deletion NOT recommended: of %d media file(s), %s.",
		r.TotalMedia, strings.Join(problems, ", "))
}
