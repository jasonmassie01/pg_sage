package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

const authModeContextKey contextKey = "sage_auth_mode"

// DatabricksAppAuthMiddleware trusts only the Databricks Apps authenticated
// ingress email. It deliberately ignores local sessions, bearer-token bypasses,
// forwarded role/user headers and request-body identity. Never install it on an
// application port exposed directly to callers outside the Apps ingress.
func DatabricksAppAuthMiddleware(pool *pgxpool.Pool, cfg auth.DatabricksAppConfig) func(http.Handler) http.Handler {
	return databricksAppAuthMiddleware(func(ctx context.Context, email string) (*auth.User, error) {
		return auth.ResolveDatabricksAppUser(ctx, pool, cfg, email)
	})
}

func databricksAppAuthMiddleware(resolve func(context.Context, string) (*auth.User, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.URL.Path == "/api/v1/auth/mode" {
				next.ServeHTTP(w, r)
				return
			}
			values := r.Header.Values("X-Forwarded-Email")
			if len(values) != 1 {
				jsonError(w, "Databricks authentication required", http.StatusUnauthorized)
				return
			}
			email, err := auth.DatabricksAppEmail(values[0])
			if err != nil {
				jsonError(w, "invalid Databricks identity", http.StatusUnauthorized)
				return
			}
			user, err := resolve(r.Context(), email)
			if err != nil || user == nil {
				slog.Warn("Databricks app identity could not be resolved")
				jsonError(w, "Databricks account is unavailable", http.StatusForbidden)
				return
			}
			ctx := context.WithValue(r.Context(), userContextKey, user)
			ctx = context.WithValue(ctx, authModeContextKey, auth.DatabricksAppMode)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func databricksAppProtocolGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unsupportedDatabricksAppProtocol(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			jsonResponse(w, map[string]string{"error": "This protocol is unavailable in Databricks Apps: platform identity cannot yet preserve its agent/task/database or source authorization. Use the authenticated pg_sage user API.", "code": "databricks_app_protocol_unsupported"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A platform email must never be silently promoted into an agent identity or a
// source grant. These transports retain their existing standalone contracts.
func unsupportedDatabricksAppProtocol(r *http.Request) bool {
	p := r.URL.Path
	return p == "/mcp" || strings.HasPrefix(p, "/mcp/") || p == mcpEndpointPath || strings.HasPrefix(p, mcpEndpointPath+"/") ||
		strings.HasPrefix(p, specialistPrefix) || isSignedIngestPath(p) ||
		(strings.HasPrefix(p, "/api/v1/agents/") && strings.HasSuffix(p, "/tokens"))
}

func registerAuthModeRoute(mux *http.ServeMux, appMode bool) {
	mode := "session"
	if appMode {
		mode = auth.DatabricksAppMode
	}
	mux.HandleFunc("GET /api/v1/auth/mode", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, map[string]string{"mode": mode})
	})
}

func registerDatabricksAppAuthRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	// No local password, logout, OAuth/link, break-glass or account-creation
	// endpoints exist in this mode. App access is managed in Databricks; the
	// retained role endpoints govern authorization inside pg_sage.
	mux.HandleFunc("GET /api/v1/auth/me", meHandler())
	mux.Handle("GET /api/v1/users", RequireRole(auth.RoleAdmin)(http.HandlerFunc(listUsersHandler(pool))))
	mux.Handle("PUT /api/v1/users/{id}/role", RequireRole(auth.RoleAdmin)(http.HandlerFunc(updateUserRoleHandler(pool))))
}
