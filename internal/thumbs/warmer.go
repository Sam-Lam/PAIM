package thumbs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// defaultWarmConcurrency bounds how many thumbnails a warm-up generates at once.
// It is intentionally lower than the interactive generation bound (2 vs 4) so a
// background warm-up cannot starve an import/backup or the responsive on-demand
// path the browser depends on.
const defaultWarmConcurrency = 2

// Warmer pre-generates 512px thumbnails for a set of asset IDs so browsing is
// instant instead of lazy-on-first-view. It is deliberately thin: it walks IDs,
// resolves each to a source path + quick hash, and calls Cache.Ensure (which
// no-ops on a cache hit, so a warm-up is resumable and cheap to re-run). All
// lifecycle/eventing/quit-guard concerns live in the service that drives it.
type Warmer struct {
	cache    *Cache
	resolver AssetResolver
	conc     int
	log      *slog.Logger
}

// NewWarmer constructs a Warmer over a cache and asset resolver. concurrency <= 0
// falls back to the default bound of 2.
func NewWarmer(cache *Cache, resolver AssetResolver, concurrency int, logger *slog.Logger) *Warmer {
	if logger == nil {
		logger = slog.Default()
	}
	if concurrency < 1 {
		concurrency = defaultWarmConcurrency
	}
	return &Warmer{
		cache:    cache,
		resolver: resolver,
		conc:     concurrency,
		log:      logger.With(slog.String("subsystem", "thumbs")),
	}
}

// Warm ensures a 512px thumbnail exists for every id in ids, with bounded
// concurrency. progress (may be nil) is called after each id finishes with the
// running done/total counts. A missing source file or a generation failure is
// counted as done and skipped (never fatal — those tiles fall back to a
// placeholder). It honors ctx cancellation between and while queuing items and
// returns ctx.Err() when cancelled.
func (w *Warmer) Warm(ctx context.Context, ids []string, progress func(done, total int)) error {
	total := len(ids)
	if total == 0 {
		return nil
	}

	var (
		mu   sync.Mutex
		done int
	)
	report := func() {
		if progress == nil {
			return
		}
		mu.Lock()
		d := done
		mu.Unlock()
		progress(d, total)
	}

	sem := make(chan struct{}, w.conc)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(assetID string) {
			defer wg.Done()
			defer func() { <-sem }()
			w.warmOne(ctx, assetID)
			mu.Lock()
			done++
			mu.Unlock()
			report()
		}(id)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// warmOne resolves and ensures a single asset's grid thumbnail. Non-cancellation
// errors are swallowed (logged at debug) because a warm-up is best-effort: the
// on-demand path and placeholder tiles handle any asset that cannot render.
func (w *Warmer) warmOne(ctx context.Context, assetID string) {
	absPath, quickHash, err := w.resolver.Resolve(ctx, assetID)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.log.Debug("warm-up: resolve asset", "assetId", assetID, "error", err.Error())
		return
	}
	if _, err := w.cache.Ensure(ctx, absPath, quickHash, SizeGrid); err != nil {
		// ErrSourceMissing / ErrGenerationFailed are expected for some assets and
		// already handled (placeholder tile / negative marker); nothing to do.
		w.log.Debug("warm-up: ensure thumbnail", "assetId", assetID, "error", err.Error())
	}
}
