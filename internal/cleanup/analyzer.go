// Package cleanup implements PAIM's Cleanup Assistant: a strictly read-only
// analysis of an arbitrary folder that classifies every media file against the
// archive and produces a delete-safety recommendation.
//
// Analyze performs NO database writes and NO filesystem writes. It only reads
// files (to hash them) and issues SELECT-style repository lookups. The resulting
// Report drives Recommendation, which follows the Safe Delete Rules: deletion is
// recommended only when every media file is already archived, those assets are
// verified, their required backups are complete, and the database is consistent.
//
// # Classification
//
// Each media file is placed in exactly one class:
//
//   - already_archived: quick-hash match to an archive asset, confirmed by a full
//     BLAKE3 comparison, where the matched asset is not itself a duplicate and is
//     not verification-failed.
//   - duplicate: the file's content (full hash) matches an archive asset that is
//     itself a duplicate, OR the same content appears a second (or later) time
//     within the scanned folder.
//   - new: no archive asset shares the content (either no quick-hash candidate,
//     or quick-hash collided but no full-hash match).
//   - verification_failed: the file matches an asset whose VerificationStatus is
//     failed, or the file could not be read while computing its full hash.
//   - unknown: a non-media file, or a media file that could not be read at all
//     (open/quick-hash failure).
//
// Non-media files are reported (as unknown) for transparency but do not, by
// themselves, block a deletion recommendation — the Safe Delete Rules are scoped
// to media files. An unreadable *media* file, however, does block.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/hashing"
	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
)

// maxFilesPerClass caps how many file paths are retained per class in a Report.
// Beyond the cap the count and bytes keep accumulating but the file list stops
// growing and Truncated is set.
const maxFilesPerClass = 10000

// Class is a cleanup classification bucket.
type Class string

// Class values.
const (
	ClassAlreadyArchived    Class = "already_archived"
	ClassDuplicate          Class = "duplicate"
	ClassNew                Class = "new"
	ClassVerificationFailed Class = "verification_failed"
	ClassUnknown            Class = "unknown"
)

// AllClasses lists every class in report order.
func AllClasses() []Class {
	return []Class{ClassAlreadyArchived, ClassDuplicate, ClassNew, ClassVerificationFailed, ClassUnknown}
}

// AssetLookup is the read-only subset of the asset repository the Analyzer needs.
// *repo.AssetRepo satisfies it. Only lookups are used; the Analyzer never writes.
type AssetLookup interface {
	FindByQuickHash(ctx context.Context, quickHash string) ([]domain.Asset, error)
	FindByFullHash(ctx context.Context, fullHash string) ([]domain.Asset, error)
}

// QuickHashFn computes a file's quick hash. Defaults to hashing.QuickHash.
type QuickHashFn func(path string) (string, error)

// FullHashFn computes a file's full hash. Defaults to hashing.FullHash.
type FullHashFn func(ctx context.Context, path string) (string, error)

// ProgressFn is called once per processed file with the running processed count
// and the current path. It must be non-blocking.
type ProgressFn func(processed int, path string)

// Analyzer performs read-only cleanup analysis.
type Analyzer struct {
	assets    AssetLookup
	quickHash QuickHashFn
	fullHash  FullHashFn
	log       *slog.Logger

	// Root is the portable-library root used to resolve assets' relative
	// CurrentArchivePath values to absolute paths before reading/stat'ing the
	// archived files. Empty leaves stored paths as-is (dev/legacy absolute paths).
	Root string
}

// NewAnalyzer constructs an Analyzer. Nil hashing funcs fall back to the hashing
// package; a nil logger falls back to slog.Default().
func NewAnalyzer(assets AssetLookup, quickHash QuickHashFn, fullHash FullHashFn, logger *slog.Logger) *Analyzer {
	if quickHash == nil {
		quickHash = hashing.QuickHash
	}
	if fullHash == nil {
		fullHash = hashing.FullHash
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Analyzer{assets: assets, quickHash: quickHash, fullHash: fullHash, log: logger}
}

// ClassStat holds the per-class rollup. Files is capped at maxFilesPerClass;
// Truncated indicates the list (not the counts) was truncated.
type ClassStat struct {
	Count     int      `json:"count"`
	Bytes     int64    `json:"bytes"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

func (s *ClassStat) add(path string, size int64, cap int, log *slog.Logger, class Class) {
	s.Count++
	s.Bytes += size
	if len(s.Files) < cap {
		s.Files = append(s.Files, path)
	} else if !s.Truncated {
		s.Truncated = true
		log.Warn("cleanup: class file list truncated", "class", string(class), "cap", cap)
	}
}

// Report is the read-only result of an analysis.
type Report struct {
	Root string `json:"root"`

	// Classes holds the per-class rollups keyed by Class.
	Classes map[Class]*ClassStat `json:"classes"`

	TotalFiles int `json:"totalFiles"` // every regular file walked
	MediaFiles int `json:"mediaFiles"` // files with a recognized media extension
	NonMedia   int `json:"nonMedia"`   // non-media files (a subset of unknown)

	// UnreadableMedia counts media files that could not be read at all (a subset
	// of unknown). Unlike non-media unknowns, these block deletion.
	UnreadableMedia int `json:"unreadableMedia"`

	// ArchivedNotVerified counts already_archived files whose matched asset is not
	// yet verified (VerificationStatus != verified).
	ArchivedNotVerified int `json:"archivedNotVerified"`

	// BackupIncomplete counts already_archived files whose matched asset's
	// required backups are not complete (BackupStatus != complete).
	BackupIncomplete int `json:"backupIncomplete"`

	// DuplicatesArchived counts duplicate files whose content is confirmed
	// preserved in the archive — it maps (by full hash) to a verified,
	// fully-backed, non-duplicate archived asset whose copy is present on disk.
	// Such duplicates are effectively already_archived for delete-safety purposes
	// (the bytes are safe in the archive), so they do NOT block a deletion
	// recommendation even though they remain classified as duplicates for display.
	DuplicatesArchived int `json:"duplicatesArchived"`

	// DBInconsistencies counts already_archived matches whose archive file is
	// missing or unreadable on disk (the DB claims an archived copy that is gone).
	DBInconsistencies int `json:"dbInconsistencies"`
}

// Class returns the stat for c, allocating it if necessary.
func (r *Report) Class(c Class) *ClassStat {
	s := r.Classes[c]
	if s == nil {
		s = &ClassStat{}
		r.Classes[c] = s
	}
	return s
}

// Count returns the count for class c (0 if absent).
func (r *Report) Count(c Class) int {
	if s := r.Classes[c]; s != nil {
		return s.Count
	}
	return 0
}

// Bytes returns the byte total for class c (0 if absent).
func (r *Report) Bytes(c Class) int64 {
	if s := r.Classes[c]; s != nil {
		return s.Bytes
	}
	return 0
}

func newReport(root string) *Report {
	r := &Report{Root: root, Classes: make(map[Class]*ClassStat, len(AllClasses()))}
	for _, c := range AllClasses() {
		r.Classes[c] = &ClassStat{}
	}
	return r
}

// Analyze walks root recursively and classifies every regular file. It is
// strictly read-only. progressFn (optional) is called once per file.
func (a *Analyzer) Analyze(ctx context.Context, root string, progressFn ProgressFn) (*Report, error) {
	report := newReport(root)

	// folderQuick tracks, per quick hash, the files already seen within the
	// scanned folder so a second occurrence of identical content (confirmed by
	// full hash) is classified as a duplicate. Full hashes are computed lazily.
	folderQuick := make(map[string][]*folderFile)

	processed := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read: log and skip its subtree, do not abort.
			a.log.Warn("cleanup: skipping unreadable path", "path", path, "error", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, devices, sockets
		}

		report.TotalFiles++
		a.classify(ctx, report, folderQuick, path, d)

		processed++
		if progressFn != nil {
			progressFn(processed, path)
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("cleanup: analyze %q cancelled: %w", root, walkErr)
		}
		return nil, fmt.Errorf("cleanup: analyze %q: %w", root, walkErr)
	}

	a.log.Info("cleanup: analysis complete",
		"root", root, "totalFiles", report.TotalFiles, "mediaFiles", report.MediaFiles,
		"alreadyArchived", report.Count(ClassAlreadyArchived), "new", report.Count(ClassNew),
		"duplicate", report.Count(ClassDuplicate), "verificationFailed", report.Count(ClassVerificationFailed),
		"unknown", report.Count(ClassUnknown), "backupIncomplete", report.BackupIncomplete)
	return report, nil
}

// folderFile records a file seen during the walk and its (lazily computed) full
// hash so later files with the same quick hash can be confirmed as duplicates.
type folderFile struct {
	path   string
	full   string
	hashed bool
}

// classify determines the class of a single file and records it in the report.
func (a *Analyzer) classify(ctx context.Context, report *Report, folderQuick map[string][]*folderFile, path string, d fs.DirEntry) {
	ext := filepath.Ext(path)

	size := int64(0)
	if info, err := d.Info(); err == nil {
		size = info.Size()
	}

	// Non-media files are unknown and non-blocking.
	if !mediatype.IsMedia(ext) {
		report.NonMedia++
		report.Class(ClassUnknown).add(path, size, maxFilesPerClass, a.log, ClassUnknown)
		return
	}

	report.MediaFiles++

	qh, err := a.quickHash(path)
	if err != nil {
		// Cannot read the media file at all -> unknown, and a blocking condition.
		a.log.Warn("cleanup: media file unreadable (quick hash)", "path", path, "error", err)
		report.UnreadableMedia++
		report.Class(ClassUnknown).add(path, size, maxFilesPerClass, a.log, ClassUnknown)
		return
	}

	candidates, err := a.assets.FindByQuickHash(ctx, qh)
	if err != nil {
		a.log.Error("cleanup: quick-hash lookup failed", "path", path, "error", err)
		report.Class(ClassUnknown).add(path, size, maxFilesPerClass, a.log, ClassUnknown)
		report.UnreadableMedia++ // treat lookup failure conservatively as blocking
		return
	}

	prior := folderQuick[qh]
	needFull := len(candidates) > 0 || len(prior) > 0

	var fh string
	if needFull {
		fh, err = a.fullHash(ctx, path)
		if err != nil {
			// Read failure mid full-hash -> verification_failed (blocking).
			a.log.Warn("cleanup: media file unreadable (full hash)", "path", path, "error", err)
			report.Class(ClassVerificationFailed).add(path, size, maxFilesPerClass, a.log, ClassVerificationFailed)
			folderQuick[qh] = append(prior, &folderFile{path: path})
			return
		}
		// Second (or later) occurrence of identical content within this folder.
		// Prior files' full hashes are computed lazily here for the comparison.
		for _, e := range prior {
			if !e.hashed {
				if h, herr := a.fullHash(ctx, e.path); herr == nil {
					e.full = h
				}
				e.hashed = true
			}
			if e.full != "" && e.full == fh {
				// An in-folder duplicate. Its content may nonetheless already live
				// safely in the archive (e.g. an in-folder copy of an archived
				// original); record that so it does not needlessly block deletion.
				a.recordDuplicate(ctx, report, path, size, candidates, fh)
				folderQuick[qh] = append(prior, &folderFile{path: path, full: fh, hashed: true})
				return
			}
		}
	}
	folderQuick[qh] = append(prior, &folderFile{path: path, full: fh, hashed: needFull})

	if len(candidates) == 0 {
		report.Class(ClassNew).add(path, size, maxFilesPerClass, a.log, ClassNew)
		return
	}

	matched := a.matchArchive(ctx, candidates, fh)
	if matched == nil {
		// Quick-hash collision but no full-hash match: genuinely new content.
		report.Class(ClassNew).add(path, size, maxFilesPerClass, a.log, ClassNew)
		return
	}

	a.classifyMatched(ctx, report, path, size, matched, candidates, fh)
}

// recordDuplicate files path under the duplicate class and, when its content is
// confirmed preserved in the archive (a verified, fully-backed, non-duplicate
// archived asset with the same full hash and a present copy on disk), also counts
// it under DuplicatesArchived so the recommendation treats it as safe.
func (a *Analyzer) recordDuplicate(ctx context.Context, report *Report, path string, size int64, candidates []domain.Asset, fh string) {
	report.Class(ClassDuplicate).add(path, size, maxFilesPerClass, a.log, ClassDuplicate)
	if a.contentSafelyArchived(ctx, candidates, fh) {
		report.DuplicatesArchived++
	}
}

// contentSafelyArchived reports whether some candidate is a verified, fully-
// backed, non-duplicate archived asset whose content (full hash) equals fh and
// whose archived copy still exists on disk — i.e. the duplicate's bytes are
// safely preserved in the archive.
func (a *Analyzer) contentSafelyArchived(ctx context.Context, candidates []domain.Asset, fh string) bool {
	if fh == "" {
		return false
	}
	for i := range candidates {
		c := &candidates[i]
		if c.DuplicateOfAssetID != nil && *c.DuplicateOfAssetID != "" {
			continue // a duplicate placeholder is not itself an archived copy
		}
		if c.VerificationStatus != domain.VerificationStatusVerified {
			continue
		}
		if c.BackupStatus != domain.BackupStatusComplete {
			continue
		}
		cFull := c.FullHash
		archiveAbs := library.ResolvePath(a.Root, c.CurrentArchivePath)
		if cFull == "" {
			computed, err := a.fullHash(ctx, archiveAbs)
			if err != nil {
				continue
			}
			cFull = computed
		}
		if cFull != fh {
			continue
		}
		// The content matches a fully-backed verified asset; require its archived
		// copy to actually exist on disk before declaring the bytes preserved.
		if _, err := os.Stat(archiveAbs); err != nil {
			continue
		}
		return true
	}
	return false
}

// matchArchive returns the candidate whose content equals fh, confirming by the
// candidate's stored full hash when present or by reading its archive file
// otherwise. It returns nil when no candidate's content matches.
func (a *Analyzer) matchArchive(ctx context.Context, candidates []domain.Asset, fh string) *domain.Asset {
	for i := range candidates {
		c := &candidates[i]
		cFull := c.FullHash
		if cFull == "" {
			// Confirm by reading the archived file (read-only; no DB backfill).
			archiveAbs := library.ResolvePath(a.Root, c.CurrentArchivePath)
			computed, err := a.fullHash(ctx, archiveAbs)
			if err != nil {
				// Cannot confirm this candidate; skip it (its inconsistency, if any,
				// is captured when a match is finally made and stat'd).
				a.log.Warn("cleanup: cannot read archived file to confirm match",
					"asset", c.ID, "path", archiveAbs, "error", err)
				continue
			}
			cFull = computed
		}
		if cFull == fh {
			return c
		}
	}
	return nil
}

// classifyMatched records a file that matched an archive asset, splitting into
// verification_failed / duplicate / already_archived and updating the blocking
// counters.
func (a *Analyzer) classifyMatched(ctx context.Context, report *Report, path string, size int64, matched *domain.Asset, candidates []domain.Asset, fh string) {
	switch {
	case matched.VerificationStatus == domain.VerificationStatusFailed:
		report.Class(ClassVerificationFailed).add(path, size, maxFilesPerClass, a.log, ClassVerificationFailed)
		return
	case matched.DuplicateOfAssetID != nil && *matched.DuplicateOfAssetID != "":
		// The file matched a duplicate placeholder row. Its content is still safe if
		// another candidate is the real, fully-backed archived original.
		a.recordDuplicate(ctx, report, path, size, candidates, fh)
		return
	}

	// already_archived. Verify the archived copy actually exists on disk; a
	// missing copy is a DB/filesystem inconsistency and blocks deletion.
	archiveAbs := library.ResolvePath(a.Root, matched.CurrentArchivePath)
	if _, err := os.Stat(archiveAbs); err != nil {
		a.log.Warn("cleanup: archived copy missing on disk",
			"asset", matched.ID, "path", archiveAbs, "error", err)
		report.DBInconsistencies++
	}
	if matched.VerificationStatus != domain.VerificationStatusVerified {
		report.ArchivedNotVerified++
	}
	if matched.BackupStatus != domain.BackupStatusComplete {
		report.BackupIncomplete++
	}
	report.Class(ClassAlreadyArchived).add(path, size, maxFilesPerClass, a.log, ClassAlreadyArchived)
}
