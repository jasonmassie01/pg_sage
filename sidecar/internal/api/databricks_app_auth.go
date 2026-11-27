package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

const authModeContextKey contextKey = "sage_auth_mode"

// DatabricksAppAuthMiddleware trusts only the Databricks Apps authenticated
// ingress email. It deliberately ignores local sessions, bearer-token bypasses,
// forwarded role headers and request-body identity. X-Forwarded-User never
// selects an account or a role: it is only compared with the value pinned to
// the email's account. Never install it on an application port exposed
// directly to callers outside the Apps ingress.
func DatabricksAppAuthMiddleware(pool *pgxpool.Pool, cfg auth.DatabricksAppConfig) func(http.Handler) http.Handler {
	return databricksAppAuthMiddleware(func(
		ctx context.Context, email, user string,
	) (*auth.User, error) {
		return auth.ResolveDatabricksAppUser(ctx, pool, cfg, email, user)
	})
}

// databricksAppResolver maps the platform email and forwarded IdP user value
// ("" when absent) to the durable account.
type databricksAppResolver func(
	ctx context.Context, email, forwardedUser string,
) (*auth.User, error)

func databricksAppAuthMiddleware(resolve databricksAppResolver) func(http.Handler) http.Handler {
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
			forwardedUser, ok := databricksAppForwardedUser(r)
			if !ok {
				jsonError(w, "ambiguous Databricks identity", http.StatusUnauthorized)
				return
			}
			user, err := resolve(r.Context(), email, forwardedUser)
			if msg, code, ok := databricksAppRefusal(err); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				jsonResponse(w, map[string]string{"error": msg, "code": code})
				return
			}
			if err != nil || user == nil {
				// Database/pool/context failures do not establish an identity
				// rejection. Fail closed without reusing a previous role or
				// disclosing raw resolver errors, SQL or connection details.
				slog.Warn("Databricks app account verification is unavailable")
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusServiceUnavailable)
				jsonResponse(w, map[string]string{
					"error": "Account verification is temporarily unavailable. Retry shortly; if this continues, ask an app administrator to check the metadata database and connection capacity.",
					"code":  "databricks_app_auth_unavailable",
				})
				return
			}
			ctx := context.WithValue(r.Context(), userContextKey, user)
			ctx = context.WithValue(ctx, authModeContextKey, auth.DatabricksAppMode)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// databricksAppForwardedUser returns the single X-Forwarded-User value, or
// "" when the header is absent or empty (today's email-only behavior). Two
// or more values are ambiguous, the same as duplicate emails.
func databricksAppForwardedUser(r *http.Request) (string, bool) {
	values := r.Header.Values("X-Forwarded-User")
	switch len(values) {
	case 0:
		return "", true
	case 1:
		return values[0], true
	}
	return "", false
}

// databricksAppRefusal maps a resolver error that rejects the identity to
// its 403 message and code; ok is false for any other error.
func databricksAppRefusal(err error) (msg, code string, ok bool) {
	switch {
	case errors.Is(err, auth.ErrDatabricksAppAudienceDenied):
		return "This app is restricted to the configured organization. " +
			"Sign in with an allowed organization account.", "databricks_app_audience_denied", true
	case errors.Is(err, auth.ErrOAuthLinkRequired):
		return "This Databricks identity requires a reviewed account link. " +
			"Contact an app administrator.", "databricks_app_account_link_required", true
	case errors.Is(err, auth.ErrUserDisabled):
		return "This pg_sage account is deactivated. " +
			"Ask an app administrator to reactivate it.", "databricks_app_user_disabled", true
	case errors.Is(err, auth.ErrDatabricksAppIdentityChanged):
		return "This Databricks identity no longer matches the account it was first seen with. " +
				"Ask an app administrator to review it and reset the identity pin.",
			"databricks_app_identity_changed", true
	case errors.Is(err, auth.ErrOAuthEmailUnverified):
		return "This Databricks identity could not be verified. " +
			"Contact an app administrator.", "databricks_app_identity_rejected", true
	}
	return "", "", false
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
	route(mux, "GET /api/v1/auth/mode", gatePublic,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			jsonResponse(w, map[string]string{"mode": mode})
		}))
}

func registerDatabricksAppAuthRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	// No local password, logout, OAuth/link, break-glass or account-creation
	// endpoints exist in this mode. App access is managed in Databricks; the
	// retained role endpoints govern authorization inside pg_sage.
	route(mux, "GET /api/v1/auth/me", gateViewer, http.HandlerFunc(meHandler()))
	route(mux, "GET /api/v1/users", gateAdmin, http.HandlerFunc(listUsersHandler(pool)))
	route(mux, "PUT /api/v1/users/{id}/role", gateAdmin,
		http.HandlerFunc(updateUserRoleHandler(pool)))
	route(mux, "POST /api/v1/users/{id}/identity-pin/reset", gateAdmin,
		resetDatabricksAppPinHandler(pool))
}

// resetDatabricksAppPinHandler clears a user's forwarded-identity pin after
// an administrator has reviewed a databricks_app_identity_changed event; the
// next request from that email pins its forwarded user value again.
func resetDatabricksAppPinHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id < 1 {
			jsonError(w, "invalid user ID", http.StatusBadRequest)
			return
		}
		cleared, err := auth.ResetDatabricksAppUserPin(r.Context(), pool, actorID(r), id,
			requestClientIP(r))
		if errors.Is(err, auth.ErrUserNotFound) {
			jsonError(w, "user not found", http.StatusNotFound)
			return
		}
		if err != nil {
			internalError(w, r, "reset Databricks identity pin", err)
			return
		}
		jsonResponse(w, map[string]any{"status": "reset", "previously_pinned": cleared})
	}
}
