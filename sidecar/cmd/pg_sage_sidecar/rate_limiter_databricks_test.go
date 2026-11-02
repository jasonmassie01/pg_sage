package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

func rateLimitAppConfig(t *testing.T) auth.DatabricksAppConfig {
	t.Helper()
	env := map[string]string{"SAGE_AUTH_MODE": auth.DatabricksAppMode, "DATABRICKS_APP_PORT": "8000", "DATABRICKS_HOST": "https://workspace.cloud.databricks.com", "SAGE_DATABRICKS_ADMIN_EMAIL": "owner@databricks.com", "SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS": "databricks.com"}
	cfg, err := auth.LoadDatabricksAppConfigFromEnv(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	return *cfg
}

func rateLimitAppRequest(h http.Handler, values ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/mode", nil)
	r.RemoteAddr = "198.51.100.99:4321"
	r.Header["X-Forwarded-Email"] = values
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDatabricksAppRateLimiterSeparatesEmployeesAtSharedIP(t *testing.T) {
	cfg := rateLimitAppConfig(t)
	rl := NewRateLimiter(0)
	t.Cleanup(rl.Stop)
	h := databricksAppRateLimitMiddleware(rl, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for i := 0; i < config.DefaultRateLimit; i++ {
		email := "one@databricks.com"
		if i%2 == 1 {
			email = "ONE@DATABRICKS.COM"
		}
		if w := rateLimitAppRequest(h, email); w.Code != http.StatusNoContent {
			t.Fatalf("request%d=%d", i+1, w.Code)
		}
	}
	if w := rateLimitAppRequest(h, "One@Databricks.Com"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("request61=%d", w.Code)
	}
	if w := rateLimitAppRequest(h, "two@databricks.com"); w.Code != http.StatusNoContent {
		t.Fatalf("second employee inherited bucket: %d", w.Code)
	}
	other := cfg
	other.Workspace = "https://other.cloud.databricks.com"
	h = databricksAppRateLimitMiddleware(rl, other, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if w := rateLimitAppRequest(h, "one@databricks.com"); w.Code != http.StatusNoContent {
		t.Fatalf("other workspace inherited bucket: %d", w.Code)
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.windows) != 3 {
		t.Fatalf("buckets=%d", len(rl.windows))
	}
	for key := range rl.windows {
		if !strings.HasPrefix(key, "databricks-app:") || strings.Contains(key, "@") || strings.Contains(key, "workspace") {
			t.Fatalf("raw identity key: %q", key)
		}
	}
}

func TestDatabricksAppRateLimiterInvalidIdentityFallsBackToIP(t *testing.T) {
	cfg := rateLimitAppConfig(t)
	rl := NewRateLimiter(1)
	t.Cleanup(rl.Stop)
	h := databricksAppRateLimitMiddleware(rl, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if w := rateLimitAppRequest(h); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	for _, values := range [][]string{{""}, {"one@databricks.com", "two@databricks.com"}, {"one@databricks.com,two@databricks.com"}, {"One <one@databricks.com>"}, {" one@databricks.com"}, {"one@external.example"}, {"one@sub.databricks.com"}, {"one@databricks.com.evil.example"}} {
		if w := rateLimitAppRequest(h, values...); w.Code != http.StatusTooManyRequests {
			t.Fatalf("%q allocated new bucket: %d", values, w.Code)
		}
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.windows) != 1 || len(rl.windows["198.51.100.99"]) != 1 {
		t.Fatalf("invalid identity buckets: %v", rl.windows)
	}
}

func TestDatabricksAppRateLimiterRequiresExplicitWiring(t *testing.T) {
	for _, appMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "session", true: "app"}[appMode], func(t *testing.T) {
			cfg := config.DefaultConfig()
			rl := NewRateLimiter(1)
			t.Cleanup(rl.Stop)
			params := WireParams{Cfg: cfg, RateLimiter: rl}
			if appMode {
				app := rateLimitAppConfig(t)
				params.DatabricksAppAuth = &app
			}
			h := wireRouter(params).Handler
			for i, email := range []string{"one@databricks.com", "two@databricks.com", "ONE@DATABRICKS.COM"} {
				w := rateLimitAppRequest(h, email)
				want := http.StatusOK
				if i == 2 || (!appMode && i == 1) {
					want = http.StatusTooManyRequests
				}
				if w.Code != want {
					t.Fatalf("request%d=%d want%d: %s", i+1, w.Code, want, w.Body.String())
				}
			}
			if appMode {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
				r.RemoteAddr = "198.51.100.99:4321"
				r.Header.Set("X-Forwarded-Email", "one@databricks.com")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusTooManyRequests {
					t.Fatalf("exhausted App request reached nil auth store: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
