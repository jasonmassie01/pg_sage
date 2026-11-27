package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// apiauth-10: every state-changing API request passes Go's cross-origin
// protection, so a same-site page (a sibling subdomain, another port on
// the host) cannot drive the multipart import or any other write.

const csrfImportPath = "/api/v1/databases/managed/import"

// csrfRouter builds the standalone router with a caller middleware that
// records whether a request got through the chain and answers 202 without
// touching a database, so a refusal is visible as "never reached".
func csrfRouter(t *testing.T, cfg *config.Config, rt *RuntimeDeps) (http.Handler, *bool) {
	t.Helper()
	reached := new(bool)
	probe := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*reached = true
			w.WriteHeader(http.StatusAccepted)
		})
	}
	h := newTestRouter(fleet.NewManager(cfg), cfg, nil, nil, nil, nil, rt, probe)
	return h, reached
}

// multipartImport is the admin CSV import a plain HTML form can send.
func multipartImport(t *testing.T, fetchSite string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "dbs.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("name,host,port\nevil,attacker.example,5432\n"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://sage.example"+csrfImportPath, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "sage_session", Value: "admin-session"})
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	return req
}

func serveCSRF(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCSRF_MultipartImportFromSameSiteIsRefused(t *testing.T) {
	for _, site := range []string{"same-site", "cross-site"} {
		t.Run(site, func(t *testing.T) {
			h, reached := csrfRouter(t, config.DefaultConfig(), nil)
			req := multipartImport(t, site)
			req.Header.Set("Origin", "https://sibling.sage.example")
			rec := serveCSRF(h, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s multipart import = %d, want 403", site, rec.Code)
			}
			if *reached {
				t.Fatalf("%s multipart import reached the API chain", site)
			}
			if !strings.Contains(rec.Body.String(), "cross-origin") {
				t.Fatalf("refusal body %q does not say why", rec.Body.String())
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("refusal Content-Type = %q, want application/json",
					rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("refusal lost the security headers")
			}
		})
	}
}

func TestCSRF_SameOriginMultipartImportPasses(t *testing.T) {
	for _, site := range []string{"same-origin", "none"} {
		h, reached := csrfRouter(t, config.DefaultConfig(), nil)
		rec := serveCSRF(h, multipartImport(t, site))
		if rec.Code != http.StatusAccepted || !*reached {
			t.Fatalf("Sec-Fetch-Site %s import = %d reached=%v, want 202 reached",
				site, rec.Code, *reached)
		}
	}
}

func TestCSRF_NonBrowserClientWithoutOriginIsUnchanged(t *testing.T) {
	h, reached := csrfRouter(t, config.DefaultConfig(), nil)
	req := httptest.NewRequest(http.MethodPost, "http://sidecar:8080/api/v1/emergency-stop",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := serveCSRF(h, req)
	if rec.Code != http.StatusAccepted || !*reached {
		t.Fatalf("curl-style POST = %d reached=%v, want 202 reached", rec.Code, *reached)
	}
	// The JSON content-type check still guards every other write route.
	h, reached = csrfRouter(t, config.DefaultConfig(), nil)
	req = httptest.NewRequest(http.MethodPost, "http://sidecar:8080/api/v1/emergency-stop",
		strings.NewReader("a=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serveCSRF(h, req); rec.Code != http.StatusUnsupportedMediaType || *reached {
		t.Fatalf("form POST = %d reached=%v, want 415 unreached", rec.Code, *reached)
	}
}

// oldBrowserPost is a write from a browser that sends Origin but no
// Sec-Fetch-Site, reaching a sidecar whose Host a proxy rewrote.
func oldBrowserPost(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/emergency-stop",
		strings.NewReader(`{}`))
	req.Host = "sidecar:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	return req
}

func TestCSRF_OriginHostMismatchRefusedUntilTrusted(t *testing.T) {
	cfg := config.DefaultConfig()
	h, reached := csrfRouter(t, cfg, nil)
	if rec := serveCSRF(h, oldBrowserPost("https://ui.example")); rec.Code !=
		http.StatusForbidden || *reached {
		t.Fatalf("untrusted Origin = %d reached=%v, want 403", rec.Code, *reached)
	}
	// An Origin naming the request's own Host is same-origin.
	h, reached = csrfRouter(t, cfg, nil)
	if rec := serveCSRF(h, oldBrowserPost("http://sidecar:8080")); rec.Code !=
		http.StatusAccepted || !*reached {
		t.Fatalf("matching Origin = %d reached=%v, want 202", rec.Code, *reached)
	}

	trusted := config.DefaultConfig()
	trusted.API.TrustedOrigins = []string{"https://ui.example"}
	h, reached = csrfRouter(t, trusted, nil)
	if rec := serveCSRF(h, oldBrowserPost("https://ui.example")); rec.Code !=
		http.StatusAccepted || !*reached {
		t.Fatalf("trusted Origin = %d reached=%v, want 202", rec.Code, *reached)
	}
	// Trust is exact: another port or scheme of the same host is not listed.
	for _, other := range []string{"https://ui.example:8443", "http://ui.example",
		"https://evil.ui.example"} {
		h, reached = csrfRouter(t, trusted, nil)
		if rec := serveCSRF(h, oldBrowserPost(other)); rec.Code != http.StatusForbidden ||
			*reached {
			t.Fatalf("%s with only https://ui.example trusted = %d reached=%v",
				other, rec.Code, *reached)
		}
	}
}

func TestCSRF_TrustedOriginAdmitsCrossSiteFetch(t *testing.T) {
	trusted := config.DefaultConfig()
	trusted.API.TrustedOrigins = []string{"https://ui.example"}
	h, reached := csrfRouter(t, trusted, nil)
	req := multipartImport(t, "cross-site")
	req.Header.Set("Origin", "https://ui.example")
	if rec := serveCSRF(h, req); rec.Code != http.StatusAccepted || !*reached {
		t.Fatalf("trusted cross-site import = %d reached=%v, want 202", rec.Code, *reached)
	}
}

func TestCSRF_EmptyTrustedOriginsAddsNothing(t *testing.T) {
	for name, origins := range map[string][]string{"nil": nil, "empty": {}} {
		cfg := config.DefaultConfig()
		cfg.API.TrustedOrigins = origins
		h, reached := csrfRouter(t, cfg, nil)
		req := multipartImport(t, "cross-site")
		req.Header.Set("Origin", "https://ui.example")
		if rec := serveCSRF(h, req); rec.Code != http.StatusForbidden || *reached {
			t.Fatalf("%s trusted_origins: cross-site = %d reached=%v, want 403",
				name, rec.Code, *reached)
		}
	}
}

// The router itself requires a config; the protection builder must still
// refuse same-site writes when handed none.
func TestCSRF_NilConfigStillProtects(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	h := newCrossOriginProtection(nil, false).Handler(next)
	rec := serveCSRF(h, multipartImport(t, "same-site"))
	if rec.Code != http.StatusForbidden || reached {
		t.Fatalf("nil config same-site import = %d reached=%v, want 403", rec.Code, reached)
	}
}

func TestCSRF_SafeMethodsAreNotRefused(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		h, reached := csrfRouter(t, config.DefaultConfig(), nil)
		req := httptest.NewRequest(method, "/api/v1/databases", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "https://other.example")
		if rec := serveCSRF(h, req); rec.Code != http.StatusAccepted || !*reached {
			t.Fatalf("cross-site %s = %d reached=%v, want 202", method, rec.Code, *reached)
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		h, reached := csrfRouter(t, config.DefaultConfig(), nil)
		req := httptest.NewRequest(method, "/api/v1/config", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		if rec := serveCSRF(h, req); rec.Code != http.StatusForbidden || *reached {
			t.Fatalf("same-site %s = %d reached=%v, want 403", method, rec.Code, *reached)
		}
	}
}

func TestCSRF_NoCredentialedCORSForDevOrigins(t *testing.T) {
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:8080"} {
		h, _ := csrfRouter(t, config.DefaultConfig(), nil)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil)
		req.Header.Set("Origin", origin)
		rec := serveCSRF(h, req)
		for _, header := range []string{"Access-Control-Allow-Origin",
			"Access-Control-Allow-Credentials", "Access-Control-Allow-Methods"} {
			if got := rec.Header().Get(header); got != "" {
				t.Fatalf("GET with Origin %s set %s = %q", origin, header, got)
			}
		}
	}
	// A preflight is no longer answered 200 ahead of the API chain.
	h, reached := csrfRouter(t, config.DefaultConfig(), nil)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/databases", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := serveCSRF(h, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || !*reached {
		t.Fatalf("preflight = %d allow-origin=%q reached=%v, want no CORS, chain reached",
			rec.Code, rec.Header().Get("Access-Control-Allow-Origin"), *reached)
	}
}

func TestCSRF_DatabricksAppModeIgnoresTrustedOrigins(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.TrustedOrigins = []string{"https://ui.example"}
	h, reached := csrfRouter(t, cfg, &RuntimeDeps{DatabricksAppAuth: true})
	req := httptest.NewRequest(http.MethodPost,
		"https://sage.databricksapps.com/api/v1/emergency-stop", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://ui.example")
	if rec := serveCSRF(h, req); rec.Code != http.StatusForbidden || *reached {
		t.Fatalf("App mode trusted-origin write = %d reached=%v, want 403", rec.Code,
			*reached)
	}
}

// No concurrent-access test here: the protection is built once per router
// and only read afterwards; secure_cookie_test.go covers the shared
// forwarded-proto trust function under concurrency.

// The Vite dev server proxies /api without changing Host (changeOrigin is
// off), so a dashboard write through it is same-origin by either check.
func TestCSRF_ViteDevProxyWriteIsSameOrigin(t *testing.T) {
	for _, fetchSite := range []string{"same-origin", ""} {
		h, reached := csrfRouter(t, config.DefaultConfig(), nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(`{}`))
		req.Host = "localhost:5173"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:5173")
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		if rec := serveCSRF(h, req); rec.Code != http.StatusAccepted || !*reached {
			t.Fatalf("Vite-proxied login (Sec-Fetch-Site %q) = %d reached=%v, want 202",
				fetchSite, rec.Code, *reached)
		}
	}
}
