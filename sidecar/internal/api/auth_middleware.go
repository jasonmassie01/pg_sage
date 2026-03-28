package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

type contextKey string

const userContextKey contextKey = "sage_user"

// SessionAuthMiddleware validates the sage_session cookie and sets the
// user in the request context. Skips auth for login, health, and
// static file paths.
func SessionAuthMiddleware(
	pool *pgxpool.Pool,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			if shouldSkipAuth(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := r.Cookie("sage_session")
			if err != nil {
				jsonError(w, "authentication required",
					http.StatusUnauthorized)
				return
			}

			user, err := auth.ValidateSession(
				r.Context(), pool, cookie.Value,
			)
			if err != nil {
				jsonError(w, "session invalid or expired",
					http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(
				r.Context(), userContextKey, user,
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole returns middleware that checks the user has one of the
// specified roles. Returns 403 if not.
func RequireRole(
	roles ...string,
) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			user := UserFromContext(r.Context())
			if user == nil {
				jsonError(w, "authentication required",
					http.StatusUnauthorized)
				return
			}
			if !allowed[user.Role] {
				jsonError(w, "insufficient permissions",
					http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserFromContext extracts the authenticated user from context.
func UserFromContext(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userContextKey).(*auth.User)
	return u
}

// shouldSkipAuth returns true for paths that don't require auth.
func shouldSkipAuth(path string) bool {
	switch {
	case path == "/api/v1/auth/login":
		return true
	case path == "/health":
		return true
	case !strings.HasPrefix(path, "/api/"):
		return true
	}
	return false
}
