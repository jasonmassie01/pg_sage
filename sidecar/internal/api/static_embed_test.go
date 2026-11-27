package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func staticRequest(t *testing.T, path, acceptEncoding string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	return req
}

func staticServe(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// entryChunkPath returns the module script that the embedded index.html loads.
func entryChunkPath(t *testing.T) string {
	t.Helper()
	index, err := fs.ReadFile(staticFiles, "dist/index.html")
	if err != nil {
		t.Fatalf("embedded index.html: %v", err)
	}
	m := regexp.MustCompile(`<script[^>]+src="(/assets/[^"]+\.js)"`).FindSubmatch(index)
	if m == nil {
		t.Fatal("embedded index.html names no /assets/*.js entry chunk")
	}
	return string(m[1])
}

// CHECK-20: the embedded entry chunk travels gzipped at under 40% of its size.
func TestStaticEmbedded_EntryChunkGzipUnder40Percent(t *testing.T) {
	entry := entryChunkPath(t)
	raw, err := fs.ReadFile(staticFiles, "dist"+entry)
	if err != nil {
		t.Fatalf("read %s: %v", entry, err)
	}
	r := testRouter("db1")
	req := staticRequest(t, entry, "gzip, deflate, br")
	rec := staticServe(r, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", entry, rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("GET %s not gzipped: %v", entry, rec.Header())
	}
	if rec.Header().Get("Cache-Control") != immutableCache {
		t.Fatalf("GET %s Cache-Control = %q", entry, rec.Header().Get("Cache-Control"))
	}
	if got, limit := rec.Body.Len(), len(raw)*40/100; got >= limit {
		t.Fatalf("gzip entry chunk = %d bytes, want < %d (40%% of %d)", got, limit, len(raw))
	}
}

// CHECK-21: the dashboard shell through the real router revalidates every load.
func TestStaticEmbedded_RootIsNoCache(t *testing.T) {
	r := testRouter("db1")
	for _, p := range []string{"/", "/cases/12"} {
		rec := staticServe(r, staticRequest(t, p, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s Cache-Control = %q, want no-cache", p, got)
		}
		if rec.Header().Get("ETag") == "" {
			t.Fatalf("GET %s has no ETag", p)
		}
	}
	rec := staticServe(r, staticRequest(t, "/health", "gzip"))
	if rec.Body.String() != `{"status":"ok"}` || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("/health changed: %q %v", rec.Body.String(), rec.Header())
	}
}
