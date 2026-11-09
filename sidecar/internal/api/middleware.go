package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// maxBodyMiddleware wraps r.Body with a size limit for
// non-GET/HEAD requests to prevent memory exhaustion.
func maxBodyMiddleware(next http.Handler) http.Handler {
	const maxBody = 1 << 20 // 1 MB
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet &&
			r.Method != http.MethodHead &&
			r.Method != http.MethodOptions {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware sets standard security headers
// on every response. HSTS and CSP are excluded as they
// require deployment-specific configuration. Cache-Control:
// no-store keeps tokens, grants and data out of browser and
// proxy caches; the SSE stream alone replaces it with no-cache.
func securityHeadersMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.Header().Set(
			"X-Content-Type-Options", "nosniff")
		w.Header().Set(
			"X-Frame-Options", "DENY")
		w.Header().Set(
			"Referrer-Policy",
			"strict-origin-when-cross-origin")
		w.Header().Set(
			"Permissions-Policy",
			"camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// defaultRequestTimeout is the per-request deadline applied by
// timeoutMiddleware. It bounds downstream work (Postgres queries,
// LLM calls) so a slow or stuck backend cannot pin a handler
// goroutine indefinitely. The value is intentionally generous —
// LLM-backed endpoints like /explain and the briefing flows can
// take ~20s on slow providers. Individual handlers that need a
// shorter deadline should apply their own context.WithTimeout.
const defaultRequestTimeout = 30 * time.Second

// timeoutMiddleware attaches a per-request context deadline. If
// the handler has not returned by the deadline, the context is
// cancelled, which propagates into pgx.Query / http.Client and
// unwinds the handler. We do NOT return a synthetic 504 from the
// middleware itself: doing so would race with the handler's own
// WriteHeader. Instead, handlers observe ctx.Err() via their
// database / HTTP calls and write an appropriate 500 via
// internalError.
func timeoutMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		// Long-lived endpoints must opt out of the request
		// deadline — cancelling a 30s-bound context on a stream
		// kills the SSE connection every half-minute.
		if r.URL.Path == "/api/v1/events" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(
			r.Context(), defaultRequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireJSONMiddleware rejects POST/PUT/PATCH requests
// that do not send Content-Type: application/json. The multipart CSV
// import is exempt; the cross-origin check, not this one, is what keeps a
// same-site HTML form from reaching it (apiauth-10).
func requireJSONMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		switch r.Method {
		case http.MethodPost, http.MethodPut,
			http.MethodPatch:
			ct := r.Header.Get("Content-Type")
			if r.URL.Path == "/api/v1/databases/managed/import" &&
				strings.HasPrefix(ct, "multipart/form-data") {
				break
			}
			if !strings.HasPrefix(ct, "application/json") {
				jsonError(w,
					"Content-Type must be application/json",
					http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
