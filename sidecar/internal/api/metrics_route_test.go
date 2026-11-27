package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// observability-08: with prometheus.serve_on_api, GET /metrics on the API
// listener serves the Prometheus exposition to a metrics-scoped token (or,
// in a Databricks App, to the platform identity of any pg_sage role)
// and refuses everything else without leaking a metric name.

const metricsStubBody = "# HELP pg_sage_info pg_sage version\n" +
	"# TYPE pg_sage_info gauge\npg_sage_info{version=\"test\"} 1\n"

func metricsStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprint(w, metricsStubBody)
	})
}

type fakeMetricsTokens struct {
	mu      sync.Mutex
	err     error
	secrets []string
}

func (f *fakeMetricsTokens) ValidateMetrics(_ context.Context, secret string,
) (mcptoken.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets = append(f.secrets, secret)
	if f.err != nil {
		return mcptoken.Grant{}, f.err
	}
	return mcptoken.Grant{TokenID: "tok-1", Scopes: []string{mcptoken.ScopeMetrics}}, nil
}

func scrape(h http.Handler, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func requireExposition(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type = %q, want text/plain; version=0.0.4", ct)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("body does not parse as Prometheus text: %v\n%s", err, w.Body.String())
	}
	if _, ok := families["pg_sage_info"]; !ok {
		t.Fatalf("families = %v, want pg_sage_info", families)
	}
}

func requireRefused(t *testing.T, w *httptest.ResponseRecorder, codes ...int) {
	t.Helper()
	ok := false
	for _, c := range codes {
		ok = ok || w.Code == c
	}
	if !ok {
		t.Fatalf("status = %d, want one of %v; body %q", w.Code, codes, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "pg_sage_") {
		t.Fatalf("refusal body leaks metric names: %q", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}
}

func TestMetricsRouteServesAMetricsToken(t *testing.T) {
	tokens := &fakeMetricsTokens{}
	h := metricsAuth(metricsStub(), tokens, false)
	requireExposition(t, scrape(h, "Bearer pgs_mcp_good"))
	if len(tokens.secrets) != 1 || tokens.secrets[0] != "pgs_mcp_good" {
		t.Fatalf("validated secrets = %v, want [pgs_mcp_good]", tokens.secrets)
	}
	// The scheme is case-insensitive (RFC 7235).
	requireExposition(t, scrape(h, "bearer pgs_mcp_good"))
}

func TestMetricsRouteRefusesMissingAndForeignCredentials(t *testing.T) {
	tokens := &fakeMetricsTokens{}
	h := metricsAuth(metricsStub(), tokens, false)
	for _, header := range []string{"", "Basic dXNlcjpwYXNz", "Bearer", "Bearer   ", "pgs_mcp_x"} {
		t.Run(header, func(t *testing.T) {
			w := scrape(h, header)
			requireRefused(t, w, http.StatusUnauthorized)
			if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge",
					w.Header().Get("WWW-Authenticate"))
			}
		})
	}
	if len(tokens.secrets) != 0 {
		t.Fatalf("validated %v; a missing credential must not reach the store", tokens.secrets)
	}
}

// Error propagation: the store's verdict maps to distinct statuses.
func TestMetricsRouteMapsValidationErrors(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{mcptoken.ErrUnauthorized, http.StatusUnauthorized},
		{fmt.Errorf("wrapped: %w", mcptoken.ErrUnauthorized), http.StatusUnauthorized},
		{mcptoken.ErrScopeMissing, http.StatusForbidden},
		{errors.New("connection refused"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			h := metricsAuth(metricsStub(), &fakeMetricsTokens{err: tc.err}, false)
			w := scrape(h, "Bearer pgs_mcp_whatever")
			requireRefused(t, w, tc.want)
			if strings.Contains(w.Body.String(), "connection refused") {
				t.Fatalf("body leaks the storage error: %q", w.Body.String())
			}
		})
	}
}

// Nil/empty: no token store (no control database) refuses, never serves.
func TestMetricsRouteWithoutTokenStoreRefuses(t *testing.T) {
	h := metricsAuth(metricsStub(), nil, false)
	requireRefused(t, scrape(h, "Bearer pgs_mcp_whatever"), http.StatusServiceUnavailable)
}

func asUser(h http.Handler, user *auth.User) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user != nil {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		}
		h.ServeHTTP(w, r)
	})
}

// Databricks App: the platform identity authenticates; every pg_sage role
// holds the read-level metrics grant (least privilege for a scraper is a
// viewer); an unknown role is refused; a bearer token is not consulted.
func TestMetricsRouteDatabricksIdentity(t *testing.T) {
	tokens := &fakeMetricsTokens{}
	for _, tc := range []struct {
		user *auth.User
		want int
	}{
		{&auth.User{ID: 1, Email: "a@x", Role: auth.RoleAdmin}, http.StatusOK},
		{&auth.User{ID: 2, Email: "o@x", Role: auth.RoleOperator}, http.StatusOK},
		{&auth.User{ID: 3, Email: "v@x", Role: auth.RoleViewer}, http.StatusOK},
		{&auth.User{ID: 5, Email: "e@x", Role: ""}, http.StatusForbidden},
		{&auth.User{ID: 4, Email: "?@x", Role: "robot"}, http.StatusForbidden},
		{nil, http.StatusUnauthorized},
	} {
		h := asUser(metricsAuth(metricsStub(), tokens, true), tc.user)
		w := scrape(h, "Bearer pgs_mcp_ignored")
		if tc.want == http.StatusOK {
			requireExposition(t, w)
			continue
		}
		requireRefused(t, w, tc.want)
	}
	if len(tokens.secrets) != 0 {
		t.Fatalf("validated %v; Databricks mode must not consult tokens", tokens.secrets)
	}
}

func TestMetricsRouteConcurrentScrapes(t *testing.T) {
	h := metricsAuth(metricsStub(), &fakeMetricsTokens{}, false)
	var wg sync.WaitGroup
	codes := make(chan int, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- scrape(h, "Bearer pgs_mcp_good").Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a concurrent scrape got %d", code)
		}
	}
}

func metricsRouter(t *testing.T, route *MetricsRoute) http.Handler {
	t.Helper()
	return newTestRouter(nil, config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{Metrics: route})
}

// State transition: serve_on_api off (no route) leaves /metrics to the
// dashboard's SPA fallback, exactly as today; on, the route is guarded.
func TestMetricsRouteMountedOnlyWhenConfigured(t *testing.T) {
	off := scrape(metricsRouter(t, nil), "")
	if strings.Contains(off.Body.String(), "pg_sage_info") {
		t.Fatalf("without serve_on_api /metrics served the exposition: %q", off.Body.String())
	}
	on := metricsRouter(t, &MetricsRoute{Handler: metricsStub()})
	// No control pool: the token check is unavailable, so it fails closed.
	requireRefused(t, scrape(on, "Bearer pgs_mcp_x"), http.StatusServiceUnavailable)
	requireRefused(t, scrape(on, ""), http.StatusUnauthorized)
	post := httptest.NewRecorder()
	on.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if strings.Contains(post.Body.String(), "pg_sage_info") {
		t.Fatalf("POST /metrics served the exposition: %q", post.Body.String())
	}
}

// The route's middlewares (rate limiter, platform identity) run before the
// credential check, outermost first.
func TestMetricsRouteRunsItsMiddlewaresInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mark := func(name string, stop bool) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				if stop {
					http.Error(w, "limited", http.StatusTooManyRequests)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
	}
	h := metricsRouter(t, &MetricsRoute{Handler: metricsStub(),
		Middlewares: []func(http.Handler) http.Handler{mark("outer", false),
			mark("inner", true)}})
	w := scrape(h, "Bearer pgs_mcp_x")
	if w.Code != http.StatusTooManyRequests || strings.Join(order, ",") != "outer,inner" {
		t.Fatalf("status %d, order %v; want 429 after outer,inner", w.Code, order)
	}
}
