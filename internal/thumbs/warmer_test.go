package thumbs

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mapResolver is a tiny AssetResolver over an in-memory id → (path, quickHash) map.
type mapResolver map[string][2]string

func (m mapResolver) Resolve(_ context.Context, id string) (string, string, error) {
	e, ok := m[id]
	if !ok {
		return "", "", ErrAssetNotFound
	}
	return e[0], e[1], nil
}

// buildWarmerFixture writes n source JPEGs and returns a resolver over them.
func buildWarmerFixture(t *testing.T, dir string, n int) ([]string, mapResolver) {
	t.Helper()
	ids := make([]string, 0, n)
	res := mapResolver{}
	for i := 0; i < n; i++ {
		id := "asset" + string(rune('a'+i))
		src := filepath.Join(dir, id+".jpg")
		writeJPEG(t, src)
		ids = append(ids, id)
		res[id] = [2]string{src, "hash" + string(rune('a'+i))}
	}
	return ids, res
}

func TestWarmerProgressAndSkipCached(t *testing.T) {
	dir := t.TempDir()
	ids, res := buildWarmerFixture(t, dir, 3)
	gen := &fakeGen{}
	cache := newCache(dir, gen, 4, nil)
	w := NewWarmer(cache, res, 2, nil)

	var mu sync.Mutex
	var lastDone, lastTotal int
	progress := func(done, total int) {
		mu.Lock()
		if done > lastDone {
			lastDone = done
		}
		lastTotal = total
		mu.Unlock()
	}

	if err := w.Warm(context.Background(), ids, progress); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Errorf("final progress = %d/%d, want 3/3", lastDone, lastTotal)
	}
	if got := gen.calls.Load(); got != 3 {
		t.Errorf("generator ran %d times, want 3", got)
	}

	// Second pass: every thumbnail is cached, so the generator must not run again.
	if err := w.Warm(context.Background(), ids, nil); err != nil {
		t.Fatalf("warm (cached): %v", err)
	}
	if got := gen.calls.Load(); got != 3 {
		t.Errorf("generator ran %d times after cached re-run, want 3", got)
	}
}

func TestWarmerCancel(t *testing.T) {
	dir := t.TempDir()
	ids, res := buildWarmerFixture(t, dir, 6)
	// A blocking generator keeps the warm-up in flight until we cancel.
	gen := &fakeGen{release: make(chan struct{})}
	cache := newCache(dir, gen, 2, nil)
	w := NewWarmer(cache, res, 2, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Warm(ctx, ids, nil) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	close(gen.release) // let any in-flight generations unwind

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("warm err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warm did not return after cancel")
	}
}

func TestWarmerEmptyIsNoop(t *testing.T) {
	gen := &fakeGen{}
	cache := newCache(t.TempDir(), gen, 2, nil)
	w := NewWarmer(cache, mapResolver{}, 2, nil)
	if err := w.Warm(context.Background(), nil, nil); err != nil {
		t.Fatalf("warm empty: %v", err)
	}
	if got := gen.calls.Load(); got != 0 {
		t.Errorf("generator ran %d times for empty warm, want 0", got)
	}
}
