package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testStatic() *staticHandler {
	return &staticHandler{
		root: fstest.MapFS{
			"index.html":          {Data: []byte("<html></html>")},
			"assets/index-abc.js": {Data: []byte("console.log(1)")},
			"favicon.svg":         {Data: []byte("<svg/>")},
		},
		indexHTML: []byte("<html></html>"),
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Hashed assets are immutable for a year; the shell is never cached.
func TestHashedAssetsAreImmutableAndShellIsNoCache(t *testing.T) {
	h := testStatic()

	if got := get(h, "/assets/index-abc.js").Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q, want long immutable", got)
	}
	if got := get(h, "/").Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", got)
	}
	if got := get(h, "/favicon.svg").Header().Get("Cache-Control"); got == "public, max-age=31536000, immutable" {
		t.Errorf("non-hashed file must not be immutable, got %q", got)
	}
}

// A missing hashed asset is a 404, not the SPA shell with a 200 (which would be
// cached as the "script" and served to a browser expecting JS).
func TestMissingAssetIs404(t *testing.T) {
	h := testStatic()
	rec := get(h, "/assets/index-gone.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Header().Get("Cache-Control") == "public, max-age=31536000, immutable" {
		t.Errorf("a 404 must not be cached as immutable")
	}
}
