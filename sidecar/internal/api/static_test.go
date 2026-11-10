package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

const (
	testIndexHTML = `<!doctype html><html><head><title>pg_sage</title>` +
		`<script type="module" src="/assets/x.js"></script></head></html>`
	immutableCache = "public, max-age=31536000, immutable"
)

var testAppJS = strings.Repeat("export const answer = 42; console.log(answer);\n", 200)

func testStaticFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte(testIndexHTML)},
		"assets/x.js":      {Data: []byte(testAppJS)},
		"assets/x.css":     {Data: []byte(strings.Repeat("body{color:red}\n", 100))},
		"assets/meme.jpg":  {Data: bytes.Repeat([]byte{0xff, 0xd8, 0xff}, 300)},
		"favicon.svg":      {Data: []byte(strings.Repeat("<svg></svg>", 40))},
		"memes/CREDITS.md": {Data: []byte(strings.Repeat("# Credits\n", 60))},
		"empty.js":         {Data: []byte{}},
		"logo.png":         {Data: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 200)},
	}
}

func newTestStatic(t *testing.T) http.Handler {
	t.Helper()
	h, err := newStaticHandler(testStaticFS())
	if err != nil {
		t.Fatalf("newStaticHandler: %v", err)
	}
	return h
}

func staticGet(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestStatic_AssetGzipWithImmutableCache(t *testing.T) {
	h := newTestStatic(t)
	rec := staticGet(h, "/assets/x.js", map[string]string{"Accept-Encoding": "gzip, br"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != immutableCache {
		t.Fatalf("Cache-Control = %q, want %q", got, immutableCache)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Fatalf("Content-Type = %q, want text/javascript", got)
	}
	if rec.Body.Len() >= len(testAppJS) {
		t.Fatalf("gzip body %d bytes is not smaller than raw %d", rec.Body.Len(), len(testAppJS))
	}
	if got := string(gunzip(t, rec.Body.Bytes())); got != testAppJS {
		t.Fatal("gunzipped body differs from the file")
	}
	if et := rec.Header().Get("ETag"); !regexp.MustCompile(`^"[0-9a-f]+-gzip"$`).MatchString(et) {
		t.Fatalf("gzip ETag = %q, want a strong gzip-specific tag", et)
	}
}

func TestStatic_IdentityWhenGzipNotAccepted(t *testing.T) {
	h := newTestStatic(t)
	cases := map[string]map[string]string{
		"no header":      nil,
		"gzip q=0":       {"Accept-Encoding": "gzip;q=0"},
		"gzip q=0.000":   {"Accept-Encoding": "br, gzip; q=0.000"},
		"only br":        {"Accept-Encoding": "br"},
		"identity only":  {"Accept-Encoding": "identity"},
		"bad q value":    {"Accept-Encoding": "gzip;q=lots"},
		"q above one":    {"Accept-Encoding": "gzip;q=2"},
		"star q=0":       {"Accept-Encoding": "*;q=0"},
		"gzip off, star": {"Accept-Encoding": "gzip;q=0, *"},
		"gzippy coding":  {"Accept-Encoding": "gzippy"},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			rec := staticGet(h, "/assets/x.js", hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want identity", got)
			}
			if rec.Body.String() != testAppJS {
				t.Fatal("identity body differs from the file")
			}
			if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Fatalf("Vary = %q, want Accept-Encoding", got)
			}
		})
	}
}

func TestStatic_GzipAcceptedForms(t *testing.T) {
	h := newTestStatic(t)
	forms := []string{"gzip", "GZIP", "deflate, gzip;q=0.5", "*", "x-gzip",
		"br;q=1, *;q=0.1"}
	for _, ae := range forms {
		rec := staticGet(h, "/assets/x.js", map[string]string{"Accept-Encoding": ae})
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want gzip", ae, got)
		}
	}
}

func TestStatic_SPAFallbackServesIndexNoCache(t *testing.T) {
	h := newTestStatic(t)
	for _, p := range []string{"/", "/cases/12", "/assets", "/settings/", "/index.html"} {
		rec := staticGet(h, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", p, rec.Code)
		}
		if rec.Body.String() != testIndexHTML {
			t.Fatalf("%s: body is not index.html: %q", p, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: Cache-Control = %q, want no-cache", p, got)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Fatalf("%s: Content-Type = %q, want text/html", p, got)
		}
	}
}

func TestStatic_MissingDottedPathIs404(t *testing.T) {
	h := newTestStatic(t)
	for _, p := range []string{"/missing.js", "/assets/gone.css", "/../index.html.bak",
		"/assets/x.js/extra.js"} {
		rec := staticGet(h, p, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "pg_sage") {
			t.Fatalf("%s: 404 served index.html", p)
		}
		if rec.Header().Get("Cache-Control") == immutableCache {
			t.Fatalf("%s: 404 marked immutable", p)
		}
	}
}

func TestStatic_OtherPublicFilesCacheOneHour(t *testing.T) {
	h := newTestStatic(t)
	rec := staticGet(h, "/memes/CREDITS.md", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q, want public, max-age=3600", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/markdown") {
		t.Fatalf("Content-Type = %q, want text/markdown (not sniffed gzip)", got)
	}
	if !strings.HasPrefix(string(gunzip(t, rec.Body.Bytes())), "# Credits") {
		t.Fatal("CREDITS.md gzip body is wrong")
	}
}

func TestStatic_EmptyAndBinaryFilesServedUncompressed(t *testing.T) {
	h := newTestStatic(t)
	ae := map[string]string{"Accept-Encoding": "gzip"}
	empty := staticGet(h, "/empty.js", ae)
	if empty.Code != http.StatusOK || empty.Body.Len() != 0 {
		t.Fatalf("empty.js = %d with %d bytes", empty.Code, empty.Body.Len())
	}
	if got := empty.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("empty file Content-Encoding = %q, want none", got)
	}
	png := staticGet(h, "/logo.png", ae)
	if got := png.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("png Content-Encoding = %q, want none", got)
	}
	if !bytes.Equal(png.Body.Bytes(), testStaticFS()["logo.png"].Data) {
		t.Fatal("png body differs from the file")
	}
	if png.Header().Get("ETag") == "" {
		t.Fatal("png has no ETag validator")
	}
	jpg := staticGet(h, "/assets/meme.jpg", ae)
	if jpg.Header().Get("Content-Encoding") != "" ||
		jpg.Header().Get("Cache-Control") != immutableCache {
		t.Fatalf("hashed jpg headers = %v", jpg.Header())
	}
}

func TestStatic_IfNoneMatchReturns304(t *testing.T) {
	h := newTestStatic(t)
	first := staticGet(h, "/favicon.svg", nil)
	etag := first.Header().Get("ETag")
	if !regexp.MustCompile(`^"[0-9a-f]+"$`).MatchString(etag) {
		t.Fatalf("identity ETag = %q, want a strong tag", etag)
	}
	again := staticGet(h, "/favicon.svg", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Fatalf("304 carried %d body bytes", again.Body.Len())
	}
	// The gzip representation has its own tag, so the identity tag must not
	// validate it and vice versa.
	gz := staticGet(h, "/favicon.svg", map[string]string{
		"If-None-Match": etag, "Accept-Encoding": "gzip"})
	if gz.Code != http.StatusOK || gz.Header().Get("ETag") == etag {
		t.Fatalf("gzip with identity tag = %d etag %q", gz.Code, gz.Header().Get("ETag"))
	}
	gz304 := staticGet(h, "/favicon.svg", map[string]string{
		"If-None-Match": gz.Header().Get("ETag"), "Accept-Encoding": "gzip"})
	if gz304.Code != http.StatusNotModified {
		t.Fatalf("gzip revalidation = %d, want 304", gz304.Code)
	}
	stale := staticGet(h, "/favicon.svg", map[string]string{"If-None-Match": `"deadbeef"`})
	if stale.Code != http.StatusOK || stale.Body.Len() == 0 {
		t.Fatalf("stale tag = %d with %d bytes, want 200 and body", stale.Code, stale.Body.Len())
	}
}

func TestStatic_HeadAndMethods(t *testing.T) {
	h := newTestStatic(t)
	req := httptest.NewRequest(http.MethodHead, "/assets/x.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d with %d bytes", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Cache-Control") != immutableCache {
		t.Fatalf("HEAD Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", got)
	}
}

func TestStatic_NilAndEmptyFS(t *testing.T) {
	if _, err := newStaticHandler(nil); err == nil {
		t.Fatal("nil fs: want an error")
	}
	h, err := newStaticHandler(fstest.MapFS{})
	if err != nil {
		t.Fatalf("empty fs: %v", err)
	}
	if rec := staticGet(h, "/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("empty fs GET / = %d, want 404", rec.Code)
	}
}

// brokenFS exposes only Open, so fs.ReadFile and fs.WalkDir must go through it.
type brokenFS struct{ inner fstest.MapFS }

func (b brokenFS) Open(name string) (fs.File, error) {
	if name == "assets/x.js" {
		return nil, fs.ErrPermission
	}
	return b.inner.Open(name)
}

func TestStatic_ReadErrorPropagates(t *testing.T) {
	_, err := newStaticHandler(brokenFS{testStaticFS()})
	if err == nil {
		t.Fatal("unreadable file: want an error")
	}
	if !strings.Contains(err.Error(), "assets/x.js") {
		t.Fatalf("error %q does not name the file", err)
	}
}

func TestStatic_ConcurrentRequests(t *testing.T) {
	// -race is unavailable on this machine (no cgo); this still checks that
	// parallel readers get complete, correct bodies from the shared handler.
	h := newTestStatic(t)
	var wg sync.WaitGroup
	errs := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hdr := map[string]string{}
			if i%2 == 0 {
				hdr["Accept-Encoding"] = "gzip"
			}
			rec := staticGet(h, "/assets/x.js", hdr)
			body := rec.Body.Bytes()
			if i%2 == 0 {
				zr, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					errs <- err.Error()
					return
				}
				body, _ = io.ReadAll(zr)
			}
			if rec.Code != http.StatusOK || string(body) != testAppJS {
				errs <- "request " + rec.Result().Status + " returned a wrong body"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
