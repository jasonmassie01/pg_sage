package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// These tests pin the Databricks Apps service-principal boundary while the
// databricks-09 spike is open: until a live header capture proves what the
// Apps proxy forwards for a bearer-token service principal (and that it
// overwrites caller-supplied copies), no platform header may establish an
// agent identity, and the MCP, specialist and agent-token transports stay
// 501 even when their handlers are wired.
//
// No state-transition or PostgreSQL integration tests: there is no
// service-principal binding state until the spike's build step ships; the
// email path's database behaviour is covered by databricks_app_pin_db_test.go.

const (
	spikeAppID      = "6a1f3e2b-9c4d-4e5f-8a7b-0c1d2e3f4a5b"
	spikeForgedJWT  = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiI2YTFmM2UyYiJ9.c2ln"
	spikeViewerMail = "viewer@example.com"
)

// spikeHeaders sets every header a service principal's call might carry
// through the Apps proxy, or that a caller might forge.
func spikeHeaders(r *http.Request) {
	r.Header.Set("X-Forwarded-User", spikeAppID)
	r.Header.Set("X-Forwarded-Preferred-Username", "sage-agent-sp")
	r.Header.Set("X-Forwarded-Access-Token", spikeForgedJWT)
	r.Header.Set("Authorization", "Bearer "+spikeForgedJWT)
	r.Header.Set("X-Databricks-App-Id", spikeAppID)
}

func TestDatabricksAppSPApplicationIDNeverAuthenticatesAsUser(t *testing.T) {
	for _, tc := range []struct{ name, email string }{
		{"application id", spikeAppID},
		{"upper-case application id", strings.ToUpper(spikeAppID)},
		{"numeric principal id", "1234567890123456"},
		{"display name", "sage-agent-sp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := databricksAppAuthMiddleware(func(
				context.Context, string, string,
			) (*auth.User, error) {
				called = true
				return &auth.User{ID: 1, Role: auth.RoleAdmin}, nil
			})(http.HandlerFunc(meHandler()))
			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
			r.Header.Set("X-Forwarded-Email", tc.email)
			spikeHeaders(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || called {
				t.Fatalf("non-email platform subject %q: status=%d resolver=%v body=%s",
					tc.email, w.Code, called, w.Body.String())
			}
		})
	}
}

func TestDatabricksAppSPHeadersNeverSelectAnotherIdentity(t *testing.T) {
	var gotEmail, gotUser string
	calls := 0
	h := databricksAppAuthMiddleware(func(
		_ context.Context, email, forwardedUser string,
	) (*auth.User, error) {
		calls++
		gotEmail, gotUser = email, forwardedUser
		return &auth.User{ID: 8, Email: email, Role: auth.RoleViewer}, nil
	})(http.HandlerFunc(meHandler()))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.Header.Set("X-Forwarded-Email", spikeViewerMail)
	spikeHeaders(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	// The account is selected by the email alone; X-Forwarded-User is only
	// the pin value, and no token or username header reaches the resolver.
	if gotEmail != spikeViewerMail || gotUser != spikeAppID {
		t.Fatalf("resolver got email=%q user=%q", gotEmail, gotUser)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"role":"viewer"`) || strings.Contains(body, "sage-agent-sp") {
		t.Fatalf("identity body = %s", body)
	}
}

// spikeAppRouter builds an App-mode router with the MCP and specialist
// handlers wired, so the protocol guard is the only thing keeping them off.
func spikeAppRouter(t *testing.T, appMode bool, hits *atomic.Int32) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Transport = "http"
	record := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	})
	rt := &RuntimeDeps{DatabricksAppAuth: appMode, MCPHandler: record, Specialist: record}
	var mws []func(http.Handler) http.Handler
	if appMode {
		mws = append(mws, databricksAppAuthMiddleware(func(
			_ context.Context, email, _ string,
		) (*auth.User, error) {
			return &auth.User{ID: 1, Email: email, Role: auth.RoleAdmin}, nil
		}))
	}
	return newTestRouter(fleet.NewManager(cfg), cfg, nil, nil, nil, nil, rt, mws...)
}

var spikeAgentPaths = []string{
	"/mcp", "/mcp/databases/orders", mcpEndpointPath, mcpDatabasePrefix + "orders",
	specialistPrefix + "investigate", "/api/v1/agents/sage-agent/tokens",
}

func TestDatabricksAppSPAgentTransportsStayUnsupported(t *testing.T) {
	var hits atomic.Int32
	h := spikeAppRouter(t, true, &hits)
	for _, path := range spikeAgentPaths {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			spikeHeaders(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("service-principal headers without an email: status=%d body=%s",
					w.Code, w.Body.String())
			}
			r.Header.Set("X-Forwarded-Email", "owner@example.com")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotImplemented ||
				!strings.Contains(w.Body.String(), "databricks_app_protocol_unsupported") {
				t.Fatalf("agent transport in App mode: status=%d body=%s",
					w.Code, w.Body.String())
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("an agent handler ran %d times in App mode", n)
	}
}

// The control: the same wiring outside App mode reaches the MCP route, so
// the App-mode refusals above come from the guard, not from the fixture.
func TestDatabricksAppSPStandaloneStillMountsMCP(t *testing.T) {
	var hits atomic.Int32
	h := spikeAppRouter(t, false, &hits)
	r := httptest.NewRequest(http.MethodPost, mcpEndpointPath, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Email", "owner@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), "mcp_token_required") {
		t.Fatalf("standalone MCP route: status=%d body=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, specialistPrefix+"investigate",
		strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot || hits.Load() != 1 {
		t.Fatalf("standalone specialist route: status=%d hits=%d", w.Code, hits.Load())
	}
}

func TestDatabricksAppSPConcurrentCallsStayUnsupported(t *testing.T) {
	var hits atomic.Int32
	h := spikeAppRouter(t, true, &hits)
	const workers = 16
	codes := make([]int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-Email", "owner@example.com")
			spikeHeaders(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusNotImplemented {
			t.Fatalf("request %d status=%d", i, code)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("an agent handler ran %d times in App mode", n)
	}
}
