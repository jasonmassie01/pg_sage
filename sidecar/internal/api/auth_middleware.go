package api

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

type contextKey string

const userContextKey contextKey = "sage_user"

// SessionAuthMiddleware validates the sage_session cookie and sets the
// user in the request context. Skips auth for login, health, and
// static file paths. A user whose password must change reaches only the
// change form, /auth/me and logout (apiauth-03).
func SessionAuthMiddleware(
	pool *pgxpool.Pool,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			if skipsSessionAuth(r) {
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := r.Cookie("sage_session")
			if err != nil {
				jsonError(w, "authentication required",
					http.StatusUnauthorized)
				return
			}

			// A nil pool is handled by ValidateSession: a malformed
			// cookie is still 401, a well-formed one cannot be checked.
			user, err := auth.ValidateSession(
				r.Context(), pool, cookie.Value,
			)
			if errors.Is(err, auth.ErrSessionInvalid) {
				jsonError(w, "session invalid or expired",
					http.StatusUnauthorized)
				return
			}
			if err != nil {
				// Fail closed, but do not bounce a signed-in user to
				// the login page because the session store blinked.
				writeSessionStoreUnavailable(w, r, err)
				return
			}

			ctx := context.WithValue(
				r.Context(), userContextKey, user,
			)
			requirePasswordChanged(next).ServeHTTP(w, r.WithContext(ctx))
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

// skipsSessionAuth reports a request the session middleware leaves to
// its handler: public paths and the self-authenticating transports. The
// decision reads the decoded path before routing, so only a canonical
// path can skip: "/api/v1/specialist/../users" (or its %2e%2e and %2F
// spellings) looks like a skipped prefix but is not the route it names.
func skipsSessionAuth(r *http.Request) bool {
	if !isCanonicalPath(r.URL.Path) {
		return false
	}
	return shouldSkipAuth(r.URL.Path) || isMCPTokenRequest(r) || isSpecialistRequest(r) ||
		isLintTokenRequest(r) || isPlanTokenRequest(r) ||
		isCLITokenRequest(r)
}

// isCanonicalPath reports a rooted path with no ".", ".." or empty
// segments: the path ServeMux would serve without redirecting.
func isCanonicalPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean == p
}

// UserFromContext extracts the authenticated user from context.
func UserFromContext(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userContextKey).(*auth.User)
	return u
}

// shouldSkipAuth returns true for paths that don't require auth.
func shouldSkipAuth(path string) bool {
	switch {
	case path == "/api/v1/auth/mode":
		return true
	case path == "/api/v1/auth/login":
		return true
	case path == "/api/v1/auth/oauth/callback":
		return true
	case path == "/api/v1/auth/oauth/config":
		return true
	case path == "/api/v1/auth/oauth/authorize":
		// intent=link resolves the session inside the handler (D7).
		return true
	case path == breakGlassPath:
		// Authenticated by the break-glass password in the body (E1).
		return true
	case path == oauthLinkGrantPath:
		// Authenticated by the single-use link grant in the body (D7).
		return true
	case path == "/health":
		return true
	case isSignedIngestPath(path):
		// Authenticated by the HMAC signature (signed change events, SLIs).
		return true
	case !strings.HasPrefix(path, "/api/"):
		return true
	}
	return false
}
