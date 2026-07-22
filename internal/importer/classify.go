package importer

import (
	"context"
	"fmt"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/library"
)

// Disposition is the classification of a scanned file against the asset DB.
type Disposition string

const (
	// DispositionNew: content not present in the library; will be imported.
	DispositionNew Disposition = "new"
	// DispositionDuplicate: content matches an existing verified asset at a
	// different origin path; recorded as a duplicate, not copied.
	DispositionDuplicate Disposition = "duplicate"
	// DispositionAlreadyImported: content matches an existing verified asset that
	// was imported from this exact source path; skipped, no new row.
	DispositionAlreadyImported Disposition = "already_imported"
)

// classification is the resolved identity decision for a single file.
type classification struct {
	Disposition Disposition
	QuickHash   string
	// FullHash is set whenever a quick-hash collision forced full hashing (i.e.
	// for Duplicate and AlreadyImported, and for New-after-collision).
	FullHash string
	// MatchedAssetID is the canonical asset this file duplicates or re-imports
	// (empty for New).
	MatchedAssetID string
}

// classify decides whether a file is new, a duplicate, or already imported. It
// performs Stage 1 (quick hash lookup) and, on a collision, Stage 2 (full-hash
// confirmation), backfilling the existing asset's FullHash when it was null. It
// never writes anything except the FullHash backfill. quickHash and fullHash may
// be supplied (reused from a dry run, already staleness-gated by the caller) or
// empty to compute them here. A supplied fullHash is threaded onto the returned
// classification even for a brand-new file so adopt mode can reuse it as the
// in-place baseline instead of re-reading the whole file.
func (p *Pipeline) classify(ctx context.Context, path string, quickHash, fullHash string) (classification, error) {
	if quickHash == "" {
		qh, err := p.quickHash(path)
		if err != nil {
			return classification{}, fmt.Errorf("classify: quick hash %q: %w", path, err)
		}
		quickHash = qh
	}

	candidates, err := p.assets.FindByQuickHash(ctx, quickHash)
	if err != nil {
		return classification{}, fmt.Errorf("classify: lookup quick hash for %q: %w", path, err)
	}
	// Only verified assets anchor identity; ignore duplicate placeholder rows
	// that carry no archived file (they are already resolved elsewhere).
	verified := candidates[:0]
	for _, a := range candidates {
		if a.VerificationStatus == domain.VerificationStatusVerified {
			verified = append(verified, a)
		}
	}
	if len(verified) == 0 {
		// New content. Carry a precomputed full hash forward (adopt baseline reuse);
		// it is empty for the common case where the dry run only quick-hashed.
		return classification{Disposition: DispositionNew, QuickHash: quickHash, FullHash: fullHash}, nil
	}

	// Stage 2: confirm identity with a full hash (reusing a precomputed one).
	if fullHash == "" {
		fh, err := p.fullHash(ctx, path)
		if err != nil {
			return classification{}, fmt.Errorf("classify: full hash %q: %w", path, err)
		}
		fullHash = fh
	}

	var duplicateOf string
	for i := range verified {
		a := &verified[i]
		existingFull := a.FullHash
		if existingFull == "" {
			// Backfill from the archived file so future collisions are cheap.
			if a.CurrentArchivePath == "" {
				continue
			}
			archiveAbs := library.ResolvePath(p.libraryRoot, a.CurrentArchivePath)
			computed, herr := p.fullHash(ctx, archiveAbs)
			if herr != nil {
				// The archived file may be offline; skip this candidate rather than
				// fail the whole classification.
				p.log.Warn("classify: cannot backfill full hash of existing asset",
					"assetId", a.ID, "path", archiveAbs, "error", herr.Error())
				continue
			}
			existingFull = computed
			if uerr := p.assets.UpdateFullHash(ctx, a.ID, computed); uerr != nil {
				p.log.Warn("classify: cannot persist backfilled full hash",
					"assetId", a.ID, "error", uerr.Error())
			}
		}
		if existingFull != fullHash {
			continue // genuine quick-hash collision on distinct content
		}
		// Confirmed identical content. A match on either the original source path
		// or the current archive path means this exact file is already registered
		// (the latter covers re-scanning after an adopt+reorganize move).
		if a.OriginalFullPath == path || library.ResolvePath(p.libraryRoot, a.CurrentArchivePath) == path {
			return classification{
				Disposition:    DispositionAlreadyImported,
				QuickHash:      quickHash,
				FullHash:       fullHash,
				MatchedAssetID: a.ID,
			}, nil
		}
		if duplicateOf == "" {
			duplicateOf = a.ID
		}
	}

	if duplicateOf != "" {
		return classification{
			Disposition:    DispositionDuplicate,
			QuickHash:      quickHash,
			FullHash:       fullHash,
			MatchedAssetID: duplicateOf,
		}, nil
	}
	// Quick-hash collision but no full-hash match: genuinely new content.
	return classification{Disposition: DispositionNew, QuickHash: quickHash, FullHash: fullHash}, nil
}
