package thumbs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// URLPrefix is the path prefix the Wails asset-server middleware routes to the
// thumbnail handler.
const URLPrefix = "/thumb/"

// AssetResolver maps an asset ID to the absolute source path and quick hash the
// cache needs. It is implemented in internal/services over the AssetRepo and the
// portable-library path resolver. A missing/deleted asset must return
// ErrAssetNotFound.
type AssetResolver interface {
	Resolve(ctx context.Context, assetID string) (absPath, quickHash string, err error)
}

// Binding is the currently-open library's thumbnail dependencies, supplied to
// the Handler per request so it always sees the live catalog.
type Binding struct {
	Assets AssetResolver
	Cache  *Cache
}

// Handler serves GET /thumb/{assetID}?s=512|2048. It owns all HTTP concerns
// (routing, conditional requests, cache headers, streaming); generation logic
// lives in Cache and asset resolution in AssetResolver, so the main.go
// middleware stays a one-line prefix check.
type Handler struct {
	bind func(ctx context.Context) (Binding, bool)
	log  *slog.Logger
}

// NewHandler constructs a Handler. bind returns the live library binding, or
// ok=false when no library is open (→ 503).
func NewHandler(bind func(ctx context.Context) (Binding, bool), logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{bind: bind, log: logger.With(slog.String("subsystem", "thumbs"))}
}

// ServeHTTP implements the thumbnail endpoint. Asset identity is by ID only, so
// path traversal is impossible by construction — no filesystem path is ever
// taken from the request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	assetID := strings.TrimPrefix(r.URL.Path, URLPrefix)
	if assetID == "" || strings.Contains(assetID, "/") {
		http.NotFound(w, r)
		return
	}
	size := parseSize(r.URL.Query().Get("s"))

	binding, ok := h.bind(r.Context())
	if !ok {
		http.Error(w, "no library open", http.StatusServiceUnavailable)
		return
	}

	absPath, quickHash, err := binding.Assets.Resolve(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, ErrAssetNotFound) {
			http.NotFound(w, r)
			return
		}
		h.log.Warn("thumb: resolve asset", "assetId", assetID, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The ETag is keyed by the immutable content (quick hash) plus the size, so a
	// re-imported file with new content gets a new ETag while identical content is
	// cached forever by the client.
	etag := `"` + quickHash + "-" + strconv.Itoa(size) + `"`
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	thumbPath, err := binding.Cache.Ensure(r.Context(), absPath, quickHash, size)
	if err != nil {
		// Missing source or a failed render both surface as a 404; the frontend
		// swaps in a placeholder tile via <img onError>. Neither is logged loudly
		// here (Cache already logs generation failures once).
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(thumbPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "image/jpeg")
	if info, statErr := f.Stat(); statErr == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// parseSize maps the ?s= query value to a supported size, defaulting to the grid
// size for anything absent or unrecognized.
func parseSize(s string) int {
	if v, err := strconv.Atoi(s); err == nil && v == SizePreview {
		return SizePreview
	}
	return SizeGrid
}

// etagMatches reports whether an If-None-Match header value matches etag,
// tolerating a comma-separated list and a leading weak validator marker.
func etagMatches(header, etag string) bool {
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
}
