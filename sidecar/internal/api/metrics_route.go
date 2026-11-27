package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// MetricsRoute serves GET /metrics on the API listener when
// prometheus.serve_on_api is set (observability-08): the Prometheus
// exposition, guarded by a token holding the metrics scope or, in a
// Databricks App, by the platform identity of any pg_sage role.
type MetricsRoute struct {
	// Handler renders the exposition (the separate listener's handler).
	Handler http.Handler
	// Middlewares run before the credential check, outermost first: the
	// API rate limiter and, in a Databricks App, the platform identity.
	Middlewares []func(http.Handler) http.Handler
}

// metricsTokens validates scrape credentials (*mcptoken.Store).
type metricsTokens interface {
	ValidateMetrics(ctx context.Context, secret string) (mcptoken.Grant, error)
}

// registerMetricsRoute mounts the guarded route beside /ready; nil route
// (serve_on_api off) leaves /metrics to the dashboard as before.
func registerMetricsRoute(root *http.ServeMux, metricsRoute *MetricsRoute, pool *pgxpool.Pool,
	appMode bool) {
	if metricsRoute == nil || metricsRoute.Handler == nil {
		return
	}
	var tokens metricsTokens
	if store := mcpTokenStore(pool); store != nil {
		tokens = store
	}
	h := metricsAuth(metricsRoute.Handler, tokens, appMode)
	for i := len(metricsRoute.Middlewares) - 1; i >= 0; i-- {
		h = metricsRoute.Middlewares[i](h)
	}
	// The scrape authenticates itself (metricsAuth): a metrics token, or the
	// App identity in a Databricks App.
	route(root, "GET /metrics", gateSelfAuth, securityHeadersMiddleware(h))
}

// metricsAuth admits a scrape to next. In a Databricks App the platform
// identity (set by DatabricksAppAuthMiddleware) must hold a pg_sage role
// (viewer is enough: metrics is read-level), and no token is consulted;
// elsewhere the request needs `Authorization: Bearer` with a valid,
// unrevoked metrics token. Refusals carry no metric names.
func metricsAuth(next http.Handler, tokens metricsTokens, appMode bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if appMode {
			serveMetricsForIdentity(w, r, next)
			return
		}
		secret, ok := bearerCredential(r)
		if !ok || secret == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pg_sage metrics"`)
			jsonError(w, "a bearer token with the metrics scope is required",
				http.StatusUnauthorized)
			return
		}
		if tokens == nil {
			jsonError(w, "metrics token validation unavailable: no control database",
				http.StatusServiceUnavailable)
			return
		}
		_, err := tokens.ValidateMetrics(r.Context(), secret)
		switch {
		case errors.Is(err, mcptoken.ErrUnauthorized):
			w.Header().Set("WWW-Authenticate", `Bearer realm="pg_sage metrics"`)
			jsonError(w, "invalid, expired or revoked metrics token", http.StatusUnauthorized)
		case errors.Is(err, mcptoken.ErrScopeMissing):
			jsonError(w, "the token lacks the metrics scope", http.StatusForbidden)
		case err != nil:
			// Not the scraper's fault; the secret is never logged.
			slog.Error("metrics token validation failed", "err", err)
			jsonError(w, "metrics token validation unavailable", http.StatusServiceUnavailable)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// serveMetricsForIdentity admits a platform identity with a pg_sage role.
func serveMetricsForIdentity(w http.ResponseWriter, r *http.Request, next http.Handler) {
	user := UserFromContext(r.Context())
	switch {
	case user == nil:
		jsonError(w, "Databricks authentication required", http.StatusUnauthorized)
	case user.Role != auth.RoleAdmin && user.Role != auth.RoleOperator &&
		user.Role != auth.RoleViewer:
		jsonError(w, "metrics require a pg_sage role", http.StatusForbidden)
	default:
		next.ServeHTTP(w, r)
	}
}
