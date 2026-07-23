package backup_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
	"github.com/Sam-Lam/PAIM/internal/domain"
)

// hookPlugin is a backup.Plugin whose Upload and Verify behavior are supplied by
// injected hooks, so feature tests can drive cooldowns, mirror-verify failures,
// and upload ordering.
type hookPlugin struct {
	name string
	caps backup.Capabilities

	mu       sync.Mutex
	uploadFn func(remoteRel string) error
	verifyFn func() (bool, error)
	uploaded []string
}

func (h *hookPlugin) Name() string                             { return h.name }
func (h *hookPlugin) Initialize(context.Context, string) error { return nil }
func (h *hookPlugin) Authenticate(context.Context) error       { return nil }
func (h *hookPlugin) Capabilities() backup.Capabilities        { return h.caps }
func (h *hookPlugin) Delete(context.Context, string) error     { return nil }

func (h *hookPlugin) Upload(ctx context.Context, localPath, remoteRel string, _ func(int64, int64)) error {
	h.mu.Lock()
	fn := h.uploadFn
	h.mu.Unlock()
	if fn != nil {
		if err := fn(remoteRel); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.uploaded = append(h.uploaded, remoteRel)
	h.mu.Unlock()
	return nil
}

func (h *hookPlugin) Verify(ctx context.Context, localPath, remoteRel string) (bool, error) {
	h.mu.Lock()
	fn := h.verifyFn
	h.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return true, nil
}

func (h *hookPlugin) uploadedPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.uploaded...)
}

// testClock is a controllable clock for cooldown timing.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (h *testHarness) addProviderFull(t *testing.T, plugin string, mirror bool, order domain.UploadOrder) *domain.BackupProvider {
	t.Helper()
	p := &domain.BackupProvider{PluginName: plugin, ConfigJSON: "{}", Enabled: true, Mirror: mirror, UploadOrder: order}
	if err := h.db.Create(p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return p
}

func (h *testHarness) addAssetWith(t *testing.T, archivePath string, capture *time.Time) *domain.Asset {
	t.Helper()
	a := &domain.Asset{
		OriginalFilename:   "IMG.JPG",
		OriginalExtension:  "jpg",
		QuickHash:          "qh-" + archivePath,
		FileSize:           1234,
		MediaType:          domain.MediaTypePhoto,
		CurrentArchivePath: archivePath,
		CaptureDate:        capture,
		ImportDate:         time.Now(),
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusNone,
	}
	if err := h.assets.Create(context.Background(), a); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

// ---- Mirror exclusion matrix (unit) ----

func TestAggregateBackupStatusMirrorMatrix(t *testing.T) {
	isMirror := func(id string) bool { return id == "mirror" }
	job := func(dest string, st domain.JobStatus) domain.BackupJob {
		return domain.BackupJob{Destination: dest, Status: st}
	}
	cases := []struct {
		name string
		jobs []domain.BackupJob
		want domain.BackupStatus
	}{
		{
			name: "real complete + mirror pending -> complete",
			jobs: []domain.BackupJob{job("real", domain.JobStatusCompleted), job("mirror", domain.JobStatusPending)},
			want: domain.BackupStatusComplete,
		},
		{
			name: "real failed + mirror complete -> failed",
			jobs: []domain.BackupJob{job("real", domain.JobStatusFailed), job("mirror", domain.JobStatusCompleted)},
			want: domain.BackupStatusFailed,
		},
		{
			name: "real complete + mirror failed -> complete",
			jobs: []domain.BackupJob{job("real", domain.JobStatusCompleted), job("mirror", domain.JobStatusFailed)},
			want: domain.BackupStatusComplete,
		},
		{
			name: "real pending + mirror complete -> pending",
			jobs: []domain.BackupJob{job("real", domain.JobStatusPending), job("mirror", domain.JobStatusCompleted)},
			want: domain.BackupStatusPending,
		},
		{
			name: "only mirror complete -> none (no required jobs)",
			jobs: []domain.BackupJob{job("mirror", domain.JobStatusCompleted)},
			want: domain.BackupStatusNone,
		},
		{
			name: "two real complete -> complete (mirror predicate irrelevant)",
			jobs: []domain.BackupJob{job("real", domain.JobStatusCompleted), job("real", domain.JobStatusCompleted)},
			want: domain.BackupStatusComplete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backup.AggregateBackupStatus(tc.jobs, isMirror); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- Mirror verify is best-effort ----

func TestManagerMirrorVerifyBestEffort(t *testing.T) {
	h := newHarness(t)
	plugin := &hookPlugin{
		name:     "rclone",
		caps:     backup.Capabilities{SupportsVerify: true},
		verifyFn: func() (bool, error) { return false, nil }, // verify says "not there"
	}
	h.registry.Register("rclone", func() backup.Plugin { return plugin })
	prov := h.addProviderFull(t, "rclone", true, domain.UploadOrderNewestFirst)
	asset := h.addAssetWith(t, "2024/a.jpg", nil)

	ctx := context.Background()
	job, _, err := h.jobs.Enqueue(ctx, asset.ID, prov.PluginName, prov.ID, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 2*time.Second, "mirror job completed with note", func() bool {
		j := h.jobByID(t, job.ID)
		return j.Status == domain.JobStatusCompleted && j.ErrorMessage == "verify unavailable (mirror)"
	})
}

// ---- Provider quota cooldown ----

func TestManagerProviderCooldown(t *testing.T) {
	h := newHarness(t)
	clock := &testClock{t: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}

	var mu sync.Mutex
	cooling := true
	plugin := &hookPlugin{
		name: "rclone",
		caps: backup.Capabilities{SupportsVerify: true},
		uploadFn: func(string) error {
			mu.Lock()
			defer mu.Unlock()
			if cooling {
				return &backup.ErrProviderCooldown{RetryAfter: time.Hour, Reason: "quota"}
			}
			return nil
		},
	}
	h.registry.Register("rclone", func() backup.Plugin { return plugin })
	prov := h.addProviderFull(t, "rclone", false, domain.UploadOrderOldestFirst)
	asset := h.addAssetWith(t, "2024/a.jpg", nil)

	ctx := context.Background()
	job, _, err := h.jobs.Enqueue(ctx, asset.ID, prov.PluginName, prov.ID, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	opts := fastOptions()
	opts.Now = clock.now
	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, opts)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	// The provider enters cooldown; the job returns to pending with NO retry
	// increment (a cooldown is not a failure).
	eventually(t, 2*time.Second, "provider cooling", func() bool {
		return len(m.Cooldowns()) == 1
	})
	eventually(t, 2*time.Second, "job pending, retries untouched", func() bool {
		j := h.jobByID(t, job.ID)
		return j.Status == domain.JobStatusPending && j.Retries == 0
	})

	// While cooling, the job stays pending (provider is skipped by the claim loop).
	time.Sleep(50 * time.Millisecond)
	if j := h.jobByID(t, job.ID); j.Status != domain.JobStatusPending || j.Retries != 0 {
		t.Fatalf("job = %s retries=%d, want pending/0 while cooling", j.Status, j.Retries)
	}

	// Clear the cooldown condition and advance past expiry: the job resumes and
	// completes without any restart.
	mu.Lock()
	cooling = false
	mu.Unlock()
	clock.advance(2 * time.Hour)

	eventually(t, 2*time.Second, "job completes after cooldown expiry", func() bool {
		return h.jobByID(t, job.ID).Status == domain.JobStatusCompleted
	})
	if len(m.Cooldowns()) != 0 {
		t.Fatalf("cooldown should have expired, got %v", m.Cooldowns())
	}
}

// ---- Sort-key stamping at enqueue (capture vs import fallback) ----

func TestEnqueueForAssetSortKey(t *testing.T) {
	h := newHarness(t)
	h.registry.Register("localfs", func() backup.Plugin { return okPlugin("localfs") })
	h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)

	ctx := context.Background()
	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())

	capture := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	withCapture := h.addAssetWith(t, "2020/a.jpg", &capture)
	if _, err := m.EnqueueForAsset(ctx, h.db, withCapture.ID, nil); err != nil {
		t.Fatalf("enqueue with capture: %v", err)
	}
	jobs, _ := h.jobs.JobsForAsset(ctx, withCapture.ID)
	if len(jobs) != 1 || jobs[0].SortKey != capture.Unix() {
		t.Fatalf("sortKey = %v, want capture unix %d", jobs, capture.Unix())
	}

	noCapture := h.addAssetWith(t, "2021/b.jpg", nil)
	if _, err := m.EnqueueForAsset(ctx, h.db, noCapture.ID, nil); err != nil {
		t.Fatalf("enqueue without capture: %v", err)
	}
	jobs2, _ := h.jobs.JobsForAsset(ctx, noCapture.ID)
	reloaded := h.assetByID(t, noCapture.ID)
	if len(jobs2) != 1 || jobs2[0].SortKey != reloaded.ImportDate.Unix() {
		t.Fatalf("sortKey = %v, want import date unix %d", jobs2, reloaded.ImportDate.Unix())
	}
}

// ---- Newest-first upload order (single worker, deterministic) ----

func TestManagerNewestFirstOrder(t *testing.T) {
	h := newHarness(t)
	plugin := &hookPlugin{name: "rclone", caps: backup.Capabilities{SupportsVerify: true}}
	h.registry.Register("rclone", func() backup.Plugin { return plugin })
	prov := h.addProviderFull(t, "rclone", true, domain.UploadOrderNewestFirst)

	ctx := context.Background()
	// Three assets with distinct sort keys, enqueued oldest-created first.
	a100 := h.addAssetWith(t, "p100.jpg", nil)
	a300 := h.addAssetWith(t, "p300.jpg", nil)
	a200 := h.addAssetWith(t, "p200.jpg", nil)
	if _, _, err := h.jobs.Enqueue(ctx, a100.ID, prov.PluginName, prov.ID, 100); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, _, err := h.jobs.Enqueue(ctx, a300.ID, prov.PluginName, prov.ID, 300); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, _, err := h.jobs.Enqueue(ctx, a200.ID, prov.PluginName, prov.ID, 200); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	opts := fastOptions()
	opts.Workers = 1 // deterministic claim order
	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, opts)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 2*time.Second, "all three uploaded", func() bool {
		return len(plugin.uploadedPaths()) == 3
	})
	got := plugin.uploadedPaths()
	want := []string{"p300.jpg", "p200.jpg", "p100.jpg"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("upload order = %v, want %v (newest sort key first)", got, want)
		}
	}
}

// ---- Two-provider fairness sanity ----

func TestManagerTwoProviderFairness(t *testing.T) {
	h := newHarness(t)
	h.registry.Register("localfs", func() backup.Plugin { return okPlugin("localfs") })
	provA := h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)
	provB := h.addProviderFull(t, "localfs", false, domain.UploadOrderOldestFirst)

	ctx := context.Background()
	var jobIDs []string
	for i := 0; i < 4; i++ {
		a := h.addAssetWith(t, "x"+string(rune('a'+i))+".jpg", nil)
		for _, prov := range []*domain.BackupProvider{provA, provB} {
			j, _, err := h.jobs.Enqueue(ctx, a.ID, prov.PluginName, prov.ID, 0)
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			jobIDs = append(jobIDs, j.ID)
		}
	}

	m := backup.NewManager(h.jobs, h.assets, h.providers, h.registry, nil, fastOptions())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	eventually(t, 3*time.Second, "all jobs across both providers complete", func() bool {
		for _, id := range jobIDs {
			if h.jobByID(t, id).Status != domain.JobStatusCompleted {
				return false
			}
		}
		return true
	})
}
