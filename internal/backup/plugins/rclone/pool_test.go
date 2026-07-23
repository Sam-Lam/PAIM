package rclone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
)

// remoteFromDst extracts the "remote:" prefix from an rclone destination argument
// like "gp2:PAIM-Backup/a.jpg".
func remoteFromDst(dst string) string {
	name, _, _ := strings.Cut(dst, ":")
	return name + ":"
}

// poolResponder scripts listremotes/config/copyto for a set of pool remotes. The
// copyto behavior is looked up per-remote so a test can make some remotes fail
// with quota errors and others succeed.
func poolResponder(remotes []string, copyto func(remote string) (stderr []byte, err error)) func(context.Context, []string, func(string)) ([]byte, []byte, error) {
	return func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		switch args[0] {
		case "listremotes":
			return []byte(strings.Join(remotes, "\n") + "\n"), nil, nil
		case "copyto":
			dst := args[len(args)-1]
			stderr, err := copyto(remoteFromDst(dst))
			return nil, stderr, err
		default:
			// config show, about, lsd, etc.
			return nil, nil, nil
		}
	}
}

func newPoolPlugin(t *testing.T, runner *fakeRunner, clock func() time.Time) *Plugin {
	t.Helper()
	p := newTestPlugin(runner)
	if clock != nil {
		p.now = clock
	}
	return p
}

func writeTemp(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "a.jpg")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return src
}

// TestPoolFailoverOnQuota: the first remote returns a quota error; the SAME file
// is retried on the next remote within one Upload call and succeeds.
func TestPoolFailoverOnQuota(t *testing.T) {
	var uploaded string
	runner := &fakeRunner{responder: poolResponder([]string{"gp1:", "gp2:"}, func(remote string) ([]byte, error) {
		if remote == "gp1:" {
			return []byte("googleapi: Error 429: rate limit exceeded, rateLimitExceeded"), errors.New("exit status 1")
		}
		uploaded = remote
		return nil, nil
	})}
	p := newPoolPlugin(t, runner, nil)
	if err := p.Initialize(context.Background(), `{"remotes":["gp1:","gp2:"],"mirror":true}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := p.Upload(context.Background(), writeTemp(t), "a.jpg", nil); err != nil {
		t.Fatalf("upload should succeed via failover, got %v", err)
	}
	if uploaded != "gp2:" {
		t.Fatalf("file uploaded via %q, want gp2:", uploaded)
	}
	// gp1 is now cooling; gp2 is not.
	if _, cooling := p.remoteCooling("gp1:"); !cooling {
		t.Fatalf("gp1 should be cooling after its quota error")
	}
	if _, cooling := p.remoteCooling("gp2:"); cooling {
		t.Fatalf("gp2 should not be cooling")
	}
}

// TestPoolAllCoolingReturnsProviderCooldown: when every remote is exhausted, Upload
// returns backup.ErrProviderCooldown with the soonest remote's expiry.
func TestPoolAllCoolingReturnsProviderCooldown(t *testing.T) {
	now := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	runner := &fakeRunner{responder: poolResponder([]string{"gp1:", "gp2:"}, func(remote string) ([]byte, error) {
		// Both remotes hit a transient rate limit (10m cooldown).
		return []byte("Error 429: rateLimitExceeded"), errors.New("exit status 1")
	})}
	p := newPoolPlugin(t, runner, clock)
	if err := p.Initialize(context.Background(), `{"remotes":["gp1:","gp2:"],"mirror":true}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	err := p.Upload(context.Background(), writeTemp(t), "a.jpg", nil)
	var cd *backup.ErrProviderCooldown
	if !errors.As(err, &cd) {
		t.Fatalf("want ErrProviderCooldown, got %v", err)
	}
	// Soonest expiry == 10m (both cooled at the same fixed clock).
	if cd.RetryAfter != shortRateCooldown {
		t.Fatalf("RetryAfter = %s, want %s (soonest remote)", cd.RetryAfter, shortRateCooldown)
	}
}

// TestPerRemoteCooldownIndependence: a cooling remote is skipped until the clock
// advances past its expiry, at which point it becomes eligible again — the other
// remote's state is independent.
func TestPerRemoteCooldownIndependence(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	var attempts []string
	gp1Fails := true
	runner := &fakeRunner{responder: poolResponder([]string{"gp1:", "gp2:"}, func(remote string) ([]byte, error) {
		mu.Lock()
		attempts = append(attempts, remote)
		fail := remote == "gp1:" && gp1Fails
		mu.Unlock()
		if fail {
			return []byte("Error 429: rateLimitExceeded"), errors.New("exit status 1")
		}
		return nil, nil
	})}
	p := newPoolPlugin(t, runner, clock)
	if err := p.Initialize(context.Background(), `{"remotes":["gp1:","gp2:"],"mirror":true}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// Upload 1: gp1 fails -> cools; failover to gp2 succeeds.
	if err := p.Upload(context.Background(), writeTemp(t), "a.jpg", nil); err != nil {
		t.Fatalf("upload 1: %v", err)
	}
	// Upload 2: gp1 still cooling -> skipped entirely; goes straight to gp2.
	attempts = nil
	if err := p.Upload(context.Background(), writeTemp(t), "b.jpg", nil); err != nil {
		t.Fatalf("upload 2: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), attempts...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "gp2:" {
		t.Fatalf("upload 2 attempts = %v, want [gp2:] (gp1 skipped while cooling)", got)
	}

	// Advance past gp1's 10m cooldown and let gp1 succeed now.
	mu.Lock()
	gp1Fails = false
	mu.Unlock()
	advance(shortRateCooldown + time.Minute)
	attempts = nil
	if err := p.Upload(context.Background(), writeTemp(t), "c.jpg", nil); err != nil {
		t.Fatalf("upload 3: %v", err)
	}
	mu.Lock()
	got = append([]string(nil), attempts...)
	mu.Unlock()
	if len(got) == 0 || got[0] != "gp1:" {
		t.Fatalf("upload 3 attempts = %v, want gp1 first (cooldown expired)", got)
	}
}

// TestSingleRemoteBackwardCompat: a single legacy remote that hits a quota returns
// a provider cooldown (there is nothing to fail over to).
func TestSingleRemoteBackwardCompat(t *testing.T) {
	now := time.Now()
	runner := &fakeRunner{responder: poolResponder([]string{"gp1:"}, func(remote string) ([]byte, error) {
		return []byte("Error 429: rateLimitExceeded"), errors.New("exit status 1")
	})}
	p := newPoolPlugin(t, runner, func() time.Time { return now })
	if err := p.Initialize(context.Background(), `{"remote":"gp1:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if len(p.remotes) != 1 {
		t.Fatalf("remotes = %v, want single-element pool", p.remotes)
	}
	err := p.Upload(context.Background(), writeTemp(t), "a.jpg", nil)
	var cd *backup.ErrProviderCooldown
	if !errors.As(err, &cd) {
		t.Fatalf("single-remote quota should surface ErrProviderCooldown, got %v", err)
	}
}

// TestNonMirrorMultiRemoteRejected: a >1 remote pool on a non-mirror provider is
// rejected at Initialize.
func TestNonMirrorMultiRemoteRejected(t *testing.T) {
	runner := &fakeRunner{responder: poolResponder([]string{"gp1:", "gp2:"}, func(string) ([]byte, error) { return nil, nil })}
	p := newTestPlugin(runner)
	err := p.Initialize(context.Background(), `{"remotes":["gp1:","gp2:"]}`)
	if err == nil || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("want mirror-required rejection, got %v", err)
	}
}

// TestConfigParseLegacyAndPool: both the legacy single remote and the pool form
// parse, and the legacy form becomes a one-element pool.
func TestConfigParseLegacyAndPool(t *testing.T) {
	runner := &fakeRunner{responder: poolResponder([]string{"gp1:", "gp2:"}, func(string) ([]byte, error) { return nil, nil })}

	legacy := newTestPlugin(runner)
	if err := legacy.Initialize(context.Background(), `{"remote":"gp1:"}`); err != nil {
		t.Fatalf("legacy initialize: %v", err)
	}
	if len(legacy.remotes) != 1 || legacy.remotes[0] != "gp1:" {
		t.Fatalf("legacy remotes = %v, want [gp1:]", legacy.remotes)
	}

	pool := newTestPlugin(runner)
	if err := pool.Initialize(context.Background(), `{"remotes":["gp1:","gp2:"],"mirror":true}`); err != nil {
		t.Fatalf("pool initialize: %v", err)
	}
	if len(pool.remotes) != 2 || pool.remotes[0] != "gp1:" || pool.remotes[1] != "gp2:" {
		t.Fatalf("pool remotes = %v, want [gp1: gp2:]", pool.remotes)
	}
}
