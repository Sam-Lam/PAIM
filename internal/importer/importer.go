// Package importer implements PAIM's import pipeline: scanning a source tree,
// producing a non-mutating dry-run prediction, and running the copy-or-adopt
// import that verifies every file before recording it. All long-running
// operations honor context cancellation and report progress through a callback.
//
// # Two-stage Live Photo pairing
//
// A Live Photo is a still (HEIC/JPG) plus a companion MOV. The spec requires
// pairing by shared basename in the same directory AND matching exiftool
// ContentIdentifier when available. ContentIdentifier is only known after
// metadata extraction, which happens during import — not during the initial
// scan. The pipeline therefore pairs in two stages:
//
//  1. Scan time (provisional): candidates are grouped by directory+basename so
//     a still and a MOV sharing a name are flagged as a probable pair. This is
//     used only for reporting and to prime the metadata batch.
//  2. Import time (authoritative): once ContentIdentifier values are extracted,
//     mediatype.PairLivePhotos reconciles the provisional pairs — a mismatched
//     ContentIdentifier breaks a provisional pair, and only confirmed pairs are
//     linked via LivePhotoPartnerID.
//
// # Duplicate policy (documented once here)
//
// Identity is content only (quick hash, confirmed by full BLAKE3 on a quick-hash
// collision) — never filename, timestamp, or EXIF. For a source file whose
// content matches an existing VERIFIED asset:
//
//   - same OriginalFullPath  -> AlreadyImported: skipped, no new row, no copy
//     (re-importing or resuming the same tree never duplicates rows).
//   - different OriginalFullPath -> Duplicate: in COPY mode a row is recorded
//     with DuplicateOfAssetID set and CurrentArchivePath = "" (the bytes are NOT
//     copied); in ADOPT mode the in-place file is registered and flagged with
//     DuplicateOfAssetID (never deleted, never skipped silently).
//
// # Counters
//
// Session counters count Asset rows / files, so a dry run and its subsequent
// import always agree file-for-file. A Live Photo pair produces two linked rows
// (still + motion); collapsing a pair into a single logical asset for display is
// a presentation concern handled above this layer via LivePhotoPartnerID. This
// keeps dry-run predictions exactly reconcilable with import results.
package importer

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/metadata"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// subsystem is the mandatory logging subsystem tag for this package.
const subsystem = "import"

// partialPrefix is the temp-file prefix used for in-progress copies. Files with
// this prefix are never recorded as assets and are safe to delete on resume.
const partialPrefix = ".paim-partial-"

// Mode selects the import strategy.
type Mode string

const (
	// ModeCopy copies each source file into the archive, verifying the copy
	// before recording it.
	ModeCopy Mode = "copy"
	// ModeAdopt registers existing files in place without copying (Initialize),
	// computing a full BLAKE3 integrity baseline instead of a copy-compare.
	ModeAdopt Mode = "adopt"
	// ModeReorganize is the notes marker for a standalone reorganize-maintenance
	// session (RunReorganize). It never drives the copy/adopt per-file loop; it
	// exists so Import History can badge the run distinctly and so a resume of a
	// reorganize session (which records no source root) refuses cleanly rather
	// than misinterpreting it as an import.
	ModeReorganize Mode = "reorganize"
)

// BackupEnqueuer enqueues backup work for a freshly recorded asset. It is a
// narrow local interface (defined at the point of consumption) so the importer
// never imports internal/backup. Implementations run inside the same DB
// transaction that inserts the asset, so the enqueue is atomic with the import.
// EnqueueForAsset returns the number of backup jobs it created, so the caller
// can record BackupStatus=pending only when there is actually backup work
// (zero enabled providers => zero jobs => BackupStatus=none).
type BackupEnqueuer interface {
	EnqueueForAsset(ctx context.Context, tx *gorm.DB, assetID string) (int, error)
}

// NoopBackupEnqueuer is a BackupEnqueuer that does nothing. It is used in tests
// and by callers that have not configured backups.
type NoopBackupEnqueuer struct{}

// EnqueueForAsset does nothing, creates no jobs, and never fails.
func (NoopBackupEnqueuer) EnqueueForAsset(context.Context, *gorm.DB, string) (int, error) {
	return 0, nil
}

// Options configures a single import run.
type Options struct {
	Mode            Mode
	SourceRoot      string
	DestinationRoot string
	EventName       string
	SourceID        string
	// Reorganize (adopt mode only) moves adopted files into the standard layout
	// via same-volume atomic rename. Cross-volume moves are refused and the file
	// is left in place (recorded in the session notes).
	Reorganize bool
	// Concurrency bounds the hashing worker pool. Zero means runtime.NumCPU.
	Concurrency int
	// Precomputed, when non-nil, supplies quick/full hashes from a prior DryRun so
	// they are not recomputed. Its per-path hashes are reused when the path and
	// size still match.
	Precomputed *DryRunReport
}

func (o Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	return runtime.NumCPU()
}

func (o Options) mode() Mode {
	if o.Mode == "" {
		return ModeCopy
	}
	return o.Mode
}

// Phase names the stage a Progress update belongs to.
type Phase string

const (
	PhaseScanning     Phase = "scanning"
	PhaseHashing      Phase = "hashing"
	PhaseImporting    Phase = "importing"
	PhaseReorganizing Phase = "reorganizing"
	PhaseDone         Phase = "done"
)

// Progress is a snapshot of a long-running operation. The caller is responsible
// for throttling; the pipeline reports at least once per file.
type Progress struct {
	Phase       Phase
	FilesDone   int
	FilesTotal  int
	BytesDone   int64
	BytesTotal  int64
	CurrentFile string
	Errors      int
}

// ProgressFunc receives Progress updates. A nil ProgressFunc is tolerated.
type ProgressFunc func(Progress)

func (f ProgressFunc) emit(p Progress) {
	if f != nil {
		f(p)
	}
}

// Pipeline is the import engine. Construct it with New; the zero value is not
// usable. It is safe for sequential use; a single Pipeline should run one
// import at a time.
type Pipeline struct {
	db        *gorm.DB
	assets    *repo.AssetRepo
	sessions  *repo.SessionRepo
	extractor metadata.Extractor
	layout    *archive.Layout
	log       *slog.Logger
	backup    BackupEnqueuer

	// libraryRoot, when set, is the portable-library root against which archive
	// paths are stored relative (forward slashes) and resolved back to absolute.
	// Empty (the dev escape hatch and most tests) stores/reads absolute paths.
	libraryRoot string

	// afterCopyHook, when non-nil, is invoked with the partial file path after a
	// copy completes but before verification. It exists solely to let tests
	// corrupt the partial and exercise the verification-failure path.
	afterCopyHook func(partialPath string)
}

// Config carries the dependencies for a Pipeline. All fields except Logger and
// Backup are required.
type Config struct {
	DB        *gorm.DB
	Assets    *repo.AssetRepo
	Sessions  *repo.SessionRepo
	Extractor metadata.Extractor
	Layout    *archive.Layout
	Logger    *slog.Logger
	Backup    BackupEnqueuer
	// LibraryRoot enables portable-library relative archive paths (see Pipeline).
	// Empty preserves the historical absolute-path behavior.
	LibraryRoot string
}

// New constructs a Pipeline from cfg, applying sane defaults for the optional
// Logger (slog.Default) and Backup (no-op) dependencies.
func New(cfg Config) *Pipeline {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("subsystem", subsystem))

	backup := cfg.Backup
	if backup == nil {
		backup = NoopBackupEnqueuer{}
	}

	return &Pipeline{
		db:          cfg.DB,
		assets:      cfg.Assets,
		sessions:    cfg.Sessions,
		extractor:   cfg.Extractor,
		layout:      cfg.Layout,
		log:         log,
		backup:      backup,
		libraryRoot: cfg.LibraryRoot,
	}
}

// effectiveLayout returns a Layout rooted at destinationRoot but preserving the
// configured template (year/date/RAW folder names). destinationRoot wins over
// the injected layout's MasterRoot; an empty destinationRoot falls back to it.
func (p *Pipeline) effectiveLayout(destinationRoot string) *archive.Layout {
	root := destinationRoot
	if root == "" && p.layout != nil {
		root = p.layout.MasterRoot
	}
	lay := archive.New(root)
	if p.layout != nil {
		if p.layout.YearLayout != "" {
			lay.YearLayout = p.layout.YearLayout
		}
		if p.layout.DateLayout != "" {
			lay.DateLayout = p.layout.DateLayout
		}
		if p.layout.RawSubfolder != "" {
			lay.RawSubfolder = p.layout.RawSubfolder
		}
	}
	return lay
}
