package backup_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/backup"
	"github.com/autolinepro/paim/internal/db"
	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/repo"
	"gorm.io/gorm"
)

// fakePlugin is a controllable backup.Plugin that records calls instead of
// touching a real destination.
type fakePlugin struct {
	name string
	caps backup.Capabilities

	mu           sync.Mutex
	uploads      int
	verifies     int
	deletes      int
	initialized  int
	failUploads  bool // when true, every Upload returns an error
	verifyResult bool // value returned by Verify when it does not error
	uploadDelay  time.Duration
	lastUpload   string
}

func (f *fakePlugin) Name() string { return f.name }

func (f *fakePlugin) Initialize(ctx context.Context, configJSON string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initialized++
	return nil
}

func (f *fakePlugin) Authenticate(ctx context.Context) error { return nil }

func (f *fakePlugin) Upload(ctx context.Context, localPath, remoteRelPath string, progressFn func(bytesDone, bytesTotal int64)) error {
	if f.uploadDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.uploadDelay):
		}
	}
	f.mu.Lock()
	f.uploads++
	f.lastUpload = remoteRelPath
	fail := f.failUploads
	f.mu.Unlock()
	if progressFn != nil {
		progressFn(100, 100)
	}
	if fail {
		return errFakeUpload
	}
	return nil
}

func (f *fakePlugin) Verify(ctx context.Context, localPath, remoteRelPath string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	return f.verifyResult, nil
}

func (f *fakePlugin) Delete(ctx context.Context, remoteRelPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return nil
}

func (f *fakePlugin) Capabilities() backup.Capabilities { return f.caps }

func (f *fakePlugin) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads
}

var errFakeUpload = &fakeErr{"fake upload failure"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

func okPlugin(name string) *fakePlugin {
	return &fakePlugin{
		name:         name,
		caps:         backup.Capabilities{SupportsVerify: true, SupportsDelete: true},
		verifyResult: true,
	}
}

func badPlugin(name string) *fakePlugin {
	return &fakePlugin{
		name:        name,
		caps:        backup.Capabilities{SupportsVerify: true, SupportsDelete: true},
		failUploads: true,
	}
}

// testHarness wires a real temp SQLite DB with the real repo-backed stores and a
// registry the caller populates.
type testHarness struct {
	db        *gorm.DB
	jobs      *backup.RepoJobQueue
	providers *backup.RepoProviderStore
	assets    *repo.AssetRepo
	registry  *backup.Registry
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "paim.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return &testHarness{
		db:        gdb,
		jobs:      backup.NewRepoJobQueue(gdb),
		providers: backup.NewRepoProviderStore(gdb),
		assets:    repo.NewAssetRepo(gdb),
		registry:  backup.NewRegistry(),
	}
}

func (h *testHarness) addProvider(t *testing.T, pluginName string) *domain.BackupProvider {
	t.Helper()
	p := &domain.BackupProvider{PluginName: pluginName, ConfigJSON: "{}", Enabled: true}
	if err := h.db.Create(p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return p
}

func (h *testHarness) addAsset(t *testing.T) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   "IMG_0001.JPG",
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + t.Name(),
		FileSize:           1234,
		MediaType:          domain.MediaTypePhoto,
		CurrentArchivePath: "/library/2024/2024-01-01/IMG_0001.JPG",
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
	}
	if err := h.assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

func fastOptions() backup.Options {
	return backup.Options{
		Workers:      2,
		MaxRetries:   3,
		BaseBackoff:  2 * time.Millisecond,
		PollInterval: 3 * time.Millisecond,
	}
}

func eventually(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, desc)
}

func (h *testHarness) jobByID(t *testing.T, id string) *domain.BackupJob {
	t.Helper()
	j, err := h.jobs.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %s: %v", id, err)
	}
	return j
}

func (h *testHarness) assetByID(t *testing.T, id string) *domain.Asset {
	t.Helper()
	a, err := h.assets.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get asset %s: %v", id, err)
	}
	return a
}

func TestManager_HappyPath(t *testing.T) {
	h := newHarness(t)
	fake := okPlugin("localfs")
	h.registry.Register("localfs", func() backup.Plugin { return fake })
	prov := h.addProvider(t, "localfs")
	asset := h.addAsset(t)

	ctx := context.Background()
	job, _, err := h.jobs.Enqueue(ctx, asset.ID, prov.PluginName, prov.ID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 2*time.Second, "job completed", func() bool {
		return h.jobByID(t, job.ID).Status == domain.JobStatusCompleted
	})
	// The aggregate asset status is recomputed after MarkCompleted lands, so
	// poll for it rather than asserting immediately.
	eventually(t, 2*time.Second, "asset backup status complete", func() bool {
		return h.assetByID(t, asset.ID).BackupStatus == domain.BackupStatusComplete
	})
	if fake.uploadCount() != 1 {
		t.Fatalf("uploads = %d, want 1", fake.uploadCount())
	}
	if fake.verifies == 0 {
		t.Fatalf("verify was not called")
	}
	if fake.lastUpload == "" {
		t.Fatalf("remote path not recorded")
	}
}

func TestManager_RetryThenFail(t *testing.T) {
	h := newHarness(t)
	fake := badPlugin("localfs")
	h.registry.Register("localfs", func() backup.Plugin { return fake })
	prov := h.addProvider(t, "localfs")
	asset := h.addAsset(t)

	ctx := context.Background()
	job, _, err := h.jobs.Enqueue(ctx, asset.ID, prov.PluginName, prov.ID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 3*time.Second, "job failed after retries", func() bool {
		j := h.jobByID(t, job.ID)
		return j.Status == domain.JobStatusFailed && j.Retries >= 3
	})

	final := h.jobByID(t, job.ID)
	if final.ErrorMessage == "" {
		t.Fatalf("expected error message on failed job")
	}
	if fake.uploadCount() < 3 {
		t.Fatalf("expected >=3 upload attempts, got %d", fake.uploadCount())
	}
	// recomputeAsset runs after the final MarkFailed lands, so poll for the
	// aggregate status rather than asserting immediately.
	eventually(t, 2*time.Second, "asset backup status failed", func() bool {
		return h.assetByID(t, asset.ID).BackupStatus == domain.BackupStatusFailed
	})
}

func TestManager_PausePreventsClaimingThenResume(t *testing.T) {
	h := newHarness(t)
	fake := okPlugin("localfs")
	h.registry.Register("localfs", func() backup.Plugin { return fake })
	prov := h.addProvider(t, "localfs")
	asset := h.addAsset(t)

	ctx := context.Background()
	job, _, err := h.jobs.Enqueue(ctx, asset.ID, prov.PluginName, prov.ID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := m0Pause(ctx, h, job.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	// Give workers time to (not) claim the paused job.
	time.Sleep(50 * time.Millisecond)
	if got := h.jobByID(t, job.ID).Status; got != domain.JobStatusPaused {
		t.Fatalf("paused job status = %q, want paused (should not be claimed)", got)
	}
	if fake.uploadCount() != 0 {
		t.Fatalf("paused job should not have uploaded, uploads=%d", fake.uploadCount())
	}

	if err := m.Resume(ctx, job.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	eventually(t, 2*time.Second, "job completed after resume", func() bool {
		return h.jobByID(t, job.ID).Status == domain.JobStatusCompleted
	})
}

// m0Pause pauses via the manager without starting it, exercising Manager.Pause.
func m0Pause(ctx context.Context, h *testHarness, jobID string) error {
	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	return m.Pause(ctx, jobID)
}

func TestManager_CancelPreventsClaiming(t *testing.T) {
	h := newHarness(t)
	fake := okPlugin("localfs")
	h.registry.Register("localfs", func() backup.Plugin { return fake })
	prov := h.addProvider(t, "localfs")
	asset := h.addAsset(t)

	ctx := context.Background()
	job, _, err := h.jobs.Enqueue(ctx, asset.ID, prov.PluginName, prov.ID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Cancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	time.Sleep(50 * time.Millisecond)
	if got := h.jobByID(t, job.ID).Status; got != domain.JobStatusCancelled {
		t.Fatalf("cancelled job status = %q, want cancelled", got)
	}
	if fake.uploadCount() != 0 {
		t.Fatalf("cancelled job should not upload, uploads=%d", fake.uploadCount())
	}
}

func TestManager_TwoProvidersOneFailingIsPartial(t *testing.T) {
	h := newHarness(t)
	good := okPlugin("good")
	bad := badPlugin("bad")
	h.registry.Register("good", func() backup.Plugin { return good })
	h.registry.Register("bad", func() backup.Plugin { return bad })
	provGood := h.addProvider(t, "good")
	provBad := h.addProvider(t, "bad")
	asset := h.addAsset(t)

	ctx := context.Background()
	// EnqueueForAsset inside a transaction, one job per enabled provider.
	err := h.db.Transaction(func(tx *gorm.DB) error {
		m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
		return m.EnqueueForAsset(ctx, tx, asset.ID)
	})
	if err != nil {
		t.Fatalf("enqueue for asset: %v", err)
	}

	jobs, err := h.jobs.JobsForAsset(ctx, asset.ID)
	if err != nil {
		t.Fatalf("jobs for asset: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	_ = provGood
	_ = provBad

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 3*time.Second, "one job completed, one failed", func() bool {
		js, _ := h.jobs.JobsForAsset(ctx, asset.ID)
		var c, f int
		for _, j := range js {
			switch j.Status {
			case domain.JobStatusCompleted:
				c++
			case domain.JobStatusFailed:
				f++
			}
		}
		return c == 1 && f == 1
	})

	// The aggregate is recomputed after each job's terminal state lands, so poll.
	eventually(t, 2*time.Second, "asset backup status partial", func() bool {
		return h.assetByID(t, asset.ID).BackupStatus == domain.BackupStatusPartial
	})
}

func TestManager_ResetRunningOnStartupRevivesOrphans(t *testing.T) {
	h := newHarness(t)
	fake := okPlugin("localfs")
	h.registry.Register("localfs", func() backup.Plugin { return fake })
	prov := h.addProvider(t, "localfs")
	asset := h.addAsset(t)

	// Simulate a job left "running" by a crash.
	orphan := &domain.BackupJob{
		AssetID:     asset.ID,
		Plugin:      prov.PluginName,
		Destination: prov.ID,
		Status:      domain.JobStatusRunning,
	}
	if err := h.db.Create(orphan).Error; err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	ctx := context.Background()
	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 2*time.Second, "orphan revived and completed", func() bool {
		return h.jobByID(t, orphan.ID).Status == domain.JobStatusCompleted
	})
}

func TestManager_EnqueueForAssetIdempotentInTx(t *testing.T) {
	h := newHarness(t)
	h.registry.Register("good", func() backup.Plugin { return okPlugin("good") })
	h.registry.Register("bad", func() backup.Plugin { return okPlugin("bad") })
	h.addProvider(t, "good")
	h.addProvider(t, "bad")
	asset := h.addAsset(t)

	ctx := context.Background()
	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())

	// Two enqueues (as if import ran, crashed, and re-ran) inside transactions.
	for i := 0; i < 2; i++ {
		err := h.db.Transaction(func(tx *gorm.DB) error {
			return m.EnqueueForAsset(ctx, tx, asset.ID)
		})
		if err != nil {
			t.Fatalf("enqueue for asset (iter %d): %v", i, err)
		}
	}

	jobs, err := h.jobs.JobsForAsset(ctx, asset.ID)
	if err != nil {
		t.Fatalf("jobs for asset: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs (idempotent), got %d", len(jobs))
	}
}

func TestAggregateBackupStatus(t *testing.T) {
	mk := func(statuses ...domain.JobStatus) []domain.BackupJob {
		jobs := make([]domain.BackupJob, len(statuses))
		for i, s := range statuses {
			jobs[i].Status = s
		}
		return jobs
	}
	cases := []struct {
		name string
		jobs []domain.BackupJob
		want domain.BackupStatus
	}{
		{"none", nil, domain.BackupStatusNone},
		{"all cancelled -> none", mk(domain.JobStatusCancelled, domain.JobStatusCancelled), domain.BackupStatusNone},
		{"all complete", mk(domain.JobStatusCompleted, domain.JobStatusCompleted), domain.BackupStatusComplete},
		{"some complete some pending -> partial", mk(domain.JobStatusCompleted, domain.JobStatusPending), domain.BackupStatusPartial},
		{"complete + failed -> partial", mk(domain.JobStatusCompleted, domain.JobStatusFailed), domain.BackupStatusPartial},
		{"all failed -> failed", mk(domain.JobStatusFailed, domain.JobStatusFailed), domain.BackupStatusFailed},
		{"pending only -> pending", mk(domain.JobStatusPending), domain.BackupStatusPending},
		{"cancelled + complete -> complete", mk(domain.JobStatusCancelled, domain.JobStatusCompleted), domain.BackupStatusComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backup.AggregateBackupStatus(tc.jobs); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	r := backup.NewRegistry()
	if _, ok := r.New("missing"); ok {
		t.Fatalf("expected missing plugin to be absent")
	}
	r.Register("x", func() backup.Plugin { return okPlugin("x") })
	p, ok := r.New("x")
	if !ok || p.Name() != "x" {
		t.Fatalf("expected registered plugin x, got ok=%v name=%v", ok, p)
	}
	if len(r.Names()) != 1 {
		t.Fatalf("expected 1 name, got %v", r.Names())
	}
}
