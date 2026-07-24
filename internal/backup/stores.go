package backup

import (
	"context"
	"fmt"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"gorm.io/gorm"
)

// The Manager depends on three narrow interfaces defined here at the point of
// consumption (per the architecture spec) so it is testable with fakes and does
// not couple to concrete repositories.
//
// JobQueue is the subset of the backup-job repository the Manager uses, plus two
// capabilities repo.BackupRepo does not expose directly:
//
//   - JobsForAsset, needed to recompute an asset's aggregate BackupStatus across
//     all of its jobs.
//   - WithTx returning JobQueue, needed so EnqueueForAsset can enqueue inside a
//     caller-supplied transaction. (repo.BackupRepo.WithTx returns its own
//     concrete type, which does not satisfy the interface, so the adapter below
//     bridges it.)
//
// RepoJobQueue adapts *repo.BackupRepo to this interface; main.go wires it with
// backup.NewRepoJobQueue(db).
type JobQueue interface {
	// ClaimNextForProvider claims one provider's next pending job, honoring its
	// upload order (newestFirst -> highest SortKey first). The Manager iterates
	// eligible providers so it can round-robin and skip cooling ones.
	ClaimNextForProvider(ctx context.Context, destination string, newestFirst bool) (*domain.BackupJob, error)
	GetByID(ctx context.Context, id string) (*domain.BackupJob, error)
	MarkCompleted(ctx context.Context, id string) error
	MarkCompletedWithNote(ctx context.Context, id, note string) error
	MarkFailed(ctx context.Context, id, errMsg string) error
	Requeue(ctx context.Context, id string) error
	// RequeueAllFailed transitions every failed job to pending in one UPDATE,
	// returning the count (the bulk form used by "Retry all failed").
	RequeueAllFailed(ctx context.Context) (int64, error)
	// CancelAllPendingPaused transitions every pending or paused job to cancelled in
	// one UPDATE, returning the count (the bulk form used by "Cancel all pending").
	// Running jobs are untouched.
	CancelAllPendingPaused(ctx context.Context) (int64, error)
	// RevertToPending returns a running job to pending without a retry increment
	// (used for provider cooldowns, where abandoning the attempt is not a failure).
	RevertToPending(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Cancel(ctx context.Context, id string) error
	ResetRunningOnStartup(ctx context.Context) (int64, error)
	ListJobs(ctx context.Context, status *domain.JobStatus, page repo.Page) ([]domain.BackupJob, int64, error)
	Enqueue(ctx context.Context, assetID, plugin, destination string, sortKey int64) (*domain.BackupJob, bool, error)
	// EnqueueOptedOut idempotently records an opted-out job (durable per-import
	// provider opt-out) for the triple, sharing Enqueue's idempotency set.
	EnqueueOptedOut(ctx context.Context, assetID, plugin, destination string, sortKey int64) (*domain.BackupJob, bool, error)

	// JobsForAsset returns every non-deleted job belonging to assetID.
	JobsForAsset(ctx context.Context, assetID string) ([]domain.BackupJob, error)
	// WithTx binds the queue to a transaction handle.
	WithTx(tx *gorm.DB) JobQueue
}

// AssetStore is the subset of the asset repository the Manager needs: it reads an
// asset (for its archive path) and writes back the recomputed aggregate backup
// status. *repo.AssetRepo satisfies it directly.
type AssetStore interface {
	GetByID(ctx context.Context, id string) (*domain.Asset, error)
	UpdateBackupStatus(ctx context.Context, id string, status domain.BackupStatus) error
}

// ProviderStore lists and resolves configured backup providers. repo has no
// provider repository yet, so RepoProviderStore (below) provides a gorm-backed
// implementation; main.go wires it with backup.NewRepoProviderStore(db).
type ProviderStore interface {
	// ListEnabled returns all enabled, non-deleted providers.
	ListEnabled(ctx context.Context) ([]domain.BackupProvider, error)
	// GetByID returns a single provider (enabled or not) by ID, or ErrNotFound.
	GetByID(ctx context.Context, id string) (*domain.BackupProvider, error)
	// MirrorIDs returns the set of provider IDs marked Mirror, so the Manager can
	// exclude their jobs from the aggregate BackupStatus (mirrors never block a
	// safety verdict).
	MirrorIDs(ctx context.Context) (map[string]bool, error)
	// WithTx binds the store to a transaction handle.
	WithTx(tx *gorm.DB) ProviderStore
}

// RepoJobQueue adapts *repo.BackupRepo to JobQueue. It embeds the repo so all of
// its methods are promoted, and adds JobsForAsset (a jobs-by-asset query the repo
// does not expose) and a WithTx that returns the interface type.
type RepoJobQueue struct {
	*repo.BackupRepo
	db *gorm.DB
}

// NewRepoJobQueue constructs a RepoJobQueue over the given database handle.
func NewRepoJobQueue(db *gorm.DB) *RepoJobQueue {
	return &RepoJobQueue{BackupRepo: repo.NewBackupRepo(db), db: db}
}

// WithTx binds the queue (and its underlying repo) to tx.
func (q *RepoJobQueue) WithTx(tx *gorm.DB) JobQueue {
	return &RepoJobQueue{BackupRepo: q.BackupRepo.WithTx(tx), db: tx}
}

// JobsForAsset returns every non-deleted job for assetID.
func (q *RepoJobQueue) JobsForAsset(ctx context.Context, assetID string) ([]domain.BackupJob, error) {
	var jobs []domain.BackupJob
	if err := q.db.WithContext(ctx).Where("asset_id = ?", assetID).Find(&jobs).Error; err != nil {
		return nil, fmt.Errorf("backup: jobs for asset %q: %w", assetID, err)
	}
	return jobs, nil
}

var _ JobQueue = (*RepoJobQueue)(nil)

// RepoProviderStore is a gorm-backed ProviderStore over domain.BackupProvider.
type RepoProviderStore struct {
	db *gorm.DB
}

// NewRepoProviderStore constructs a RepoProviderStore over the given handle.
func NewRepoProviderStore(db *gorm.DB) *RepoProviderStore {
	return &RepoProviderStore{db: db}
}

// WithTx binds the store to tx.
func (s *RepoProviderStore) WithTx(tx *gorm.DB) ProviderStore {
	return &RepoProviderStore{db: tx}
}

// ListEnabled returns all enabled, non-deleted providers ordered by creation.
func (s *RepoProviderStore) ListEnabled(ctx context.Context) ([]domain.BackupProvider, error) {
	var providers []domain.BackupProvider
	err := s.db.WithContext(ctx).
		Where("enabled = ?", true).
		Order("created_at ASC, id ASC").
		Find(&providers).Error
	if err != nil {
		return nil, fmt.Errorf("backup: list enabled providers: %w", err)
	}
	return providers, nil
}

// MirrorIDs returns the set of enabled-or-disabled provider IDs marked Mirror.
func (s *RepoProviderStore) MirrorIDs(ctx context.Context) (map[string]bool, error) {
	var ids []string
	if err := s.db.WithContext(ctx).
		Model(&domain.BackupProvider{}).
		Where("mirror = ?", true).
		Pluck("id", &ids).Error; err != nil {
		return nil, fmt.Errorf("backup: list mirror provider ids: %w", err)
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// GetByID returns the provider with the given ID, or repo.ErrNotFound.
func (s *RepoProviderStore) GetByID(ctx context.Context, id string) (*domain.BackupProvider, error) {
	var p domain.BackupProvider
	err := s.db.WithContext(ctx).First(&p, "id = ?", id).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("backup: get provider %q: %w", id, repo.ErrNotFound)
		}
		return nil, fmt.Errorf("backup: get provider %q: %w", id, err)
	}
	return &p, nil
}

var _ ProviderStore = (*RepoProviderStore)(nil)
