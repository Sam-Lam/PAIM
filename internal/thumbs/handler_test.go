package thumbs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// stubResolver implements AssetResolver over a fixed map of asset ID -> source.
type stubResolver struct {
	src   map[string]string // assetID -> abs source path
	quick map[string]string // assetID -> quick hash
}

func (s stubResolver) Resolve(_ context.Context, id string) (string, string, error) {
	p, ok := s.src[id]
	if !ok {
		return "", "", ErrAssetNotFound
	}
	return p, s.quick[id], nil
}

func newTestHandler(t *testing.T, open bool) (*Handler, *fakeGen) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "img.jpg")
	writeJPEG(t, src)

	gen := &fakeGen{}
	cache := newCache(dir, gen, 4, nil)
	res := stubResolver{
		src:   map[string]string{"asset-1": src},
		quick: map[string]string{"asset-1": "abcdef0123"},
	}
	bind := func(context.Context) (Binding, bool) {
		if !open {
			return Binding{}, false
		}
		return Binding{Assets: res, Cache: cache}, true
	}
	return NewHandler(bind, nil), gen
}

func TestHandlerNoLibrary503(t *testing.T) {
	h, _ := newTestHandler(t, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/asset-1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandlerMissingAsset404(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerServesJPEGWithCacheHeaders(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/asset-1?s=512", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Errorf("cache-control = %q", cc)
	}
	if et := rec.Header().Get("ETag"); et != `"abcdef0123-512"` {
		t.Errorf("etag = %q", et)
	}
	if rec.Body.Len() == 0 {
		t.Error("empty body")
	}
}

func TestHandlerIfNoneMatch304(t *testing.T) {
	h, _ := newTestHandler(t, true)

	// Prime the ETag from a first request.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/asset-1", nil))
	etag := rec.Header().Get("ETag")

	req := httptest.NewRequest(http.MethodGet, "/thumb/asset-1", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Error("304 must have empty body")
	}
}

func TestHandlerDefaultsToGridSize(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/asset-1", nil))
	if et := rec.Header().Get("ETag"); et != `"abcdef0123-512"` {
		t.Errorf("default size etag = %q, want grid (512)", et)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/thumb/asset-1?s=2048", nil))
	if et := rec2.Header().Get("ETag"); et != `"abcdef0123-2048"` {
		t.Errorf("preview etag = %q, want 2048", et)
	}
}

func TestHandlerRejectsPathTraversal(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := httptest.NewRecorder()
	// An ID containing a slash is rejected outright (IDs never contain slashes).
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thumb/a/b", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for slashed id", rec.Code)
	}
}
