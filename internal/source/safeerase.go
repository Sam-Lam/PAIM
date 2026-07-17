package source

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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
}

// AssetLookup finds archived assets by content hash. Implemented over the asset
// repo and wired in main.go/services; kept narrow so this package stays testable
// with fakes.
type AssetLookup interface {
	// FindByQuickHash returns every non-deleted asset sharing the quick hash.
	FindByQuickHash(ctx context.Context, quickHash string) ([]ArchivedAsset, error)
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
}

// EvaluateSafeToErase walks the media files under root and reports whether the
// source is safe to erase. Every media file must map — by quick hash, confirmed
// by full hash on a quick-hash collision — to a verified asset whose required
// backups are complete. Any New, Unverified, or BackupIncomplete file makes the
// volume unsafe.
//
// lookup and isMedia are injected (rather than being constructor dependencies of
// the Identifier) because they are only needed for this evaluation.
func (id *Identifier) EvaluateSafeToErase(
	ctx context.Context,
	sourceID string,
	root string,
	lookup AssetLookup,
	isMedia func(ext string) bool,
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

		report.TotalMedia++
		classifyFile(ctx, id.hasher, fullHasher, lookup, path, report)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("source: walk %q for safe-to-erase: %w", root, walkErr)
	}

	finalizeReport(report)
	return report, nil
}

// classifyFile buckets a single media file into exactly one of the report's
// counters (Archived, New, Unverified, BackupIncomplete).
func classifyFile(
	ctx context.Context,
	hasher FileHasher,
	fullHasher FullHasher,
	lookup AssetLookup,
	path string,
	report *SafeToEraseReport,
) {
	quick, err := hasher.QuickHash(path)
	if err != nil {
		// Unreadable/unhashable source file: cannot prove it is archived.
		report.Unverified++
		return
	}
	candidates, err := lookup.FindByQuickHash(ctx, quick)
	if err != nil || len(candidates) == 0 {
		report.New++
		return
	}

	// Narrow to the asset that actually corresponds to this file. With a single
	// candidate the quick hash is accepted; with several (a quick-hash collision)
	// the full hash disambiguates.
	asset, resolved := resolveAsset(fullHasher, path, candidates)
	if !resolved {
		// Could not confirm which colliding asset this file is: be conservative.
		report.Unverified++
		return
	}

	switch {
	case !asset.Verified:
		report.Unverified++
	case !asset.BackupComplete:
		report.BackupIncomplete++
	default:
		report.Archived++
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
func finalizeReport(r *SafeToEraseReport) {
	if r.TotalMedia == 0 {
		r.Safe = true
		r.Reason = "No media files found on the volume — nothing would be lost by erasing it."
		return
	}
	if r.New == 0 && r.Unverified == 0 && r.BackupIncomplete == 0 {
		r.Safe = true
		r.Reason = fmt.Sprintf(
			"All %d media file(s) are archived, verified, and fully backed up — safe to erase.",
			r.TotalMedia)
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
