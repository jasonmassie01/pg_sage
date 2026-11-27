package api

import (
	"io/fs"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// vanishingFS indexes normally, then fails every Open once gone is set, as an
// fs whose file disappears after startup would.
type vanishingFS struct {
	inner fstest.MapFS
	gone  *atomic.Bool
}

func (v vanishingFS) Open(name string) (fs.File, error) {
	if v.gone.Load() && name != "." {
		return nil, fs.ErrNotExist
	}
	return v.inner.Open(name)
}

func TestStatic_ReadFailureAfterStartupIsNotCached(t *testing.T) {
	gone := &atomic.Bool{}
	h, err := newStaticHandler(vanishingFS{inner: testStaticFS(), gone: gone})
	if err != nil {
		t.Fatalf("newStaticHandler: %v", err)
	}
	gone.Store(true)
	rec := staticGet(h, "/assets/meme.jpg", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("500 Cache-Control = %q, want no-store (never cache the failure)", got)
	}
	if et := rec.Header().Get("ETag"); et != "" {
		t.Fatalf("500 carried the asset ETag %q", et)
	}
	// The precompressed copy lives in memory, so gzip clients are unaffected.
	gz := staticGet(h, "/assets/x.js", map[string]string{"Accept-Encoding": "gzip"})
	if gz.Code != http.StatusOK || gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip copy = %d %v, want 200 gzip", gz.Code, gz.Header())
	}
}

func TestEmbeddedDashboard_BuiltOncePerProcess(t *testing.T) {
	first, second := embeddedDashboard(), embeddedDashboard()
	if first == nil {
		t.Fatal("embeddedDashboard returned nil")
	}
	a, okA := first.(*staticHandler)
	b, okB := second.(*staticHandler)
	if !okA || !okB {
		t.Fatalf("embedded dashboard is %T, want *staticHandler", first)
	}
	if a != b {
		t.Fatal("embeddedDashboard rebuilt the handler (re-gzipping the dist) on a second call")
	}
	if _, ok := a.assets[staticIndex]; !ok {
		t.Fatal("embedded handler has no index.html")
	}
}
