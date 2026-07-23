// Package thumbs implements PAIM's disposable, content-addressed thumbnail
// cache. Thumbnails live under <root>/.paim/thumbs/<size>/<hash[0:2]>/<quickhash>.jpg
// and travel with the portable library; the cache is purely derived data and can
// be cleared at any time.
//
// Generation uses macOS QuickLook (qlmanage) to render a poster image for any
// supported still, RAW, or video, then sips to compress it to JPEG. Two sizes
// are produced lazily on first request: 512 (grid tiles) and 2048 (detail
// preview). Concurrent requests for the same key collapse to a single generation
// (singleflight), generation is globally bounded by a semaphore, and a failed
// render leaves a negative marker so it is not retried until the next app
// restart (stale markers are swept on construction).
//
// The package is deliberately decoupled from the catalog: the HTTP Handler
// depends on an AssetResolver interface (implemented in internal/services) to map
// an asset ID to a source path and quick hash. No filesystem path ever crosses
// the Wails bridge — the frontend only ever references /thumb/<assetID>.
package thumbs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Supported thumbnail sizes in pixels (long edge).
const (
	SizeGrid    = 512  // grid tiles
	SizePreview = 2048 // detail preview
)

// Defaults.
const (
	defaultQuality     = 80 // sips JPEG quality (0-100)
	defaultConcurrency = 4  // max simultaneous generations
	failMarkerExt      = ".fail"
	thumbExt           = ".jpg"
)

// Sentinel errors. The Handler maps these to HTTP status codes; callers use
// errors.Is to distinguish them.
var (
	// ErrNoLibrary indicates no library is open (Handler → 503).
	ErrNoLibrary = errors.New("thumbs: no library open")
	// ErrAssetNotFound indicates the asset ID does not resolve to a live asset
	// (Handler → 404).
	ErrAssetNotFound = errors.New("thumbs: asset not found")
	// ErrSourceMissing indicates the asset's source file is missing or moved. It
	// is NOT cached as a negative result: if the file returns the thumbnail can be
	// generated (Handler → 404, frontend shows a placeholder).
	ErrSourceMissing = errors.New("thumbs: source file missing")
	// ErrGenerationFailed indicates QuickLook/sips could not render a thumbnail. A
	// negative marker is written so the render is not retried until app restart
	// (Handler → 404, frontend shows a placeholder).
	ErrGenerationFailed = errors.New("thumbs: thumbnail generation failed")
	// ErrUnsupportedSize indicates a size other than the two supported values.
	ErrUnsupportedSize = errors.New("thumbs: unsupported size")
)

// generator renders a JPEG thumbnail of srcPath at sizePx (long edge) into
// dstPath at the given JPEG quality. The concrete implementation is the macOS
// qlmanage+sips pipeline; tests substitute fakes so they need not depend on
// QuickLook.
type generator interface {
	generate(ctx context.Context, srcPath, dstPath string, sizePx, quality int) error
}

// Cache is a content-addressed thumbnail cache. Its directory is normally
// <root>/.paim/thumbs but may be re-pointed at runtime (see Repoint) when the
// user moves the cache to this Mac's local disk; dmu guards the mutable dir so
// concurrent Ensure calls and a Repoint never race.
type Cache struct {
	dmu     sync.RWMutex // guards dir
	dir     string
	gen     generator
	quality int
	sem     chan struct{}
	flight  flight
	log     *slog.Logger
}

// New constructs a Cache at the library's default in-library location
// (<root>/.paim/thumbs). It sweeps stale negative markers so a failed render from
// a previous run is retried once after a restart.
func New(root string, logger *slog.Logger) *Cache {
	return NewInDir(filepath.Join(root, ".paim", "thumbs"), logger)
}

// NewInDir constructs a Cache whose thumbnails live directly under dir. It backs
// the configurable cache-location feature: main.go resolves the per-machine
// preference to a directory and hands it here.
func NewInDir(dir string, logger *slog.Logger) *Cache {
	return newCacheAt(dir, qlGenerator{}, defaultConcurrency, logger)
}

// newCache is the injectable constructor used by tests to supply a fake
// generator and a small concurrency bound. It keeps the historical
// "<root>/.paim/thumbs" layout so existing tests are unchanged.
func newCache(root string, gen generator, concurrency int, logger *slog.Logger) *Cache {
	return newCacheAt(filepath.Join(root, ".paim", "thumbs"), gen, concurrency, logger)
}

// newCacheAt is the shared constructor taking an explicit thumbnail directory.
func newCacheAt(dir string, gen generator, concurrency int, logger *slog.Logger) *Cache {
	if logger == nil {
		logger = slog.Default()
	}
	if concurrency < 1 {
		concurrency = 1
	}
	c := &Cache{
		dir:     dir,
		gen:     gen,
		quality: defaultQuality,
		sem:     make(chan struct{}, concurrency),
		log:     logger.With(slog.String("subsystem", "thumbs")),
	}
	c.sweepFailMarkers()
	return c
}

// dirOf returns the current cache directory under the read lock.
func (c *Cache) dirOf() string {
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	return c.dir
}

// Dir returns the directory thumbnails are currently written to and served from.
func (c *Cache) Dir() string { return c.dirOf() }

// Repoint switches the cache to a new directory (e.g. moving between the library
// and this Mac's local disk). It takes effect immediately for subsequent
// requests; the old location is left in place (its contents are disposable). New
// negative markers under the new dir are swept so a fresh location starts clean.
func (c *Cache) Repoint(dir string) {
	c.dmu.Lock()
	if c.dir == dir {
		c.dmu.Unlock()
		return
	}
	c.dir = dir
	c.dmu.Unlock()
	c.sweepFailMarkers()
	c.log.Info("thumbnail cache re-pointed", "dir", dir)
}

// Clear removes the contents of the active cache directory. The cache is
// disposable, so this only forces regeneration on next view. It removes the whole
// tree and recreates the (empty) directory; callers are responsible for ensuring
// the dir is a known cache root before invoking it.
func (c *Cache) Clear() error {
	dir := c.dirOf()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("thumbs: clear cache %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("thumbs: recreate cache dir %q: %w", dir, err)
	}
	c.log.Info("thumbnail cache cleared", "dir", dir)
	return nil
}

// validSize reports whether px is one of the two supported sizes.
func validSize(px int) bool { return px == SizeGrid || px == SizePreview }

// shard returns the two-character fan-out directory for a quick hash, guarding
// against implausibly short hashes.
func shard(quickHash string) string {
	if len(quickHash) >= 2 {
		return quickHash[:2]
	}
	return "__"
}

// thumbPath is the final cache path for a (size, quickHash).
func (c *Cache) thumbPath(sizePx int, quickHash string) string {
	return filepath.Join(c.dirOf(), strconv.Itoa(sizePx), shard(quickHash), quickHash+thumbExt)
}

// failPath is the negative-marker path for a (size, quickHash).
func (c *Cache) failPath(sizePx int, quickHash string) string {
	return filepath.Join(c.dirOf(), strconv.Itoa(sizePx), shard(quickHash), quickHash+failMarkerExt)
}

// Ensure returns the path to the cached JPEG thumbnail for (srcPath, quickHash)
// at sizePx, generating it if necessary. Concurrent callers for the same key
// share one generation. It returns ErrSourceMissing when srcPath does not exist
// and ErrGenerationFailed (with a persisted negative marker) when rendering fails.
func (c *Cache) Ensure(ctx context.Context, srcPath, quickHash string, sizePx int) (string, error) {
	if !validSize(sizePx) {
		return "", ErrUnsupportedSize
	}
	if quickHash == "" {
		return "", ErrAssetNotFound
	}
	dst := c.thumbPath(sizePx, quickHash)
	// Fast path: already cached. A cheap stat avoids taking the flight lock for the
	// overwhelmingly common cache-hit case.
	if fileExists(dst) {
		return dst, nil
	}

	key := strconv.Itoa(sizePx) + "/" + quickHash
	return c.flight.Do(key, func() (string, error) {
		// Re-check under the flight: a concurrent caller for this key may have just
		// finished, and the negative marker may already exist.
		if fileExists(dst) {
			return dst, nil
		}
		if fileExists(c.failPath(sizePx, quickHash)) {
			return "", ErrGenerationFailed
		}
		if srcPath == "" || !fileExists(srcPath) {
			// Missing/moved source: not a cacheable failure — the file may return.
			return "", ErrSourceMissing
		}

		// Bound global generation concurrency; honor cancellation while queued.
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-c.sem }()

		if err := c.generateAtomic(ctx, srcPath, dst, sizePx); err != nil {
			// A cancelled context is transient — do not poison the cache with a
			// negative marker for it.
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			c.markFailed(sizePx, quickHash)
			c.log.Warn("thumbnail generation failed",
				"src", srcPath, "size", sizePx, "error", err.Error())
			return "", fmt.Errorf("%w: %v", ErrGenerationFailed, err)
		}
		return dst, nil
	})
}

// generateAtomic renders into a temp file in the destination directory and then
// atomically renames it into place, so a concurrent reader never observes a
// partially-written thumbnail.
func (c *Cache) generateAtomic(ctx context.Context, srcPath, dst string, sizePx int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("thumbs: create cache dir: %w", err)
	}
	tmp := dst + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := c.gen.generate(ctx, srcPath, tmp, sizePx, c.quality); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The generator must have produced a non-empty file.
	if info, err := os.Stat(tmp); err != nil || info.Size() == 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("thumbs: generator produced no output for %q", srcPath)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("thumbs: publish thumbnail: %w", err)
	}
	return nil
}

// markFailed writes a negative marker so this key is not retried until the next
// restart. Best effort: a failure to write the marker only costs a future retry.
func (c *Cache) markFailed(sizePx int, quickHash string) {
	p := c.failPath(sizePx, quickHash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// sweepFailMarkers deletes every negative marker under the cache root so a render
// that failed transiently in a previous run gets one fresh attempt after
// restart. Runs once at construction; missing cache dir is not an error.
func (c *Cache) sweepFailMarkers() {
	dir := c.dirOf()
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // keep sweeping despite per-entry errors
		}
		if !d.IsDir() && filepath.Ext(path) == failMarkerExt {
			_ = os.Remove(path)
		}
		return nil
	})
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// flight is a minimal singleflight: concurrent Do calls sharing a key run the
// function once and all receive the same result. PAIM does not vendor
// golang.org/x/sync, so this is implemented directly.
type flight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	val string
	err error
}

// Do runs fn for key, deduplicating concurrent callers. The winner executes fn;
// followers block until it completes and return its result.
func (f *flight) Do(key string, fn func() (string, error)) (string, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = make(map[string]*flightCall)
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &flightCall{}
	c.wg.Add(1)
	f.calls[key] = c
	f.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	return c.val, c.err
}
