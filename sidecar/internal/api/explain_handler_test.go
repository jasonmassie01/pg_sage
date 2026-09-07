package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func explainTestRouterForUser(user *auth.User) http.Handler {
	cfg := &config.Config{
		Mode:    "fleet",
		Explain: config.ExplainConfig{Enabled: true},
	}
	mgr := fleet.NewManager(cfg)
	userMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			ctx := context.WithValue(
				r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return NewRouterFull(mgr, cfg, nil, nil, nil, nil,
		userMiddleware)
}

func TestExplainRouteRequiresOperatorRole(t *testing.T) {
	r := explainTestRouterForUser(testViewerUser())
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/explain",
		strings.NewReader(`{"query":"SELECT 1","plan_only":true}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s",
			w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestExplainRouteAllowsOperatorRole(t *testing.T) {
	r := explainTestRouterForUser(testOperatorUser())
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/explain",
		strings.NewReader(`{"query":"SELECT 1","plan_only":true}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("operator was denied; body=%s", w.Body.String())
	}
}
