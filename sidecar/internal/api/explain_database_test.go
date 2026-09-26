package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// G1-B22: explain against an unknown database must 404 instead of
// silently running on the primary pool.
func TestExplainHandler_UnknownDatabaseReturns404(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cfg := &config.Config{
		Mode: "fleet",
		Explain: config.ExplainConfig{
			Enabled: true, TimeoutMs: 5000,
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "primary", Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true},
	})
	h := explainHandler(mgr, cfg, nil)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/explain?database=does-not-exist",
		strings.NewReader(`{"query":"SELECT 1","plan_only":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "database not found") {
		t.Errorf("body = %s, want database not found", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "plan_json") {
		t.Errorf("explain ran on a fallback pool: %s", w.Body.String())
	}
}

// The named database still explains normally.
func TestExplainHandler_KnownDatabaseRuns(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cfg := &config.Config{
		Mode: "fleet",
		Explain: config.ExplainConfig{
			Enabled: true, TimeoutMs: 5000,
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "primary", Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true},
	})
	h := explainHandler(mgr, cfg, nil)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/explain?database=primary",
		strings.NewReader(`{"query":"SELECT 1","plan_only":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"node_type":"Result"`) {
		t.Errorf("body = %s, want a Result plan node", w.Body.String())
	}
}
